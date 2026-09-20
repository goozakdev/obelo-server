package musicbrainz

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The provider's Search request/parse layer (the Edit-item Enrichment-override
// picker, ADR-0019), ported from internal/enrich/search_test.go with its canned
// JSON and its assertions intact.

const mbRecordingSearchJSON = `{"recordings": [
  {"id": "rec-1", "title": "Come as You Are", "disambiguation": "album version",
   "first-release-date": "1991-09-24", "artist-credit": [{"name": "Nirvana"}]},
  {"id": "rec-2", "title": "Come as You Are", "artist-credit": [{"name": "Nirvana (60s band)"}]}
]}`

// pathHandler answers one path with one body and 404s everything else — the shape
// of most of the ported search stubs.
func pathHandler(path, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}
}

// TestMusicBrainzSearchTrackParsesCandidates: a track query hits /recording and
// maps recordings into candidates carrying the MBID, title, year, and an
// artist-credit + disambiguation hint (the "wrong Nirvana" tell).
func TestMusicBrainzSearchTrackParsesCandidates(t *testing.T) {
	p, host := newProvider(t, pathHandler("/recording", mbRecordingSearchJSON), noPacing())

	cands, err := search(p, "track", "Come as You Are", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if seen := requestPaths(host); len(seen) != 1 || seen[0] != "/recording" {
		t.Fatalf("expected one /recording call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].ExternalID != "rec-1" || cands[0].Title != "Come as You Are" ||
		cands[0].Year != 1991 || cands[0].Kind != "track" {
		t.Errorf("candidate[0] = %+v", cands[0])
	}
	if !strings.Contains(cands[0].Disambiguation, "Nirvana") ||
		!strings.Contains(cands[0].Disambiguation, "album version") {
		t.Errorf("disambiguation = %q", cands[0].Disambiguation)
	}
	if !strings.Contains(cands[1].Disambiguation, "60s band") {
		t.Errorf("candidate[1] disambiguation = %q", cands[1].Disambiguation)
	}
}

// TestMusicBrainzTrackLookupByPinnedMBID: a pinned recording MBID resolves BY id
// (/recording/{mbid}) — the durable Enrichment override path — instead of a
// name search.
func TestMusicBrainzTrackLookupByPinnedMBID(t *testing.T) {
	p, host := newProvider(t, pathHandler("/recording/rec-42",
		`{"id": "rec-42", "title": "Corrected Title"}`), noPacing())

	meta, err := lookup(p, pluginapi.MediaRef{Kind: "track", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "rec-42"}, Track: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if seen := requestPaths(host); len(seen) != 1 || seen[0] != "/recording/rec-42" {
		t.Fatalf("expected a by-id /recording/rec-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.Name != "Corrected Title" || meta.ExternalID != "rec-42" {
		t.Errorf("meta = %+v", meta)
	}
	if meta.FromSearch {
		t.Error("a record resolved BY ID must not be marked FromSearch — an id IS the identification")
	}
}

// TestMusicBrainzSearchEscapesLuceneQuery: a title with Lucene metacharacters
// (e.g. AC/DC) is still escaped, so it can't produce a malformed query the server
// rejects (which would surface as a false SEARCH_UNAVAILABLE) — even though the terms
// are no longer wrapped in a recording:"…" exact phrase (item-editing/search-
// improvements). The stub captures the outgoing query param.
func TestMusicBrainzSearchEscapesLuceneQuery(t *testing.T) {
	var gotQuery string
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recording" {
			gotQuery = r.URL.Query().Get("query")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"recordings": [{"id": "rec-1", "title": "Back in Black"}]}`))
			return
		}
		http.NotFound(w, r)
	}, noPacing())

	cands, err := search(p, "track", `AC/DC "Heroes"`, pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	// Every metacharacter is backslash-escaped, but NOT phrase-wrapped: the query is
	// relevance-ranked terms, not recording:"…".
	want := `AC\/DC \"Heroes\"`
	if gotQuery != want {
		t.Errorf("escaped query = %q, want %q", gotQuery, want)
	}
	if strings.Contains(gotQuery, `recording:"`) {
		t.Errorf("query %q is still an exact-phrase recording:\"…\" — the phrase fix regressed", gotQuery)
	}
}

// TestMusicBrainzSearchYearFromReleases: a recording hit carries no top-level
// first-release-date, so the disambiguating year is derived from its releases /
// release-groups — the earliest (original) year across them.
func TestMusicBrainzSearchYearFromReleases(t *testing.T) {
	const body = `{"recordings": [{
	  "id": "rec-1", "title": "Star Wars (Main Title)",
	  "releases": [
	    {"date": "1997-03-01", "release-group": {"first-release-date": "1997-03-01"}},
	    {"date": "1977-05-25", "release-group": {"first-release-date": "1977-05-01"}}
	  ]
	}]}`
	p, _ := newProvider(t, pathHandler("/recording", body), noPacing())

	cands, err := search(p, "track", "Star Wars", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 || cands[0].Year != 1977 {
		t.Fatalf("candidate year = %+v, want earliest 1977", cands)
	}
}

// TestMusicBrainzSearchUnsupportedKindUnavailable: a kind MusicBrainz does not
// own (e.g. a video kind, or a season) is OutcomeUnavailable — only
// track/artist/album are searchable music kinds (item-editing/02) — and it is NOT
// an empty candidate list, which the Edit-item box would render as "no results".
func TestMusicBrainzSearchUnsupportedKindUnavailable(t *testing.T) {
	p, host := newProvider(t, jsonHandler(`{}`), noPacing())
	for _, kind := range []string{"movie", "show", "season", "episode"} {
		resp, err := p.Search(t.Context(), pluginapi.SearchRequest{Kind: kind, Query: "Dune"})
		if err != nil {
			t.Fatalf("%s search: %v", kind, err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("%s search outcome = %q, want unavailable", kind, resp.Outcome)
		}
	}
	if n := len(requestPaths(host)); n != 0 {
		t.Errorf("an unsupported kind cost %d requests; it must cost none", n)
	}
}

// A blank query is answered without a call, with an empty candidate list — which
// is "found nothing", not "cannot search".
func TestMusicBrainzBlankQueryAsksNothing(t *testing.T) {
	p, host := newProvider(t, jsonHandler(`{}`), noPacing())
	resp, err := p.Search(t.Context(), pluginapi.SearchRequest{Kind: "track", Query: "   "})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 0 {
		t.Errorf("blank query = %+v, want matched with no candidates", resp)
	}
	if n := len(requestPaths(host)); n != 0 {
		t.Errorf("a blank query cost %d requests", n)
	}
}

const mbArtistSearchJSON = `{"artists": [
  {"id": "art-1", "name": "Nirvana", "type": "Group", "disambiguation": "90s grunge band",
   "area": {"name": "United States"}},
  {"id": "art-2", "name": "Nirvana", "type": "Group", "disambiguation": "60s UK band",
   "area": {"name": "United Kingdom"}}
]}`

// TestMusicBrainzSearchArtistParsesCandidates: an artist query hits /artist and
// maps artists into candidates carrying the MBID, name, and a type/area/
// disambiguation hint (the "wrong Nirvana" tell).
func TestMusicBrainzSearchArtistParsesCandidates(t *testing.T) {
	p, host := newProvider(t, pathHandler("/artist", mbArtistSearchJSON), noPacing())

	cands, err := search(p, "artist", "Nirvana", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if seen := requestPaths(host); len(seen) != 1 || seen[0] != "/artist" {
		t.Fatalf("expected one /artist call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].ExternalID != "art-1" || cands[0].Title != "Nirvana" || cands[0].Kind != "artist" {
		t.Errorf("candidate[0] = %+v", cands[0])
	}
	if !strings.Contains(cands[0].Disambiguation, "90s grunge") ||
		!strings.Contains(cands[0].Disambiguation, "United States") {
		t.Errorf("disambiguation = %q", cands[0].Disambiguation)
	}
	if !strings.Contains(cands[1].Disambiguation, "60s UK") {
		t.Errorf("candidate[1] disambiguation = %q", cands[1].Disambiguation)
	}
}

// TestMusicBrainzSearchAlbumCarriesTracklist: an album (release-group) query hits
// /release-group for candidates and /release (inc=recordings) for each candidate's
// tracklist preview — the ordered disc/position/title an Admin confirms before
// applying (ADR-0019; the positional cascade consumes it). The thumbnail comes off
// URL2, the Cover Art Archive.
func TestMusicBrainzSearchAlbumCarriesTracklist(t *testing.T) {
	const rgJSON = `{"release-groups": [
	  {"id": "rg-1", "title": "OK Computer", "first-release-date": "1997-05-21",
	   "artist-credit": [{"name": "Radiohead"}]}
	]}`
	const releaseJSON = `{"releases": [
	  {"media": [{"position": 1, "tracks": [
	    {"position": 1, "title": "Airbag"},
	    {"position": 2, "title": "Paranoid Android"}
	  ]}]}
	]}`
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			_, _ = w.Write([]byte(rgJSON))
		case "/release":
			_, _ = w.Write([]byte(releaseJSON))
		default:
			http.NotFound(w, r)
		}
	}, noPacing())

	cands, err := search(p, "album", "OK Computer", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	c := cands[0]
	if c.ExternalID != "rg-1" || c.Title != "OK Computer" || c.Year != 1997 || c.Kind != "album" {
		t.Errorf("candidate = %+v", c)
	}
	if c.ThumbnailURL != caaHost+"/release-group/rg-1/front-250" {
		t.Errorf("thumbnail = %q, want the 250px cover off URL2", c.ThumbnailURL)
	}
	if len(c.Tracklist) != 2 || c.Tracklist[0].Title != "Airbag" || c.Tracklist[1].Position != 2 {
		t.Errorf("tracklist = %+v", c.Tracklist)
	}
	if c.Tracklist[0].Disc != 1 {
		t.Errorf("tracklist disc = %d, want 1", c.Tracklist[0].Disc)
	}
}

// A tracklist preview that FAILS is non-fatal: the candidate is still offered,
// without a preview (ADR-0001). It is the one place this provider swallows a
// fetch failure, and it did before the port too.
func TestAnAlbumCandidateSurvivesAFailedTracklistPreview(t *testing.T) {
	fastBackoff(t)
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release-group" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg-1","title":"OK Computer"}]}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}, noPacing())

	cands, err := search(p, "album", "OK Computer", pluginapi.Page{}, "", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 || cands[0].ExternalID != "rg-1" {
		t.Fatalf("candidates = %+v, want the album offered anyway", cands)
	}
	if len(cands[0].Tracklist) != 0 {
		t.Errorf("tracklist = %+v, want none", cands[0].Tracklist)
	}
}

// TestMusicBrainzArtistLookupByPinnedMBID: a pinned artist MBID resolves BY id
// (/artist/{mbid}) — the durable artist Enrichment override path — instead of a
// name search.
func TestMusicBrainzArtistLookupByPinnedMBID(t *testing.T) {
	p, host := newProvider(t, pathHandler("/artist/art-42",
		`{"id": "art-42", "name": "Corrected Artist", "type": "Group", "tags": [{"name": "grunge"}]}`), noPacing())

	meta, err := lookup(p, pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "art-42"}, Title: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if seen := requestPaths(host); len(seen) != 1 || seen[0] != "/artist/art-42" {
		t.Fatalf("expected a by-id /artist/art-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.Name != "Corrected Artist" || meta.ExternalID != "art-42" {
		t.Errorf("meta = %+v", meta)
	}
	if len(meta.Genres) != 1 || meta.Genres[0] != "grunge" {
		t.Errorf("genres = %v", meta.Genres)
	}
}

// TestMusicBrainzAlbumLookupByPinnedMBID: a pinned release-group MBID resolves BY
// id (/release-group/{mbid}) — the durable album Enrichment override path.
func TestMusicBrainzAlbumLookupByPinnedMBID(t *testing.T) {
	p, host := newProvider(t, pathHandler("/release-group/rg-42",
		`{"id": "rg-42", "title": "Corrected Album", "first-release-date": "2000-01-01", "tags": [{"name": "rock"}]}`),
		noPacing())

	meta, err := lookup(p, pluginapi.MediaRef{Kind: "album", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "rg-42"}, Album: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if seen := requestPaths(host); len(seen) != 1 || seen[0] != "/release-group/rg-42" {
		t.Fatalf("expected a by-id /release-group/rg-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.ExternalID != "rg-42" {
		t.Errorf("meta = %+v", meta)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "cover" {
		t.Errorf("expected a cover artwork ref, got %+v", meta.Artwork)
	}
}

// A pasted /release/ URL pins the ALBUM: the release resolves to its parent
// release-group and the record is that release-group's (ADR-0038).
func TestMusicBrainzLookupResolvesReleaseToReleaseGroup(t *testing.T) {
	p, host := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release/rel-1":
			_, _ = w.Write([]byte(`{"release-group":{"id":"rg-7"}}`))
		case "/release-group/rg-7":
			_, _ = w.Write([]byte(`{"id":"rg-7","title":"Parent Album","first-release-date":"1994-01-01"}`))
		default:
			http.NotFound(w, r)
		}
	}, noPacing())

	meta, err := lookup(p, pluginapi.MediaRef{Kind: "album", ReleaseMBID: "rel-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.ExternalID != "rg-7" || meta.Name != "Parent Album" {
		t.Errorf("meta = %+v, want the parent release-group", meta)
	}
	want := []string{"/release/rel-1", "/release-group/rg-7"}
	if got := requestPaths(host); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

// A 404 anywhere on a lookup is the definitive "no such record", NOT a
// connectivity failure: the Admin who pasted a stale id is told there is no
// record, and the item is not retried forever.
func TestMusicBrainzLookupNotFoundIsNoMatch(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, noPacing())

	_, err := lookup(p, pluginapi.MediaRef{Kind: "album", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "rg-gone"}})
	if !errors.Is(err, errNoMatch) {
		t.Errorf("err = %v, want no-match", err)
	}
}
