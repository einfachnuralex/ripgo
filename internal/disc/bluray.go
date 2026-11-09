package disc

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// minPlaylistSeconds hides menu loops and trailers from the playlist list.
const minPlaylistSeconds = 120

// scanBluray reads all playlists from BDMV/PLAYLIST. No ffprobe is needed:
// durations, streams (PID, codec, language) and chapters come from the MPLS files.
func scanBluray(fsys discFS, info *DiscInfo) error {
	names, err := fsys.ReadDir("BDMV/PLAYLIST")
	if err != nil {
		return err
	}
	slices.Sort(names)

	for _, name := range names {
		num, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(name), ".mpls"))
		if err != nil || !strings.HasSuffix(strings.ToLower(name), ".mpls") {
			continue
		}
		data, err := fsys.ReadFile("BDMV/PLAYLIST/" + name)
		if err != nil {
			info.Warnings = append(info.Warnings, "reading "+name+": "+err.Error())
			continue
		}
		pl, err := parseMPLS(data)
		if err != nil {
			info.Warnings = append(info.Warnings, name+": "+err.Error())
			continue
		}
		dur := int(pl.duration() / mplsTicks)
		if dur < minPlaylistSeconds {
			continue
		}

		t := Title{ID: num, SourceFile: name, Duration: dur, chapters: pl.chapters()}
		t.Chapters = len(t.chapters)
		for i, s := range pl.streams {
			s.Index = i
			s.Source = pidSource(pl.pids[i])
			t.Streams = append(t.Streams, s)
		}
		info.Titles = append(info.Titles, t)
	}

	if name := blurayTitle(fsys); name != "" {
		info.Name = name
	}
	if !hasKeyDB() {
		info.Warnings = append(info.Warnings, "no AACS KEYDB.cfg found (~/.config/aacs/); encrypted Blu-rays cannot be ripped")
	}
	return nil
}

var diName = regexp.MustCompile(`<di:name>([^<]+)</di:name>`)

// blurayTitle returns the disc title from the BD metadata (bdmt_eng.xml, else any language).
func blurayTitle(fsys discFS) string {
	names, err := fsys.ReadDir("BDMV/META/DL")
	if err != nil {
		return ""
	}
	slices.SortFunc(names, func(a, b string) int {
		// English first, then alphabetical
		ae, be := strings.EqualFold(a, "bdmt_eng.xml"), strings.EqualFold(b, "bdmt_eng.xml")
		switch {
		case ae && !be:
			return -1
		case be && !ae:
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, n := range names {
		lower := strings.ToLower(n)
		if !strings.HasPrefix(lower, "bdmt_") || !strings.HasSuffix(lower, ".xml") {
			continue
		}
		data, err := fsys.ReadFile("BDMV/META/DL/" + n)
		if err != nil {
			continue
		}
		if m := diName.FindSubmatch(data); m != nil {
			return strings.TrimSpace(string(m[1]))
		}
	}
	return ""
}

// hasKeyDB reports whether libaacs will find a KEYDB.cfg.
func hasKeyDB() bool {
	var dirs []string
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		dirs = append(dirs, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config"), filepath.Join(home, "Library", "Preferences"))
	}
	dirs = append(dirs, "/etc/xdg")
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "aacs", "KEYDB.cfg")); err == nil {
			return true
		}
	}
	return false
}
