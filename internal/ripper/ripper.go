package ripper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/einfachnuralex/ripgo/internal/config"
	"github.com/einfachnuralex/ripgo/internal/encoder"
	"github.com/einfachnuralex/ripgo/internal/makemkv"
	"github.com/einfachnuralex/ripgo/internal/metadata"
)

// Options carries the runtime parameters parsed from the CLI.
type Options struct {
	Output       string
	Mode         string // "auto", "movie", "tv"
	Title        string
	Year         int
	Season       int
	EpisodeStart int
	DiscType     string
	Sequential   bool
	KeepCache    bool
	ForceClean   bool
	Debug        bool
}

// Ripper orchestrates disc scanning, ripping, encoding, and file naming.
type Ripper struct {
	cfg  *config.Config
	opts Options
}

// New creates a Ripper with the given config and options.
func New(cfg *config.Config, opts Options) *Ripper {
	return &Ripper{cfg: cfg, opts: opts}
}

// Run executes the full rip-and-encode pipeline.
func (r *Ripper) Run(ctx context.Context) error {
	if err := os.MkdirAll(r.opts.Output, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	tempDir, autoTemp, err := r.prepareTempDir()
	if err != nil {
		return err
	}
	if autoTemp {
		defer os.RemoveAll(tempDir)
	}
	printInfo("Cache directory: %s", tempDir)

	// ---- Phase 1: scan/detect/metadata (plain stdout, no TUI) ----

	printInfo("Scanning disc %s…", r.cfg.DiscPath)
	disc, err := makemkv.ScanDisc(ctx, r.cfg.DiscPath, r.opts.Debug)
	if err != nil {
		return fmt.Errorf("scanning disc: %w", err)
	}
	if r.opts.DiscType != "" {
		disc.Type = r.opts.DiscType
	}
	printOK("Disc: %s (%s), %d title(s) found", disc.Name, strings.ToUpper(disc.Type), len(disc.Titles))

	// resolveContentType may call fmt.Scanln — must run before TUI takes the terminal.
	isTV, err := r.resolveContentType(disc)
	if err != nil {
		return err
	}

	titles := makemkv.FilterTitles(disc.Titles, isTV,
		r.cfg.MinMovieDuration, r.cfg.MinEpisodeDuration, r.cfg.MaxEpisodeDuration)
	if len(titles) == 0 {
		return fmt.Errorf("no titles found matching duration criteria")
	}
	printInfo("Processing %d title(s) as %s", len(titles), contentLabel(isTV))

	sourceTitle := r.opts.Title
	if sourceTitle == "" {
		sourceTitle = disc.Name
	}
	if sourceTitle == "" {
		if isTV {
			sourceTitle = "TV Episode"
		} else {
			sourceTitle = "Movie"
		}
		printWarn("No title available; using fallback %q", sourceTitle)
	}

	metaCfg := metadata.Config{
		TMDBKey: r.cfg.TMDBKey,
		OMDBKey: r.cfg.OMDBKey,
		TVDBKey: r.cfg.TVDBKey,
		Enabled: r.cfg.MetadataEnabled,
	}
	meta, err := metadata.Lookup(ctx, sourceTitle, r.opts.Year, isTV, metaCfg)
	if err != nil || meta == nil {
		printWarn("Metadata lookup failed; using disc title")
		meta = &metadata.Info{Title: sourceTitle, Year: r.opts.Year}
	} else {
		printOK("Metadata: %s (%d)", meta.Title, meta.Year)
	}

	// ---- Phase 2: rip+encode with bubbletea TUI ----

	p := tea.NewProgram(uiModel{}, tea.WithOutput(os.Stdout))
	uiProg = p

	go func() {
		var workErr error
		if r.opts.Sequential {
			workErr = r.runSequential(ctx, titles, meta, isTV, tempDir, metaCfg)
		} else {
			workErr = r.runParallel(ctx, titles, meta, isTV, tempDir, metaCfg)
		}
		p.Send(msgQuit{err: workErr})
	}()

	finalModel, runErr := p.Run()
	uiProg = nil
	if runErr != nil {
		return fmt.Errorf("tui: %w", runErr)
	}

	workErr := finalModel.(uiModel).workErr

	// Clean up user-specified cache on success (auto-temp handled by defer above)
	if !autoTemp && workErr == nil && !r.opts.KeepCache {
		if err := os.RemoveAll(tempDir); err != nil {
			printWarn("Failed to clean cache: %v", err)
		} else {
			printOK("Cleaned cache directory %s", tempDir)
		}
	}

	return workErr
}

// ---- parallel pipeline (rip → encode concurrently) ----

type ripJob struct {
	title      makemkv.Title
	index      int // position among selected titles (0-based)
	rippedPath string
}

func (r *Ripper) runParallel(ctx context.Context, titles []makemkv.Title, meta *metadata.Info, isTV bool, tempDir string, metaCfg metadata.Config) error {
	if uiProg != nil {
		uiProg.Send(msgOverallInit{totalSteps: len(titles) * 2})
	}

	ripCh := make(chan ripJob, 1)

	var encErrs []string
	var mu sync.Mutex
	var encWg sync.WaitGroup

	encWg.Add(1)
	go func() {
		defer encWg.Done()
		for job := range ripCh {
			if ctx.Err() != nil {
				return
			}
			if err := r.encodeOne(ctx, job, meta, isTV, r.opts.Output, metaCfg); err != nil {
				mu.Lock()
				encErrs = append(encErrs, err.Error())
				mu.Unlock()
			}
		}
	}()

	for i, title := range titles {
		if ctx.Err() != nil {
			break
		}

		titleDir := filepath.Join(tempDir, fmt.Sprintf("title_%02d", title.ID))
		if err := os.MkdirAll(titleDir, 0o755); err != nil {
			printErr("Failed to create dir for title %d: %v", title.ID, err)
			continue
		}

		label := episodeLabel(i, isTV, r.opts.Season, r.opts.EpisodeStart, title)
		printInfo("[%d/%d] Ripping %s…", i+1, len(titles), label)

		pb := newBar(slotRip, fmt.Sprintf("Rip  %s", label))
		err := makemkv.RipTitle(ctx, r.cfg.DiscPath, title.ID, titleDir,
			func(frac float64, caption string) {
				if frac >= 0 {
					pb.set(frac)
				}
				if caption != "" {
					pb.setCaption(caption)
				}
			}, r.opts.Debug)
		pb.done()

		if err != nil {
			printErr("Rip failed for title %d: %v", title.ID, err)
			continue
		}

		rippedPath, err := makemkv.FindRippedFile(titleDir)
		if err != nil {
			printErr("Cannot find ripped MKV for title %d: %v", title.ID, err)
			continue
		}

		ripCh <- ripJob{title: title, index: i, rippedPath: rippedPath}
	}

	close(ripCh)
	encWg.Wait()

	if len(encErrs) > 0 {
		return fmt.Errorf("%d title(s) failed to encode: %s", len(encErrs), strings.Join(encErrs, "; "))
	}
	return nil
}

// ---- per-title encoding + naming ----

func (r *Ripper) encodeOne(ctx context.Context, job ripJob, meta *metadata.Info, isTV bool, outputDir string, metaCfg metadata.Config) error {
	label := episodeLabel(job.index, isTV, r.opts.Season, r.opts.EpisodeStart, job.title)
	printInfo("Encoding %s…", label)

	var epTitle string
	if isTV {
		ep := r.opts.EpisodeStart + job.index
		if t, err := metadata.EpisodeTitle(ctx, meta.Title, r.opts.Season, ep, metaCfg); err == nil {
			epTitle = t
		}
	}

	outPath := buildOutputPath(outputDir, meta, isTV, r.opts.Season, r.opts.EpisodeStart+job.index, epTitle, job.index, len(job.title.Name) > 0)

	settings := encoder.EncodeSettings{
		Languages:     r.cfg.Languages,
		Subtitles:     r.cfg.Subtitles,
		StereoAudio:   r.cfg.StereoAudio,
		SurroundAudio: r.cfg.SurroundAudio,
	}

	pb := newBar(slotEnc, fmt.Sprintf("Enc  %s", label))
	err := encoder.Encode(ctx, job.rippedPath, outPath, settings,
		func(frac float64) { pb.set(frac) },
		r.opts.Debug)
	pb.done()

	if err != nil {
		return fmt.Errorf("encoding %s: %w", label, err)
	}

	printOK("→ %s", outPath)
	return nil
}

// ---- temp dir management ----

func (r *Ripper) prepareTempDir() (dir string, autoCreated bool, err error) {
	if r.cfg.TempDir != "" {
		if err := os.MkdirAll(r.cfg.TempDir, 0o755); err != nil {
			return "", false, fmt.Errorf("creating temp dir: %w", err)
		}

		entries, err := os.ReadDir(r.cfg.TempDir)
		if err != nil {
			return "", false, fmt.Errorf("reading cache dir: %w", err)
		}
		if len(entries) > 0 {
			if !r.opts.ForceClean {
				return "", false, fmt.Errorf("cache directory %s is not empty (use --force-clean to clear)", r.cfg.TempDir)
			}
			for _, e := range entries {
				os.RemoveAll(filepath.Join(r.cfg.TempDir, e.Name()))
			}
			printInfo("Cleaned cache directory %s", r.cfg.TempDir)
		}

		stamped := filepath.Join(r.cfg.TempDir, time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(stamped, 0o755); err != nil {
			return "", false, fmt.Errorf("creating timestamped cache dir: %w", err)
		}
		return stamped, false, nil
	}

	dir, err = os.MkdirTemp("", "ripgo-")
	if err != nil {
		return "", false, fmt.Errorf("creating temp dir: %w", err)
	}
	return dir, true, nil
}

// ---- content type resolution ----

func (r *Ripper) resolveContentType(disc *makemkv.DiscInfo) (bool, error) {
	switch r.opts.Mode {
	case "movie":
		return false, nil
	case "tv":
		return true, nil
	}

	det := makemkv.DetectContentType(disc.Titles)
	if det.Confidence >= 0.70 {
		printInfo("Auto-detected: %s (%.0f%% confidence)", contentLabel(det.IsTV), det.Confidence*100)
		return det.IsTV, nil
	}

	hint := ""
	if det.Confidence > 0 {
		hint = fmt.Sprintf(" (detected %s, %.0f%% confidence)", contentLabel(det.IsTV), det.Confidence*100)
	}
	fmt.Printf("\n%sIs this a TV series?%s [y/N]: ", colorYellow, hint+colorReset)

	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// ---- output path construction ----

var invalidChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitize(s string) string {
	return strings.TrimSpace(invalidChars.ReplaceAllString(s, "_"))
}

func buildOutputPath(dir string, meta *metadata.Info, isTV bool, season, episode int, epTitle string, titleIndex int, _ bool) string {
	if isTV {
		name := fmt.Sprintf("%s - S%02dE%02d", sanitize(meta.Title), season, episode)
		if epTitle != "" {
			name += " - " + sanitize(epTitle)
		}
		return filepath.Join(dir, name+".mkv")
	}

	base := sanitize(meta.Title)
	if meta.Year > 0 {
		base = fmt.Sprintf("%s (%d)", base, meta.Year)
	}
	if titleIndex > 0 {
		base = fmt.Sprintf("%s - title%02d", base, titleIndex+1)
	}
	return filepath.Join(dir, base+".mkv")
}

// ---- display helpers ----

func episodeLabel(index int, isTV bool, season, episodeStart int, title makemkv.Title) string {
	if isTV {
		return fmt.Sprintf("S%02dE%02d", season, episodeStart+index)
	}
	if title.Name != "" {
		return title.Name
	}
	if index == 0 {
		return "Main Feature"
	}
	return fmt.Sprintf("Title %d", index+1)
}

func contentLabel(isTV bool) string {
	if isTV {
		return "TV series"
	}
	return "movie"
}

// ---- bubbletea TUI ----

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

var uiProg *tea.Program

type barSlot int

const (
	slotRip barSlot = 0
	slotEnc barSlot = 1
)

// ---- TUI message types ----

type msgLog struct{ text string }
type msgBarStart struct {
	slot  barSlot
	label string
}
type msgProgress struct {
	slot    barSlot
	frac    float64 // 0.0–1.0; negative = no fraction update
	label   string  // empty = no label change
	caption string  // empty = no caption change
}
type msgBarDone struct {
	slot  barSlot
	label string
}
type msgOverallInit struct {
	totalSteps int // len(titles) * 2
}
type msgQuit struct{ err error }

// ---- TUI model state ----

type liveBar struct {
	label    string
	frac     float64
	start    time.Time
	captions []string
}

const maxCaptions = 3

func (lb *liveBar) pushCaption(c string) {
	if c == "" {
		return
	}
	lb.captions = append(lb.captions, c)
	if len(lb.captions) > maxCaptions {
		lb.captions = lb.captions[len(lb.captions)-maxCaptions:]
	}
}

type uiModel struct {
	slots          [2]*liveBar
	termWidth      int
	totalSteps     int
	completedSteps int
	overallStart   time.Time
	workErr        error
}

// ---- lipgloss styles ----

var (
	ripPanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("2")).
			Padding(0, 1)

	encPanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("6")).
			Padding(0, 1)

	overallPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("3")).
				Padding(0, 1)

	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	boldStyle  = lipgloss.NewStyle().Bold(true)
)

