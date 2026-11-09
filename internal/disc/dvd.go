package disc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/einfachnuralex/ripgo/internal/shellquote"
)

// dvdTitle is one entry of the VMG title search pointer table (TT_SRPT).
type dvdTitle struct {
	angles   int
	chapters int
}

// parseTTSRPT reads the title table from VIDEO_TS.IFO.
func parseTTSRPT(ifo []byte) ([]dvdTitle, error) {
	if len(ifo) < 0xc8 || string(ifo[:12]) != "DVDVIDEO-VMG" {
		return nil, errors.New("invalid VIDEO_TS.IFO")
	}
	off := int(be32(ifo[0xc4:])) * sectorSize
	if off == 0 || off+8 > len(ifo) {
		return nil, errors.New("VIDEO_TS.IFO has no title table")
	}
	n := int(be16(ifo[off:]))
	var titles []dvdTitle
	for i := 0; i < n; i++ {
		e := ifo[min(off+8+12*i, len(ifo)):]
		if len(e) < 12 {
			return nil, errors.New("truncated title table in VIDEO_TS.IFO")
		}
		titles = append(titles, dvdTitle{angles: int(e[1]), chapters: int(be16(e[2:]))})
	}
	return titles, nil
}

// DVDProbeArgs returns the ffprobe arguments used to analyze DVD title titleID.
func DVDProbeArgs(source string, titleID int) []string {
	return []string{
		"-v", "error",
		"-f", "dvdvideo", "-title", strconv.Itoa(titleID),
		"-show_streams", "-show_format",
		"-of", "json",
		"-i", source,
	}
}

// scanDVD lists the titles from VIDEO_TS.IFO and probes each one with ffprobe
// (dvdvideo demuxer) for its duration, streams and languages.
func scanDVD(ctx context.Context, fsys discFS, info *DiscInfo, debug bool) error {
	ifo, err := fsys.ReadFile("VIDEO_TS/VIDEO_TS.IFO")
	if err != nil {
		return err
	}
	entries, err := parseTTSRPT(ifo)
	if err != nil {
		return err
	}

	for i, e := range entries {
		id := i + 1
		t, err := probeDVDTitle(ctx, info.Source, id, debug)
		if err != nil {
			info.Warnings = append(info.Warnings, fmt.Sprintf("title %d: %v", id, err))
			continue
		}
		t.Chapters = e.chapters
		info.Titles = append(info.Titles, t)
	}
	return nil
}

func probeDVDTitle(ctx context.Context, source string, id int, debug bool) (Title, error) {
	args := DVDProbeArgs(source, id)
	if debug {
		fmt.Fprintf(os.Stderr, "[ffprobe] %s\n", shellquote.Join("ffprobe", args...))
	}
	out, err := exec.CommandContext(ctx, "ffprobe", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return Title{}, fmt.Errorf("ffprobe: %w: %s", err, lastLine(exitErr.Stderr))
		}
		return Title{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseDVDProbe(id, out)
}

// parseDVDProbe converts ffprobe's JSON output for a DVD title. All video,
// audio and subtitle streams are ripped, so stream i of the rip is the i-th of these.
func parseDVDProbe(id int, out []byte) (Title, error) {
	var probe struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Channels  int    `json:"channels"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return Title{}, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	t := Title{ID: id}
	dur, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	t.Duration = int(dur + 0.5)
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video", "audio", "subtitle":
		default:
			continue
		}
		t.Streams = append(t.Streams, Stream{
			Index:    len(t.Streams),
			Source:   fmt.Sprintf("0:%d", s.Index),
			Type:     s.CodecType,
			Codec:    s.CodecName,
			Language: s.Tags.Language,
			Width:    s.Width,
			Height:   s.Height,
			Channels: s.Channels,
		})
	}
	return t, nil
}
