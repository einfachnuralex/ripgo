package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// EncodingProfile defines named ffmpeg video encoding parameters.
type EncodingProfile struct {
	Name        string
	Codec       string
	CRF         int
	Preset      string
	PixFmt      string
	VideoFilter string
}

// rawConfig mirrors the structure of the YAML config file.
type rawConfig struct {
	Disc struct {
		DefaultPath    string `yaml:"default_path"`
		DefaultTempDir string `yaml:"default_temp_dir"`
	} `yaml:"disc"`

	Output struct {
		MoviesDir string `yaml:"movies_dir"`
		TVDir     string `yaml:"tv_dir"`
	} `yaml:"output"`

	Encoding struct {
		IncludeSubtitles     bool     `yaml:"include_subtitles"`
		IncludeStereoAudio   bool     `yaml:"include_stereo_audio"`
		IncludeSurroundAudio bool     `yaml:"include_surround_audio"`
		Languages            []string `yaml:"languages"`
		MinMovieDuration     int      `yaml:"min_movie_duration_seconds"`
		MinEpisodeDuration   int      `yaml:"min_episode_duration_seconds"`
		MaxEpisodeDuration   int      `yaml:"max_episode_duration_seconds"`
		Profiles             []struct {
			Name        string `yaml:"name"`
			Codec       string `yaml:"codec"`
			CRF         int    `yaml:"crf"`
			Preset      string `yaml:"preset"`
			PixFmt      string `yaml:"pix_fmt"`
			VideoFilter string `yaml:"video_filter"`
		} `yaml:"profiles"`
	} `yaml:"encoding"`

	Metadata struct {
		LookupEnabled bool   `yaml:"lookup_enabled"`
		TMDBKey       string `yaml:"tmdb_api_key"`
		OMDBKey       string `yaml:"omdb_api_key"`
		TVDBKey       string `yaml:"tvdb_api_key"`
	} `yaml:"metadata"`
}

// Config holds all runtime settings merged from the config file and environment variables.
type Config struct {
	DiscPath           string
	TempDir            string
	MoviesDir          string
	TVDir              string
	Languages          []string
	Subtitles          bool
	StereoAudio        bool
	SurroundAudio      bool
	MinMovieDuration   int // seconds
	MinEpisodeDuration int // seconds
	MaxEpisodeDuration int // seconds
	MetadataEnabled    bool
	TMDBKey            string
	OMDBKey            string
	TVDBKey            string
	EncodingProfiles   []EncodingProfile
}

// Defaults returns a Config with sensible default values.
func Defaults() *Config {
	return &Config{
		DiscPath:           "disc:0",
		Languages:          []string{"eng", "ger"},
		StereoAudio:        true,
		SurroundAudio:      true,
		MinMovieDuration:   1800, // 30 min
		MinEpisodeDuration: 1200, // 20 min
		MaxEpisodeDuration: 3600, // 60 min
		MetadataEnabled:    true,
		EncodingProfiles: []EncodingProfile{
			{
				Name:        "h264_crf22_slow",
				Codec:       "libx264",
				CRF:         22,
				Preset:      "slow",
				PixFmt:      "yuv420p",
				VideoFilter: "bwdif=mode=send_frame:parity=auto:deint=interlaced",
			},
		},
	}
}

// Load finds and parses the config file, then applies environment variable overrides.
func Load() (*Config, error) {
	path, err := findFile()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var raw rawConfig

	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg := Defaults()

	if raw.Disc.DefaultPath != "" {
		cfg.DiscPath = raw.Disc.DefaultPath
	}
	if raw.Disc.DefaultTempDir != "" {
		cfg.TempDir = expandHome(raw.Disc.DefaultTempDir)
	}
	if raw.Output.MoviesDir != "" {
		cfg.MoviesDir = expandHome(raw.Output.MoviesDir)
	}
	if raw.Output.TVDir != "" {
		cfg.TVDir = expandHome(raw.Output.TVDir)
	}
	if len(raw.Encoding.Languages) > 0 {
		cfg.Languages = raw.Encoding.Languages
	}
	cfg.Subtitles = raw.Encoding.IncludeSubtitles
	cfg.StereoAudio = raw.Encoding.IncludeStereoAudio
	cfg.SurroundAudio = raw.Encoding.IncludeSurroundAudio
	if raw.Encoding.MinMovieDuration > 0 {
		cfg.MinMovieDuration = raw.Encoding.MinMovieDuration
	}
	if raw.Encoding.MinEpisodeDuration > 0 {
		cfg.MinEpisodeDuration = raw.Encoding.MinEpisodeDuration
	}
	if raw.Encoding.MaxEpisodeDuration > 0 {
		cfg.MaxEpisodeDuration = raw.Encoding.MaxEpisodeDuration
	}
	if len(raw.Encoding.Profiles) > 0 {
		cfg.EncodingProfiles = make([]EncodingProfile, len(raw.Encoding.Profiles))
		for i, p := range raw.Encoding.Profiles {
			cfg.EncodingProfiles[i] = EncodingProfile{
				Name:        p.Name,
				Codec:       p.Codec,
				CRF:         p.CRF,
				Preset:      p.Preset,
				PixFmt:      p.PixFmt,
				VideoFilter: p.VideoFilter,
			}
		}
	}
	cfg.MetadataEnabled = raw.Metadata.LookupEnabled
	cfg.TMDBKey = raw.Metadata.TMDBKey
	cfg.OMDBKey = raw.Metadata.OMDBKey
	cfg.TVDBKey = raw.Metadata.TVDBKey

	// Environment variables override config file
	if v := os.Getenv("TMDB_API_KEY"); v != "" {
		cfg.TMDBKey = v
	}
	if v := os.Getenv("OMDB_API_KEY"); v != "" {
		cfg.OMDBKey = v
	}
	if v := os.Getenv("TVDB_API_KEY"); v != "" {
		cfg.TVDBKey = v
	}

	return cfg, nil
}

func findFile() (string, error) {
	for _, p := range searchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config file found")
}

func searchPaths() []string {
	var paths []string
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			paths = append(paths, filepath.Join(appdata, "ripgo", "config.yaml"))
		}
		if home != "" {
			paths = append(paths, filepath.Join(home, ".ripgo.yaml"))
		}
	default: // linux, darwin
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" && home != "" {
			xdg = filepath.Join(home, ".config")
		}
		if xdg != "" {
			paths = append(paths, filepath.Join(xdg, "ripgo", "config.yaml"))
		}
		if home != "" {
			paths = append(paths, filepath.Join(home, ".ripgo.yaml"))
		}
	}

	paths = append(paths, "ripgo.yaml", "config.yaml")
	return paths
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
