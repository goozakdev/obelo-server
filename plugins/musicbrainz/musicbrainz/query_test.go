package musicbrainz

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The QUERY on the wire: the relevance-ranked (no exact-phrase) music query, the
// type-hint disambiguation, artist/release scoping and paging. Ported from
// internal/enrich's search_improvements_test.go, track_release_scope_test.go and
// artist_article_clause_test.go — the assertions read the wire string, because the
// wire string is what MusicBrainz parses.

// TestMusicBrainzAlbumRelevanceQueryNotPhrase is the headline regression: a
// descriptor-carrying album query ("Anastasia Soundtrack") must NOT be sent as an
// exact releasegroup:"…" phrase (which found nothing, because the canonical release-
// group title is just "Anastasia" with secondary-type Soundtrack), and a fixture whose
// title is only "Anastasia" is returned with an "Album · Soundtrack" type hint.
func TestMusicBrainzAlbumRelevanceQueryNotPhrase(t *testing.T) {
	const rgJSON = `{"release-groups": [
	  {"id": "629a5133", "title": "Anastasia", "first-release-date": "1997-01-01",
	   "primary-type": "Album", "secondary-types": ["Soundtrack"],
	   "artist-credit": [{"name": "Lynn Ahrens"}]}
	]}`
	var gotQuery string
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			gotQuery = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(rgJSON))
		case "/release":
			_, _ = w.Write([]byte(`{"releases": []}`))
		default:
			http.NotFound(w, r)
		}
	}, noPacing())

	cands, err := search(p, "album", "Anastasia Soundtrack", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// The query is the escaped terms, NOT an exact releasegroup:"…" phrase.
	if gotQuery != "Anastasia Soundtrack" {
		t.Errorf("query = %q, want the bare relevance terms %q", gotQuery, "Anastasia Soundtrack")
	}
	if strings.Contains(gotQuery, `releasegroup:"`) {
		t.Errorf("query %q is still an exact-phrase releasegroup:\"…\" — the phrase fix regressed", gotQuery)
	}
	if len(cands) != 1 || cands[0].Title != "Anastasia" || cands[0].ExternalID != "629a5133" {
		t.Fatalf("candidate = %+v, want the Anastasia soundtrack release-group", cands)
	}
	if cands[0].TypeLabel != "Album · Soundtrack" {
		t.Errorf("type label = %q, want %q", cands[0].TypeLabel, "Album · Soundtrack")
	}
}

// TestMusicBrainzAlbumArtistScopingAndPaging: an artist term AND-narrows the query as
// a field-scoped clause, and limit/offset are threaded to the request for "show more".
func TestMusicBrainzAlbumArtistScopingAndPaging(t *testing.T) {
	var got struct{ query, limit, offset string }
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/release-group" {
			got.query = r.URL.Query().Get("query")
			got.limit = r.URL.Query().Get("limit")
			got.offset = r.URL.Query().Get("offset")
			_, _ = w.Write([]byte(`{"release-groups": []}`))
			return
		}
		http.NotFound(w, r)
	}, noPacing())

	_, err := search(p, "album", "Greatest Hits", pluginapi.Page{Limit: 12, Offset: 12}, "Queen", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.query != `Greatest Hits AND artist:"Queen"` {
		t.Errorf("query = %q, want the artist-AND clause", got.query)
	}
	if got.limit != "12" || got.offset != "12" {
		t.Errorf("paging = limit %q offset %q, want 12/12", got.limit, got.offset)
	}
}

// TestMusicBrainzSearchJoinsFullArtistCredit: a multi-artist album (a collaboration)
// surfaces its WHOLE artist-credit — "Ben Folds & Nick Hornby", preserving the join
// phrase — in the candidate, not just the first credited artist.
func TestMusicBrainzSearchJoinsFullArtistCredit(t *testing.T) {
	const rgJSON = `{"release-groups": [
	  {"id": "rg-lonely", "title": "Lonely Avenue", "primary-type": "Album",
	   "artist-credit": [
	     {"name": "Ben Folds", "joinphrase": " & "},
	     {"name": "Nick Hornby"}
	   ]}
	]}`
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			_, _ = w.Write([]byte(rgJSON))
		case "/release":
			_, _ = w.Write([]byte(`{"releases": []}`))
		default:
			http.NotFound(w, r)
		}
	}, noPacing())

	cands, err := search(p, "album", "Lonely Avenue", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if !strings.Contains(cands[0].Disambiguation, "Ben Folds & Nick Hornby") {
		t.Errorf("candidate disambiguation = %q, want it to contain the full credit %q",
			cands[0].Disambiguation, "Ben Folds & Nick Hornby")
	}
}

