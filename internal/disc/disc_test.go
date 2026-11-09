package disc

import (
	"context"
	"encoding/binary"
	"os"
	"slices"
	"strings"
	"testing"
)

// buildMPLS assembles a playlist with two play items (60 s and 30 s), an STN
// table (video, two audio streams, one sub-path audio, one PG stream) on the
// first item and three marks (two entry marks, one link point).
func buildMPLS() []byte {
	be16 := func(v int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
	be32 := func(v int) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }

	entry := func(pid int) []byte { return cat([]byte{9, 1}, be16(pid), make([]byte, 6)) }
	subEntry := cat([]byte{9, 2, 0, 0}, be16(0x1a00), make([]byte, 4))
	video := []byte{5, 0xea, 0x61, 0, 0, 0}                // VC-1, 1080p
	audio := func(coding, pres byte, lang string) []byte { // presentation type in the high nibble
		return cat([]byte{5, coding, pres<<4 | 1}, []byte(lang))
	}
	pg := cat([]byte{5, 0x90}, []byte("deu"), []byte{0})

	stnBody := cat(
		[]byte{0, 0},                // reserved
		[]byte{1, 3, 1, 0, 0, 0, 0}, // video, audio, PG, IG, sec. audio, sec. video, PiP PG
		make([]byte, 5),
		entry(0x1011), video,
		entry(0x1100), audio(0x83, 6, "eng"),
		subEntry, audio(0x81, 3, "fra"),
		entry(0x1101), audio(0x81, 3, "ger"),
		entry(0x1200), pg,
	)
	stn := cat(be16(len(stnBody)), stnBody)

	item := func(clip string, in, out int, withSTN bool) []byte {
		body := cat([]byte(clip), []byte("M2TS"), []byte{0, 0}, []byte{0}, be32(in), be32(out), make([]byte, 8), []byte{0, 0}, be16(0))
		if withSTN {
			body = cat(body, stn)
		} else {
			body = cat(body, be16(14), make([]byte, 14))
		}
		return cat(be16(len(body)), body)
	}
	items := cat(item("00001", 90000, 90000+60*mplsTicks, true), item("00002", 0, 30*mplsTicks, false))
	plBody := cat([]byte{0, 0}, be16(2), be16(0), items)
	pl := cat(be32(len(plBody)), plBody)

	mark := func(typ byte, itemID, ts int) []byte {
		return cat([]byte{0, typ}, be16(itemID), be32(ts), be16(0xffff), be32(0))
	}
	marks := cat(be16(3),
		mark(1, 0, 90000),
		mark(2, 0, 90000+10*mplsTicks), // link point: not a chapter
		mark(1, 1, 5*mplsTicks),
	)
	plm := cat(be32(len(marks)), marks)

	const header = 40
	return cat([]byte("MPLS0200"), be32(header), be32(header+len(pl)), be32(0), make([]byte, header-20), pl, plm)
}

func TestParseMPLS(t *testing.T) {
	pl, err := parseMPLS(buildMPLS())
	if err != nil {
		t.Fatal(err)
	}
	if got := pl.duration(); got != 90*mplsTicks {
		t.Errorf("duration = %d, want %d", got, 90*mplsTicks)
	}
	if got, want := pl.chapters(), []uint64{0, 65 * mplsTicks}; !slices.Equal(got, want) {
		t.Errorf("chapters = %v, want %v", got, want)
	}
	want := []Stream{
		{Type: "video", Codec: "vc1", Width: 1920, Height: 1080},
		{Type: "audio", Codec: "truehd", Language: "eng", Channels: 6},
		{Type: "audio", Codec: "ac3", Language: "ger", Channels: 2},
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "deu"},
	}
	if !slices.Equal(pl.streams, want) {
		t.Errorf("streams = %+v, want %+v", pl.streams, want)
	}
	if want := []uint16{0x1011, 0x1100, 0x1101, 0x1200}; !slices.Equal(pl.pids, want) {
		t.Errorf("pids = %x, want %x", pl.pids, want)
	}
}

