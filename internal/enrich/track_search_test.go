package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ADR-0050: "A search hit must pass an acceptance test before it becomes a record."
//
// The pass's last-resort track search (trackDetails) used to send an unescaped
// exact phrase, `recording:"<title>"`, and store Recordings[0] whatever it was.
// This file covers the two halves of the fix, which are only correct TOGETHER:
//
//   - the query becomes the interactive picker's (musicQuery — escaped,
//     relevance-ranked, artist-narrowed), so a title MusicBrainz punctuates
//     differently can be found at all; and
//   - the top hit is ACCEPTED only if its normalized title is the local one's,
//     because a relevance query essentially always returns something and taking it
//     blind would trade an honest empty answer for a confident wrong one.
//
// And one constraint that is invisible in the rows and easy to lose to a
// well-meaning fallback: ONE request, always (ADR-0049 — the search cluster is the
// dependency that sheds load globally, so a retry issued during its failures pushes
// the wrong way). Every outcome below asserts the request count.

// --- a MusicBrainz search that behaves like the real one -----------------------

// mbSearchStub serves /recording?query= from a fixed, relevance-ordered list of
// recording titles (ids "rec-1", "rec-2", …). It reproduces the two behaviours
// these tests turn on:
//
//   - it PARSES the query, and answers 400 to an unescaped Lucene metacharacter,
//     exactly as the live server does — which is how an unescaped `AC/DC` used to
//     surface as a provider failure rather than as a search; and
//   - an exact-phrase recording:"X" only ever matches a recording titled exactly
//     X, while relevance terms are scored rather than filtered, so everything the
//     fixture holds comes back. That is the whole difference between the old query
//     and the new one, so a regression to the phrase shows up as zero hits.
//
// The returned func reports every query the provider sent, in order — the request
// COUNT is an assertion in its own right here.
func mbSearchStub(t *testing.T, hits ...string) (*MusicBrainzProvider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recording" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		if !luceneParses(q) {
			http.Error(w, `{"error":"invalid query"}`, http.StatusBadRequest)
			return
		}
		type row struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		rows := []row{}
		for i, title := range hits {
			if phraseAdmits(q, title) {
				rows = append(rows, row{ID: fmt.Sprintf("rec-%d", i+1), Title: title})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]row{"recordings": rows})
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0 // don't throttle the test
	return p, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

// phraseAdmits reports whether a recording titled title would come back for this
// query. An exact phrase demands the title be spelled exactly as asked — which is
// why `( I Could Only ) Whisper Your Name` matched nothing at MusicBrainz.
func phraseAdmits(query, title string) bool {
	const prefix = `recording:"`
	if !strings.HasPrefix(query, prefix) {
		return true // relevance terms: scored, not filtered
	}
	phrase := query[len(prefix):]
	if i := strings.Index(phrase, `"`); i >= 0 {
		phrase = phrase[:i]
	}
	return phrase == title
}

// luceneParses is a crude stand-in for the query parser: every metacharacter in the
// search TEXT must be backslash-escaped. The `AND artist:"…"` clause is query
// structure rather than text, so only its contents are checked.
func luceneParses(query string) bool {
	const clause = ` AND artist:"`
	terms := query
	if i := strings.Index(query, clause); i >= 0 {
		terms = query[:i]
		if !luceneEscaped(strings.TrimSuffix(query[i+len(clause):], `"`)) {
			return false
		}
	}
	return luceneEscaped(terms)
}

func luceneEscaped(s string) bool {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\\' {
			i++ // whatever follows is escaped
			continue
		}
		if strings.ContainsRune(`+-!(){}[]^"~*?:/&|`, rs[i]) {
			return false
		}
	}
	return true
}

func trackLookup(t *testing.T, p *MusicBrainzProvider, track, artist string) (TitleMetadata, error) {
	t.Helper()
	return p.Lookup(context.Background(), TitleRef{Kind: "track", Track: track, Artist: artist})
}

// --- the query -----------------------------------------------------------------

// The pass sends the PICKER's query, not the exact phrase it used to. The picker
// was deliberately moved off `recording:"…"` (item-editing/search-improvements);
// leaving the automatic matcher on it made it strictly worse than the manual one it
// hands its failures to.
func TestTrackSearchSendsTheRelevanceQueryNotAnExactPhrase(t *testing.T) {
	p, queries := mbSearchStub(t, "Whisper Your Name")

	if _, err := trackLookup(t, p, "Whisper Your Name", "Harry Connick Jr."); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	got := queries()
	if len(got) != 1 {
		t.Fatalf("sent %d queries, want exactly 1 (%v)", len(got), got)
	}
	if strings.Contains(got[0], `recording:"`) {
		t.Errorf("query %q is still an exact-phrase recording:\"…\" — the pass is back on the "+
			"shape the picker abandoned, and misses every title MusicBrainz punctuates "+
			"differently (ADR-0050)", got[0])
	}
	// The artist still narrows, as the field-scoped clause musicQuery builds.
	if want := `Whisper Your Name AND artist:"Harry Connick Jr."`; got[0] != want {
		t.Errorf("query = %q, want %q", got[0], want)
	}
}