// --- The recording search's narrowing axes (needs-fixing/06) ------------------

// mbRecordingQueryStub captures the `query` the recording search actually sends and
// answers with one hit.
func mbRecordingQueryStub(t *testing.T, got *string) *Provider {
	t.Helper()
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/recording" {
			*got = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(`{"recordings": [
			  {"id": "7dd6030f", "title": "(I Could Only) Whisper Your Name",
			   "artist-credit": [{"name": "Harry Connick, Jr."}],
			   "releases": [{"title": "She", "release-group": {"title": "She"}}]}
			]}`))
			return
		}
		http.NotFound(w, r)
	}, noPacing())
	return p
}

// TestRecordingSearchNarrowsByArtistAndRelease: both scope terms reach the query as
// field-scoped AND clauses. This is what turns the nine recordings called some
// variant of "Whisper Your Name" into the one on the album the file sits in.
func TestRecordingSearchNarrowsByArtistAndRelease(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	cands, err := search(p, "track", "(I Could Only) Whisper Your Name", pluginapi.Page{},
		"Harry Connick, Jr.", "She")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := `\(I Could Only\) Whisper Your Name AND artist:"Harry Connick, Jr." AND release:"She"`
	if got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
	if len(cands) != 1 || cands[0].ExternalID != "7dd6030f" {
		t.Fatalf("candidates = %+v, want the one narrowed recording", cands)
	}
}

// TestRecordingSearchOmitsBlankScope: a blank artist/release adds no clause, so the
// query a row with no tags (or one the Admin deliberately widened) sends is exactly
// the bare relevance terms it has always sent.
func TestRecordingSearchOmitsBlankScope(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	if _, err := search(p, "track", "Whisper Your Name", pluginapi.Page{}, "  ", "  "); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != "Whisper Your Name" {
		t.Errorf("query = %q, want the bare terms with no clauses", got)
	}
	if strings.Contains(got, "release:") || strings.Contains(got, "artist:") {
		t.Errorf("query %q carries an empty narrowing clause", got)
	}
}

// TestRecordingReleaseScopeIsEscaped: the release term is Lucene-escaped like every
// other term, so an album whose title carries a metacharacter (`AC/DC`, `!!!`,
// `"Heroes"`) narrows the search instead of 4xx-ing the parser.
func TestRecordingReleaseScopeIsEscaped(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	if _, err := search(p, "track", "Heroes", pluginapi.Page{}, "", `"Heroes" / AC-DC`); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(got, `AND release:"\"Heroes\" \/ AC\-DC"`) {
		t.Errorf("query = %q, want an escaped release clause", got)
	}
}

// TestAlbumSearchIgnoresReleaseScope: a release-group search IS the album search, so
// there is no release axis left to narrow on and the option is dropped rather than
// AND-ed into a field the release-group index does not have.
func TestAlbumSearchIgnoresReleaseScope(t *testing.T) {
	var got string
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/release-group" {
			got = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(`{"release-groups": []}`))
			return
		}
		http.NotFound(w, r)
	}, noPacing())

	if _, err := search(p, "album", "She", pluginapi.Page{}, "Harry Connick, Jr.", "She"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != `She AND artist:"Harry Connick, Jr."` {
		t.Errorf("query = %q, want the artist clause only", got)
	}
}

// --- The article-insensitive artist clause (ADR-0037's amendment) -------------

// TestArtistClauseWithoutAnArticleIsTodaysClause: a name with no leading article
// emits the plain single-phrase clause, byte for byte — the alternatives collapse
// to one, so nothing about the common case moves.
func TestArtistClauseWithoutAnArticleIsTodaysClause(t *testing.T) {
	for _, name := range []string{"Eagles", "Radiohead", "Anthrax", "The"} {
		if got, want := artistClause(name), `"`+escapeLucene(name)+`"`; got != want {
			t.Errorf("artistClause(%q) = %q, want the plain clause %q", name, got, want)
		}
	}
}

