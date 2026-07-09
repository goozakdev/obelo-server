package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Unit tests for item-editing/search-improvements: the relevance-ranked (no exact-
// phrase) music query, the type-hint disambiguation, artist scoping + paging on the
// wire, and the paste-a-MusicBrainz/TMDB-id/URL parsers. All zero-network.

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "album", "Anastasia Soundtrack", SearchOptions{})
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			got.query = r.URL.Query().Get("query")
			got.limit = r.URL.Query().Get("limit")
			got.offset = r.URL.Query().Get("offset")
			_, _ = w.Write([]byte(`{"release-groups": []}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	_, err := p.Search(context.Background(), "album", "Greatest Hits",
		SearchOptions{Artist: "Queen", Limit: 12, Offset: 12})
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

func TestParseMusicBrainzRef(t *testing.T) {
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
		k, id, ok := ParseMusicBrainzRef(c.in)
		if k != c.wantKind || id != c.wantID || ok != c.wantOK {
			t.Errorf("ParseMusicBrainzRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, id, ok, c.wantKind, c.wantID, c.wantOK)
		}
	}
}

func TestParseTMDBRef(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantID   string
		wantOK   bool
	}{
		{"438631", "", "438631", true},
		{"https://www.themoviedb.org/movie/438631-dune", "movie", "438631", true},
		{"https://www.themoviedb.org/tv/1399", "tv", "1399", true},
		{"themoviedb.org/tv/1399-game-of-thrones/seasons", "tv", "1399", true},
		{"not-a-ref", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		k, id, ok := parseTMDBRef(c.in)
		if k != c.wantKind || id != c.wantID || ok != c.wantOK {
			t.Errorf("parseTMDBRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, id, ok, c.wantKind, c.wantID, c.wantOK)
		}
	}
}

// TestExternalIDForKind: a bare id is trusted for the item's kind; a typed URL of the
// wrong kind is rejected; an unreadable paste is invalid.
func TestExternalIDForKind(t *testing.T) {
	mbID := "629a5133-a2b4-41ec-9db4-2b266d7a0e7a"
	// A release-group URL applied to an album — accepted.
	if id, err := externalIDForKind("album", "https://musicbrainz.org/release-group/"+mbID); err != nil || id != mbID {
		t.Errorf("album + release-group url = (%q,%v), want (%q,nil)", id, err, mbID)
	}
	// An artist URL applied to a track — kind mismatch that carries what was pasted
	// (artist) vs what's needed (track) so the handler can guide the Admin.
	if _, err := externalIDForKind("track", "https://musicbrainz.org/artist/"+mbID); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("track + artist url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "artist" || m.Want != "track" {
			t.Errorf("track + artist url mismatch = %+v, want {Got:artist Want:track}", m)
		}
	}
	// A release-group (album) URL applied to an artist — the reported real-world case.
	if _, err := externalIDForKind("artist", "https://musicbrainz.org/release-group/"+mbID); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("artist + release-group url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "album" || m.Want != "artist" {
			t.Errorf("artist + release-group url mismatch = %+v, want {Got:album Want:artist}", m)
		}
	}
	// A bare UUID is trusted for the item's kind.
	if id, err := externalIDForKind("track", mbID); err != nil || id != mbID {
		t.Errorf("track + bare uuid = (%q,%v), want (%q,nil)", id, err, mbID)
	}
	// A tv URL applied to a movie — kind mismatch (pasted show, item is a movie).
	if _, err := externalIDForKind("movie", "https://www.themoviedb.org/tv/1399"); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("movie + tv url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "show" || m.Want != "movie" {
			t.Errorf("movie + tv url mismatch = %+v, want {Got:show Want:movie}", m)
		}
	}
	// Garbage — invalid.
	if _, err := externalIDForKind("album", "gibberish"); err != ErrExternalRefInvalid {
		t.Errorf("album + gibberish err = %v, want ErrExternalRefInvalid", err)
	}
	// A valid MusicBrainz link of an entity we can't pin (a /work/ URL — the common
	// "I grabbed the wrong id" case) is distinguished from unreadable garbage so the
	// Admin is told what to paste instead.
	if _, err := externalIDForKind("album", "https://musicbrainz.org/work/"+mbID); err != ErrExternalRefUnsupportedKind {
		t.Errorf("album + work url err = %v, want ErrExternalRefUnsupportedKind", err)
	}
	// A /release/ URL (one edition of a release-group) is likewise recognized-but-
	// unsupported, not gibberish.
	if _, err := externalIDForKind("album", "https://musicbrainz.org/release/"+mbID); err != ErrExternalRefUnsupportedKind {
		t.Errorf("album + release url err = %v, want ErrExternalRefUnsupportedKind", err)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			_, _ = w.Write([]byte(rgJSON))
		case "/release":
			_, _ = w.Write([]byte(`{"releases": []}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "album", "Lonely Avenue", SearchOptions{})
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

// TestMusicBrainzLookupNotFoundIsNoMatch: a by-id lookup that 404s (a pasted id that
// names a different/stale entity — e.g. a work MBID looked up as a release-group) is
// ErrNoMatch ("no record found"), NOT a generic error the handler would report as
// "source may be unreachable". Guards the paste-id error-mapping fix.
func TestMusicBrainzLookupNotFoundIsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // every MusicBrainz endpoint 404s
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	if _, err := p.releaseGroupByID(context.Background(), "1e64933c-a2b4-41ec-9db4-2b266d7a0e7a"); err != ErrNoMatch {
		t.Errorf("releaseGroupByID on a 404 = %v, want ErrNoMatch", err)
	}
	if _, err := p.artistByID(context.Background(), "1e64933c-a2b4-41ec-9db4-2b266d7a0e7a"); err != ErrNoMatch {
		t.Errorf("artistByID on a 404 = %v, want ErrNoMatch", err)
	}
}

// TestMusicBrainzLookupResolvesReleaseToReleaseGroup: an album Lookup pinned to a
// MusicBrainz *release* (a pasted /release/ URL) resolves to that release's parent
// release-group and returns the release-group record — so the album is pinned, not
// the individual edition.
func TestMusicBrainzLookupResolvesReleaseToReleaseGroup(t *testing.T) {
	const releaseID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const rgID = "629a5133-b9e6-43c5-8cb6-594a7cbfbfed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/release/"+releaseID):
			_, _ = w.Write([]byte(`{"release-group": {"id": "` + rgID + `"}}`))
		case strings.HasPrefix(r.URL.Path, "/release-group/"+rgID):
			_, _ = w.Write([]byte(`{"id": "` + rgID + `", "title": "Anastasia", "first-release-date": "1997-11-18"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "album", ReleaseMBID: releaseID})
	if err != nil {
		t.Fatalf("Lookup(release ref): %v", err)
	}
	if !meta.Matched || meta.ExternalID != rgID || meta.Name != "Anastasia" {
		t.Errorf("release→release-group meta = %+v, want the %q release-group", meta, rgID)
	}

	// A release with no parent group (or an unknown release) is ErrNoMatch, not a hang.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "album", ReleaseMBID: "no-such-release"}); err != ErrNoMatch {
		t.Errorf("Lookup(unknown release) = %v, want ErrNoMatch", err)
	}
}
