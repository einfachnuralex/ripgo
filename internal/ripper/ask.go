package ripper

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/einfachnuralex/ripgo/internal/disc"
	"github.com/einfachnuralex/ripgo/internal/encoder"
)

// ---- ask mode (--ask): interactive questions after the disc scan ----

// prompter reads answers line by line. All questions share one buffered reader
// so no input is lost between them.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	eof bool
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

// ask prints "label [def]: " and returns the trimmed answer; an empty answer or
// EOF yields def. After EOF p.eof is set so retry loops can give up.
func (p *prompter) ask(label, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s%s%s [%s]: ", colorYellow, label, colorReset, def)
	} else {
		fmt.Fprintf(p.out, "%s%s%s: ", colorYellow, label, colorReset)
	}
	line, err := p.in.ReadString('\n')
	if err != nil {
		p.eof = true
		if line == "" {
			fmt.Fprintln(p.out)
		}
	}
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

// ParseIndexList parses a comma-separated list of non-negative numbers,
// dropping duplicates and keeping the order. An empty string yields nil.
func ParseIndexList(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var list []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid entry %q: must be a non-negative number", strings.TrimSpace(part))
		}
		if !slices.Contains(list, n) {
			list = append(list, n)
		}
	}
	return list, nil
}

func formatIndexList(list []int) string {
	parts := make([]string, len(list))
	for i, n := range list {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// pickTitles returns the titles with the given IDs, in the order of ids.
func pickTitles(all []disc.Title, ids []int) ([]disc.Title, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("no title selected")
	}
	var picked []disc.Title
	for _, id := range ids {
		i := slices.IndexFunc(all, func(t disc.Title) bool { return t.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("title %d does not exist on the disc", id)
		}
		picked = append(picked, all[i])
	}
	return picked, nil
}

// chooseTitles lists all titles of the disc (defaults marked with *) and asks
// which ones to process. The answer order is kept: for TV it sets the episode order.
func chooseTitles(p *prompter, discType string, all, defaults []disc.Title) ([]disc.Title, error) {
	isDefault := make(map[int]bool, len(defaults))
	defIDs := make([]int, 0, len(defaults))
	for _, t := range defaults {
		isDefault[t.ID] = true
		defIDs = append(defIDs, t.ID)
	}

	showPlaylist := discType != "dvd" && slices.ContainsFunc(all, func(t disc.Title) bool { return t.SourceFile != "" })

	fmt.Fprintln(p.out)
	header := fmt.Sprintf("  %-2s %4s  %-8s  %4s", "", "ID", "Duration", "Ch.")
	if showPlaylist {
		header += fmt.Sprintf("  %-12s", "Playlist")
	}
	fmt.Fprintln(p.out, dimStyle.Render(header+"  Audio"))
	for _, t := range all {
		mark := " "
		if isDefault[t.ID] {
			mark = "*"
		}
		line := fmt.Sprintf("  %-2s %4d  %-8s  %4d", mark, t.ID, formatDuration(t.Duration), t.Chapters)
		if showPlaylist {
			line += fmt.Sprintf("  %-12s", t.SourceFile)
		}
		fmt.Fprintln(p.out, line+"  "+audioLanguages(t))
	}
	fmt.Fprintln(p.out, dimStyle.Render("  * = selected by the duration filter"))

	label := "Titles to process (comma-separated IDs)"
	if discType != "dvd" {
		label = "Playlists to process (comma-separated title IDs)"
	}
	for {
		ids, err := ParseIndexList(p.ask(label, formatIndexList(defIDs)))
		var picked []disc.Title
		if err == nil {
			picked, err = pickTitles(all, ids)
		}
		if err == nil {
			return picked, nil
		}
		if p.eof {
			return nil, err
		}
		printErr("%v", err)
	}
}

// chooseAudio lists the audio streams (defaults marked with *) and asks which
// stream indices to include. An empty answer with no defaults keeps the
// automatic language/channel selection (returns nil).
func chooseAudio(p *prompter, streams []encoder.Stream, defaults []int) ([]int, error) {
	fmt.Fprintln(p.out)
	for _, s := range streams {
		if s.Type != "audio" {
			continue
		}
		mark := " "
		if slices.Contains(defaults, s.Index) {
			mark = "*"
		}
		fmt.Fprintf(p.out, "  %s %s\n", mark, describeStream(s))
	}

	for {
		tracks, err := ParseIndexList(p.ask("Audio tracks (comma-separated stream indices)", formatIndexList(defaults)))
		if err == nil {
			if missing := encoder.MissingAudioTracks(streams, tracks); len(missing) > 0 {
				err = fmt.Errorf("stream(s) %v are not audio streams of this title", missing)
			}
		}
		if err == nil {
			return tracks, nil
		}
		if p.eof {
			return nil, err
		}
		printErr("%v", err)
	}
}

// askAudio asks for the audio tracks, listing the streams of the first selected
// title that has stream info. The choice applies to all selected titles.
// It returns the explicit tracks, or the unchanged --audio-tracks value if the
// scan has no stream info.
func (r *Ripper) askAudio(p *prompter, selected []disc.Title, settings encoder.EncodeSettings) ([]int, error) {
	i := slices.IndexFunc(selected, func(t disc.Title) bool { return len(t.Streams) > 0 })
	if i < 0 {
		printWarn("No stream info in the disc scan; cannot ask for audio tracks")
		return r.opts.AudioTracks, nil
	}
	ref := selected[i]
	streams := toEncoderStreams(ref)

	defaults := r.opts.AudioTracks
	if len(defaults) == 0 {
		for _, s := range encoder.SelectStreams(streams, settings) {
			if s.Type == "audio" {
				defaults = append(defaults, s.Index)
			}
		}
	}

	printInfo("Audio streams of title %d", ref.ID)
	tracks, err := chooseAudio(p, streams, defaults)
	if err != nil {
		return nil, err
	}

	for _, t := range selected {
		if len(t.Streams) == 0 || t.ID == ref.ID {
			continue
		}
		if missing := encoder.MissingAudioTracks(toEncoderStreams(t), tracks); len(missing) > 0 {
			printWarn("Title %d has no audio stream(s) %v; encoding it will fail", t.ID, missing)
		}
	}
	return tracks, nil
}

// audioLanguages returns the languages of the audio streams of t, e.g. "eng,ger".
func audioLanguages(t disc.Title) string {
	var langs []string
	for _, s := range t.Streams {
		if s.Type != "audio" {
			continue
		}
		lang := s.Language
		if lang == "" {
			lang = "und"
		}
		langs = append(langs, lang)
	}
	return strings.Join(langs, ",")
}

// toEncoderStreams converts the scanned streams of a title to the encoder's stream type.
func toEncoderStreams(t disc.Title) []encoder.Stream {
	streams := make([]encoder.Stream, 0, len(t.Streams))
	for _, s := range t.Streams {
		streams = append(streams, encoder.Stream{
			Index:    s.Index,
			Type:     s.Type,
			Codec:    s.Codec,
			Language: s.Language,
			Width:    s.Width,
			Height:   s.Height,
			Channels: s.Channels,
		})
	}
	return streams
}
