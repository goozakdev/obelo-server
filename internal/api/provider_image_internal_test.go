package api

import (
	"net/http"
	"strings"
	"testing"
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

// TestProviderImageRedirectPolicy: redirects are followed (the Cover Art Archive
// needs it) but bounded, and never onto an address that only this server can
// reach. The blocked list is what stops a hostile or hijacked provider from using
// this proxy to read 127.0.0.1, a LAN admin panel, or 169.254.169.254.
//
// Every case uses an IP LITERAL so the resolver answers without a DNS query — the
// test stays hermetic and cannot fail on somebody's flaky network.
func TestProviderImageRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		hops    int
		wantErr bool
	}{
		{"public address, first hop", "https://93.184.216.34/cover.jpg", 1, false},
		{"public address, last allowed hop", "https://93.184.216.34/cover.jpg", maxProviderImageRedirects - 1, false},
		{"public address, one hop too many", "https://93.184.216.34/cover.jpg", maxProviderImageRedirects, true},
		{"loopback", "http://127.0.0.1:8080/admin", 1, true},
		{"IPv6 loopback", "http://[::1]:8080/admin", 1, true},
		{"RFC1918 private", "http://192.168.1.1/admin", 1, true},
		{"link-local (cloud metadata)", "http://169.254.169.254/latest/meta-data/", 1, true},
		{"unspecified", "http://0.0.0.0/", 1, true},
		{"non-http scheme", "file:///etc/passwd", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatalf("building request for %q: %v", tc.target, err)
			}
			via := make([]*http.Request, tc.hops)
			err = checkProviderImageRedirect(req, via)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkProviderImageRedirect(%q, %d hops) = %v, want blocked=%v", tc.target, tc.hops, err, tc.wantErr)
			}
		})
	}
}
