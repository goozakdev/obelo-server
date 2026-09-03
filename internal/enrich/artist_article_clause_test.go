package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The artist-narrowing clause is article-insensitive (album-resolves-its-tracks/15,
// the amendment to ADR-0037).
//
// The operator's report: open the album row for the Eagles' *Hell Freezes Over*,
// press "Fix the album", and the pre-filled search returns nothing — because the
// artist box says "The Eagles" and MusicBrainz credits that release-group to
// "Eagles". `artist:"…"` is a Lucene PHRASE over the analyzed artist-credit field,
// so a tagged article contributes a token the credit does not have and the phrase
// cannot match. Verified live against MusicBrainz on 2026-09-02:
//
//	Hell Freezes Over AND artist:"The Eagles"                  → count 0
//	Hell Freezes Over AND artist:("The Eagles" OR "Eagles")    → count 3, top by "Eagles"
//	Disintegration    AND artist:"Cure"                        → count 4, top by "The Cure"
//	Different Light   AND artist:"The Bangles"                 → count 0
//	Different Light   AND artist:"Bangles"                     → count 2, the album
//
// The last three are why the clause strips an article but never ADDS one: a
// one-token phrase already matches inside a longer credit, so `artist:"Cure"` finds
// "The Cure" unaided, and the match set of `artist:"The X"` is a strict subset of
// `artist:"X"`'s. Adding that alternative cannot return one extra row — it can only
// rewrite the URL of every article-less artist in the library, which is exactly what
// TestArtistClauseWithoutAnArticleIsTodaysClause exists to prevent.
//
// One clause serves the album picker, the track picker and the enrichment pass's own
// track search, so the tests below assert it on the wire on each of those paths.

// mbAlbumQueryStub captures the `query` the release-group search sends and answers
// with one hit. The wire string is what MusicBrainz parses, so it is what is
// asserted — not a helper's return value.
func mbAlbumQueryStub(t *testing.T, got *string) *MusicBrainzProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group" {
			http.NotFound(w, r)
			return
		}
		*got = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"release-groups": [
		  {"id": "rg-hfo", "title": "Hell Freezes Over", "primary-type": "Album",
		   "first-release-date": "1994-11-08",
		   "artist-credit": [{"name": "Eagles"}]}
		]}`))
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0
	return p
}

func albumSearchQuery(t *testing.T, album, artist string) (string, []Candidate) {
	t.Helper()
	var got string
	p := mbAlbumQueryStub(t, &got)
	cands, err := p.Search(context.Background(), "album", album, SearchOptions{Artist: artist})
	if err != nil {
		t.Fatalf("Search(%q, artist=%q): %v", album, artist, err)
	}
	return got, cands
}

// TestAlbumSearchNarrowsArticleInsensitively is the reported bug: the album the
// operator could not find is found with the artist box left exactly as the tags
// filled it.
func TestAlbumSearchNarrowsArticleInsensitively(t *testing.T) {
	got, cands := albumSearchQuery(t, "Hell Freezes Over", "The Eagles")

	want := `Hell Freezes Over AND artist:("The Eagles" OR "Eagles")`
	if got != want {
		t.Errorf("query = %q, want %q\n\nThe pre-filled artist is the tagged name; "+
			"MusicBrainz credits this release-group to \"Eagles\", and the exact phrase "+
			"`artist:\"The Eagles\"` returns nothing (verified live).", got, want)
	}
	if len(cands) != 1 || cands[0].ExternalID != "rg-hfo" {
		t.Fatalf("candidates = %+v, want the one narrowed release-group", cands)
	}
}

