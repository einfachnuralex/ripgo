package ripper

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

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
	cfg      *config.Config
	opts     Options
	tempAuto bool
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

	tempDir, err := r.prepareTempDir()
	if err != nil {
		return err
	}
	if r.tempAuto {
		defer os.RemoveAll(tempDir)
	}

	// Scan disc
	printInfo("Scanning disc %s…", r.cfg.DiscPath)
	disc, err := makemkv.ScanDisc(ctx, r.cfg.DiscPath, r.opts.Debug)
	if err != nil {
		return fmt.Errorf("scanning disc: %w", err)
	}
	if r.opts.DiscType != "" {
		disc.Type = r.opts.DiscType
	}
	printOK("Disc: %s (%s), %d title(s) found", disc.Name, strings.ToUpper(disc.Type), len(disc.Titles))

	// Determine content type
	isTV, err := r.resolveContentType(disc)
	if err != nil {
		return err
	}

	// Filter to relevant titles
	titles := makemkv.FilterTitles(disc.Titles, isTV,
		r.cfg.MinMovieDuration, r.cfg.MinEpisodeDuration, r.cfg.MaxEpisodeDuration)
	if len(titles) == 0 {
		return fmt.Errorf("no titles found matching duration criteria")
	}
	printInfo("Processing %d title(s) as %s", len(titles), contentLabel(isTV))

	// Metadata lookup
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

	if r.opts.Sequential {
		return r.runSequential(ctx, titles, meta, isTV, tempDir, metaCfg)
	}
	return r.runParallel(ctx, titles, meta, isTV, tempDir, metaCfg)
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

		pb := newBar(fmt.Sprintf("Rip  %s", label))
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

		pb := newBar(fmt.Sprintf("Rip  %s", label))
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

	pb := newBar(fmt.Sprintf("Enc  %s", label))
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

func (r *Ripper) prepareTempDir() (string, error) {
	if r.cfg.TempDir != "" {
		if err := os.MkdirAll(r.cfg.TempDir, 0o755); err != nil {
			return "", fmt.Errorf("creating temp dir: %w", err)
		}
		return r.cfg.TempDir, nil
	}

	// Auto-generate a unique temp directory
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("ripsharp-%x", rand.Int63()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	r.tempAuto = true
	return dir, nil
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

	// Low confidence — ask the user
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

func buildOutputPath(dir string, meta *metadata.Info, isTV bool, season, episode int, epTitle string, titleIndex int, hasTitleName bool) string {
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
	// Disambiguate multiple titles (alternate cuts, bonus discs, etc.)
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

// ---- terminal progress bar ----

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

type bar struct {
	mu      sync.Mutex
	label   string
	lastPct int
	start   time.Time
}

func newBar(label string) *bar {
	b := &bar{label: label, start: time.Now()}
	fmt.Printf("  %-36s [          ]   0%%\r", truncate(label, 36))
	return b
}

func (b *bar) set(frac float64) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	pct := int(frac * 100)

	b.mu.Lock()
	defer b.mu.Unlock()
	if pct == b.lastPct {
		return
	}
	b.lastPct = pct

	filled := pct / 10
	barStr := strings.Repeat("=", filled) + strings.Repeat(" ", 10-filled)
	fmt.Printf("  %-36s [%s] %3d%%\r", truncate(b.label, 36), barStr, pct)
}

func (b *bar) setLabel(label string) {
	b.mu.Lock()
	b.label = label
	b.mu.Unlock()
}

func (b *bar) done() {
	elapsed := time.Since(b.start).Round(time.Second)
	fmt.Printf("  %-36s [==========] 100%% (%s)\n", truncate(b.label, 36), elapsed)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func printOK(format string, args ...any) {
	fmt.Printf(colorGreen+"✓ "+colorReset+format+"\n", args...)
}

func printErr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, colorRed+"✗ "+colorReset+format+"\n", args...)
}

func printWarn(format string, args ...any) {
	fmt.Printf(colorYellow+"⚠ "+colorReset+format+"\n", args...)
}

func printInfo(format string, args ...any) {
	fmt.Printf(colorCyan+"→ "+colorReset+format+"\n", args...)
}
