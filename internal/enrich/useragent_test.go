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
// so these tests assert that it actually reaches every request we make.
//
// The SHAPE of the string is asserted where the string now lives
// (internal/useragent), because since .scratch/bundled-plugins issue 02 it is not
// enrichment's alone: every fetch a sandboxed guest makes carries it too.
//
// THE MUSICBRAINZ HALF OF THIS FILE IS GONE (issue 06), and its absence is the
// point. It asserted that both hosts the Music provider talks to — the web service
// and the Cover Art Archive manifest — were told who was calling, because that
// provider set the header itself on every request. A guest cannot and must not: the
// HOST writes the agent for every fetch and drops one a guest sent (ADR-0059
// decision 7). So the assertion moved in two directions — internal/plugins owns
// "the agent leaves the server", and plugins/musicbrainz/musicbrainz owns "this
// guest does not try to set one" — and what is left here is the download this
// package still makes with a client of its own.

// TestArtworkFetcherSendsUserAgent: the image download identifies itself. This is
// the request that carries the actual cover bytes off the Cover Art Archive, and it
// is the HOST's own fetch — the Plugin returns a URL, never bytes (ADR-0007).
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