// TestAlbumSearchNarrowsArticleInsensitively: "The Eagles" reaches the wire as the
// two alternatives, so an album MusicBrainz credits to "Eagles" is found.
func TestAlbumSearchNarrowsArticleInsensitively(t *testing.T) {
	var got string
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/release-group" {
			got = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(`{"release-groups": []}`))
			return
		}
		http.NotFound(w, r)
	}, noPacing())

	if _, err := search(p, "album", "Hell Freezes Over", pluginapi.Page{}, "The Eagles", ""); err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := `Hell Freezes Over AND artist:("The Eagles" OR "Eagles")`
	if got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

// TestArtistClauseAlternativesAreEscapedAndQuoted: both alternatives go through the
// Lucene escaper, so an article-led name carrying a metacharacter still parses.
func TestArtistClauseAlternativesAreEscapedAndQuoted(t *testing.T) {
	got := artistClause(`The AC/DC`)
	want := `("The AC\/DC" OR "AC\/DC")`
	if got != want {
		t.Errorf("artistClause = %q, want %q", got, want)
	}
}

// TestBlankArtistStillEmitsNoClause: a blank artist adds nothing, article rule or
// not.
func TestBlankArtistStillEmitsNoClause(t *testing.T) {
	if got := musicQuery("Song", "   ", ""); got != "Song" {
		t.Errorf("musicQuery = %q, want the bare terms", got)
	}
}

// TestReleaseClauseIsNotArticleInsensitive: an album's leading article is usually
// part of its title, so the release clause gets no alternatives.
func TestReleaseClauseIsNotArticleInsensitive(t *testing.T) {
	got := musicQuery("Song", "", "The Wall")
	want := `Song AND release:"The Wall"`
	if got != want {
		t.Errorf("musicQuery = %q, want %q", got, want)
	}
}

// TestTrackPickerNarrowsArticleInsensitively: the recording search gets the same
// clause, because it is the same musicQuery.
func TestTrackPickerNarrowsArticleInsensitively(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)
	if _, err := search(p, "track", "Hotel California", pluginapi.Page{}, "The Eagles", ""); err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := `Hotel California AND artist:("The Eagles" OR "Eagles")`
	if got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

