package makemkv

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Title represents a single track found on the disc.
type Title struct {
	ID       int
	Name     string
	Duration int   // seconds
	Size     int64 // bytes reported by MakeMKV
}

// DiscInfo holds the result of scanning a disc.
type DiscInfo struct {
	Name   string
	Type   string // "dvd", "bd", "uhd"
	Titles []Title
}

// Detection holds the result of content-type auto-detection.
type Detection struct {
	IsTV       bool
	Confidence float64 // 0.0 - 1.0
}

// ScanDisc runs `makemkvcon -r --robot info` and returns disc metadata.
func ScanDisc(ctx context.Context, disc string, debug bool) (*DiscInfo, error) {
	cmd := exec.CommandContext(ctx, "makemkvcon", "-r", "--robot", "info", disc)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	info := &DiscInfo{Type: "dvd"}
	titles := map[int]*Title{}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if debug {
			fmt.Fprintf(os.Stderr, "[makemkv] %s\n", line)
		}
		parseScanLine(line, info, titles)
	}

	// makemkvcon exits non-zero even on success; only fail if we got nothing
	if err := cmd.Wait(); err != nil && info.Name == "" && len(titles) == 0 {
		return nil, fmt.Errorf("makemkvcon: %w", err)
	}

	info.Titles = sortedTitles(titles)
	return info, nil
}

func parseScanLine(line string, info *DiscInfo, titles map[int]*Title) {
	switch {
	case strings.HasPrefix(line, "DRV:"):
		parts := splitCSV(line[4:])
		if len(parts) >= 6 && parts[5] != "" {
			info.Name = unquote(parts[5])
		}

	case strings.HasPrefix(line, "CINFO:"):
		parts := splitCSV(line[6:])
		if len(parts) < 3 {
			return
		}
		id, _ := strconv.Atoi(parts[0])
		val := unquote(parts[2])
		switch id {
		case 1: // disc name
			if info.Name == "" {
				info.Name = val
			}
		case 11: // disc type string
			lower := strings.ToLower(val)
			switch {
			case strings.Contains(lower, "uhd"), strings.Contains(lower, "4k"):
				info.Type = "uhd"
			case strings.Contains(lower, "blu"), strings.Contains(lower, "bd"):
				info.Type = "bd"
			case strings.Contains(lower, "dvd"):
				info.Type = "dvd"
			}
		}

	case strings.HasPrefix(line, "TINFO:"):
		parts := splitCSV(line[6:])
		if len(parts) < 4 {
			return
		}
		titleID, err := strconv.Atoi(parts[0])
		if err != nil {
			return
		}
		infoID, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		if titles[titleID] == nil {
			titles[titleID] = &Title{ID: titleID}
		}
		t := titles[titleID]
		val := unquote(parts[3])
		switch infoID {
		case 2: // title name
			if t.Name == "" {
				t.Name = val
			}
		case 9: // duration HH:MM:SS
			t.Duration = parseDuration(val)
		case 10: // size in bytes
			t.Size, _ = strconv.ParseInt(val, 10, 64)
		case 27: // output filename - use as name fallback
			if t.Name == "" {
				base := filepath.Base(val)
				t.Name = strings.TrimSuffix(base, filepath.Ext(base))
			}
		}
	}
}

// RipTitle runs `makemkvcon mkv` to extract a single title.
// progressFn is called with a 0..1 fraction; negative fraction signals a caption-only update.
func RipTitle(ctx context.Context, disc string, titleID int, outputDir string, progressFn func(frac float64, caption string), debug bool) error {
	args := []string{"-r", "mkv", disc, strconv.Itoa(titleID), outputDir}
	cmd := exec.CommandContext(ctx, "makemkvcon", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start makemkvcon: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if debug {
			fmt.Fprintf(os.Stderr, "[makemkv rip] %s\n", line)
		}
		switch {
		case strings.HasPrefix(line, "PRGV:"):
			// PRGV:current,total,max
			parts := strings.SplitN(line[5:], ",", 3)
			if len(parts) >= 2 {
				cur, _ := strconv.ParseFloat(parts[0], 64)
				total, _ := strconv.ParseFloat(parts[1], 64)
				if total > 0 {
					progressFn(cur/total, "")
				}
			}
		case strings.HasPrefix(line, "PRGC:"):
			// PRGC:type,id,"caption"
			parts := splitCSV(line[5:])
			if len(parts) >= 3 {
				progressFn(-1, unquote(parts[2]))
			}
		}
	}

	return cmd.Wait()
}

// FindRippedFile returns the path of the single MKV file in outputDir.
func FindRippedFile(outputDir string) (string, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", fmt.Errorf("reading dir %s: %w", outputDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".mkv") {
			return filepath.Join(outputDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no MKV file found in %s", outputDir)
}

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

func parseDuration(s string) int {
	// HH:MM:SS
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	sec, _ := strconv.Atoi(parts[2])
	return h*3600 + m*60 + sec
}

// splitCSV splits a comma-separated robot-protocol line respecting quoted strings.
func splitCSV(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func sortedTitles(m map[int]*Title) []Title {
	result := make([]Title, 0, len(m))
	for _, t := range m {
		result = append(result, *t)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
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
