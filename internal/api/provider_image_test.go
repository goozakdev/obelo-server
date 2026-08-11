package api_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/marioquake/obelo-server/internal/enrich"
	"github.com/marioquake/obelo-server/internal/testharness"
)

// Black-box tests for the metadata-provider image proxy (GET /providerImage,
// internal/api/provider_image.go): the admin Edit-item pickers must be able to
// show provider thumbnails WITHOUT the browser ever contacting TMDB / the Cover
// Art Archive / fanart.tv itself (ADR-0001), and the endpoint that makes that
// possible must not become an open proxy while doing it.
//
// Everything here runs against a LOCAL stub image host (httptest), so there is
// zero real network. The stub also counts requests, which is how the "a forged
// reference makes the server fetch NOTHING" assertions are actually proved rather
// than inferred from a status code.

// --- stub provider image host ------------------------------------------------

// pngBytes is a byte string http.DetectContentType sniffs as image/png (the PNG
// signature followed by filler). Small on purpose: the tests care about the
// sniffed type and the round-trip, not about decodable pixels.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("obelo"), 20)...)

// stubImageHost is a fake provider image host. It serves a PNG, a deliberately
// non-image body, an oversized body, a dead endpoint, and a redirect — and
// records every path it was asked for, so a test can assert the server fetched
// exactly what it should have (and, more importantly, nothing it shouldn't).
type stubImageHost struct {
	*httptest.Server
	mu   sync.Mutex
	hits []string
}

func newStubImageHost(t *testing.T) *stubImageHost {
	t.Helper()
	h := &stubImageHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits = append(h.hits, r.URL.Path)
		h.mu.Unlock()

		switch r.URL.Path {
		case "/poster.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		case "/lying.png":
			// Claims to be an image, is HTML. The proxy must sniff, not believe.
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("<!doctype html><html><body>not an image</body></html>"))
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
			_, _ = w.Write(bytes.Repeat([]byte("x"), (16<<20)+1))
		case "/dead.png":
			w.WriteHeader(http.StatusInternalServerError)
		case "/redirect-inward.png":
			// The Cover Art Archive shape (a redirect), but pointed back at this
			// loopback host — the hop the proxy must refuse.
			http.Redirect(w, r, h.Server.URL+"/poster.png", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

// hitCount reports how many times the stub host was asked for anything.
func (h *stubImageHost) hitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hits)
}

// --- helpers -----------------------------------------------------------------

// thumbProvider wires a fake MetadataProvider whose search candidates AND artwork
// candidates point their images at the stub host, so both picker surfaces can be
// driven end to end.
func thumbProvider(host *stubImageHost, imgPath string) *fakeProvider {
	return &fakeProvider{
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "999", Title: "Dune", Year: 2021, Kind: kind,
				ThumbnailURL: host.Server.URL + imgPath, Disambiguation: "Paul Atreides on Arrakis.",
			}}, nil
		},
		artworkFn: func(_ enrich.TitleRef, _ string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{
				{URL: host.Server.URL + imgPath, Width: 1000, Height: 1500, Source: "tmdb"},
			}, nil
		},
	}
}

// thumbnailFixture boots a server whose provider offers a thumbnail at imgPath on
// the stub host, scans + enriches a Movie library, and returns everything a test
// needs to drive the pickers.
func thumbnailFixture(t *testing.T, imgPath string) (*stubImageHost, *testharness.Server, string, string) {
	t.Helper()
	requireFixtures(t)
	host := newStubImageHost(t)
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(thumbProvider(host, imgPath)),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("POSTERBYTES")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	return host, srv, token, titleIDByName(t, srv, token, libID, "Dune")
}

// candidateThumbnailURL runs the record-search picker and returns the one
// candidate's thumbnailUrl, asserting it is same-origin (the whole point).
func candidateThumbnailURL(t *testing.T, srv *testharness.Server, token, titleID string) string {
	t.Helper()
	cands := searchCandidates(t, srv, token, titleID, "Dune")
	if len(cands.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1; %+v", len(cands.Candidates), cands.Candidates)
	}
	got := cands.Candidates[0].ThumbnailURL
	if !strings.HasPrefix(got, "/api/v1/providerImage?ref=") {
		t.Fatalf("thumbnailUrl = %q, want a same-origin /api/v1/providerImage reference", got)
	}
	return got
}

