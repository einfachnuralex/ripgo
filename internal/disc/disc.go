// Package disc scans DVDs and Blu-rays and rips single titles with ffmpeg.
//
// The disc structure is read by ripgo itself (UDF, VIDEO_TS.IFO, BDMV/*.mpls);
// ffmpeg reads the content through its dvdvideo demuxer (libdvdnav, CSS via
// libdvdcss) and bluray protocol (libbluray, AACS via libaacs).
package disc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/einfachnuralex/ripgo/internal/ffmpeg"
)

// Title represents a single title (DVD) or playlist (Blu-ray) of the disc.
type Title struct {
	// ID is the DVD title number (1-based, ffmpeg -title) or the Blu-ray
	// playlist number (e.g. 800 for 00800.mpls, ffmpeg -playlist).
	ID int
	// SourceFile is the Blu-ray playlist file, e.g. "00800.mpls"; empty for DVDs.
	SourceFile string
	Chapters   int // chapter count
	Duration   int // seconds
	Streams    []Stream

	chapters []uint64 // Blu-ray chapter starts in 45 kHz ticks
}

// Stream describes a stream of a title as it will appear in the ripped file:
// Index is the stream index in the ripped MKV. Codec uses ffprobe-style names
// (e.g. "ac3", "h264") so it can be fed to the encoder.
type Stream struct {
	Index    int
	Source   string // ffmpeg -map specifier on the disc input, e.g. "0:3" or "0:i:0x1100"
	Type     string // "video", "audio", "subtitle"
	Codec    string
	Language string
	Width    int
	Height   int
	Channels int
}

// DiscInfo holds the result of scanning a disc.
type DiscInfo struct {
	Source   string // normalized source (device, image or directory)
	Name     string
	Type     string // "dvd", "bd", "uhd"
	Titles   []Title
	Warnings []string // non-fatal problems found during the scan
}

// Detection holds the result of content-type auto-detection.
type Detection struct {
	IsTV       bool
	Confidence float64 // 0.0 - 1.0
}

// ---- source ----

var legacyDisc = regexp.MustCompile(`^disc:(\d+)$`)

// NormalizeSource converts the MakeMKV-style "disc:N" and "file:PATH" notations
// to a device or file path.
func NormalizeSource(s string) string {
	if m := legacyDisc.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return DefaultDevice(n)
	}
	if rest, ok := strings.CutPrefix(s, "file:"); ok {
		return rest
	}
	return s
}

// DefaultDevice returns the Linux device path of the n-th optical drive.
func DefaultDevice(n int) string {
	return fmt.Sprintf("/dev/sr%d", n)
}

// discFS gives read access to the files of a disc.
type discFS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]string, error)
	Label() string
	Close() error
}

// dirFS is a disc copied to (or mounted at) a directory.
type dirFS string

func (d dirFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(d), filepath.FromSlash(name)))
}

