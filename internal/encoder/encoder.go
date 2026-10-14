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

	"github.com/einfachnuralex/ripgo/internal/shellquote"
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

// VideoProfile defines ffmpeg video encoding parameters.
type VideoProfile struct {
	Name        string
	Codec       string   // e.g. "libx264", "libx265", "hevc_vaapi"
	CRF         int      // quality value; 0 = omit quality flag
	Preset      string   // e.g. "slow", "medium"; empty = omit -preset
	PixFmt      string   // e.g. "yuv420p"; empty = omit -pix_fmt
	VideoFilter string   // value for -vf flag; empty = no filter
	HWDevice    string   // hardware device path, e.g. "/dev/dri/renderD128"; produces -vaapi_device before -i
	ExtraArgs   []string // arbitrary extra ffmpeg flags appended after codec args
}

// EncodeSettings controls which streams are included and how audio and video are handled.
type EncodeSettings struct {
	Languages     []string
	Subtitles     bool
	StereoAudio   bool
	SurroundAudio bool
	Profile       VideoProfile
}

// ProbeArgs returns the ffprobe arguments used to analyze path.
func ProbeArgs(path string) []string {
	return []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	}
}

// Analyze runs ffprobe on path and returns stream information.
func Analyze(ctx context.Context, path string) (*FileInfo, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", ProbeArgs(path)...).Output()
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

	// -progress is a global option; ripgo parses it from stderr
	args := append([]string{"-progress", "pipe:2"}, BuildArgs(inputFile, outputFile, info.Streams, settings)...)

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

// BuildArgs returns the ffmpeg arguments for encoding input to output, selecting
// from streams according to settings. Progress reporting flags are not included.
func BuildArgs(input, output string, streams []Stream, settings EncodeSettings) []string {
	return buildArgs(input, output, SelectStreams(streams, settings), settings.Profile)
}

// SelectStreams returns the streams that will be mapped into the output file.
func SelectStreams(streams []Stream, s EncodeSettings) []Stream {
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

// CopiesAudio reports whether an audio stream with the given codec is stream-copied
// (true) or transcoded to AAC (false).
func CopiesAudio(codec string) bool {
	switch strings.ToLower(codec) {
	case "aac", "ac3", "eac3":
		return true
	}
	return false
}

// qualityFlagForCodec returns the appropriate ffmpeg quality flag for the given codec.
// VAAPI encoders use -global_quality; all others use -crf.
func qualityFlagForCodec(codec string) string {
	if strings.HasSuffix(strings.ToLower(codec), "_vaapi") {
		return "global_quality"
	}
	return "crf"
}

func buildArgs(input, output string, streams []Stream, profile VideoProfile) []string {
	args := []string{"-probesize", "400M", "-analyzeduration", "400M"}

	// Hardware device must precede -i (required for VAAPI etc.)
	if profile.HWDevice != "" {
		args = append(args, "-vaapi_device", profile.HWDevice)
	}
	args = append(args, "-i", input)

	// Map selected streams
	for _, s := range streams {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index))
	}
	args = append(args, "-map_chapters", "0")

	// Video codec (always required)
	args = append(args, "-c:v", profile.Codec)

	// Quality: CRF=0 means omit; flag name depends on codec family
	if profile.CRF > 0 {
		args = append(args, "-"+qualityFlagForCodec(profile.Codec), strconv.Itoa(profile.CRF))
	}
	// Preset optional
	if profile.Preset != "" {
		args = append(args, "-preset", profile.Preset)
	}
	// Pixel format optional
	if profile.PixFmt != "" {
		args = append(args, "-pix_fmt", profile.PixFmt)
	}
	// Video filter optional
	if profile.VideoFilter != "" {
		args = append(args, "-vf", profile.VideoFilter)
	}
	// Extra args appended after standard codec args
	args = append(args, profile.ExtraArgs...)

	// Audio: copy compatible codecs, transcode others to AAC; detect subtitles in same pass
	audioIdx := 0
	hasAudio := false
	hasSubs := false
	for _, s := range streams {
		switch s.Type {
		case "audio":
			hasAudio = true
			if CopiesAudio(s.Codec) {
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
		"-loglevel", "warning",
		output,
	)
	return args
}
