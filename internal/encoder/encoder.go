package encoder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Stream describes a single media stream inside a file.
type Stream struct {
	Index    int
	Type     string // "video", "audio", "subtitle"
	Codec    string
	Language string
	Width    int
	Height   int
	Channels int
}

// FileInfo holds the result of probing a media file.
type FileInfo struct {
	Duration float64
	Streams  []Stream
}

// EncodeSettings controls which streams are included and how audio is handled.
type EncodeSettings struct {
	Languages     []string
	Subtitles     bool
	StereoAudio   bool
	SurroundAudio bool
}

// Analyze runs ffprobe on path and returns stream information.
func Analyze(ctx context.Context, path string) (*FileInfo, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	}
	out, err := exec.CommandContext(ctx, "ffprobe", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

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
		return nil, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	info := &FileInfo{}
	info.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)

	for _, s := range probe.Streams {
		info.Streams = append(info.Streams, Stream{
			Index:    s.Index,
			Type:     s.CodecType,
			Codec:    s.CodecName,
			Language: s.Tags.Language,
			Width:    s.Width,
			Height:   s.Height,
			Channels: s.Channels,
		})
	}
	return info, nil
}

// Encode transcodes inputFile to outputFile.
// progressFn receives encoding progress as a 0..1 fraction.
func Encode(ctx context.Context, inputFile, outputFile string, settings EncodeSettings, progressFn func(float64), debug bool) error {
	info, err := Analyze(ctx, inputFile)
	if err != nil {
		return fmt.Errorf("analyzing %s: %w", inputFile, err)
	}

	selected := selectStreams(info.Streams, settings)
	args := buildArgs(inputFile, outputFile, selected)

	if debug {
		fmt.Fprintf(os.Stderr, "[ffmpeg] %s\n", strings.Join(args, " "))
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if debug {
			fmt.Fprintf(os.Stderr, "[ffmpeg] %s\n", line)
		}
		if strings.HasPrefix(line, "out_time_us=") {
			us, err := strconv.ParseFloat(strings.TrimPrefix(line, "out_time_us="), 64)
			if err == nil && info.Duration > 0 {
				frac := (us / 1e6) / info.Duration
				if frac > 1 {
					frac = 1
				}
				progressFn(frac)
			}
		}
	}

	return cmd.Wait()
}

func selectStreams(streams []Stream, s EncodeSettings) []Stream {
	langSet := make(map[string]bool, len(s.Languages))
	for _, l := range s.Languages {
		langSet[strings.ToLower(l)] = true
	}

	langMatch := func(lang string) bool {
		if lang == "" || lang == "und" {
			return true
		}
		return langSet[strings.ToLower(lang)]
	}

	var selected []Stream

	// Best video stream (highest pixel count)
	var bestVideo *Stream
	for i := range streams {
		if streams[i].Type != "video" {
			continue
		}
		if bestVideo == nil || streams[i].Width*streams[i].Height > bestVideo.Width*bestVideo.Height {
			bestVideo = &streams[i]
		}
	}
	if bestVideo != nil {
		selected = append(selected, *bestVideo)
	}

	// Audio streams matching language and channel configuration
	for _, stream := range streams {
		if stream.Type != "audio" || !langMatch(stream.Language) {
			continue
		}
		if stream.Channels <= 2 && s.StereoAudio {
			selected = append(selected, stream)
		} else if stream.Channels > 2 && s.SurroundAudio {
			selected = append(selected, stream)
		}
	}

	// Subtitle streams when requested
	if s.Subtitles {
		for _, stream := range streams {
			if stream.Type == "subtitle" && langMatch(stream.Language) {
				selected = append(selected, stream)
			}
		}
	}

	return selected
}

func buildArgs(input, output string, streams []Stream) []string {
	args := []string{
		"-probesize", "400M",
		"-analyzeduration", "400M",
		"-i", input,
	}

	// Map selected streams
	for _, s := range streams {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index))
	}
	args = append(args, "-map_chapters", "0")

	// Video: x264, slow preset, CRF 22, deinterlace
	args = append(args,
		"-c:v", "libx264",
		"-preset", "slow",
		"-crf", "22",
		"-pix_fmt", "yuv420p",
		"-vf", "bwdif=mode=send_frame:parity=auto:deint=interlaced",
	)

	// Audio: copy compatible codecs, transcode others to AAC; detect subtitles in same pass
	audioIdx := 0
	hasAudio := false
	hasSubs := false
	for _, s := range streams {
		switch s.Type {
		case "audio":
			hasAudio = true
			codec := strings.ToLower(s.Codec)
			if codec == "aac" || codec == "ac3" || codec == "eac3" {
				args = append(args, fmt.Sprintf("-c:a:%d", audioIdx), "copy")
			} else {
				args = append(args,
					fmt.Sprintf("-c:a:%d", audioIdx), "aac",
					fmt.Sprintf("-b:a:%d", audioIdx), "160k",
				)
			}
			audioIdx++
		case "subtitle":
			hasSubs = true
		}
	}
	if !hasAudio {
		args = append(args, "-an")
	}

	// Subtitles: copy (soft subs, no burn-in)
	if hasSubs {
		args = append(args, "-c:s", "copy")
	} else {
		args = append(args, "-sn")
	}

	args = append(args,
		"-y",
		"-progress", "pipe:2",
		"-loglevel", "warning",
		output,
	)
	return args
}
