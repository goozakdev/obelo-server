package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// White-box tests for the trust decision itself (forwarded.go): which peer's
// headers are read, and where in X-Forwarded-For the client is found. The
// behaviour that matters to a caller — that a forged header cannot mint a fresh
// login-limiter budget, and that the cookie's Secure flag follows the same rule —
// is asserted from outside in auth_test.go and cookie_test.go. Both halves are
// wanted: this file pins the walk, those files prove the walk is actually wired
// to the controls that depend on it.

// prefixes parses an allowlist for the tests, failing loudly on a typo in the
// test itself rather than quietly producing an empty (trust-nothing) list, which
// would make every assertion below pass for the wrong reason.
func prefixes(t *testing.T, cidrs ...string) trustedProxies {
	t.Helper()
	out := make(trustedProxies, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("test allowlist %q does not parse: %v", c, err)
		}
		out = append(out, p.Masked())
	}
	return out
}

func requestFrom(peer string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.RemoteAddr = peer
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return r
}

// TestResolveOriginClientIP walks the whole X-Forwarded-For decision table. The
// case that carries the security property is "forged entries padded on the left":
// the header is built by APPENDING, so anything the client wrote sits to the left
// of what our own trusted peer wrote, and a right-to-left walk therefore never
// reaches it. Reading the left-most entry instead — the folklore version — would
// return 9.9.9.9 in that case and hand the caller a fresh rate-limit budget per
// request.
func TestResolveOriginClientIP(t *testing.T) {
	tests := []struct {
		name    string
		peer    string
		trusted []string
		xff     []string
		want    string
	}{
		{
			name: "no allowlist ignores the header entirely",
			peer: "203.0.113.5:41000",
			xff:  []string{"9.9.9.9"},
			want: "203.0.113.5",
		},
		{
			name:    "untrusted peer ignores the header entirely",
			peer:    "203.0.113.5:41000",
			trusted: []string{"10.0.0.0/8"},
			xff:     []string{"9.9.9.9"},
			want:    "203.0.113.5",
		},
		{
			name:    "trusted peer with no header falls back to the peer",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			want:    "10.0.0.1",
		},
		{
			name:    "trusted peer, single hop",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "forged entries padded on the left are never reached",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"9.9.9.9, 8.8.8.8, 198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "chain of trusted proxies resolves to the client, not an intermediate",
			peer:    "10.0.0.3:41000",
			trusted: []string{"10.0.0.0/24"},
			xff:     []string{"198.51.100.7, 10.0.0.1, 10.0.0.2"},
			want:    "198.51.100.7",
		},
		{
			name:    "forged entries plus a trusted chain still resolve to the client",
			peer:    "10.0.0.3:41000",
			trusted: []string{"10.0.0.0/24"},
			xff:     []string{"9.9.9.9, 198.51.100.7, 10.0.0.1, 10.0.0.2"},
			want:    "198.51.100.7",
		},
		{
			name:    "a forged entry that IMPERSONATES a trusted proxy is still skipped past",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.0/24"},
			xff:     []string{"10.0.0.9, 198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "several header lines are one chain in header order",
			peer:    "10.0.0.3:41000",
			trusted: []string{"10.0.0.0/24"},
			xff:     []string{"9.9.9.9, 198.51.100.7", "10.0.0.1, 10.0.0.2"},
			want:    "198.51.100.7",
		},
		{
			name:    "every hop trusted yields the left-most entry",
			peer:    "10.0.0.2:41000",
			trusted: []string{"10.0.0.0/8"},
			xff:     []string{"10.1.2.3, 10.0.0.1"},
			want:    "10.1.2.3",
		},
		{
			name:    "an unparseable right-most entry collapses to the peer",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"198.51.100.7, unknown"},
			want:    "10.0.0.1",
		},
		{
			name:    "an entry with a port is read as its address",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"198.51.100.7:9999"},
			want:    "198.51.100.7",
		},
		{
			name:    "an IPv6 client is normalized, brackets and all",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"[2001:db8::1]"},
			want:    "2001:db8::1",
		},
		{
			name:    "an IPv4-mapped peer matches an IPv4 prefix",
			peer:    "[::ffff:10.0.0.1]:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "an IPv4-mapped forwarded entry is unmapped so it keys as one address",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"::ffff:198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "an empty header is no header",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			xff:     []string{"   "},
			want:    "10.0.0.1",
		},
		{
			name: "a RemoteAddr with no port is kept whole rather than dropped",
			peer: "unix-socket",
			want: "unix-socket",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tc.xff {
				h.Add("X-Forwarded-For", v)
			}
			got := resolveOrigin(requestFrom(tc.peer, h), prefixes(t, tc.trusted...)).ClientIP
			if got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveOriginHTTPS pins the scheme half. r.TLS is the only signal that is
// not hearsay — we terminated that handshake — so it wins outright and a header
// can only ever add HTTPS, never take it away.
func TestResolveOriginHTTPS(t *testing.T) {
	tests := []struct {
		name    string
		peer    string
		trusted []string
		proto   string
		tlsOn   bool
		want    bool
	}{
		{name: "plain HTTP with no header", peer: "203.0.113.5:41000"},
		{
			name:  "plain HTTP, header from an untrusted peer",
			peer:  "203.0.113.5:41000",
			proto: "https",
			want:  false,
		},
		{
			name:    "plain HTTP, header from a trusted peer",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			proto:   "https",
			want:    true,
		},
		{
			name:    "the first hop of a chain is the original scheme",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			proto:   "https, http",
			want:    true,
		},
		{
			name:    "case and spacing do not matter",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			proto:   " HTTPS ",
			want:    true,
		},
		{
			name:    "a trusted peer saying http is believed",
			peer:    "10.0.0.1:41000",
			trusted: []string{"10.0.0.1/32"},
			proto:   "http",
			want:    false,
		},
		{
			name:  "real TLS wins with no allowlist at all",
			peer:  "203.0.113.5:41000",
			tlsOn: true,
			want:  true,
		},
		{
			name:  "real TLS is not turned off by a header claiming http",
			peer:  "203.0.113.5:41000",
			proto: "http",
			tlsOn: true,
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.proto != "" {
				h.Set("X-Forwarded-Proto", tc.proto)
			}
			r := requestFrom(tc.peer, h)
			if tc.tlsOn {
				r.TLS = &tls.ConnectionState{}
			}
			if got := resolveOrigin(r, prefixes(t, tc.trusted...)).HTTPS; got != tc.want {
				t.Errorf("HTTPS = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestXRealIPIsNeverConsulted: the other header a caller might expect to work.
// It is not read from anybody, trusted or not, because it carries no chain — a
// proxy that sets it and a proxy that passes the client's own copy through are
// identical on the wire, so there is no walk to perform and no way to tell the
// forged one from the genuine one.
func TestXRealIPIsNeverConsulted(t *testing.T) {
	h := http.Header{"X-Real-IP": []string{"198.51.100.7"}}
	got := resolveOrigin(requestFrom("10.0.0.1:41000", h), prefixes(t, "10.0.0.1/32")).ClientIP
	if got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want the peer %q — X-Real-IP must not be honoured", got, "10.0.0.1")
	}
}

// TestOriginFallsBackToZeroTrustWithoutTheMiddleware: clientIP and
// requestIsHTTPS are also reachable from a handler invoked directly, with no
// context attached. That path must resolve as if no proxy were trusted — missing
// configuration can only ever mean "trust nothing", never "trust anything".
func TestOriginFallsBackToZeroTrustWithoutTheMiddleware(t *testing.T) {
	r := requestFrom("203.0.113.5:41000", http.Header{
		"X-Forwarded-For":   []string{"9.9.9.9"},
		"X-Forwarded-Proto": []string{"https"},
	})
	if got := clientIP(r); got != "203.0.113.5" {
		t.Errorf("clientIP with no request context = %q, want the peer address", got)
	}
	if requestIsHTTPS(r) {
		t.Error("requestIsHTTPS with no request context = true; an unresolved request must not be treated as TLS")
	}
}