func TestParseMPLSInvalid(t *testing.T) {
	b := buildMPLS()
	for _, n := range []int{0, 10, 60, len(b) - 20} {
		if _, err := parseMPLS(b[:n]); err == nil {
			t.Errorf("truncated to %d bytes: expected error", n)
		}
	}
}

func TestParseTTSRPT(t *testing.T) {
	ifo := make([]byte, 2*sectorSize)
	copy(ifo, "DVDVIDEO-VMG")
	binary.BigEndian.PutUint32(ifo[0xc4:], 1) // TT_SRPT in sector 1
	tt := ifo[sectorSize:]
	binary.BigEndian.PutUint16(tt, 2)
	// title 1: 1 angle, 12 chapters; title 2: 3 angles, 1 chapter
	copy(tt[8:], []byte{0x3c, 1, 0, 12, 0, 0, 1, 1, 0, 0, 0, 0})
	copy(tt[20:], []byte{0x3c, 3, 0, 1, 0, 0, 2, 1, 0, 0, 0, 0})

	got, err := parseTTSRPT(ifo)
	if err != nil {
		t.Fatal(err)
	}
	if want := []dvdTitle{{angles: 1, chapters: 12}, {angles: 3, chapters: 1}}; !slices.Equal(got, want) {
		t.Errorf("titles = %+v, want %+v", got, want)
	}
	if _, err := parseTTSRPT(ifo[:100]); err == nil {
		t.Error("expected error for truncated IFO")
	}
}