// ---- render helpers ----

func renderProgressBar(frac float64, width int) string {
	if width < 1 {
		width = 1
	}
	filled := int(frac * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatETA(elapsed time.Duration, frac float64) string {
	if frac <= 0.01 {
		return ""
	}
	total := time.Duration(float64(elapsed) / frac)
	remaining := total - elapsed
	if remaining < time.Second {
		remaining = 0
	}
	return "~" + formatElapsed(remaining) + " left"
}

func renderPanel(heading string, lb *liveBar, style lipgloss.Style, width int) string {
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 20 {
		innerWidth = 20
	}

	var content strings.Builder

	if lb == nil {
		content.WriteString(boldStyle.Render(heading))
		content.WriteByte('\n')
		content.WriteString(dimStyle.Render("Waiting…"))
		return style.Width(innerWidth).Render(content.String())
	}

	content.WriteString(boldStyle.Render(heading))
	content.WriteByte('\n')
	content.WriteString(truncate(lb.label, innerWidth))
	content.WriteByte('\n')

	elapsed := time.Since(lb.start)
	pctText := fmt.Sprintf(" %5.1f%%", lb.frac*100)
	elapsedText := "  " + formatElapsed(elapsed)
	etaText := ""
	if eta := formatETA(elapsed, lb.frac); eta != "" {
		etaText = "  " + eta
	}
	suffix := pctText + elapsedText + etaText

	barWidth := innerWidth - len(suffix)
	if barWidth < 5 {
		barWidth = 5
	}

	content.WriteString(renderProgressBar(lb.frac, barWidth))
	content.WriteString(suffix)

	for _, c := range lb.captions {
		content.WriteByte('\n')
		content.WriteString(dimStyle.Render(truncate(c, innerWidth)))
	}

	return style.Width(innerWidth).Render(content.String())
}

func renderOverallPanel(m uiModel, width int) string {
	innerWidth := width - overallPanelStyle.GetHorizontalFrameSize()
	if innerWidth < 20 {
		innerWidth = 20
	}

	var content strings.Builder
	content.WriteString(boldStyle.Render("Overall Progress"))
	content.WriteByte('\n')

	if m.totalSteps == 0 {
		content.WriteString(dimStyle.Render("Waiting…"))
		return overallPanelStyle.Width(innerWidth).Render(content.String())
	}

	frac := float64(m.completedSteps) / float64(m.totalSteps)
	if frac > 1 {
		frac = 1
	}

	elapsed := time.Since(m.overallStart)
	stepsText := fmt.Sprintf(" %d/%d", m.completedSteps, m.totalSteps)
	pctText := fmt.Sprintf("  %5.1f%%", frac*100)
	elapsedText := "  " + formatElapsed(elapsed)
	etaText := ""
	if eta := formatETA(elapsed, frac); eta != "" {
		etaText = "  " + eta
	}
	suffix := stepsText + pctText + elapsedText + etaText

	barWidth := innerWidth - len(suffix)
	if barWidth < 5 {
		barWidth = 5
	}

	content.WriteString(renderProgressBar(frac, barWidth))
	content.WriteString(suffix)

	return overallPanelStyle.Width(innerWidth).Render(content.String())
}

// ---- TUI Init / Update / View ----

func (m uiModel) Init() tea.Cmd { return nil }

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
	case msgLog:
		return m, tea.Println(msg.text)
	case msgOverallInit:
		m.totalSteps = msg.totalSteps
		m.overallStart = time.Now()
	case msgBarStart:
		m.slots[msg.slot] = &liveBar{label: msg.label, start: time.Now()}
	case msgProgress:
		if s := m.slots[msg.slot]; s != nil {
			if msg.frac >= 0 {
				s.frac = msg.frac
			}
			if msg.label != "" {
				s.label = msg.label
			}
			s.pushCaption(msg.caption)
		}
	case msgBarDone:
		elapsed := ""
		if s := m.slots[msg.slot]; s != nil {
			elapsed = formatElapsed(time.Since(s.start))
		}
		m.slots[msg.slot] = nil
		m.completedSteps++
		line := fmt.Sprintf("  %-36s  100.0%%  (%s)", truncate(msg.label, 36), elapsed)
		return m, tea.Println(line)
	case msgQuit:
		m.workErr = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m uiModel) View() string {
	w := m.termWidth
	if w == 0 {
		w = 80
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		renderPanel("Ripping", m.slots[slotRip], ripPanelStyle, w),
		renderPanel("Encoding", m.slots[slotEnc], encPanelStyle, w),
		renderOverallPanel(m, w),
	) + "\n"
}

// ---- progress bar handle ----

type bar struct {
	slot     barSlot
	label    string
	lastPerm int // 0–1000 for 0.1% dedup
}

func newBar(slot barSlot, label string) *bar {
	if uiProg != nil {
		uiProg.Send(msgBarStart{slot: slot, label: label})
	}
	return &bar{slot: slot, label: label}
}

func (b *bar) set(frac float64) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	perm := int(frac * 1000)
	if perm == b.lastPerm {
		return
	}
	b.lastPerm = perm
	if uiProg != nil {
		uiProg.Send(msgProgress{slot: b.slot, frac: frac})
	}
}

