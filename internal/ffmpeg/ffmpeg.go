// Package ffmpeg runs ffmpeg with machine-readable progress output.
package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/einfachnuralex/ripgo/internal/shellquote"
)

// Progress is a snapshot of a running ffmpeg process.
type Progress struct {
	Frac  float64 // 0..1; negative if the duration is unknown
	Speed string  // e.g. "4.2x"; empty until ffmpeg reports it
	Size  int64   // bytes written so far
}

// tailLines is the number of non-progress stderr lines kept for error messages.
const tailLines = 10

// Run executes ffmpeg with args, prefixed by "-progress pipe:2 -nostats".
// duration (seconds) is used to compute Progress.Frac; onProgress is called
// once per progress block. A failing run returns an error that includes the
// last lines ffmpeg printed to stderr.
func Run(ctx context.Context, args []string, duration float64, onProgress func(Progress), debug bool) error {
	args = append([]string{"-progress", "pipe:2", "-nostats"}, args...)
	if debug {
		fmt.Fprintf(os.Stderr, "[ffmpeg] %s\n", shellquote.Join("ffmpeg", args...))
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	tail := parseProgress(stderr, duration, onProgress, debug)

	if err := cmd.Wait(); err != nil {
		if len(tail) > 0 {
			return fmt.Errorf("%w: %s", err, strings.Join(tail, " | "))
		}
		return err
	}
	return nil
}

// parseProgress reads ffmpeg's stderr, calls onProgress at the end of every
// progress block ("progress=continue|end") and returns the last non-progress lines.
func parseProgress(r io.Reader, duration float64, onProgress func(Progress), debug bool) []string {
	var tail []string
	p := Progress{Frac: -1}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if debug {
			fmt.Fprintf(os.Stderr, "[ffmpeg] %s\n", line)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(key, " \t[") {
			if line = strings.TrimSpace(line); line != "" {
				tail = append(tail, line)
				if len(tail) > tailLines {
					tail = tail[1:]
				}
			}
			continue
		}
		switch key {
		case "out_time_us":
			us, err := strconv.ParseFloat(val, 64)
			if err == nil && duration > 0 {
				p.Frac = min(max((us/1e6)/duration, 0), 1)
			}
		case "speed":
			if val != "N/A" {
				p.Speed = strings.TrimSpace(val)
			}
		case "total_size":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				p.Size = n
			}
		case "progress":
			if onProgress != nil {
				onProgress(p)
			}
		}
	}
	return tail
}
