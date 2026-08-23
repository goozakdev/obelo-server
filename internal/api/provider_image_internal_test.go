package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// White-box tests for the two provider-image rules that cannot be observed from
// outside: which URLs are signable at all, and which redirect hops the fetch will
// follow. The black-box behavior (proxying, refusal, auth) lives in
// provider_image_test.go.

// TestProviderImageProxyURLOnlySignsAbsoluteHTTP: only an absolute http(s) URL
// becomes a proxy reference. Everything else yields "" — "show no thumbnail" —
// rather than passing the string through to the browser, which is what would put
// the browser back in touch with a third party (or hand it a javascript: URL).
func TestProviderImageProxyURLOnlySignsAbsoluteHTTP(t *testing.T) {
	p := newProviderImageProxy()

	for _, tc := range []struct {
		name string
		raw  string
		want bool // want a proxy reference back
	}{
		{"https provider url", "https://image.tmdb.org/t/p/w500/abc.jpg", true},
		{"http mirror on the operator's LAN", "http://mirror.lan:8080/p/abc.jpg", true},
		{"empty", "", false},
		{"blank", "   ", false},
		{"relative path", "/t/p/w500/abc.jpg", false},
		{"scheme-relative", "//image.tmdb.org/abc.jpg", false},
		{"no host", "https:///abc.jpg", false},
		{"javascript", "javascript:alert(1)", false},
		{"data url", "data:image/png;base64,AAAA", false},
		{"file url", "file:///etc/passwd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.proxyURL(tc.raw)
			if tc.want != (got != "") {
				t.Fatalf("proxyURL(%q) = %q, want signable=%v", tc.raw, got, tc.want)
			}
			if !tc.want {
				return
			}
			if !strings.HasPrefix(got, APIPrefix+providerImagePath+"?ref=") {
				t.Fatalf("proxyURL(%q) = %q, want a %s%s reference", tc.raw, got, APIPrefix, providerImagePath)
			}
		})
	}
}

// TestProviderImageRefIsKeyBound: a reference round-trips through the proxy that
// minted it, and is refused by any other — which is the per-boot key's whole
// contract. It is also why a picker left open across a restart must refetch its
// candidate list (the lists are per-process and TTL'd anyway; see the file
// header).
func TestProviderImageRefIsKeyBound(t *testing.T) {
	const target = "https://coverartarchive.org/release-group/abc/front-250"
	minted := newProviderImageProxy()
	other := newProviderImageProxy() // a "restarted" server: a fresh key

	ref := strings.TrimPrefix(minted.proxyURL(target), APIPrefix+providerImagePath+"?ref=")
	if ref == "" {
		t.Fatal("proxyURL returned no reference for a plain https URL")
	}

	got, ok := minted.resolveRef(ref)
	if !ok || got != target {
		t.Fatalf("resolveRef by the minting proxy = (%q, %v), want (%q, true)", got, ok, target)
	}
	if _, ok := other.resolveRef(ref); ok {
		t.Error("a reference signed with one boot's key verified against another's — the key is not doing anything")
	}
	if _, ok := minted.resolveRef(""); ok {
		t.Error("an empty reference verified")
	}
}

// TestProviderImageClientCarriesTheRedirectPolicy: the proxy's client is built by
// safefetch and keeps its policy. The policy's own table of blocked hops now lives
// with it (internal/safefetch), because the artwork fetcher and the subtitle
// download need the identical rule and a second copy would drift; what has to be
// asserted HERE is only that this client still has it. The end-to-end refusal —
// a provider redirecting the proxy back onto loopback yields a 404 and the bytes
// never arrive — is TestProviderImageUpstreamFailuresAreAll404 in
// provider_image_test.go.
func TestProviderImageClientCarriesTheRedirectPolicy(t *testing.T) {
	p := newProviderImageProxy()
	if p.client.CheckRedirect == nil {
		t.Fatal("the provider-image client follows redirects unchecked")
	}
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if err := p.client.CheckRedirect(req, []*http.Request{nil}); !errors.Is(err, safefetch.ErrRedirectBlocked) {
		t.Fatalf("redirect to the cloud metadata address = %v, want safefetch.ErrRedirectBlocked", err)
	}
}
