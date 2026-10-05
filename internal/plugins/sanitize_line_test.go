package plugins

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A guest's log line must not carry terminal escapes or line separators into the
// server log, and truncation must not split a rune.
func TestSanitizeLine(t *testing.T) {
	parallel(t)
	for in, want := range map[string]string{
		"a\x1b[31mred":   "a [31mred",
		"a b c":          "a b c",
		"a\tb\x00c\x7fd": "a b c d",
		"plain é text":   "plain é text",
		"line1\r\nline2": "line1  line2",
	} {
		if got := sanitizeLine(in); got != want {
			t.Errorf("sanitizeLine(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", maxLogLine)
	got := sanitizeLine(strings.Repeat("a", maxLogLine-1) + long)
	if !utf8.ValidString(got) {
		t.Errorf("truncation split a rune: %q", got[len(got)-20:])
	}
}