// TestArtistClauseWithoutAnArticleIsTodaysClause: an artist with no leading article
// sends EXACTLY the query it sent before this change — one phrase, no parentheses,
// no OR. This is the assertion that keeps an article-insensitive clause from
// quietly becoming an OR-everything one: the common case is provably untouched, and
// a name with no article has nothing to be insensitive about.
func TestArtistClauseWithoutAnArticleIsTodaysClause(t *testing.T) {
	cases := []struct {
		name   string
		artist string
		want   string
	}{
		{"a plain name", "Radiohead", `OK Computer AND artist:"Radiohead"`},
		// The band issue 15 read as the reverse direction. It is not one:
		// MusicBrainz credits *Different Light* to "Bangles", and this very query
		// returns it (count 2, live) — while `artist:"The Bangles"` returns nothing.
		{"a name whose credit may carry an article", "Bangles", `OK Computer AND artist:"Bangles"`},
		{"a metacharacter name", "AC/DC", `OK Computer AND artist:"AC\/DC"`},
		{"punctuation only", "!!!", `OK Computer AND artist:"\!\!\!"`},
		// "Anthrax" begins with the letters of an article but not with the WORD, and
		// a bare "The" has nothing after the article to strip.
		{"a name that merely starts like an article", "Anthrax", `OK Computer AND artist:"Anthrax"`},
		{"an artist named for the article itself", "The", `OK Computer AND artist:"The"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := albumSearchQuery(t, "OK Computer", tc.artist)
			if got != tc.want {
				t.Errorf("query = %q, want the single-phrase clause %q", got, tc.want)
			}
			if strings.Contains(got, " OR ") || strings.Contains(got, "artist:(") {
				t.Errorf("query %q widened an artist that has no article to widen", got)
			}
		})
	}
}

// TestArtistClauseAlternativesAreEscapedAndQuoted: both alternatives go through the
// same Lucene escaping the single phrase always did, so an article artist whose name
// carries a metacharacter narrows the search instead of 4xx-ing the parser.
func TestArtistClauseAlternativesAreEscapedAndQuoted(t *testing.T) {
	cases := []struct {
		name   string
		artist string
		want   string
	}{
		{"the", "The Eagles", `AND artist:("The Eagles" OR "Eagles")`},
		{"a", "A Perfect Circle", `AND artist:("A Perfect Circle" OR "Perfect Circle")`},
		{"an", "An Emerald City", `AND artist:("An Emerald City" OR "Emerald City")`},
		{"lower-cased article", "the eagles", `AND artist:("the eagles" OR "eagles")`},
		{"hyphens and quotes", `The "B-52's"`, `AND artist:("The \"B\-52's\"" OR "\"B\-52's\"")`},
		{"a slash", "The AC/DC Tribute", `AND artist:("The AC\/DC Tribute" OR "AC\/DC Tribute")`},
		// Only ONE article comes off, exactly as the identity key strips one
		// (ADR-0037): "The The" keeps a name to search for.
		{"only one article", "The The", `AND artist:("The The" OR "The")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := albumSearchQuery(t, "Greatest Hits", tc.artist)
			if !strings.Contains(got, tc.want) {
				t.Errorf("query = %q, want it to carry %q", got, tc.want)
			}
		})
	}
}

// TestBlankArtistStillEmitsNoClause: a widened clause is still no clause at all when
// there is no artist to narrow by — an empty `artist:("" OR "The ")` would match
// nothing and turn a working search into a dead one.
func TestBlankArtistStillEmitsNoClause(t *testing.T) {
	got, _ := albumSearchQuery(t, "Hell Freezes Over", "   ")
	if got != "Hell Freezes Over" {
		t.Errorf("query = %q, want the bare terms with no clause", got)
	}
	if strings.Contains(got, "artist:") {
		t.Errorf("query %q carries an empty artist clause", got)
	}
}

