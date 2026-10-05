package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestRedirectBaseURLHonoursForwardedHostOnlyFromTrustedPeer: the OAuth
// redirect_uri host must not be steerable by an arbitrary caller's
// X-Forwarded-Host.
func TestRedirectBaseURLHonoursForwardedHostOnlyFromTrustedPeer(t *testing.T) {
	t.Parallel()
	trusted := trustedProxies{netip.MustParsePrefix("10.0.0.0/8")}
	build := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://obelo.lan:8080/api/v1/auth/sign-in/start", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-Host", "evil.example")
		return r.WithContext(withRequestOrigin(r.Context(), resolveOrigin(r, trusted)))
	}
	if got := redirectBaseURL(build("203.0.113.9:5555")); got != "http://obelo.lan:8080" {
		t.Errorf("untrusted peer: base = %q, want the real Host", got)
	}
	if got := redirectBaseURL(build("10.1.2.3:5555")); got != "http://evil.example" {
		t.Errorf("trusted proxy: base = %q, want its forwarded host", got)
	}
}
