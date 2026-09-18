package musicbrainz

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ADR-0050's last tier — the pass's track search — ported from
// internal/enrich/track_search_test.go and artist_article_clause_test.go.
//
// The pass's last-resort track search used to send an unescaped exact phrase,
// `recording:"<title>"`, and hand back Recordings[0] whatever it was. Two halves
// fixed it, and they are only correct TOGETHER:
//
//   - the query becomes the interactive picker's (musicQuery — escaped,
//     relevance-ranked, artist-narrowed), so a title MusicBrainz punctuates
//     differently can be found at all; and
//   - the top hit is handed over UNJUDGED, marked FromSearch, because a relevance
//     query essentially always returns something and taking it blind would trade an
//     honest empty answer for a confident wrong one.
//
// THE SECOND HALF IS THE HOST'S NOW (ADR-0057) and its tests stayed there. What
// this file asserts is the first half, plus the one obligation the provider still
// carries for the second: FromSearch set, and Name carrying the CANDIDATE's own
// title, because that is the string the host judges by.
//
// And one constraint that is invisible in the rows and easy to lose to a
// well-meaning fallback: ONE request, always (ADR-0049 — the search cluster is the
// dependency that sheds load globally, so a retry issued during its failures pushes
// the wrong way). Every outcome below asserts the request count.

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
func mbSearchStub(t *testing.T, hits ...string) (*Provider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
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
	}, noPacing())
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
// search TEXT must be backslash-escaped. The `AND artist:…` clause is query
// structure rather than text, so only the contents of its phrases are checked.
func luceneParses(query string) bool {
	const clause = ` AND artist:`
	terms := query
	if i := strings.Index(query, clause); i >= 0 {
		terms = query[:i]
		alts, ok := artistClausePhrases(query[i+len(clause):])
		if !ok {
			return false // malformed clause: the real parser would 400 too
		}
		for _, alt := range alts {
			if !luceneEscaped(alt) {
				return false
			}
		}
	}
	return luceneEscaped(terms)
}

