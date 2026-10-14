// Package shellquote formats commands so they can be copy-pasted into a POSIX shell.
package shellquote

import "strings"

// Join returns name followed by args, each quoted for a POSIX shell where necessary.
func Join(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, Quote(name))
	for _, a := range args {
		parts = append(parts, Quote(a))
	}
	return strings.Join(parts, " ")
}

// Quote returns s unchanged if it only contains shell-safe characters,
// otherwise wraps it in single quotes (escaping embedded single quotes).
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if isSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}