// refOf pulls the opaque ?ref= out of a proxy URL.
func refOf(t *testing.T, proxyURL string) string {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parsing proxy URL %q: %v", proxyURL, err)
	}
	ref := u.Query().Get("ref")
	if ref == "" {
		t.Fatalf("proxy URL %q carries no ref", proxyURL)
	}
	return ref
}

// getRaw does an authenticated GET and returns the status, content type, and body
// bytes (the proxy serves image bytes, so nothing here decodes JSON).
func getRaw(t *testing.T, srv *testharness.Server, path, token string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(path), nil)
	if err != nil {
		t.Fatalf("building GET %s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// --- the happy path ----------------------------------------------------------

// TestProviderThumbnailIsProxiedAndServed: the record-search picker's
// thumbnailUrl is a same-origin proxy URL (NOT the provider's host), and fetching
// it returns the provider's image bytes with the sniffed content type — with the
// server, not the browser, doing the talking to the provider.
func TestProviderThumbnailIsProxiedAndServed(t *testing.T) {
	host, srv, token, titleID := thumbnailFixture(t, "/poster.png")

	proxyURL := candidateThumbnailURL(t, srv, token, titleID)
	if strings.Contains(proxyURL, host.Server.URL) {
		t.Fatalf("thumbnailUrl leaks the provider host: %q", proxyURL)
	}
	// Listing candidates must not itself fetch any image — the bytes move only
	// when a browser asks for one.
	if n := host.hitCount(); n != 0 {
		t.Fatalf("listing candidates fetched %d image(s) from the provider, want 0", n)
	}

	status, ct, body := getRaw(t, srv, proxyURL, token)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", proxyURL, status, body)
	}
	if ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Errorf("proxied body (%d bytes) != the provider's image (%d bytes)", len(body), len(pngBytes))
	}
	if n := host.hitCount(); n != 1 {
		t.Errorf("provider host hits = %d, want exactly 1", n)
	}
}

// TestArtworkCandidateThumbnailIsProxied: the Fix-label image grid gets a proxied
// thumbnailUrl for display while `url` stays the provider's own URL — the pick
// identity the PUT /artwork body carries. Both halves matter: one keeps the
// browser off the provider, the other keeps picking working.
func TestArtworkCandidateThumbnailIsProxied(t *testing.T) {
	host, srv, token, titleID := thumbnailFixture(t, "/poster.png")

	var cands struct {
		Candidates []struct {
			URL          string `json:"url"`
			ThumbnailURL string `json:"thumbnailUrl"`
		} `json:"candidates"`
	}
	status, body := srv.AuthGET("/api/v1/titles/"+titleID+"/artworkCandidates?role=poster", token, &cands)
	if status != http.StatusOK {
		t.Fatalf("GET artworkCandidates = %d, want 200; body: %s", status, body)
	}
	if len(cands.Candidates) != 1 {
		t.Fatalf("artwork candidates = %d, want 1; body: %s", len(cands.Candidates), body)
	}
	c := cands.Candidates[0]
	if c.URL != host.Server.URL+"/poster.png" {
		t.Errorf("url = %q, want the provider URL %q (it is the pick identity)", c.URL, host.Server.URL+"/poster.png")
	}
	if !strings.HasPrefix(c.ThumbnailURL, "/api/v1/providerImage?ref=") {
		t.Fatalf("thumbnailUrl = %q, want a same-origin proxy reference", c.ThumbnailURL)
	}
	if st, _, b := getRaw(t, srv, c.ThumbnailURL, token); st != http.StatusOK || !bytes.Equal(b, pngBytes) {
		t.Errorf("fetching the artwork thumbnail = %d (%d bytes), want 200 with the image", st, len(b))
	}
}

// --- the SSRF surface --------------------------------------------------------

