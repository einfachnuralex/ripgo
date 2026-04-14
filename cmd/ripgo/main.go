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

	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}

	// CLI flags override config values
	if opts.disc != "" {
		cfg.DiscPath = opts.disc
	}
	if opts.temp != "" {
		cfg.TempDir = opts.temp
	}

	if err := checkPrerequisites(); err != nil {
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
		Debug:        opts.debug,
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
	debug        bool
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
		debug        = fs.Bool("debug", false, "Enable verbose debug output")
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
		debug:        *debug,
	}, nil
}

func checkPrerequisites() error {
	for _, tool := range []string{"makemkvcon", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH", tool)
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
    --debug                    Enable verbose debug output
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
`, version)
}
