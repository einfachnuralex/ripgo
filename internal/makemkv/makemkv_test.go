package makemkv

import "testing"

func TestParseScanLineStreams(t *testing.T) {
	lines := []string{
		`TINFO:3,9,0,"0:42:10"`,
		`TINFO:3,27,0,"Show_t03.mkv"`,
		`SINFO:3,0,1,6201,"Video"`,
		`SINFO:3,0,5,0,"V_MPEG4/ISO/AVC"`,
		`SINFO:3,0,19,0,"1920x1080"`,
		`SINFO:3,1,1,6202,"Audio"`,
		`SINFO:3,1,3,0,"eng"`,
		`SINFO:3,1,5,0,"A_AC3"`,
		`SINFO:3,1,14,0,"6"`,
		`SINFO:3,2,1,6203,"Subtitles"`,
		`SINFO:3,2,3,0,"ger"`,
		`SINFO:3,2,5,0,"S_HDMV/PGS"`,
	}
	info := &DiscInfo{}
	titles := map[int]*Title{}
	for _, l := range lines {
		parseScanLine(l, info, titles)
	}

	got := titles[3]
	if got == nil {
		t.Fatal("title 3 not parsed")
	}
	if got.FileName != "Show_t03.mkv" {
		t.Errorf("FileName = %q", got.FileName)
	}
	if got.Duration != 42*60+10 {
		t.Errorf("Duration = %d", got.Duration)
	}
	want := []Stream{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "ac3", Language: "eng", Channels: 6},
		{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "ger"},
	}
	if len(got.Streams) != len(want) {
		t.Fatalf("got %d streams, want %d: %+v", len(got.Streams), len(want), got.Streams)
	}
	for i := range want {
		if got.Streams[i] != want[i] {
			t.Errorf("stream %d = %+v, want %+v", i, got.Streams[i], want[i])
		}
	}
}