func TestParseDVDProbe(t *testing.T) {
	out := `{"streams": [
		{"index": 0, "codec_type": "video", "codec_name": "mpeg2video", "width": 720, "height": 576},
		{"index": 1, "codec_type": "data", "codec_name": "dvd_nav_packet"},
		{"index": 2, "codec_type": "audio", "codec_name": "ac3", "channels": 6, "tags": {"language": "ger"}},
		{"index": 3, "codec_type": "subtitle", "codec_name": "dvd_subtitle", "tags": {"language": "eng"}}
	], "format": {"duration": "2712.6"}}`
	got, err := parseDVDProbe(3, []byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 3 || got.Duration != 2713 {
		t.Errorf("ID/Duration = %d/%d", got.ID, got.Duration)
	}
	want := []Stream{
		{Index: 0, Source: "0:0", Type: "video", Codec: "mpeg2video", Width: 720, Height: 576},
		{Index: 1, Source: "0:2", Type: "audio", Codec: "ac3", Language: "ger", Channels: 6},
		{Index: 2, Source: "0:3", Type: "subtitle", Codec: "dvd_subtitle", Language: "eng"},
	}
	if !slices.Equal(got.Streams, want) {
		t.Errorf("streams = %+v, want %+v", got.Streams, want)
	}
}

func TestRipArgs(t *testing.T) {
	streams := []Stream{
		{Index: 0, Source: "0:i:0x1011", Type: "video"},
		{Index: 1, Source: "0:i:0x1101", Type: "audio", Language: "eng"},
	}
	bd := &DiscInfo{Source: "/dev/sr0", Type: "bd"}
	title := Title{ID: 800, Duration: 90, Streams: streams, chapters: []uint64{0, 65 * mplsTicks}}
	got := strings.Join(RipArgs(bd, title, "/c/t"), " ")
	want := "-playlist 800 -probesize 100M -analyzeduration 100M -i bluray:/dev/sr0 -f ffmetadata -i /c/t/chapters.txt " +
		"-map 0:i:0x1011 -map 0:i:0x1101 -map_chapters 1 -metadata:s:1 language=eng -c copy -loglevel error -y /c/t/title.mkv"
	if got != want {
		t.Errorf("BD args:\n got %s\nwant %s", got, want)
	}

	dvd := &DiscInfo{Source: "movie.iso", Type: "dvd"}
	title = Title{ID: 2, Streams: []Stream{{Index: 0, Source: "0:0"}, {Index: 1, Source: "0:2", Language: "ger"}}}
	got = strings.Join(RipArgs(dvd, title, "/c/t"), " ")
	want = "-f dvdvideo -title 2 -i movie.iso -map 0:0 -map 0:2 -c copy -loglevel error -y /c/t/title.mkv"
	if got != want {
		t.Errorf("DVD args:\n got %s\nwant %s", got, want)
	}
}

func TestChapterMetadata(t *testing.T) {
	if got := ChapterMetadata(Title{Duration: 90}); got != "" {
		t.Errorf("no chapters: got %q", got)
	}
	got := ChapterMetadata(Title{Duration: 90, chapters: []uint64{0, 65 * mplsTicks}})
	want := ";FFMETADATA1\n" +
		"\n[CHAPTER]\nTIMEBASE=1/45000\nSTART=0\nEND=2925000\ntitle=Chapter 01\n" +
		"\n[CHAPTER]\nTIMEBASE=1/45000\nSTART=2925000\nEND=4050000\ntitle=Chapter 02\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestNormalizeSource(t *testing.T) {
	for in, want := range map[string]string{
		"disc:0":          "/dev/sr0",
		"disc:2":          "/dev/sr2",
		"file:/x/m.iso":   "/x/m.iso",
		"/dev/sr1":        "/dev/sr1",
		"/media/BD_MOVIE": "/media/BD_MOVIE",
	} {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDirFS(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/BDMV/PLAYLIST", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/BDMV/index.bdmv", []byte("INDX0300"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/BDMV/PLAYLIST/00800.mpls", buildMPLS(), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, err := openFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if typ, err := detectType(fsys); err != nil || typ != "uhd" {
		t.Errorf("detectType = %q, %v; want uhd", typ, err)
	}
	info := &DiscInfo{Source: root}
	if err := scanBluray(fsys, info); err != nil {
		t.Fatal(err)
	}
	if len(info.Titles) != 0 {
		t.Errorf("90 s playlist must be skipped (< %d s): %+v", minPlaylistSeconds, info.Titles)
	}
}

func TestUDFStrings(t *testing.T) {
	if got := cs0([]byte{8, 'B', 'D', 'M', 'V'}); got != "BDMV" {
		t.Errorf("cs0 8-bit = %q", got)
	}
	if got := cs0([]byte{16, 0, 'A', 0x00, 0xe4}); got != "Aä" {
		t.Errorf("cs0 16-bit = %q", got)
	}
	field := make([]byte, 32)
	copy(field, []byte{8, 'M', 'O', 'V', 'I', 'E'})
	field[31] = 6
	if got := dstring(field); got != "MOVIE" {
		t.Errorf("dstring = %q", got)
	}

	tag := make([]byte, 16)
	binary.LittleEndian.PutUint16(tag, tagFileEntry)
	tag[12] = 7
	for i, b := range tag {
		if i != 4 {
			tag[4] += b
		}
	}
	if id, ok := tagID(tag); !ok || id != tagFileEntry {
		t.Errorf("tagID = %d, %v", id, ok)
	}
	tag[12] = 8
	if _, ok := tagID(tag); ok {
		t.Error("tagID accepted a wrong checksum")
	}
}

// TestScanImage scans a real disc, image or directory when RIPGO_TEST_DISC is set,
// e.g. RIPGO_TEST_DISC=/dev/sr0 or RIPGO_TEST_DISC=movie.iso.
func TestScanImage(t *testing.T) {
	src := os.Getenv("RIPGO_TEST_DISC")
	if src == "" {
		t.Skip("RIPGO_TEST_DISC not set")
	}
	info, err := Scan(context.Background(), src, "", testing.Verbose())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%q (%s), %d titles, warnings %v", info.Name, info.Type, len(info.Titles), info.Warnings)
	for _, ti := range info.Titles {
		t.Logf("  %4d %-11s %5ds %3d ch, %d streams", ti.ID, ti.SourceFile, ti.Duration, ti.Chapters, len(ti.Streams))
	}
}
