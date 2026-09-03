package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The album-narrowing half of needs-fixing/06. A Needs-Fixing row for a Track
// searches /recording, whose subject is the RECORDING — so "(I Could Only) Whisper
// Your Name" is the query and the album is a narrowing clause, not the query.
//
// The field name was verified against the live index before this was built on:
//
//	…/ws/2/recording?query=Whisper Your Name AND artist:"Harry Connick"
//	  → 9 recordings, top hits on The Mask, Hollywood Soundtracks, Music Works!
//	…the same query AND release:"She"
//	  → count 1, the recording on the *She* release, score 100.
//
// `release` is supported, so the album box narrows by release rather than by
// release-group.

// mbRecordingQueryStub captures the `query` the recording search actually sends and
// answers with one hit, so a test asserts the wire string rather than a helper's
// return value — the wire string is the thing MusicBrainz parses.
func mbRecordingQueryStub(t *testing.T, got *string) *MusicBrainzProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/recording":
			*got = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(`{"recordings": [
			  {"id": "7dd6030f", "title": "(I Could Only) Whisper Your Name",
			   "artist-credit": [{"name": "Harry Connick, Jr."}],
			   "releases": [{"title": "She", "release-group": {"title": "She"}}]}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0
	return p
}

// TestRecordingSearchNarrowsByArtistAndRelease: both scope terms reach the query as
// field-scoped AND clauses. This is what turns the nine recordings called some
// variant of "Whisper Your Name" into the one on the album the file sits in.
func TestRecordingSearchNarrowsByArtistAndRelease(t *testing.T) {
	var got string
	p := mbRecordingQueryStub(t, &got)

	cands, err := p.Search(context.Background(), "track", "(I Could Only) Whisper Your Name",
		SearchOptions{Artist: "Harry Connick, Jr.", Release: "She"})
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

	if _, err := p.Search(context.Background(), "track", "Whisper Your Name",
		SearchOptions{Artist: "  ", Release: "  "}); err != nil {
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

	if _, err := p.Search(context.Background(), "track", "Heroes",
		SearchOptions{Release: `"Heroes" / AC-DC`}); err != nil {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			got = r.URL.Query().Get("query")
			_, _ = w.Write([]byte(`{"release-groups": []}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	if _, err := p.Search(context.Background(), "album", "She",
		SearchOptions{Artist: "Harry Connick, Jr.", Release: "She"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != `She AND artist:"Harry Connick, Jr."` {
		t.Errorf("query = %q, want the artist clause only", got)
	}
}
