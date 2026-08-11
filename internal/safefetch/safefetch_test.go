package safefetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The redirect policy every third-party fetch in this server runs under. These
// tests moved here from internal/api (provider_image_internal_test.go) when the
// rule stopped belonging to the provider-image proxy alone; the callers keep their
// own end-to-end tests that the policy is actually ON their client
// (enrich/fetcher_test.go, subfetch/opensubtitles_test.go, api/provider_image_test.go).

// TestCheckRedirectPolicy: redirects are followed (the Cover Art Archive needs it)
// but bounded, and never onto an address that only this server can reach. The
// blocked list is what stops a hostile or hijacked provider from walking one of our
// fetches onto 127.0.0.1, a LAN admin panel, or 169.254.169.254.
//
// Every case uses an IP LITERAL so the resolver answers without a DNS query — the
// test stays hermetic and cannot fail on somebody's flaky network. That is also why
// the fail-closed-on-DNS case below uses an EMPTY host rather than a .invalid name:
// a name would be a real query, and a resolver that hijacks NXDOMAIN into an ad
// server would answer with a public address and turn the assertion inside out.
func TestCheckRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		hops    int
		wantErr bool
	}{
		{"public address, first hop", "https://93.184.216.34/cover.jpg", 1, false},
		{"public address, last allowed hop", "https://93.184.216.34/cover.jpg", MaxRedirects - 1, false},
		{"public address, one hop too many", "https://93.184.216.34/cover.jpg", MaxRedirects, true},
		{"loopback", "http://127.0.0.1:8080/admin", 1, true},
		{"IPv6 loopback", "http://[::1]:8080/admin", 1, true},
		{"RFC1918 private", "http://192.168.1.1/admin", 1, true},
		{"IPv6 ULA private", "http://[fd00::1]/admin", 1, true},
		{"link-local (cloud metadata)", "http://169.254.169.254/latest/meta-data/", 1, true},
		{"unspecified", "http://0.0.0.0/", 1, true},
		{"multicast", "http://224.0.0.1/", 1, true},
		{"non-http scheme", "file:///etc/passwd", 1, true},
		{"unresolvable host fails closed", "http:///cover.jpg", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatalf("building request for %q: %v", tc.target, err)
			}
			via := make([]*http.Request, tc.hops)
			err = CheckRedirect(req, via)
			if tc.wantErr != (err != nil) {
				t.Fatalf("CheckRedirect(%q, %d hops) = %v, want blocked=%v", tc.target, tc.hops, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrRedirectBlocked) {
				t.Fatalf("CheckRedirect(%q) = %v, want ErrRedirectBlocked", tc.target, err)
			}
		})
	}
}

// TestMaxRedirectsCoversTheCoverArtChain pins the constant to the chain that forced
// it: coverartarchive.org → archive.org → the storage node. It is a bound, not a
// ban, and lowering it silently breaks every music cover.
func TestMaxRedirectsCoversTheCoverArtChain(t *testing.T) {
	if MaxRedirects < 3 {
		t.Fatalf("MaxRedirects = %d, but the Cover Art Archive needs 3 hops (coverartarchive → archive.org → storage node)", MaxRedirects)
	}
}

// TestGuardCopiesRatherThanMutating: Guard must not reach into a client somebody
// else owns. One of the values that reaches it used to be http.DefaultClient — a
// process-global — so a mutating implementation would change the redirect behaviour
// of every unrelated HTTP call in the process.
func TestGuardCopiesRatherThanMutating(t *testing.T) {
	caller := &http.Client{Timeout: 7 * time.Second}
	transport := http.DefaultTransport
	caller.Transport = transport

	guarded := Guard(caller)

	if guarded == caller {
		t.Fatal("Guard returned the caller's own client — it must copy")
	}
	if caller.CheckRedirect != nil {
		t.Error("Guard mutated the caller's client")
	}
	if guarded.CheckRedirect == nil {
		t.Fatal("the guarded copy carries no redirect policy")
	}
	// The copy shares the transport (connection pooling, injected RoundTrippers)
	// and keeps the caller's timeout — only the redirect decision is ours.
	if guarded.Transport != transport || guarded.Timeout != 7*time.Second {
		t.Errorf("guarded copy lost the caller's transport/timeout: %+v", guarded)
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Error("http.DefaultClient was mutated")
	}
	if g := Guard(nil); g.CheckRedirect == nil || g == http.DefaultClient {
		t.Error("Guard(nil) must yield a guarded stand-in, not http.DefaultClient itself")
	}
}

// TestClientRefusesInwardRedirectEndToEnd: the policy on a real client, driven
// through a real redirect — the check is worthless if it is only ever called by a
// unit test.
//
// The stub is on loopback and the DIRECT fetch of it succeeds, which is the
// asymmetry working as designed (an operator's LAN mirror must stay reachable, and
// this whole test suite is httptest servers on 127.0.0.1). The same address as a
// redirect TARGET is refused.
func TestClientRefusesInwardRedirectEndToEnd(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inward" {
			http.Redirect(w, r, srv.URL+"/ok", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := Client(5 * time.Second)

	resp, err := c.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("direct fetch of a loopback stub must still work: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = c.Get(srv.URL + "/inward")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect onto loopback was followed")
	}
	if !errors.Is(err, ErrRedirectBlocked) {
		t.Fatalf("got %v, want it to wrap ErrRedirectBlocked", err)
	}
}
