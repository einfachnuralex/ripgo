package ripper

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/einfachnuralex/ripgo/internal/encoder"
	"github.com/einfachnuralex/ripgo/internal/makemkv"
)

func TestParseIndexList(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"", nil, false},
		{"  ", nil, false},
		{"1", []int{1}, false},
		{"3, 1 ,2", []int{3, 1, 2}, false},
		{"1,2,1", []int{1, 2}, false},
		{"a", nil, true},
		{"1,-2", nil, true},
		{"1,,2", nil, true},
	}
	for _, tt := range tests {
		got, err := ParseIndexList(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseIndexList(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseIndexList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestPrompterAsk(t *testing.T) {
	p := newPrompter(strings.NewReader("  My Movie \n\nlast"), io.Discard)
	if got := p.ask("q", "def"); got != "My Movie" {
		t.Errorf("got %q", got)
	}
	if got := p.ask("q", "def"); got != "def" {
		t.Errorf("empty line: got %q, want default", got)
	}
	if got := p.ask("q", "def"); got != "last" || !p.eof {
		t.Errorf("last line without newline: got %q, eof %v", got, p.eof)
	}
	if got := p.ask("q", "def"); got != "def" {
		t.Errorf("EOF: got %q, want default", got)
	}
}

func testTitles() []makemkv.Title {
	return []makemkv.Title{{ID: 0, Name: "a"}, {ID: 1, Name: "b"}, {ID: 5, Name: "c", SourceFile: "00800.mpls"}}
}

func ids(titles []makemkv.Title) []int {
	var out []int
	for _, t := range titles {
		out = append(out, t.ID)
	}
	return out
}

func TestChooseTitles(t *testing.T) {
	all := testTitles()
	defaults := all[1:2]

	tests := []struct {
		name    string
		input   string
		want    []int
		wantErr bool
	}{
		{"enter keeps defaults", "\n", []int{1}, false},
		{"order is kept", "5,0,1\n", []int{5, 0, 1}, false},
		{"invalid then valid", "x\n9\n0\n", []int{0}, false},
		{"eof keeps defaults", "", []int{1}, false},
		{"invalid then eof fails", "9", nil, true},
	}
	for _, tt := range tests {
		got, err := chooseTitles(newPrompter(strings.NewReader(tt.input), io.Discard), "bd", all, defaults)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(ids(got), tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, ids(got), tt.want)
		}
	}
}

func TestChooseTitlesNoDefaults(t *testing.T) {
	// Without defaults an empty answer is not a selection.
	if _, err := chooseTitles(newPrompter(strings.NewReader("\n"), io.Discard), "dvd", testTitles(), nil); err == nil {
		t.Error("expected error when nothing is selected")
	}
}

func TestChooseAudio(t *testing.T) {
	streams := []encoder.Stream{
		{Index: 0, Type: "video"},
		{Index: 1, Type: "audio", Language: "eng", Codec: "ac3", Channels: 6},
		{Index: 2, Type: "audio", Language: "ger", Codec: "ac3", Channels: 2},
		{Index: 3, Type: "subtitle", Language: "eng"},
	}
	tests := []struct {
		name     string
		input    string
		defaults []int
		want     []int
		wantErr  bool
	}{
		{"enter keeps defaults", "\n", []int{1}, []int{1}, false},
		{"explicit list", "2, 1\n", []int{1}, []int{2, 1}, false},
		{"no defaults keeps automatic", "\n", nil, nil, false},
		{"video index rejected then valid", "0\n2\n", []int{1}, []int{2}, false},
		{"subtitle index rejected at eof", "3", []int{1}, nil, true},
	}
	for _, tt := range tests {
		got, err := chooseAudio(newPrompter(strings.NewReader(tt.input), io.Discard), streams, tt.defaults)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
