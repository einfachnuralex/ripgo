package shellquote

import "testing"

func TestJoin(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"ffmpeg", []string{"-i", "/tmp/a.mkv"}, "ffmpeg -i /tmp/a.mkv"},
		{"ffmpeg", []string{"-i", "/tmp/My Movie (2020).mkv"}, "ffmpeg -i '/tmp/My Movie (2020).mkv'"},
		{"echo", []string{"it's"}, `echo 'it'\''s'`},
		{"echo", []string{""}, "echo ''"},
		{"makemkvcon", []string{"-r", "mkv", "disc:0", "3"}, "makemkvcon -r mkv disc:0 3"},
	}
	for _, tt := range tests {
		if got := Join(tt.name, tt.args...); got != tt.want {
			t.Errorf("Join(%q, %q) = %q, want %q", tt.name, tt.args, got, tt.want)
		}
	}
}
