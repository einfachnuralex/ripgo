package ffmpeg

import (
	"slices"
	"strings"
	"testing"
)

func TestParseProgress(t *testing.T) {
	stderr := strings.Join([]string{
		"[matroska @ 0x55] Starting new cluster due to timestamp",
		"frame=100",
		"total_size=1048576",
		"out_time_us=5000000",
		"speed=N/A",
		"progress=continue",
		"Error while decoding stream #0:1: Invalid data found",
		"total_size=2097152",
		"out_time_us=20000000",
		"speed=4.2x",
		"progress=end",
	}, "\n")

	var got []Progress
	tail := parseProgress(strings.NewReader(stderr), 10, func(p Progress) { got = append(got, p) }, false)

	want := []Progress{
		{Frac: 0.5, Size: 1048576},
		{Frac: 1, Speed: "4.2x", Size: 2097152},
	}
	if !slices.Equal(got, want) {
		t.Errorf("progress = %+v, want %+v", got, want)
	}
	wantTail := []string{
		"[matroska @ 0x55] Starting new cluster due to timestamp",
		"Error while decoding stream #0:1: Invalid data found",
	}
	if !slices.Equal(tail, wantTail) {
		t.Errorf("tail = %q, want %q", tail, wantTail)
	}
}

func TestParseProgressUnknownDuration(t *testing.T) {
	var got []Progress
	parseProgress(strings.NewReader("out_time_us=5000000\nprogress=continue\n"), 0, func(p Progress) { got = append(got, p) }, false)
	if len(got) != 1 || got[0].Frac != -1 {
		t.Errorf("progress = %+v, want Frac -1", got)
	}
}
