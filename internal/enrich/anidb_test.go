package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const anidbAnimeXML = `<?xml version="1.0" encoding="UTF-8"?>
<anime id="1">
  <titles>
    <title xml:lang="x-jat" type="main">Cowboy Bebop</title>
    <title xml:lang="en" type="official">Cowboy Bebop</title>
    <title xml:lang="ja" type="official">カウボーイビバップ</title>
  </titles>
  <description>In 2071, a ragtag crew of bounty hunters chases a bounty.</description>
  <picture>12345.jpg</picture>
  <startdate>1998-04-03</startdate>
  <tags><tag><name>space</name></tag><tag><name>noir</name></tag></tags>
</anime>`

// TestAniDBLookupByID asserts the AniDB client resolves BY a pinned anime id into
// enrichment-only descriptive fields (title, overview, year, poster, genres) and
// NEVER surfaces anything beyond the AniDB id as identity.
func TestAniDBLookupByID(t *testing.T) {
	var gotAID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAID = r.URL.Query().Get("aid")
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(anidbAnimeXML))
	}))
	defer srv.Close()

	p := NewAniDBProvider("juicebox-client", srv.URL, "en-US")
	p.minInterval = 0 // no throttle in the test

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "whatever", AniDBID: "1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if gotAID != "1" {
		t.Errorf("requested aid = %q, want 1", gotAID)
	}
	if !meta.Matched || meta.Source != "anidb" || meta.ExternalID != "1" {
		t.Errorf("meta identity = matched:%v source:%q id:%q, want matched anidb/1", meta.Matched, meta.Source, meta.ExternalID)
	}
	if meta.Name != "Cowboy Bebop" || meta.Year != 1998 {
		t.Errorf("title/year = %q/%d, want Cowboy Bebop/1998", meta.Name, meta.Year)
	}
	if meta.Overview == "" || len(meta.Genres) != 2 {
		t.Errorf("overview/genres = %q/%v, want both populated", meta.Overview, meta.Genres)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "poster" {
		t.Errorf("artwork = %+v, want one poster", meta.Artwork)
	}
}

// TestAniDBNoIDIsNoMatch asserts a lookup with no anime id is a graceful ErrNoMatch
// (AniDB ids are not naming-derived; matching by name is out of scope) — so an
// AniDB-led chain leaves an unpinned Title unmatched rather than guessing identity.
func TestAniDBNoIDIsNoMatch(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(anidbAnimeXML))
	}))
	defer srv.Close()

	p := NewAniDBProvider("c", srv.URL, "en-US")
	p.minInterval = 0
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "No ID"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch for an unpinned title", err)
	}
	if hit {
		t.Errorf("AniDB was queried for a title with no anime id; want zero calls")
	}
}

// TestAniDBUnknownAIDIsNoMatch asserts AniDB's <error>Unknown</error> reply is a
// no-match, not a hard error the chain would surface.
func TestAniDBUnknownAIDIsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<error>Unknown anime</error>`))
	}))
	defer srv.Close()

	p := NewAniDBProvider("c", srv.URL, "en-US")
	p.minInterval = 0
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", AniDBID: "999999"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch for an unknown aid", err)
	}
}

// TestAniDBRegistryEntry asserts AniDB's registry facts (ADR-0027): a Full video
// provider, RequiresKey, and shipped globally DISABLED (no seed row) so it appears
// as a usable authoritative candidate only once keyed.
func TestAniDBRegistryEntry(t *testing.T) {
	e, ok := RegistryEntryFor(SlugAniDB)
	if !ok {
		t.Fatalf("AniDB not registered")
	}
	if e.Class != ClassFull {
		t.Errorf("AniDB class = %q, want full (leadable)", e.Class)
	}
	if !e.serves(KindVideo) || e.serves(KindMusic) {
		t.Errorf("AniDB kinds = %v, want video only", e.Kinds)
	}
	if !e.RequiresKey {
		t.Errorf("AniDB RequiresKey = false, want true (needs a registered client)")
	}

	// It is a Full VIDEO candidate...
	found := false
	for _, c := range FullProvidersForKind(KindVideo) {
		if c.Slug == SlugAniDB {
			found = true
		}
	}
	if !found {
		t.Errorf("AniDB missing from the Full video candidates")
	}
	// ...but NOT the kind default (TMDB is), so adding it changes no existing Library.
	if DefaultAuthoritativeForKind(KindVideo) == SlugAniDB {
		t.Errorf("AniDB became the video default; want TMDB unchanged")
	}
	// Shipped disabled: with NO provider rows, AniDB is neither enabled nor keyed, so
	// it is not a usable authoritative until configured.
	states := ProviderStatesFromRows(nil)
	if s := states[SlugAniDB]; s.Enabled || s.Keyed {
		t.Errorf("AniDB default state = %+v, want disabled + unkeyed (ships off)", s)
	}
}

// TestBuildAniDBLeads asserts a keyed AniDB pointed as the video authoritative
// composes as the chain's lead (with TMDB, if keyed, a fill-only supplement).
func TestBuildAniDBLeads(t *testing.T) {
	provider, en := BuildProvider(ProviderConfig{
		AuthoritativeVideo: SlugAniDB,
		AniDBAPIKey:        "anidb-client",
		TMDBAPIKey:         "tmdb-key",
	})
	if !en.Video {
		t.Errorf("enablement = %+v, want video on (AniDB keyed authoritative)", en)
	}
	comp := provider.(CompositeProvider)
	chain, ok := comp.Video.(*VideoChainProvider)
	if !ok {
		t.Fatalf("video = %T, want *VideoChainProvider (AniDB leads, TMDB supplements)", comp.Video)
	}
	if _, ok := chain.Authoritative.(*AniDBProvider); !ok {
		t.Errorf("authoritative = %T, want *AniDBProvider", chain.Authoritative)
	}
	var haveTMDB bool
	for _, s := range chain.Supplements {
		if _, ok := s.(*TMDBProvider); ok {
			haveTMDB = true
		}
	}
	if !haveTMDB {
		t.Errorf("supplements = %+v, want TMDB as a fill-only supplement", chain.Supplements)
	}
}
