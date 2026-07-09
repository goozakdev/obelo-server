package scanner

import "testing"

// Pure unit tests for the TV token parser (issue tv-music/01 acceptance
// criterion), mirroring identity_test.go. No filesystem, no ffprobe — the
// SxxExx / range / date / absolute grammar from docs/naming-convention.md.

func TestParseSeasonFolder(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"Season 01", 1, true},
		{"Season 1", 1, true},
		{"season 10", 10, true},
		{"Season 00", 0, true},
		{"Specials", 0, true},
		{"specials", 0, true},
		{"Extras", 0, false},
		{"The Bear (2022)", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseSeasonFolder(tc.in)
		if ok != tc.wantOK {
			t.Errorf("ParseSeasonFolder(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ParseSeasonFolder(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseEpisodeTokenCanonical(t *testing.T) {
	tok, ok := ParseEpisodeToken("The Bear (2022) - S01E05 - Sheridan", 1)
	if !ok {
		t.Fatal("expected SxxExx parse to succeed")
	}
	if tok.Kind != "sxxexx" || tok.Season != 1 || tok.Episode != 5 || tok.IsRange() {
		t.Errorf("token = %+v, want season 1 episode 5 (not range)", tok)
	}
}

func TestParseEpisodeTokenCaseInsensitive(t *testing.T) {
	tok, ok := ParseEpisodeToken("show - s02e10 - title", 2)
	if !ok || tok.Season != 2 || tok.Episode != 10 {
		t.Fatalf("case-insensitive parse = %+v (ok=%v), want s2e10", tok, ok)
	}
}

func TestParseEpisodeTokenRange(t *testing.T) {
	for _, in := range []string{
		"Show (2020) - S01E05-E06 - Double",
		"Show (2020) - S01E05E06",
		"Show (2020) - S01E05-06",
	} {
		tok, ok := ParseEpisodeToken(in, 1)
		if !ok {
			t.Fatalf("range parse %q failed", in)
		}
		if !tok.IsRange() || tok.Episode != 5 || tok.EpisodeEnd != 6 {
			t.Errorf("range %q = %+v, want E05..E06", in, tok)
		}
	}
}

func TestParseEpisodeTokenDate(t *testing.T) {
	tok, ok := ParseEpisodeToken("The Daily Show - 2024-01-15 - Guest", -1)
	if !ok || tok.Kind != "date" {
		t.Fatalf("date parse = %+v (ok=%v), want kind date", tok, ok)
	}
	if tok.Raw != "2024-01-15" || tok.Label != "2024-01-15" {
		t.Errorf("date token raw/label = %q/%q, want 2024-01-15", tok.Raw, tok.Label)
	}
}

func TestParseEpisodeTokenAbsolute(t *testing.T) {
	tok, ok := ParseEpisodeToken("One Piece - 135 - The Battle", -1)
	if !ok || tok.Kind != "absolute" {
		t.Fatalf("absolute parse = %+v (ok=%v), want kind absolute", tok, ok)
	}
	if tok.Episode != 135 || tok.Raw != "135" {
		t.Errorf("absolute token = %+v, want episode 135", tok)
	}
}

func TestParseEpisodeTokenNone(t *testing.T) {
	// A name with no recognized episode token → Unmatched (caller routes it).
	if _, ok := ParseEpisodeToken("cover", -1); ok {
		t.Error("expected no episode token for 'cover'")
	}
}

func TestParseEpisodeTokenSxxExxWins(t *testing.T) {
	// A SxxExx token wins even when a year-like number is also present.
	tok, ok := ParseEpisodeToken("Show (2011) - S03E07 - Title", 3)
	if !ok || tok.Kind != "sxxexx" || tok.Season != 3 || tok.Episode != 7 {
		t.Fatalf("token = %+v (ok=%v), want s3e7 sxxexx", tok, ok)
	}
}

func TestEpisodeTitleName(t *testing.T) {
	tok, _ := ParseEpisodeToken("The Bear (2022) - S01E05 - Sheridan", 1)
	if got := episodeTitleName("The Bear (2022) - S01E05 - Sheridan", tok); got != "Sheridan" {
		t.Errorf("episodeTitleName = %q, want Sheridan", got)
	}
	// No trailing title → synthesized code.
	tok2, _ := ParseEpisodeToken("The Bear (2022) - S01E05", 1)
	if got := episodeTitleName("The Bear (2022) - S01E05", tok2); got != "S01E05" {
		t.Errorf("episodeTitleName fallback = %q, want S01E05", got)
	}
}
