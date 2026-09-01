package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/server"
)

// The outbound identity, guarded. MusicBrainz requires
// "Application name/<version> ( contact )" and throttles anonymous agents harder
// than identified ones (https://musicbrainz.org/doc/MusicBrainz_API/Rate_Limiting),
// so these tests assert both the SHAPE of the string and that it actually reaches
// every request we make — including the artwork download, which for years sent
// Go's default agent while the manifest request beside it was identified.

// TestDefaultUserAgentShape: the UA carries a version and a reachable contact, and
// is none of the anonymous forms MusicBrainz calls out.
func TestDefaultUserAgentShape(t *testing.T) {
	ua := DefaultUserAgent
	if !strings.HasPrefix(ua, "obelo/") {
		t.Errorf("UA must lead with the application name: %q", ua)
	}
	openIdx, closeIdx := strings.Index(ua, "("), strings.Index(ua, ")")
	if openIdx < 0 || closeIdx < openIdx {
		t.Fatalf("UA must carry a parenthesised contact: %q", ua)
	}
	contact := ua[openIdx+1 : closeIdx]
	if !strings.Contains(contact, "https://www.obelo.tv") || !strings.Contains(contact, "metadata@obelo.tv") {
		t.Errorf("UA contact must reach the project: %q", contact)
	}
	for _, bad := range []string{"Java", "Python-urllib", "Go-http-client", "self-hosted"} {
		if strings.Contains(ua, bad) {
			t.Errorf("UA contains the anonymous/generic marker %q: %s", bad, ua)
		}
	}
}

// TestDefaultUserAgentTracksBuildVersion: the version is READ from server.Version,
// never hand-copied. The string this replaced said "obelo/1.0" against a 0.1.0
// build; a host trying to pin a misbehaving release got a version that never
// existed. Asserting on the constant (not a literal) is what keeps it honest.
func TestDefaultUserAgentTracksBuildVersion(t *testing.T) {
	if !strings.Contains(DefaultUserAgent, "obelo/"+server.Version+" ") {
		t.Errorf("UA %q does not carry the build version %q", DefaultUserAgent, server.Version)
	}
}

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
	if wsUA != DefaultUserAgent {
		t.Errorf("web service UA = %q, want %q", wsUA, DefaultUserAgent)
	}
	if caaUA != DefaultUserAgent {
		t.Errorf("cover art archive UA = %q, want %q", caaUA, DefaultUserAgent)
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
	if got != DefaultUserAgent {
		t.Errorf("artwork fetch UA = %q, want %q", got, DefaultUserAgent)
	}

	// An explicit UserAgent overrides the default (the seam a fork or a test uses).
	if _, _, err := (HTTPArtworkFetcher{UserAgent: "obelo-test/9.9 ( test@example.com )"}).Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("fetch with override: %v", err)
	}
	if got != "obelo-test/9.9 ( test@example.com )" {
		t.Errorf("override ignored, UA = %q", got)
	}
}
