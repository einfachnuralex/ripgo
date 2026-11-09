package disc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Blu-ray movie playlist (BDMV/PLAYLIST/*.mpls) parser. Only what ripgo needs:
// play item times (duration, chapters), the STN table of the first play item
// (primary video, primary audio, presentation graphics with PID and language)
// and the playlist marks (chapters).

// mplsTicks is the clock rate of all MPLS time stamps (45 kHz).
const mplsTicks = 45000

type playlist struct {
	items   []playItem
	streams []Stream // STN streams of the first play item; Index/Source not yet set
	pids    []uint16 // PID of each entry in streams
	marks   []playMark
}

type playItem struct {
	clip    string // clip information file name, e.g. "00001"
	inTime  uint32
	outTime uint32
}

type playMark struct {
	item uint16 // play item the mark refers to
	time uint32 // absolute time stamp within that play item
}

// duration returns the playlist duration in 45 kHz ticks.
func (p *playlist) duration() uint64 {
	var d uint64
	for _, it := range p.items {
		if it.outTime > it.inTime {
			d += uint64(it.outTime - it.inTime)
		}
	}
	return d
}

// chapters returns the chapter start times in 45 kHz ticks, relative to the
// start of the playlist. Only entry marks are chapters; duplicates and marks
// at the very end are dropped.
func (p *playlist) chapters() []uint64 {
	offsets := make([]uint64, len(p.items))
	var off uint64
	for i, it := range p.items {
		offsets[i] = off
		if it.outTime > it.inTime {
			off += uint64(it.outTime - it.inTime)
		}
	}
	total := off

	var starts []uint64
	for _, m := range p.marks {
		if int(m.item) >= len(p.items) {
			continue
		}
		it := p.items[m.item]
		if m.time < it.inTime {
			continue
		}
		t := offsets[m.item] + uint64(m.time-it.inTime)
		if t+mplsTicks > total { // less than a second before the end
			continue
		}
		if len(starts) > 0 && t <= starts[len(starts)-1] {
			continue
		}
		starts = append(starts, t)
	}
	return starts
}

var errMPLS = errors.New("invalid MPLS playlist")

func parseMPLS(b []byte) (*playlist, error) {
	if len(b) < 20 || string(b[:4]) != "MPLS" {
		return nil, errMPLS
	}
	plStart := int(be32(b[8:]))
	markStart := int(be32(b[12:]))
	if plStart+10 > len(b) || markStart+6 > len(b) {
		return nil, errMPLS
	}

	p := &playlist{}
	nItems := int(be16(b[plStart+6:]))
	pos := plStart + 10
	for i := 0; i < nItems; i++ {
		if pos+2 > len(b) {
			return nil, errMPLS
		}
		itemLen := int(be16(b[pos:]))
		item := b[pos:min(pos+2+itemLen, len(b))]
		if len(item) < 34 {
			return nil, errMPLS
		}
		p.items = append(p.items, playItem{
			clip:    string(item[2:7]),
			inTime:  be32(item[14:]),
			outTime: be32(item[18:]),
		})
		if i == 0 {
			if err := p.parseSTN(item); err != nil {
				return nil, err
			}
		}
		pos += 2 + itemLen
	}

	nMarks := int(be16(b[markStart+4:]))
	for i := 0; i < nMarks; i++ {
		m := b[min(markStart+6+14*i, len(b)):]
		if len(m) < 14 {
			return nil, errMPLS
		}
		if m[1] == 1 { // entry mark
			p.marks = append(p.marks, playMark{item: be16(m[2:]), time: be32(m[4:])})
		}
	}
	return p, nil
}

// parseSTN reads the stream number table of a play item.
func (p *playlist) parseSTN(item []byte) error {
	pos := 34
	if item[12]&0x10 != 0 { // is_multi_angle
		if len(item) < pos+2 {
			return errMPLS
		}
		angles := int(item[pos])
		pos += 2 + 10*max(angles-1, 0)
	}
	if len(item) < pos+16 {
		return errMPLS
	}
	stn := item[pos:]
	counts := []int{int(stn[4]), int(stn[5]), int(stn[6]) + int(stn[10])} // video, audio, PG + PiP PG
	pos = 16

	for _, n := range counts {
		for i := 0; i < n; i++ {
			if pos >= len(stn) {
				return errMPLS
			}
			entry := stn[pos:min(pos+1+int(stn[pos]), len(stn))]
			pos += 1 + int(stn[pos])
			if pos >= len(stn) {
				return errMPLS
			}
			attr := stn[pos:min(pos+1+int(stn[pos]), len(stn))]
			pos += 1 + int(stn[pos])

			// Only streams of the main clip (entry type 1) are part of the
			// transport stream libbluray delivers to ffmpeg.
			if len(entry) < 4 || entry[1] != 1 || len(attr) < 2 {
				continue
			}
			s, ok := streamFromAttributes(attr)
			if !ok {
				continue
			}
			p.streams = append(p.streams, s)
			p.pids = append(p.pids, be16(entry[2:]))
		}
	}
	return nil
}

// streamFromAttributes converts MPLS stream attributes to a Stream with
// ffprobe-style codec names.
func streamFromAttributes(attr []byte) (Stream, bool) {
	coding := attr[1]
	lang := func(off int) string {
		if len(attr) < off+3 {
			return ""
		}
		return string(attr[off : off+3])
	}
	switch coding {
	case 0x01, 0x02, 0x1b, 0x20, 0x24, 0xea:
		s := Stream{Type: "video", Codec: videoCodecs[coding]}
		if len(attr) > 2 {
			s.Width, s.Height = videoSize(attr[2] >> 4)
		}
		return s, true
	case 0x03, 0x04, 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0xa1, 0xa2:
		s := Stream{Type: "audio", Codec: audioCodecs[coding], Language: lang(3)}
		if len(attr) > 2 {
			s.Channels = audioChannels(attr[2] >> 4)
		}
		return s, true
	case 0x90:
		return Stream{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: lang(2)}, true
	}
	// Interactive graphics and text subtitles are not ripped.
	return Stream{}, false
}

var videoCodecs = map[byte]string{
	0x01: "mpeg1video", 0x02: "mpeg2video", 0x1b: "h264", 0x20: "h264", 0x24: "hevc", 0xea: "vc1",
}

var audioCodecs = map[byte]string{
	0x03: "mp2", 0x04: "mp2", 0x80: "pcm_bluray", 0x81: "ac3", 0x82: "dts", 0x83: "truehd",
	0x84: "eac3", 0x85: "dts", 0x86: "dts", 0xa1: "eac3", 0xa2: "dts",
}

func videoSize(format byte) (int, int) {
	switch format {
	case 1, 3: // 480i, 480p
		return 720, 480
	case 2, 7: // 576i, 576p
		return 720, 576
	case 5: // 720p
		return 1280, 720
	case 4, 6: // 1080i, 1080p
		return 1920, 1080
	case 8: // 2160p
		return 3840, 2160
	}
	return 0, 0
}

func audioChannels(presentation byte) int {
	switch presentation {
	case 1:
		return 1
	case 3:
		return 2
	case 6, 12: // multi-channel, stereo + multi-channel
		return 6
	}
	return 0
}

// pidSource returns the ffmpeg stream specifier for a transport stream PID.
func pidSource(pid uint16) string {
	return fmt.Sprintf("0:i:0x%04x", pid)
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