// artistClausePhrases parses the artist clause the way the query parser sees it and
// returns the phrase CONTENTS. Two shapes are legal, and only these two: the single
// phrase `"X"` this always sent, and — since ADR-0037's amendment reached the
// provider query — the disjunction `("X" OR "Y")` that makes the narrowing
// article-insensitive. Quotes, parentheses and OR are structure, not text, so they
// are not subject to escaping; everything between a pair of quotes is.
//
// (The pass's recording search never carries a release clause, so the artist clause
// runs to the end of the query here.)
func artistClausePhrases(clause string) ([]string, bool) {
	inner, group := clause, false
	if open, ok := strings.CutPrefix(clause, "("); ok {
		closed, ok := strings.CutSuffix(open, ")")
		if !ok {
			return nil, false
		}
		inner, group = closed, true
	}
	var alts []string
	for {
		if !strings.HasPrefix(inner, `"`) {
			return nil, false
		}
		var phrase strings.Builder
		i := 1
		for i < len(inner) && inner[i] != '"' {
			if inner[i] == '\\' && i+1 < len(inner) {
				phrase.WriteByte(inner[i]) // keep the escape for luceneEscaped
				i++
			}
			phrase.WriteByte(inner[i])
			i++
		}
		if i >= len(inner) {
			return nil, false // unterminated phrase
		}
		alts = append(alts, phrase.String())
		inner = inner[i+1:]
		if inner == "" {
			break
		}
		rest, ok := strings.CutPrefix(inner, " OR ")
		if !group || !ok {
			return nil, false // trailing junk, or an OR outside a group
		}
		inner = rest
	}
	if group && len(alts) < 2 {
		return nil, false // a group is only ever emitted for real alternatives
	}
	return alts, true
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

func trackLookup(p *Provider, track, artist string) (pluginapi.MetadataRecord, error) {
	return lookup(p, pluginapi.MediaRef{Kind: "track", Track: track, Artist: artist})
}

// --- the query -----------------------------------------------------------------

// The pass sends the PICKER's query, not the exact phrase it used to. The picker
// was deliberately moved off `recording:"…"` (item-editing/search-improvements);
// leaving the automatic matcher on it made it strictly worse than the manual one it
// hands its failures to.
func TestTrackSearchSendsTheRelevanceQueryNotAnExactPhrase(t *testing.T) {
	p, queries := mbSearchStub(t, "Whisper Your Name")

	if _, err := trackLookup(p, "Whisper Your Name", "Harry Connick Jr."); err != nil {
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

	if _, err := trackLookup(p, "Whisper Your Name", ""); err != nil {
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

			meta, err := trackLookup(p, tc.tagged, "Harry Connick Jr.")
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

			meta, err := trackLookup(p, tc.track, tc.artist)
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

// The enrichment pass's own last-resort track search builds its query with the SAME
// musicQuery as the picker, so an article artist's tracks stop falling to
// search-no-match. One clause, one code path — the picker and the pass cannot
// diverge.
func TestPassTrackSearchNarrowsArticleInsensitively(t *testing.T) {
	p, queries := mbSearchStub(t, "Get Over It")

	meta, err := trackLookup(p, "Get Over It", "The Eagles")
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
		t.Fatalf("meta = %+v, want the top hit", meta)
	}
}

// BOTH alternatives are escaped, so an article artist whose name carries a Lucene
// metacharacter still sends a query the parser accepts. The stub answers 400 to an
// unescaped one, exactly as the live server does — which is how the old unescaped
// phrase surfaced as a fake provider failure rather than as a search.
func TestPassTrackSearchWidenedClauseParsesWithMetacharacters(t *testing.T) {
	p, queries := mbSearchStub(t, "Get Over It")

	meta, err := trackLookup(p, "Get Over It", "The AC/DC Tribute")
	if err != nil {
		t.Fatalf("lookup: %v (the widened clause did not survive the query parser: %v)",
			err, queries())
	}
	if !meta.Matched {
		t.Fatalf("meta = %+v, want the top hit", meta)
	}
}

// --- what the provider owes the HOST's acceptance test -------------------------

// The top hit is handed back UNJUDGED, marked FromSearch, carrying its OWN title.
// This is the whole of the provider's obligation under ADR-0057: the host cannot
// apply ADR-0050's acceptance test to a candidate whose title it was not given, and
// a source that quietly kept judging would make the host's rejection unreachable.
func TestTheTopHitIsHandedBackUnjudged(t *testing.T) {
	p, queries := mbSearchStub(t, "Whispering Your Name", "Whisper Your Name")

	hit, err := trackLookup(p, "Whisper Your Name", "Harry Connick Jr.")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !hit.Matched || !hit.FromSearch || hit.Name != "Whispering Your Name" {
		t.Fatalf("meta = %+v — want the TOP hit handed back marked FromSearch and carrying "+
			"its own title, which is what the host judges it by", hit)
	}
	if hit.ExternalID != "rec-1" {
		t.Errorf("external id = %q, want the top hit's", hit.ExternalID)
	}
	// Only the top hit, and only one query: scanning down a ranked list for something
	// acceptable is a judgement, and it is the picker's human who makes it.
	if n := len(queries()); n != 1 {
		t.Fatalf("sent %d queries, want exactly 1 (%v)", n, queries())
	}
}

// Emptiness is a plain no-match, and it costs exactly one query: an empty result
// must NOT trigger a second, looser one (ADR-0049).
func TestTrackSearchEmptyResultIsPlainNoMatch(t *testing.T) {
	p, queries := mbSearchStub(t) // the source holds nothing at all

	_, err := trackLookup(p, "Whisper Your Name", "Harry Connick Jr.")
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want no-match", err)
	}
	if n := len(queries()); n != 1 {
		t.Fatalf("sent %d queries, want exactly 1 (%v)", n, queries())
	}
}

// A blank track title never reaches the network at all.
func TestTrackSearchWithNoTitleAsksNothing(t *testing.T) {
	p, queries := mbSearchStub(t, "Anything")

	if _, err := trackLookup(p, "   ", "Harry Connick Jr."); !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want no-match", err)
	}
	if n := len(queries()); n != 0 {
		t.Fatalf("sent %d queries for a Track with no title, want 0 (%v)", n, queries())
	}
}

// A blank album name is refused the same way, without a request.
func TestAlbumLookupWithNoTitleAsksNothing(t *testing.T) {
	p, host := newProvider(t, jsonHandler(`{}`), noPacing())
	if _, err := lookup(p, pluginapi.MediaRef{Kind: "album", Album: "   "}); !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want no-match", err)
	}
	if n := len(requestPaths(host)); n != 0 {
		t.Errorf("a blank album name cost %d requests", n)
	}
}
