package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The real MusicBrainz provider's HTTP/parse layer, exercised against an httptest
// server serving canned JSON — the secondary, lower seam (the project's black-box
// tests use a fake provider). No live network is ever touched.
//
// The TMDB half of this file went with the provider (ADR-0059): its tests are now
// plugins/tmdb/tmdb, driven natively against an in-memory host. What is left here
// that still needs a video source is the routing test at the bottom, and routing
// needs a source that ANSWERS rather than one that talks to anything.

// videoSourceStub is a video provider that matches every video kind and stamps a
// source name, so a test about ROUTING can assert which sub-provider answered
// without standing up either real one.
type videoSourceStub struct{ source string }

func (v videoSourceStub) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "show", "season", "episode":
		return TitleMetadata{Matched: true, Name: ref.Title, Source: v.source}, nil
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

func (v videoSourceStub) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (v videoSourceStub) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// --- MusicBrainz ------------------------------------------------------------

func mbStub(t *testing.T) (*MusicBrainzProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artist":
			_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","disambiguation":"English rock band","area":{"name":"Oxford"},"tags":[{"name":"alternative rock"},{"name":"art rock"}]}]}`))
		case "/release-group":
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg-1","first-release-date":"1997-05-21","tags":[{"name":"alternative rock"}]}]}`))
		case "/recording":
			_, _ = w.Write([]byte(`{"recordings":[{"id":"rec-1","title":"Paranoid Android"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	p.MinInterval = 0 // don't throttle the test
	return p, &seen
}

func TestMusicBrainzArtistAlbumTrack(t *testing.T) {
	p, _ := mbStub(t)

	artist, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("artist lookup: %v", err)
	}
	if !artist.Matched || artist.Overview == "" || len(artist.Genres) == 0 || artist.ExternalID != "mb-1" {
		t.Errorf("artist metadata wrong: %+v", artist)
	}

	album, err := p.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("album lookup: %v", err)
	}
	if !album.Matched || album.ReleaseDate != "1997-05-21" || len(album.Genres) == 0 {
		t.Errorf("album metadata wrong: %+v", album)
	}
	if len(album.Artwork) != 1 || album.Artwork[0].Role != "cover" ||
		album.Artwork[0].URL != "https://coverart/release-group/rg-1/front-500" {
		t.Errorf("album cover wrong: %+v", album.Artwork)
	}

	track, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Paranoid Android", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("track lookup: %v", err)
	}
	if !track.Matched || track.Name != "Paranoid Android" {
		t.Errorf("track metadata wrong: %+v", track)
	}
}

func TestMusicBrainzNoResultsIsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[],"release-groups":[],"recordings":[]}`))
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	p.MinInterval = 0
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Nobody"}); err != ErrNoMatch {
		t.Errorf("artist err = %v, want ErrNoMatch", err)
	}
}

// A 503 (MusicBrainz rate-limit/temporary-unavailable) is retried with back-off
// rather than dropped: the second attempt succeeds.
func TestMusicBrainzRetriesOn503(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","tags":[{"name":"rock"}]}]}`))
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	// Tiny throttle AND tiny back-off so the test stays fast. The two are separate
	// knobs: the 503 back-off no longer rides on MinInterval (see retryBackoff).
	p.MinInterval = time.Millisecond
	p.RetryBackoff = time.Millisecond

	got, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup after 503 retry: %v", err)
	}
	if !got.Matched || got.ExternalID != "mb-1" {
		t.Errorf("metadata wrong after retry: %+v", got)
	}
	if calls != 2 {
		t.Errorf("want 2 attempts (503 then 200), got %d", calls)
	}
}

// CompositeProvider routes by kind: video → TMDB, music → MusicBrainz.
func TestCompositeRoutesByKind(t *testing.T) {
	tmdb := videoSourceStub{source: "tmdb"}
	mb, _ := mbStub(t)
	c := CompositeProvider{Video: tmdb, Music: mb}

	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "show", Title: "GoT"}); err != nil || m.Source != "tmdb" {
		t.Errorf("show routed wrong: %+v err=%v", m, err)
	}
	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"}); err != nil || m.Source != "musicbrainz" {
		t.Errorf("artist routed wrong: %+v err=%v", m, err)
	}
	// A nil sub-provider degrades to ErrNoMatch for its kinds.
	videoOnly := CompositeProvider{Video: tmdb}
	if _, err := videoOnly.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "x"}); err != ErrNoMatch {
		t.Errorf("nil music provider err = %v, want ErrNoMatch", err)
	}
}
