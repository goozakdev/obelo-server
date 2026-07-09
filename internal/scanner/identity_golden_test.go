package scanner

import "testing"

// TestIdentityKeyGolden locks the current parse(input) → identity-key mappings.
//
// Per ADR-0014, watch state and catalog identity are keyed to the parsed Title
// identity. A future parser change that silently re-keyed an existing input
// would re-point its watch history onto a different Title (or orphan it). This
// golden table is the guard: if a change to identity.go shifts any of these
// keys, this test fails loudly, forcing a deliberate decision + migration rather
// than a silent re-keying. To intentionally change a key, update the expected
// value here in the same commit.
func TestIdentityKeyGolden(t *testing.T) {
	cases := []struct {
		in      string
		wantKey string
		wantOK  bool
	}{
		// Plain title+year: normalized title | year.
		{"Dune (2021)", "dune|2021", true},
		{"Dune (1984)", "dune|1984", true},
		// Case/punctuation folding collapses spellings to one key.
		{"The Lord of the Rings (2001)", "the lord of the rings|2001", true},
		{"the  lord   of the rings (2001)", "the lord of the rings|2001", true},
		{"Blade Runner (1982)", "blade runner|1982", true},
		{"Dune - (2021)", "dune|2021", true},
		// Yearless title is filed (year 0) → "<title>|0".
		{"Yearless Movie", "yearless movie|0", true},
		// Embedded external id IS the key (overrides title+year).
		{"Pinned Movie (2014) {tmdb-12345}", "tmdb:12345", true},
		{"Some Film (2000) {imdb-tt0123456}", "imdb:tt0123456", true},
		{"Crash (2004) {tmdb-1}", "tmdb:1", true},
		{"Crash (2004) {tmdb-2}", "tmdb:2", true},
		// imdb id is lower-cased into the key.
		{"X (1999) {imdb-TT9999999}", "imdb:tt9999999", true},
		// No minimal identity → not ok (routes to Unmatched).
		{"1080p", "", false},
		{"sample", "", false},
		{"   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseIdentity(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseIdentity(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Key != tc.wantKey {
				t.Errorf("ParseIdentity(%q).Key = %q, want %q (identity re-keyed — see ADR-0014)",
					tc.in, got.Key, tc.wantKey)
			}
		})
	}
}

// TestIdentityKeyForMatchesParse pins that IdentityKeyFor (the fix-match key
// derivation) produces the SAME key a scan's parse would for the same identity,
// so an override re-resolves to the intended Title.
func TestIdentityKeyForMatchesParse(t *testing.T) {
	parsed, _ := ParseIdentity("Dune (2021)")
	if k := IdentityKeyFor("Dune", 2021, "", ""); k != parsed.Key {
		t.Errorf("IdentityKeyFor title/year = %q, want %q", k, parsed.Key)
	}
	parsedID, _ := ParseIdentity("Whatever (2000) {tmdb-555}")
	if k := IdentityKeyFor("Whatever", 2000, "555", ""); k != parsedID.Key {
		t.Errorf("IdentityKeyFor tmdb = %q, want %q", k, parsedID.Key)
	}
}
