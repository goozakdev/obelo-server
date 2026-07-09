package scanner

import "testing"

func TestParseIdentity(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTitle string
		wantYear  int
		wantOK    bool
	}{
		{"folder with year", "Dune (2021)", "Dune", 2021, true},
		{"bare file basename with year", "Blade Runner (1982)", "Blade Runner", 1982, true},
		{"separator before year", "Dune - (2021)", "Dune", 2021, true},
		{"no year files title-only", "Some Movie", "Some Movie", 0, true},
		{"year in 1900s", "Casablanca (1942)", "Casablanca", 1942, true},
		{"empty is not ok", "   ", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseIdentity(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", got.Title, tc.wantTitle)
			}
			if got.Year != tc.wantYear {
				t.Errorf("year = %d, want %d", got.Year, tc.wantYear)
			}
			if got.Key == "" {
				t.Errorf("identity key is empty")
			}
		})
	}
}

// TestSortTitleStripsLeadingArticle: "The …" titles order by the following word
// (file under M, not T), while bare "the" and "the"-prefixed words are untouched.
func TestSortTitleStripsLeadingArticle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"The Matrix", "matrix"},
		{"the matrix", "matrix"},
		{"  The   Matrix  ", "matrix"},
		{"The Lord of the Rings", "lord of the rings"},
		{"A Beautiful Mind", "beautiful mind"},
		{"An American Tail", "american tail"},
		{"Matrix Reloaded", "matrix reloaded"},
		// Only one leading article is stripped, not cascaded.
		{"The A-Team", "a-team"},
		{"A An Apple", "an apple"},
		// Bare articles and words that merely begin with the letters are untouched.
		{"Theater", "theater"},
		{"Antenna", "antenna"},
		{"Apple", "apple"},
		{"The", "the"},
		{"An", "an"},
		{"A", "a"},
		{"Them", "them"},
	}
	for _, tc := range cases {
		if got := sortTitle(tc.in); got != tc.want {
			t.Errorf("sortTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUninvertTitle: a comma-trailed article is moved to the front (natural
// order), its casing normalized; non-inverted titles pass through unchanged.
func TestUninvertTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Island, The", "The Island"},
		{"island, the", "The island"},
		{"American Tail, An", "An American Tail"},
		{"Beautiful Mind, A", "A Beautiful Mind"},
		{"Good, the Bad and the Ugly, The", "The Good, the Bad and the Ugly"},
		{"The Lord of the Rings", "The Lord of the Rings"},
		{"Matrix", "Matrix"},
		{"Seven, Se7en", "Seven, Se7en"}, // trailing word is not an article
		{"Apollo 13", "Apollo 13"},
	}
	for _, tc := range cases {
		if got := uninvertTitle(tc.in); got != tc.want {
			t.Errorf("uninvertTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseIdentityUninvertsButKeepsKey: an inverted folder name displays in
// natural order and sorts by the real word, yet the identity key is derived from
// the parsed (inverted) title so a rescan keeps matching the same row.
func TestParseIdentityUninvertsButKeepsKey(t *testing.T) {
	inv, ok := ParseIdentity("Island, The (2005)")
	if !ok {
		t.Fatal("ParseIdentity failed")
	}
	if inv.Title != "The Island" {
		t.Errorf("display title = %q, want %q", inv.Title, "The Island")
	}
	if got := sortTitle(inv.Title); got != "island" {
		t.Errorf("sort title = %q, want %q", got, "island")
	}
	// The key must match what the OLD parse (inverted title) produced, so existing
	// rows are not orphaned. IdentityKeyFor keys off the raw title we pass in.
	wantKey := IdentityKeyFor("Island, The", 2005, "", "")
	if inv.Key != wantKey {
		t.Errorf("identity key = %q, want %q (keyed on parsed title)", inv.Key, wantKey)
	}
}

// TestIdentityKeyStable: differently-punctuated/cased spellings of the same
// movie collapse to one identity key (dedup across rescans).
func TestIdentityKeyStable(t *testing.T) {
	a, _ := ParseIdentity("The Lord of the Rings (2001)")
	b, _ := ParseIdentity("the  lord   of the rings (2001)")
	if a.Key != b.Key {
		t.Errorf("keys differ: %q vs %q", a.Key, b.Key)
	}

	// Different year is a different identity (Dune 2021 != Dune 1984).
	c, _ := ParseIdentity("Dune (2021)")
	d, _ := ParseIdentity("Dune (1984)")
	if c.Key == d.Key {
		t.Errorf("same key for different years: %q", c.Key)
	}
}

// TestParseIdentityEmbeddedID: an embedded {tmdb/imdb-id} is recorded and IS the
// identity key, so it overrides title+year and breaks a same-title-same-year
// collision (naming-convention.md).
func TestParseIdentityEmbeddedID(t *testing.T) {
	a, ok := ParseIdentity("Pinned Movie (2014) {tmdb-12345}")
	if !ok || a.TMDBID != "12345" || a.Title != "Pinned Movie" || a.Year != 2014 {
		t.Fatalf("tmdb parse = %+v (ok=%v)", a, ok)
	}
	if a.Key != "tmdb:12345" {
		t.Errorf("tmdb key = %q, want tmdb:12345", a.Key)
	}

	b, ok := ParseIdentity("Some Film (2000) {imdb-tt0123456}")
	if !ok || b.IMDBID != "tt0123456" {
		t.Fatalf("imdb parse = %+v (ok=%v)", b, ok)
	}
	if b.Key != "imdb:tt0123456" {
		t.Errorf("imdb key = %q, want imdb:tt0123456", b.Key)
	}

	// Same title+year, different tmdb ids → distinct identities (collision broken).
	x, _ := ParseIdentity("Crash (2004) {tmdb-1}")
	y, _ := ParseIdentity("Crash (2004) {tmdb-2}")
	if x.Key == y.Key {
		t.Errorf("embedded ids failed to disambiguate same-title-same-year: %q", x.Key)
	}
}

// TestParseIdentityNoMinimalIdentity: a name that is only a quality/junk token
// has no extractable identity (→ Unmatched), while a real yearless title parses.
func TestParseIdentityNoMinimalIdentity(t *testing.T) {
	for _, in := range []string{"1080p", "2160p", "sample", "   ", "----"} {
		if _, ok := ParseIdentity(in); ok {
			t.Errorf("ParseIdentity(%q) ok = true, want false (no minimal identity)", in)
		}
	}
	// A genuine yearless title still parses (filed + needs-review later).
	id, ok := ParseIdentity("Yearless Movie")
	if !ok || id.HasYear() {
		t.Errorf("Yearless Movie parse = %+v (ok=%v), want title-only", id, ok)
	}
}