// A Track with no artist still searches — unnarrowed, not with an empty clause that
// would match nothing.
func TestTrackSearchWithoutAnArtistIsUnnarrowed(t *testing.T) {
	p, queries := mbSearchStub(t, "Whisper Your Name")

	if _, err := trackLookup(t, p, "Whisper Your Name", ""); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	got := queries()
	if len(got) != 1 || got[0] != "Whisper Your Name" {
		t.Fatalf("queries = %v, want one unnarrowed %q", got, "Whisper Your Name")
	}
	if strings.Contains(got[0], "artist:") {
		t.Errorf("query %q carries an artist clause for a Track that has no artist", got[0])
	}
}

// --- recall: the titles the exact phrase could never find ----------------------

// The case this whole feature came from. The tag on disk and the record at
// MusicBrainz spell the same song differently, so the exact phrase returned
// nothing — the stub reproduces that, so a regression to the phrase fails here
// rather than passing quietly. 170 of the developer's 730 unmatched tracks carry a
// bracketed segment; this is not a one-off.
func TestTrackSearchFindsATitlePunctuatedDifferently(t *testing.T) {
	cases := []struct {
		name   string
		tagged string // what the file's tag says
		source string // what MusicBrainz calls it
	}{
		{"space inside the brackets", "( I Could Only ) Whisper Your Name", "(I Could Only) Whisper Your Name"},
		{"apostrophes", "Aint Misbehavin", "Ain't Misbehavin'"},
		{"case and diacritics", "Cafe BLEU", "Café Bleu"},
		{"a remaster suffix the tagger did not carry", "Paranoid Android", "Paranoid Android (Remastered 2011)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, queries := mbSearchStub(t, tc.source)

			meta, err := trackLookup(t, p, tc.tagged, "Harry Connick Jr.")
			if err != nil {
				t.Fatalf("%q did not resolve to %q: %v (queries: %v) — an exact phrase is "+
					"telling the truth about a spelling it cannot find, but the picker's "+
					"relevance query finds it (ADR-0050)", tc.tagged, tc.source, err, queries())
			}
			if !meta.Matched || meta.ExternalID != "rec-1" || meta.Name != tc.source {
				t.Errorf("meta = %+v, want the source's canonical %q as rec-1", meta, tc.source)
			}
			if n := len(queries()); n != 1 {
				t.Errorf("sent %d queries, want 1 (%v)", n, queries())
			}
		})
	}
}

// The other half of the picker's query: metacharacters are ESCAPED, so a title the
// Lucene parser would choke on issues a valid search instead of a 400 that surfaces
// as a provider failure. These are the exact cases escapeLucene was written for.
func TestTrackSearchEscapesTitlesTheParserWouldReject(t *testing.T) {
	cases := []struct{ name, track, artist string }{
		{"quotes in the title", `"Heroes"`, "David Bowie"},
		{"a slash in the artist", "Back in Black", "AC/DC"},
		{"exclamation marks", "Me and Giuliani Down by the School Yard", "!!!"},
		{"brackets and a dash", "Whisper Your Name [Live] - 1994", "Harry Connick Jr."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, queries := mbSearchStub(t, tc.track)

			meta, err := trackLookup(t, p, tc.track, tc.artist)
			if err != nil {
				t.Fatalf("%q / %q: %v (query: %v) — an unescaped metacharacter 4xx'd the "+
					"parser, which reaches the Admin as a provider failure rather than as "+
					"'no match'", tc.track, tc.artist, err, queries())
			}
			if !meta.Matched {
				t.Errorf("meta = %+v, want a match", meta)
			}
		})
	}
}

// --- the acceptance test -------------------------------------------------------

// The regression the acceptance test exists to prevent, asserted directly: a
// relevance query nearly always returns SOMETHING, and the top hit here is a
// different song. Storing it would be the confident wrong overview ADR-0049 ruled is
// worse than an empty one.
func TestTrackSearchRejectsATopHitThatIsADifferentSong(t *testing.T) {
	p, queries := mbSearchStub(t, "Whispering Your Name", "Whisper Your Name")

	meta, err := trackLookup(t, p, "Whisper Your Name", "Harry Connick Jr.")
	if !errors.Is(err, ErrMatchRejected) {
		t.Fatalf("err = %v, want ErrMatchRejected — the top hit was %q, a different song, and "+
			"it must not become this Track's record", err, "Whispering Your Name")
	}
	// Every existing caller tests ErrNoMatch; the new value must keep them working.
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("ErrMatchRejected does not satisfy errors.Is(err, ErrNoMatch) — every caller " +
			"that files a no-match now treats a rejection as a hard failure")
	}
	if meta.Matched || meta.ExternalID != "" || meta.Name != "" {
		t.Errorf("meta = %+v, want the zero value — a rejected hit contributes nothing", meta)
	}
	// The acceptance test guards the TOP hit only. It deliberately does not scan down
	// the list for something acceptable: that is a ranking judgement the picker's
	// human makes.
	if n := len(queries()); n != 1 {
		t.Fatalf("sent %d queries, want exactly 1 — a rejection must NOT be retried with a "+
			"looser query. ADR-0049 measured the search cluster shedding load globally, and "+
			"a second request issued during those failures pushes the wrong way (%v)",
			n, queries())
	}
}