// TestProviderImageRefusesUnmintedReference is the anti-open-proxy test. A caller
// who hand-writes a reference — or tampers with one the server minted — gets a
// 404 and, critically, the server opens NO socket on their behalf. If someone
// ever "simplifies" the ?ref= into a plain ?url=, this is the test that fails.
func TestProviderImageRefusesUnmintedReference(t *testing.T) {
	host, srv, token, titleID := thumbnailFixture(t, "/poster.png")

	// A genuine reference, so the tamper cases below start from something real.
	realRef := refOf(t, candidateThumbnailURL(t, srv, token, titleID))
	encoded, sig, _ := strings.Cut(realRef, ".")
	// The same signature over a DIFFERENT url — the attack the MAC exists to stop.
	swapped := base64URL(host.Server.URL+"/dead.png") + "." + sig

	before := host.hitCount()
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"absent", ""},
		{"not a reference at all", "just-a-string"},
		{"a bare url, the open-proxy shape", host.Server.URL + "/poster.png"},
		{"url with no signature", base64URL(host.Server.URL + "/poster.png")},
		{"url with a forged signature", base64URL(host.Server.URL+"/poster.png") + "." + base64URL("nope")},
		{"a real signature over a swapped url", swapped},
		{"a real url with a truncated signature", encoded + "." + sig[:len(sig)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/providerImage"
			if tc.ref != "" {
				path += "?ref=" + url.QueryEscape(tc.ref)
			}
			if status, _, body := getRaw(t, srv, path, token); status != http.StatusNotFound {
				t.Errorf("GET with %s ref = %d, want 404; body: %s", tc.name, status, body)
			}
		})
	}
	if n := host.hitCount() - before; n != 0 {
		t.Errorf("refused references still caused %d upstream fetch(es), want 0 — the server must never fetch a URL it did not mint", n)
	}
}

// base64URL is the reference encoding (unpadded base64url), spelled out here so
// the test forges references exactly the way an outside caller would have to.
func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// TestProviderImageRefusesBadUpstreamResponses: a provider that is dead, that
// lies about its content type, that redirects somewhere internal, or that streams
// forever all end the same way — a clean 404 and a broken thumbnail, never a 500
// storm, a hang, or an oversized allocation.
func TestProviderImageRefusesBadUpstreamResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"provider is down", "/dead.png"},
		{"body is HTML wearing an image/png header", "/lying.png"},
		{"body is over the size cap", "/huge.png"},
		{"redirects to a loopback address", "/redirect-inward.png"},
		{"path does not exist upstream", "/missing.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, srv, token, titleID := thumbnailFixture(t, tc.path)
			proxyURL := candidateThumbnailURL(t, srv, token, titleID)
			status, _, body := getRaw(t, srv, proxyURL, token)
			if status != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404; body: %s", tc.name, status, body)
			}
		})
	}
}

// --- auth wiring -------------------------------------------------------------

// TestProviderImageRequiresAdmin: the proxy is part of an Admin surface. A
// non-Admin User is refused 403 and an anonymous caller 401 — neither reaches the
// fetch.
func TestProviderImageRequiresAdmin(t *testing.T) {
	host, srv, token, titleID := thumbnailFixture(t, "/poster.png")
	proxyURL := candidateThumbnailURL(t, srv, token, titleID)

	srv.CreateUser(token, "member", adminPassword, "member")
	memberToken := srv.LoginAs("member", adminPassword)

	before := host.hitCount()
	if status, _, body := getRaw(t, srv, proxyURL, memberToken); status != http.StatusForbidden {
		t.Errorf("member GET = %d, want 403; body: %s", status, body)
	}
	if status, _, body := getRaw(t, srv, proxyURL, ""); status != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401; body: %s", status, body)
	}
	if n := host.hitCount() - before; n != 0 {
		t.Errorf("refused callers still caused %d upstream fetch(es), want 0", n)
	}
}

// TestProviderImageAcceptsMediaCookie is the test that would have caught the
// mistake this route is most likely to make: it is an <img src>, so a browser
// CANNOT send an Authorization header. Wired bearer-only, every Go test would
// still pass and every thumbnail in the picker would be broken.
func TestProviderImageAcceptsMediaCookie(t *testing.T) {
	_, srv, token, titleID := thumbnailFixture(t, "/poster.png")
	proxyURL := candidateThumbnailURL(t, srv, token, titleID)

	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-thumbnail")

	resp := cookieGET(t, srv, proxyURL, cookie, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-only GET = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Errorf("cookie-only body (%d bytes) != the provider's image", len(body))
	}

	// A garbage cookie is still 401 — the cookie path validates like the bearer one.
	bad := cookieGET(t, srv, proxyURL, &http.Cookie{Name: mediaCookieName, Value: "not-a-real-token"}, "")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("garbage cookie = %d, want 401", bad.StatusCode)
	}
}
