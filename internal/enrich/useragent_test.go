package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/useragent"
)

// The outbound identity, guarded. MusicBrainz requires
// "Application name/<version> ( contact )" and throttles anonymous agents harder
// than identified ones (https://musicbrainz.org/doc/MusicBrainz_API/Rate_Limiting),
// so these tests assert that it actually reaches every request we make —
// including the artwork download, which for years sent Go's default agent while
// the manifest request beside it was identified.
//
// The SHAPE of the string is asserted where the string now lives
// (internal/useragent), because since .scratch/bundled-plugins issue 02 it is not
// enrichment's alone: every fetch a sandboxed guest makes carries it too.

// TestMusicBrainzSendsUserAgent: both hosts the Music provider talks to — the web
// service and the Cover Art Archive manifest — must be told who is calling.
func TestMusicBrainzSendsUserAgent(t *testing.T) {
	var wsUA, caaUA string
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mbid-1","title":"Doolittle"}`))
	}))
	defer ws.Close()
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"image":"http://img/1.jpg","front":true}]}`))
	}))
	defer caa.Close()

	p := NewMusicBrainzProvider(ws.URL, caa.URL, "en")
	p.MinInterval = 0 // no throttle in tests
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: "mbid-1"}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "album", MusicbrainzID: "mbid-1"}, "cover"); err != nil {
		t.Fatalf("artwork candidates: %v", err)
	}
	if wsUA != useragent.Default {
		t.Errorf("web service UA = %q, want %q", wsUA, useragent.Default)
	}
	if caaUA != useragent.Default {
		t.Errorf("cover art archive UA = %q, want %q", caaUA, useragent.Default)
	}
}

// TestArtworkFetcherSendsUserAgent: the image download identifies itself too. This
// is the request that carries the actual cover bytes off the Cover Art Archive.
func TestArtworkFetcherSendsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 jpeg bytes"))
	}))
	defer srv.Close()

	if _, _, err := (HTTPArtworkFetcher{}).Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != useragent.Default {
		t.Errorf("artwork fetch UA = %q, want %q", got, useragent.Default)
	}

	// An explicit UserAgent overrides the default (the seam a fork or a test uses).
	if _, _, err := (HTTPArtworkFetcher{UserAgent: "obelo-test/9.9 ( test@example.com )"}).Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("fetch with override: %v", err)
	}
	if got != "obelo-test/9.9 ( test@example.com )" {
		t.Errorf("override ignored, UA = %q", got)
	}
}