// TestReleaseClauseIsNotArticleInsensitive: the album-narrowing clause is
// deliberately left alone. An album's leading article is part of its title far more
// often than a band's is part of its name ("The Wall", "A Night at the Opera"), and
// the evidence behind ADR-0037's amendment is about artists only.
func TestReleaseClauseIsNotArticleInsensitive(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	if _, err := p.Search(context.Background(), "track", "Another Brick in the Wall",
		SearchOptions{Artist: "Pink Floyd", Release: "The Wall"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := `AND release:"The Wall"`; !strings.Contains(got, want) {
		t.Errorf("query = %q, want the release clause left exact (%q)", got, want)
	}
	if strings.Contains(got, `release:(`) {
		t.Errorf("query %q guesses at the album's article; nothing argues for that", got)
	}
}

// TestTrackPickerNarrowsArticleInsensitively: the recording picker shares the one
// clause, so the Needs-Fixing track row is fixed by the same change — and its
// release scope (needs-fixing/06) still rides alongside untouched.
func TestTrackPickerNarrowsArticleInsensitively(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	if _, err := p.Search(context.Background(), "track", "Get Over It",
		SearchOptions{Artist: "The Eagles", Release: "Hell Freezes Over"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := `Get Over It AND artist:("The Eagles" OR "Eagles") AND release:"Hell Freezes Over"`
	if got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

// TestPassTrackSearchNarrowsArticleInsensitively: the enrichment pass's own last-
// resort track search (trackDetails) builds its query with the SAME musicQuery, so
// an article artist's tracks stop falling to search-no-match. One clause, one code
// path — the picker and the pass cannot diverge.
func TestPassTrackSearchNarrowsArticleInsensitively(t *testing.T) {
	p, queries := mbSearchStub(t, "Get Over It")

	meta, err := trackLookup(t, p, "Get Over It", "The Eagles")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	got := queries()
	if len(got) != 1 {
		t.Fatalf("sent %d queries, want exactly 1 (%v) — ADR-0049 allows one", len(got), got)
	}
	// The stub answers 400 to a query the Lucene parser would reject, so this also
	// asserts the widened clause is well-formed on the wire.
	if want := `Get Over It AND artist:("The Eagles" OR "Eagles")`; got[0] != want {
		t.Errorf("query = %q, want %q", got[0], want)
	}
	if !meta.Matched || meta.ExternalID != "rec-1" {
		t.Fatalf("meta = %+v, want the accepted recording", meta)
	}
}

// TestPassTrackSearchWidenedClauseParsesWithMetacharacters: BOTH alternatives are
// escaped, so an article artist whose name carries a Lucene metacharacter still
// sends a query the parser accepts. The stub answers 400 to an unescaped one,
// exactly as the live server does — which is how the old unescaped phrase surfaced
// as a fake provider failure rather than as a search.
func TestPassTrackSearchWidenedClauseParsesWithMetacharacters(t *testing.T) {
	p, queries := mbSearchStub(t, "Get Over It")

	meta, err := trackLookup(t, p, "Get Over It", "The AC/DC Tribute")
	if err != nil {
		t.Fatalf("lookup: %v (the widened clause did not survive the query parser: %v)",
			err, queries())
	}
	if !meta.Matched {
		t.Fatalf("meta = %+v, want the accepted recording", meta)
	}
}

// TestPassTrackSearchStillRejectsAWrongTitle: widening the artist clause widens what
// the SEARCH may return, and nothing else. Issue 05's acceptance test still stands
// between a hit and a record (ADR-0050) — a relevance query essentially always
// returns something, and taking it blind is the confident-wrong-answer ADR-0049
// ruled is the worse outcome.
func TestPassTrackSearchStillRejectsAWrongTitle(t *testing.T) {
	p, queries := mbSearchStub(t, "Hotel California")

	_, err := trackLookup(t, p, "Get Over It", "The Eagles")
	if !errors.Is(err, ErrMatchRejected) {
		t.Fatalf("err = %v, want ErrMatchRejected — the article-insensitive clause must "+
			"not buy recall by relaxing the acceptance test", err)
	}
	if got := queries(); len(got) != 1 {
		t.Fatalf("sent %d queries, want exactly 1 (%v)", len(got), got)
	}
}