func (b *bar) setLabel(label string) {
	b.label = label
	if uiProg != nil {
		uiProg.Send(msgProgress{slot: b.slot, frac: -1, label: label})
	}
}

func (b *bar) setCaption(caption string) {
	if uiProg != nil {
		uiProg.Send(msgProgress{slot: b.slot, frac: -1, caption: caption})
	}
}

func (b *bar) done() {
	if uiProg != nil {
		uiProg.Send(msgBarDone{slot: b.slot, label: b.label})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func printOK(format string, args ...any) {
	line := colorGreen + "✓ " + colorReset + fmt.Sprintf(format, args...)
	if uiProg != nil {
		uiProg.Send(msgLog{text: line})
	} else {
		fmt.Println(line)
	}
}

func printErr(format string, args ...any) {
	line := colorRed + "✗ " + colorReset + fmt.Sprintf(format, args...)
	if uiProg != nil {
		uiProg.Send(msgLog{text: line})
	} else {
		fmt.Println(line)
	}
}

func printWarn(format string, args ...any) {
	line := colorYellow + "⚠ " + colorReset + fmt.Sprintf(format, args...)
	if uiProg != nil {
		uiProg.Send(msgLog{text: line})
	} else {
		fmt.Println(line)
	}
}

func printInfo(format string, args ...any) {
	line := colorCyan + "→ " + colorReset + fmt.Sprintf(format, args...)
	if uiProg != nil {
		uiProg.Send(msgLog{text: line})
	} else {
		fmt.Println(line)
	}
}

// ---- sequential pipeline (rip all, then encode all) ----

func (r *Ripper) runSequential(ctx context.Context, titles []makemkv.Title, meta *metadata.Info, isTV bool, tempDir string, metaCfg metadata.Config) error {
	if uiProg != nil {
		uiProg.Send(msgOverallInit{totalSteps: len(titles) * 2})
	}

	var ripped []ripJob

	for i, title := range titles {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		titleDir := filepath.Join(tempDir, fmt.Sprintf("title_%02d", title.ID))
		if err := os.MkdirAll(titleDir, 0o755); err != nil {
			return fmt.Errorf("creating title dir: %w", err)
		}

		label := episodeLabel(i, isTV, r.opts.Season, r.opts.EpisodeStart, title)
		printInfo("[%d/%d] Ripping %s…", i+1, len(titles), label)

		pb := newBar(slotRip, fmt.Sprintf("Rip  %s", label))
		err := makemkv.RipTitle(ctx, r.cfg.DiscPath, title.ID, titleDir,
			func(frac float64, caption string) {
				if frac >= 0 {
					pb.set(frac)
				}
				if caption != "" {
					pb.setCaption(caption)
				}
			}, r.opts.Debug)
		pb.done()

		if err != nil {
			return fmt.Errorf("ripping title %d: %w", title.ID, err)
		}

		path, err := makemkv.FindRippedFile(titleDir)
		if err != nil {
			return fmt.Errorf("finding ripped file: %w", err)
		}
		ripped = append(ripped, ripJob{title: title, index: i, rippedPath: path})
	}

	for _, job := range ripped {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.encodeOne(ctx, job, meta, isTV, r.opts.Output, metaCfg); err != nil {
			return err
		}
	}
	return nil
}