func (d dirFS) ReadDir(name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(string(d), filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

func (d dirFS) Label() string { return filepath.Base(string(d)) }
func (d dirFS) Close() error  { return nil }

func openFS(source string) (discFS, error) {
	st, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return dirFS(source), nil
	}
	return openUDF(source)
}

// ---- scan ----

// Scan reads the disc structure of source (device, ISO image or directory).
// discType ("dvd", "bd", "uhd") overrides the detection when non-empty.
func Scan(ctx context.Context, source, discType string, debug bool) (*DiscInfo, error) {
	fsys, err := openFS(source)
	if err != nil {
		return nil, err
	}
	defer fsys.Close()

	info := &DiscInfo{Source: source, Name: fsys.Label(), Type: discType}
	if info.Type == "" {
		if info.Type, err = detectType(fsys); err != nil {
			return nil, err
		}
	}
	if err := CheckSupport(info.Type); err != nil {
		return nil, err
	}

	if info.Type == "dvd" {
		err = scanDVD(ctx, fsys, info, debug)
	} else {
		err = scanBluray(fsys, info)
	}
	if err != nil {
		return nil, err
	}
	if len(info.Titles) == 0 {
		return nil, errors.New("no titles found on the disc")
	}
	return info, nil
}

func detectType(fsys discFS) (string, error) {
	if b, err := fsys.ReadFile("BDMV/index.bdmv"); err == nil {
		if len(b) >= 8 && string(b[4:8]) == "0300" {
			return "uhd", nil
		}
		return "bd", nil
	}
	if _, err := fsys.ReadFile("VIDEO_TS/VIDEO_TS.IFO"); err == nil {
		return "dvd", nil
	}
	return "", errors.New("neither a DVD (VIDEO_TS) nor a Blu-ray (BDMV) found")
}

// CheckSupport verifies that ffprobe was built with the input needed for discType
// (dvdvideo demuxer for DVDs, bluray protocol for Blu-rays).
func CheckSupport(discType string) error {
	flag, name, lib := "-demuxers", "dvdvideo", "libdvdnav/libdvdread"
	if discType != "dvd" {
		flag, name, lib = "-protocols", "bluray", "libbluray"
	}
	out, err := exec.Command("ffprobe", "-hide_banner", flag).Output()
	if err != nil {
		return fmt.Errorf("ffprobe %s: %w", flag, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, f := range strings.Fields(line) {
			if f == name {
				return nil
			}
		}
	}
	return fmt.Errorf("ffmpeg has no %s support (built without %s)", name, lib)
}

// ---- rip ----

// RipFile returns the path of the ripped MKV inside titleDir.
func RipFile(titleDir string) string { return filepath.Join(titleDir, "title.mkv") }

// ChaptersFile returns the path of the FFMETADATA chapter file inside titleDir
// (Blu-ray only; DVD chapters come from the dvdvideo demuxer).
func ChaptersFile(titleDir string) string { return filepath.Join(titleDir, "chapters.txt") }

// ChapterMetadata returns the FFMETADATA1 content with the chapters of t, or ""
// if there are none.
func ChapterMetadata(t Title) string {
	if len(t.chapters) == 0 {
		return ""
	}
	end := uint64(t.Duration) * mplsTicks
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for i, start := range t.chapters {
		stop := end
		if i+1 < len(t.chapters) {
			stop = t.chapters[i+1]
		}
		fmt.Fprintf(&b, "\n[CHAPTER]\nTIMEBASE=1/%d\nSTART=%d\nEND=%d\ntitle=Chapter %02d\n", mplsTicks, start, max(stop, start), i+1)
	}
	return b.String()
}

// RipArgs returns the ffmpeg arguments that remux title t of info into titleDir
// (without the progress flags). All streams of t.Streams are mapped in order,
// so the ripped file's stream indices equal Stream.Index.
func RipArgs(info *DiscInfo, t Title, titleDir string) []string {
	var args []string
	if info.Type == "dvd" {
		args = append(args, "-f", "dvdvideo", "-title", strconv.Itoa(t.ID), "-i", info.Source)
	} else {
		args = append(args,
			"-playlist", strconv.Itoa(t.ID),
			"-probesize", "100M", "-analyzeduration", "100M",
			"-i", "bluray:"+info.Source)
	}
	hasChapters := ChapterMetadata(t) != ""
	if hasChapters {
		args = append(args, "-f", "ffmetadata", "-i", ChaptersFile(titleDir))
	}

	for _, s := range t.Streams {
		args = append(args, "-map", s.Source)
	}
	if hasChapters {
		args = append(args, "-map_chapters", "1")
	}
	if info.Type != "dvd" {
		// The transport stream carries no languages; they come from the playlist.
		for _, s := range t.Streams {
			if s.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:%d", s.Index), "language="+s.Language)
			}
		}
	}
	return append(args, "-c", "copy", "-loglevel", "error", "-y", RipFile(titleDir))
}

// Rip remuxes title t into RipFile(titleDir) without re-encoding.
// progressFn receives the rip progress (Frac 0..1, speed, size).
func Rip(ctx context.Context, info *DiscInfo, t Title, titleDir string, progressFn func(ffmpeg.Progress), debug bool) error {
	if meta := ChapterMetadata(t); meta != "" {
		if err := os.WriteFile(ChaptersFile(titleDir), []byte(meta), 0o644); err != nil {
			return fmt.Errorf("writing chapters: %w", err)
		}
	}
	return ffmpeg.Run(ctx, RipArgs(info, t, titleDir), float64(t.Duration), progressFn, debug)
}

// ---- title selection ----

// FilterTitles returns only titles matching the duration criteria.
func FilterTitles(titles []Title, isTV bool, minMovie, minEp, maxEp int) []Title {
	var result []Title
	for _, t := range titles {
		if isTV {
			if t.Duration >= minEp && t.Duration <= maxEp {
				result = append(result, t)
			}
		} else {
			if t.Duration >= minMovie {
				result = append(result, t)
			}
		}
	}
	return result
}

// DetectContentType uses statistical analysis of title durations to guess TV vs movie.
func DetectContentType(titles []Title) Detection {
	if len(titles) == 0 {
		return Detection{IsTV: false, Confidence: 0.5}
	}
	if len(titles) == 1 {
		return Detection{IsTV: false, Confidence: 0.95}
	}

	durations := make([]float64, len(titles))
	for i, t := range titles {
		durations[i] = float64(t.Duration)
	}

	if len(titles) == 2 {
		ratio := durations[0] / durations[1]
		if ratio > 3 || ratio < 1.0/3 {
			return Detection{IsTV: false, Confidence: 0.85}
		}
		return Detection{IsTV: false, Confidence: 0.75}
	}

	// 3+ titles: check for TV-like clustering (20-60 min episodes)
	tvLike := 0
	for _, d := range durations {
		if d >= 1200 && d <= 3600 {
			tvLike++
		}
	}
	avg := mean(durations)
	tvRatio := float64(tvLike) / float64(len(durations))
	cv := coefficientOfVariation(durations, avg)

	if tvRatio >= 0.7 {
		switch {
		case cv < 0.15:
			return Detection{IsTV: true, Confidence: 0.92}
		case cv < 0.25:
			return Detection{IsTV: true, Confidence: 0.78}
		}
	}

	// One dominant long title + several shorter ones → movie with extras
	longCount := 0
	for _, d := range durations {
		if d > avg*1.5 {
			longCount++
		}
	}
	if longCount == 1 && len(titles) >= 3 {
		return Detection{IsTV: false, Confidence: 0.85}
	}

	return Detection{IsTV: false, Confidence: 0.6}
}

// ---- helpers ----

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func stddev(vals []float64, m float64) float64 {
	variance := 0.0
	for _, v := range vals {
		d := v - m
		variance += d * d
	}
	return math.Sqrt(variance / float64(len(vals)))
}

func coefficientOfVariation(vals []float64, m float64) float64 {
	if m == 0 {
		return 0
	}
	return stddev(vals, m) / m
}