// TestWithoutLeadingArticle is the rule's own table: one article, longest first,
// case-insensitive, and a word that merely BEGINS with those letters is untouched.
func TestWithoutLeadingArticle(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"The Eagles", "Eagles"},
		{"the eagles", "eagles"},
		{"An Emotional Fish", "Emotional Fish"},
		{"A Tribe Called Quest", "Tribe Called Quest"},
		{"Anthrax", "Anthrax"},
		{"The", "The"},
		{"Theatre of Tragedy", "Theatre of Tragedy"},
	} {
		if got := withoutLeadingArticle(c.in); got != c.want {
			t.Errorf("withoutLeadingArticle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- The pasted reference vocabulary ------------------------------------------

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantID   string
		wantOK   bool
	}{
		{"629a5133-a2b4-41ec-9db4-2b266d7a0e7a", "", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"  629A5133-A2B4-41EC-9DB4-2B266D7A0E7A  ", "", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"https://musicbrainz.org/release-group/629a5133-a2b4-41ec-9db4-2b266d7a0e7a", "album", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"http://beta.musicbrainz.org/artist/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/releases", "artist", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true},
		{"https://musicbrainz.org/recording/11111111-2222-3333-4444-555555555555?tport=80", "track", "11111111-2222-3333-4444-555555555555", true},
		{"not a uuid or url", "", "", false},
		{"https://example.com/foo/bar", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		k, id, ok := ParseRef(c.in)
		if k != c.wantKind || id != c.wantID || ok != c.wantOK {
			t.Errorf("ParseRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, id, ok, c.wantKind, c.wantID, c.wantOK)
		}
	}
}

// TestParseExternalRef is the contract call: the four answers an Admin's paste can
// get, each of which the host renders as a different sentence.
func TestParseExternalRef(t *testing.T) {
	const mbID = "629a5133-a2b4-41ec-9db4-2b266d7a0e7a"
	p, host := newProvider(t, jsonHandler(`{}`), noPacing())

	parse := func(kind, pasted string) pluginapi.ExternalRefResponse {
		t.Helper()
		resp, err := p.ParseExternalRef(t.Context(), pluginapi.ExternalRefRequest{Kind: kind, Pasted: pasted})
		if err != nil {
			t.Fatalf("ParseExternalRef(%q, %q): %v", kind, pasted, err)
		}
		return resp
	}

	// A release-group URL applied to an album — accepted, and the id is the pin.
	if got := parse("album", "https://musicbrainz.org/release-group/"+mbID); got.Outcome != pluginapi.OutcomeMatched || got.ExternalID != mbID {
		t.Errorf("album + release-group url = %+v, want matched with the id", got)
	}
	// A bare UUID is trusted for the item's own kind.
	if got := parse("track", mbID); got.Outcome != pluginapi.OutcomeMatched || got.ExternalID != mbID {
		t.Errorf("track + bare uuid = %+v", got)
	}
	// An artist URL applied to a track — a kind mismatch that carries BOTH kinds, so
	// the host can say "that looks like an artist link, but this item is a track".
	got := parse("track", "https://musicbrainz.org/artist/"+mbID)
	if got.Outcome != pluginapi.OutcomeRefKindMismatch || got.GotKind != "artist" || got.WantKind != "track" {
		t.Errorf("track + artist url = %+v, want a kind mismatch artist/track", got)
	}
	// A release-group (album) URL applied to an artist — the reported real-world case.
	got = parse("artist", "https://musicbrainz.org/release-group/"+mbID)
	if got.Outcome != pluginapi.OutcomeRefKindMismatch || got.GotKind != "album" || got.WantKind != "artist" {
		t.Errorf("artist + release-group url = %+v, want a kind mismatch album/artist", got)
	}
	// A /release/ URL on an ALBUM is not an album pin: it names one EDITION, so it
	// rides back as ReleaseID with no ExternalID (ADR-0038, ADR-0052).
	got = parse("album", "https://musicbrainz.org/release/"+mbID)
	if got.Outcome != pluginapi.OutcomeMatched || got.ReleaseID != mbID || got.ExternalID != "" {
		t.Errorf("album + release url = %+v, want matched carrying only the release id", got)
	}
	// The same URL on a TRACK is a recognized-but-unsupported entity, not garbage.
	if got := parse("track", "https://musicbrainz.org/release/"+mbID); got.Outcome != pluginapi.OutcomeRefUnsupportedKind {
		t.Errorf("track + release url = %+v, want ref-unsupported-kind", got)
	}
	// A /work/ URL — the common "I grabbed the wrong id" case.
	if got := parse("album", "https://musicbrainz.org/work/"+mbID); got.Outcome != pluginapi.OutcomeRefUnsupportedKind {
		t.Errorf("album + work url = %+v, want ref-unsupported-kind", got)
	}
	// Garbage.
	if got := parse("album", "gibberish"); got.Outcome != pluginapi.OutcomeRefInvalid {
		t.Errorf("album + gibberish = %+v, want ref-invalid", got)
	}
	// A non-Music kind is not this source's question at all, which lets the host
	// answer for the namespaces it keeps its own columns for.
	if got := parse("movie", mbID); got.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("movie paste = %+v, want unavailable", got)
	}
	// And none of it cost a request.
	if n := len(requestPaths(host)); n != 0 {
		t.Errorf("reading a paste cost %d requests; it is pure parsing", n)
	}
}

// TestIsUUID pins the MBID shape the corroboration path validates against before
// it spends its one call (ADR-0049).
func TestIsUUID(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"629A5133-A2B4-41EC-9DB4-2B266D7A0E7A", true},
		{"629a5133a2b441ec9db42b266d7a0e7a", false},
		{"629a5133-a2b4-41ec-9db4-2b266d7a0e7", false},
		{"629a5133-a2b4-41ec-9db4-2b266d7a0e7z", false},
		{"", false},
	} {
		if got := IsUUID(c.in); got != c.want {
			t.Errorf("IsUUID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The internal sentinels never cross the contract. This is the guard on that: the
// two are unexported, and nothing here returns them to a caller.
func TestInternalSentinelsAreNotOutcomes(t *testing.T) {
	if errors.Is(errNoMatch, errNoTracklist) || errors.Is(errNoTracklist, errNoMatch) {
		t.Error("the two internal sentinels must stay distinct: one is about a RECORD, the other about an ALBUM")
	}
}
