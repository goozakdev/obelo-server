package store

import (
	"path/filepath"
	"testing"
)

// TestUnderAny pins the prefix-boundary logic that decides whether a file lies
// within an unreadable subtree. The separator boundary matters: "/lib/Artist"
// must NOT swallow a sibling "/lib/Artist Two" that merely shares a string prefix.
func TestUnderAny(t *testing.T) {
	sep := string(filepath.Separator)
	base := sep + "lib" + sep + "Artist"
	prefixes := []string{base}
	cases := []struct {
		path string
		want bool
	}{
		{base, true}, // the directory itself
		{base + sep + "Album" + sep + "01.flac", true}, // a descendant
		{base + " Two" + sep + "02.flac", false},       // sibling sharing a string prefix
		{sep + "lib" + sep + "Other" + sep + "x.flac", false},
	}
	for _, c := range cases {
		if got := underAny(c.path, prefixes); got != c.want {
			t.Errorf("underAny(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if underAny(base, nil) {
		t.Errorf("underAny with no prefixes must be false")
	}
}
