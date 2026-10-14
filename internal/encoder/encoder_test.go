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
	for _, want := range [][]string{{"-c:a:0", "copy"}, {"-c:a:1", "aac"}} {
		i := slices.Index(args, want[0])
		if i < 0 || args[i+1] != want[1] {
			t.Errorf("want %s %s: %v", want[0], want[1], args)
		}
	}
}