// Emptiness and rejection are DIFFERENT answers, and issue 06 renders them as
// different next actions ('search-no-match' vs 'search-rejected'). Both still file
// the Track as unmatched, so only the error VALUE separates them.
func TestTrackSearchEmptyResultIsPlainNoMatchNotARejection(t *testing.T) {
	p, queries := mbSearchStub(t) // the source holds nothing at all

	_, err := trackLookup(t, p, "Whisper Your Name", "Harry Connick Jr.")
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	if errors.Is(err, ErrMatchRejected) {
		t.Errorf("an empty result reported ErrMatchRejected — 'MusicBrainz found nothing' and " +
			"'MusicBrainz found something and we refused it' are different diagnoses with " +
			"different remedies")
	}
	if n := len(queries()); n != 1 {
		t.Fatalf("sent %d queries, want exactly 1 — an empty result must NOT trigger a second, "+
			"looser query (ADR-0049) (%v)", n, queries())
	}
}

// A blank track title never reaches the network at all.
func TestTrackSearchWithNoTitleAsksNothing(t *testing.T) {
	p, queries := mbSearchStub(t, "Anything")

	if _, err := trackLookup(t, p, "   ", "Harry Connick Jr."); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	if n := len(queries()); n != 0 {
		t.Fatalf("sent %d queries for a Track with no title, want 0 (%v)", n, queries())
	}
}

// ErrMatchRejected's wrapping is load-bearing on its own: the whole codebase asks
// errors.Is(err, ErrNoMatch), and this value has to answer yes without BEING it.
func TestErrMatchRejectedWrapsErrNoMatchWithoutBeingIt(t *testing.T) {
	if !errors.Is(ErrMatchRejected, ErrNoMatch) {
		t.Error("ErrMatchRejected must wrap ErrNoMatch, or every existing no-match caller breaks")
	}
	if errors.Is(ErrNoMatch, ErrMatchRejected) {
		t.Error("a plain ErrNoMatch must not report itself as a rejection — the two outcomes " +
			"would stop being distinguishable, which is the point of the new value")
	}
}

// --- through a real pass -------------------------------------------------------

// The row a rejection produces is exactly as honest as the one the exact phrase
// produced: status 'unmatched', record columns untouched. A new error value that
// accidentally parked the Track as a FAILURE (retryable, or needing an Admin) would
// be a regression the provider-level tests above cannot see.
func TestARejectedSearchLeavesTheTrackUnmatchedAndUntouched(t *testing.T) {
	prov := &albumTierProvider{
		// Two spare positions, so the tracklist tier cannot rescue the odd track out
		// by the leftover rule and it really does fall through to the search.
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "Bonus One", "rec-x"),
			entry(3, "Bonus Two", "rec-y"),
		},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
		searchErr:  ErrMatchRejected, // the search answered; nothing it offered was this song
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "Nowhere On The Release", num: 2},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("the pass recorded %d failures — a rejected hit is a no-match, not a provider "+
			"failure, and must not be retried or parked (calls: %v)", res.Failed, prov.history())
	}
	got := trackRow(t, db, "t2")
	if got.EnrichmentStatus != "unmatched" {
		t.Errorf("status = %q, want unmatched", got.EnrichmentStatus)
	}
	if got.MusicbrainzID != "" {
		t.Errorf("recorded %q — the rejected candidate was stored anyway, which is the silent "+
			"wrong overview ADR-0050's acceptance test exists to prevent", got.MusicbrainzID)
	}
	if n := prov.count("search:Nowhere On The Release"); n != 1 {
		t.Errorf("the track searched %d times, want 1 (calls: %v)", n, prov.history())
	}
	// The matched sibling is undisturbed by its neighbour's rejection.
	if sib := trackRow(t, db, "t1"); sib.EnrichmentStatus != "matched" || sib.MusicbrainzID != "rec-1" {
		t.Errorf("sibling row = %s/%s, want matched/rec-1", sib.EnrichmentStatus, sib.MusicbrainzID)
	}
}
