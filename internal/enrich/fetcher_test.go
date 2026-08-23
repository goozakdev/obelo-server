package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// The production HTTPArtworkFetcher's guards, exercised against an httptest
// server — no live network. A 404 must surface as ErrArtworkNotFound (the benign
// "no image here" case the enrich service skips quietly); an oversized body and a
// non-image content-type must surface as real errors.

func TestArtworkFetcherSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 jpeg bytes"))
	}))
	defer srv.Close()

	data, ct, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if ct != "image/jpeg" || len(data) == 0 {
		t.Errorf("got ct=%q len=%d", ct, len(data))
	}
}

func TestArtworkFetcherNotFoundIsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no cover", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("want ErrArtworkNotFound, got %v", err)
	}
}

func TestArtworkFetcherOtherStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err == nil || errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("want a non-sentinel error, got %v", err)
	}
}

func TestArtworkFetcherOversizeIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{MaxBytes: 16}.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want oversize error, got %v", err)
	}
}

func TestArtworkFetcherNonImageIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>error page</html>"))
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "non-image") {
		t.Fatalf("want non-image error, got %v", err)
	}
}

// TestArtworkFetcherRefusesInwardRedirect: the fetcher is handed a URL a THIRD
// PARTY chose (a provider's JSON, or an admin's PUT /titles/{id}/artwork), and the
// redirect off it is that party's choice too. A hop onto 127.0.0.1 must be refused,
// not followed — those bytes would land in the artwork cache and then in an admin's
// browser.
//
// Note what does NOT fail here: the direct fetch of the same loopback stub, in
// every other test in this file. Only redirect TARGETS are checked, deliberately —
// an operator may point an imageBaseURL at a mirror on their own LAN (ADR-0001).
func TestArtworkFetcherRefusesInwardRedirect(t *testing.T) {
	var srv *httptest.Server
	var hits int
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/poster.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0 secret internal bytes"))
			return
		}
		http.Redirect(w, r, srv.URL+"/poster.jpg", http.StatusFound)
	}))
	defer srv.Close()

	// Both the zero value app.go builds AND an explicitly injected bare client. The
	// second case is the one that would rot: the policy lives at the point of USE,
	// so a production wiring that started assigning its own &http.Client{} cannot
	// disarm it while every test stays green.
	for _, tc := range []struct {
		name    string
		fetcher HTTPArtworkFetcher
	}{
		{"zero value (what app.go constructs)", HTTPArtworkFetcher{}},
		{"caller-injected bare client", HTTPArtworkFetcher{HTTPClient: &http.Client{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits = 0
			data, _, err := tc.fetcher.Fetch(context.Background(), srv.URL+"/redirect-inward.jpg")
			if err == nil {
				t.Fatalf("a redirect onto loopback was followed; got %d bytes", len(data))
			}
			if !errors.Is(err, safefetch.ErrRedirectBlocked) {
				t.Fatalf("got %v, want it to wrap safefetch.ErrRedirectBlocked", err)
			}
			if hits != 1 {
				t.Errorf("stub was asked %d times, want 1 — the redirect target must never be requested", hits)
			}
		})
	}
}

// TestArtworkFetcherDoesNotMutateAnInjectedClient: the guard is applied to a copy.
// Reaching into a client the caller owns would be surprising on its own, and the
// value that used to arrive here in the nil case was a process-global.
func TestArtworkFetcherDoesNotMutateAnInjectedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 jpeg bytes"))
	}))
	defer srv.Close()

	client := &http.Client{}
	if _, _, err := (HTTPArtworkFetcher{HTTPClient: client}).Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if client.CheckRedirect != nil {
		t.Error("Fetch mutated the caller's client")
	}
}

// TestProviderJSONClientsCarryTheRedirectPolicy: all seven JSON metadata clients go
// through providerClient, including when a caller injects its own client — and none
// of them mutates what it was given. These clients only parse what comes back, so
// the risk they close is blind internal probing (a hop that connects and a hop that
// does not are distinguishable), not body disclosure.
func TestProviderJSONClientsCarryTheRedirectPolicy(t *testing.T) {
	injected := &http.Client{}
	for name, got := range map[string]*http.Client{
		"tmdb":        (&TMDBProvider{HTTPClient: injected}).client(),
		"musicbrainz": (&MusicBrainzProvider{HTTPClient: injected}).client(),
		"fanarttv":    (&FanartTVProvider{HTTPClient: injected}).client(),
		"theaudiodb":  (&TheAudioDBProvider{HTTPClient: injected}).client(),
		"anidb":       (&AniDBProvider{HTTPClient: injected}).client(),
		"omdb":        (&OMDbProvider{HTTPClient: injected}).client(),
		"thetvdb":     (&TheTVDBProvider{HTTPClient: injected}).client(),
		"nil client":  (&TMDBProvider{}).client(),
	} {
		if got.CheckRedirect == nil {
			t.Errorf("%s: client() follows redirects unchecked", name)
		}
	}
	if injected.CheckRedirect != nil {
		t.Error("client() mutated the injected client")
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Error("http.DefaultClient was mutated")
	}
}
