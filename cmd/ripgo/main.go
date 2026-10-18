package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/einfachnuralex/ripgo/internal/config"
	"github.com/einfachnuralex/ripgo/internal/ripper"
)

const version = "1.0.0"

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\nRun 'ripgo --help' for usage.\n", err)
		os.Exit(1)
	}

	cfg, cfgPath, err := config.Load()
	switch {
	case err == nil:
		fmt.Printf("\033[36m→\033[0m Config: %s\n", cfgPath)
	case errors.Is(err, config.ErrNoConfig):
		cfg = config.Defaults()
	default:
		fmt.Fprintf(os.Stderr, "\033[33m⚠\033[0m Config konnte nicht geladen werden: %v\n", err)
		cfg = config.Defaults()
	}

	// CLI flags override config values
	if opts.disc != "" {
		cfg.DiscPath = opts.disc
	}
	if opts.temp != "" {
		cfg.TempDir = opts.temp
	}

	if err := checkPrerequisites(opts.dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "prerequisite check failed: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := ripper.New(cfg, ripper.Options{
		Output:       opts.output,
		Mode:         opts.mode,
		Title:        opts.title,
		Year:         opts.year,
		Season:       opts.season,
		EpisodeStart: opts.episodeStart,
		DiscType:     opts.discType,
		Sequential:   opts.sequential,
		KeepCache:    opts.keepCache,
		ForceClean:   opts.forceClean,
		Profile:      opts.profile,
		AudioTracks:  opts.audioTracks,
		Debug:        opts.debug,
		DryRun:       opts.dryRun,
	})

	if err := r.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type cliOptions struct {
	output       string
	mode         string
	disc         string
	temp         string
	title        string
	year         int
	season       int
	episodeStart int
	discType     string
	sequential   bool
	keepCache    bool
	forceClean   bool
	profile      string
	audioTracks  []int
	debug        bool
	dryRun       bool
}

// parseAudioTracks parses a comma-separated list of stream indices, dropping duplicates.
func parseAudioTracks(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var tracks []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid audio-tracks entry %q: must be a non-negative stream index", part)
		}
		if !slices.Contains(tracks, n) {
			tracks = append(tracks, n)
		}
	}
	return tracks, nil
}

func parseArgs(args []string) (*cliOptions, error) {
	fs := flag.NewFlagSet("ripsharp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		printHelp()
		os.Exit(0)
	}

	var (
		output       = fs.String("output", "", "Output directory (required)")
		mode         = fs.String("mode", "auto", "Content type: auto, movie, tv")
		disc         = fs.String("disc", "", "Disc path")
		temp         = fs.String("temp", "", "Temporary directory for ripped files")
		title        = fs.String("title", "", "Override title for metadata lookup")
		year         = fs.Int("year", 0, "Release year")
		season       = fs.Int("season", 1, "Season number")
		episodeStart = fs.Int("episode-start", 1, "Starting episode number")
		discType     = fs.String("disc-type", "", "Override disc type: dvd, bd, uhd")
		sequential   = fs.Bool("sequential", false, "Disable parallel rip+encode pipeline")
		keepCache    = fs.Bool("keep-cache", false, "Don't delete cache after successful rip+encode")
		forceClean   = fs.Bool("force-clean", false, "Clean cache directory if not empty at startup")
		profile      = fs.String("profile", "", "Encoding profile name (defined in config file)")
		audioTracks  = fs.String("audio-tracks", "", "Audio stream indices to include, comma-separated (e.g. 1,3)")
		debug        = fs.Bool("debug", false, "Enable verbose debug output")
		dryRun       = fs.Bool("dry-run", false, "Scan disc and print commands without ripping or encoding")
		showVersion  = fs.Bool("version", false, "Show version")
	)
	fs.StringVar(output, "o", "", "Output directory (required)")
	fs.BoolVar(showVersion, "v", false, "Show version")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp()
			os.Exit(0)
		}
		return nil, err
	}

	if *showVersion {
		fmt.Printf("ripsharp %s\n", version)
		os.Exit(0)
	}

	switch *mode {
	case "auto", "movie", "tv":
	case "series":
		*mode = "tv"
	default:
		return nil, fmt.Errorf("invalid mode %q: must be auto, movie, or tv", *mode)
	}

	if *discType != "" {
		switch *discType {
		case "dvd", "bd", "uhd":
		default:
			return nil, fmt.Errorf("invalid disc-type %q: must be dvd, bd, or uhd", *discType)
		}
	}

	tracks, err := parseAudioTracks(*audioTracks)
	if err != nil {
		return nil, err
	}

	if *output == "" {
		return nil, fmt.Errorf("--output is required")
	}

	return &cliOptions{
		output:       *output,
		mode:         *mode,
		disc:         *disc,
		temp:         *temp,
		title:        *title,
		year:         *year,
		season:       *season,
		episodeStart: *episodeStart,
		discType:     *discType,
		sequential:   *sequential,
		keepCache:    *keepCache,
		forceClean:   *forceClean,
		profile:      *profile,
		audioTracks:  tracks,
		debug:        *debug,
		dryRun:       *dryRun,
	}, nil
}

// checkPrerequisites verifies the external tools are in PATH.
// A dry run only executes the disc scan, so ffmpeg/ffprobe are optional there.
func checkPrerequisites(dryRun bool) error {
	if _, err := exec.LookPath("makemkvcon"); err != nil {
		return fmt.Errorf("makemkvcon not found in PATH")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if !dryRun {
				return fmt.Errorf("%s not found in PATH", tool)
			}
			fmt.Fprintf(os.Stderr, "\033[33m⚠\033[0m %s not found in PATH (not needed for --dry-run)\n", tool)
		}
	}
	return nil
}

func printHelp() {
	fmt.Printf(`ripsharp %s - Disc ripping and encoding tool

USAGE:
    ripsharp --output <path> [options]

OPTIONS:
    --output, -o <path>        Output directory (required)
    --mode <mode>              Content type: auto, movie, tv (default: auto)
    --disc <path>              Disc path (default: disc:0)
    --temp <path>              Temporary directory for ripped files
    --title <text>             Override title for metadata lookup
    --year <year>              Release year (movies)
    --season <n>               Season number for TV mode (default: 1)
    --episode-start <n>        Starting episode number (default: 1)
    --disc-type <type>         Override disc type: dvd, bd, uhd
    --sequential               Disable parallel rip+encode pipeline
    --keep-cache               Don't delete cache after successful rip+encode
    --force-clean              Clean cache directory if not empty at startup
    --profile <name>           Encoding profile name (defined in config file)
    --audio-tracks <i,j,...>   Audio stream indices to include (e.g. 1,3); overrides the
                               language/channel selection. AAC is copied, others become AAC 160k
    --debug                    Enable verbose debug output
    --dry-run                 Scan disc and print makemkvcon/ffprobe/ffmpeg commands
                               without ripping, encoding or creating directories
    -v, --version              Show version
    -h, --help                 Show this help

ENVIRONMENT:
    TMDB_API_KEY               TMDB API key for movie/TV metadata
    OMDB_API_KEY               OMDB API key (optional fallback)
    TVDB_API_KEY               TVDB API key for episode titles

EXAMPLES:
    ripsharp --output ~/Movies
    ripsharp --output ~/TV --mode tv --title "Breaking Bad" --season 2
    ripsharp --output ~/Movies --disc "file:/path/to/movie.iso"
    ripsharp --output ~/Movies --disc disc:1 --temp /tmp/rip
    ripsharp --output ~/Movies --dry-run
    ripsharp --output ~/Movies --dry-run --audio-tracks 1,3
`, version)
}
