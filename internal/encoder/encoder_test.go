package encoder

import (
	"slices"
	"testing"
)

func TestBuildArgsVAAPI(t *testing.T) {
	streams := []Stream{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "ac3", Language: "eng", Channels: 6},
		{Index: 2, Type: "audio", Codec: "dts", Language: "fra", Channels: 6},
		{Index: 3, Type: "audio", Codec: "truehd", Language: "eng", Channels: 8},
		{Index: 4, Type: "audio", Codec: "aac", Language: "eng", Channels: 6},
	}
	settings := EncodeSettings{
		Languages:     []string{"eng"},
		SurroundAudio: true,
		Profile: VideoProfile{
			Codec:       "hevc_vaapi",
			CRF:         23,
			HWDevice:    "/dev/dri/renderD128",
			VideoFilter: "format=nv12,hwupload",
		},
	}
	args := BuildArgs("in.mkv", "out.mkv", streams, settings)

	dev := slices.Index(args, "-vaapi_device")
	in := slices.Index(args, "-i")
	if dev < 0 || in < 0 || dev > in {
		t.Errorf("-vaapi_device must precede -i: %v", args)
	}
	if i := slices.Index(args, "-global_quality"); i < 0 || args[i+1] != "23" {
		t.Errorf("missing -global_quality 23: %v", args)
	}
	if slices.Contains(args, "-progress") {
		t.Errorf("BuildArgs must not include -progress: %v", args)
	}
	if args[len(args)-1] != "out.mkv" {
		t.Errorf("output must be last: %v", args)
	}
	if slices.Contains(args, "0:2") {
		t.Errorf("fra audio must not be mapped: %v", args)
	}
	// ac3 and truehd are transcoded, the aac stream is copied
	for _, want := range [][]string{
		{"-c:a:0", "aac"}, {"-b:a:0", "160k"},
		{"-c:a:1", "aac"}, {"-b:a:1", "160k"},
		{"-c:a:2", "copy"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || args[i+1] != want[1] {
			t.Errorf("want %s %s: %v", want[0], want[1], args)
		}
	}
	if slices.Contains(args, "-b:a:2") {
		t.Errorf("copied aac stream must not get a bitrate: %v", args)
	}
}

func TestSelectStreamsAudioTracks(t *testing.T) {
	streams := []Stream{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "ac3", Language: "eng", Channels: 6},
		{Index: 2, Type: "audio", Codec: "dts", Language: "fra", Channels: 2},
		{Index: 3, Type: "audio", Codec: "aac", Language: "ger", Channels: 2},
		{Index: 4, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng"},
	}
	// Explicit tracks override language and channel filters (fra is not in Languages,
	// StereoAudio is off), but not video/subtitle selection.
	settings := EncodeSettings{
		Languages:     []string{"eng"},
		SurroundAudio: true,
		Subtitles:     true,
		AudioTracks:   []int{3, 2},
	}
	var got []int
	for _, s := range SelectStreams(streams, settings) {
		got = append(got, s.Index)
	}
	if want := []int{0, 2, 3, 4}; !slices.Equal(got, want) {
		t.Errorf("selected indices = %v, want %v", got, want)
	}

	if m := MissingAudioTracks(streams, []int{1, 4, 9}); !slices.Equal(m, []int{4, 9}) {
		t.Errorf("MissingAudioTracks = %v, want [4 9]", m)
	}
}
