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
	return finalModel.(uiModel).workErr
}

// ---- parallel pipeline (rip → encode concurrently) ----

type ripJob struct {
	title      makemkv.Title
	index      int // position among selected titles (0-based)
	rippedPath string
}

func (r *Ripper) runParallel(ctx context.Context, titles []makemkv.Title, meta *metadata.Info, isTV bool, tempDir string, metaCfg metadata.Config) error {
	// ripCh carries ripped files to the encoder; buffered so the ripper can run one title ahead.
	ripCh := make(chan ripJob, 1)

	var encErrs []string
	var mu sync.Mutex
	var encWg sync.WaitGroup

	// Encoder goroutine: reads from ripCh and encodes each file.
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

	// Rip titles in the current goroutine, feeding the encoder.
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
					pb.setLabel(fmt.Sprintf("Rip  %s", caption))
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
		return r.cfg.TempDir, false, nil
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

	// Auto-detect
	det := makemkv.DetectContentType(disc.Titles)
	if det.Confidence >= 0.70 {
		printInfo("Auto-detected: %s (%.0f%% confidence)", contentLabel(det.IsTV), det.Confidence*100)
		return det.IsTV, nil
	}

	// Low confidence — ask the user (safe here: TUI not started yet)
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

	// Movie
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

// uiProg is nil during Phase 1 (scan/detect/metadata) and set for Phase 2 (rip+encode).
var uiProg *tea.Program

// barSlot identifies which of the two progress bar slots to use.
type barSlot int

const (
	slotRip barSlot = 0
	slotEnc barSlot = 1
)

// TUI message types
type msgLog struct{ text string }
type msgBarStart struct {
	slot  barSlot
	label string
}
type msgProgress struct {
	slot  barSlot
	pct   int    // -1 = label-only update
	label string // empty = no label change
}
type msgBarDone struct {
	slot  barSlot
	label string
}
type msgQuit struct{ err error }

type liveBar struct {
	label string
	pct   int
	start time.Time
}

type uiModel struct {
	slots   [2]*liveBar
	workErr error
}

func (m uiModel) Init() tea.Cmd { return nil }

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	case msgLog:
		return m, tea.Println(msg.text)
	case msgBarStart:
		m.slots[msg.slot] = &liveBar{label: msg.label, start: time.Now()}
	case msgProgress:
		if s := m.slots[msg.slot]; s != nil {
			if msg.pct >= 0 {
				s.pct = msg.pct
			}
			if msg.label != "" {
				s.label = msg.label
			}
		}
	case msgBarDone:
		elapsed := ""
		if s := m.slots[msg.slot]; s != nil {
			elapsed = time.Since(s.start).Round(time.Second).String()
		}
		m.slots[msg.slot] = nil
		line := fmt.Sprintf("  %-36s [==========] 100%% (%s)", truncate(msg.label, 36), elapsed)
		return m, tea.Println(line)
	case msgQuit:
		m.workErr = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m uiModel) View() string {
	var sb strings.Builder
	for _, s := range m.slots {
		if s == nil {
			continue
		}
		filled := s.pct / 10
		barStr := strings.Repeat("=", filled) + strings.Repeat(" ", 10-filled)
		fmt.Fprintf(&sb, "  %-36s [%s] %3d%%\n", truncate(s.label, 36), barStr, s.pct)
	}
	return sb.String()
}

// ---- progress bar handle ----

type bar struct {
	slot    barSlot
	label   string
	lastPct int
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
	pct := int(frac * 100)
	if pct == b.lastPct {
		return
	}
	b.lastPct = pct
	if uiProg != nil {
		uiProg.Send(msgProgress{slot: b.slot, pct: pct})
	}
}

func (b *bar) setLabel(label string) {
	b.label = label
	if uiProg != nil {
		uiProg.Send(msgProgress{slot: b.slot, pct: -1, label: label})
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
			func(frac float64, _ string) {
				if frac >= 0 {
					pb.set(frac)
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
