package plugins

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// hostTransport answers every request by its host, without a network: a host
// with no entry is answered 404, and every request is counted by host.
type hostTransport struct {
	bodies map[string]string
	seen   map[string]int
}

func (rt *hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.seen[r.URL.Hostname()]++
	body, ok := rt.bodies[r.URL.Hostname()+r.URL.Path]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, ""
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// TestADiscoveryDocumentAdmitsExactlyItsTokenAndUserinfoHosts: a redirect
// Sign-in provider whose issuer's discovery document names its token and
// userinfo endpoints on other hosts than the issuer (Google's shape) reaches
// those two hosts during the call that read the document — and nothing else the
// document names, nothing before the document is read, nothing on a later call
// that did not read it, and nothing a document from another path or naming
// another issuer says.
func TestADiscoveryDocumentAdmitsExactlyItsTokenAndUserinfoHosts(t *testing.T) {
	resolveAs(t, "203.0.113.7")
	const issuer = "https://idp.example.test/realm"
	doc := func(iss string) string {
		raw, _ := json.Marshal(map[string]string{
			"issuer":                 iss,
			"authorization_endpoint": "https://login.example.test/authorize",
			"token_endpoint":         "https://tokens.example.test/token",
			"userinfo_endpoint":      "https://people.example.test/userinfo",
			"jwks_uri":               "https://keys.example.test/certs",
		})
		return string(raw)
	}
	rt := &hostTransport{seen: map[string]int{}, bodies: map[string]string{
		"idp.example.test/realm/.well-known/openid-configuration":       doc(issuer),
		"idp.example.test/elsewhere/.well-known/openid-configuration":   doc("https://idp.example.test/elsewhere"),
		"idp.example.test/realm/other/.well-known/openid-configuration": doc(issuer),
		"tokens.example.test/token":                                     "{}",
		"people.example.test/userinfo":                                  "{}",
		"login.example.test/authorize":                                  "{}",
		"keys.example.test/certs":                                       "{}",
	}}
	p := newPlugin("oidc", "", Options{
		Logf:             func(string, ...any) {},
		HTTPClient:       &http.Client{Transport: rt},
		FailureThreshold: 1000,
	}.withDefaults())
	p.resolveLimits()
	defer p.close(context.Background())
	h := &hostFuncs{p: p}
	get := func(u string) pluginapi.FetchResponse {
		return h.fetch(context.Background(), pluginapi.FetchRequest{URL: u})
	}
	call := func(discovery string, body func()) {
		t.Helper()
		if err := p.beginCall(addrOf(issuer), false); err != nil {
			t.Fatal(err)
		}
		end := p.beginDiscovery(discovery)
		body()
		end()
		p.endCall()
	}
	wantRefused := func(u string) {
		t.Helper()
		if resp := get(u); resp.Refused != refusedAllowlist {
			t.Errorf("fetch %s = %+v, want refused %q", u, resp, refusedAllowlist)
		}
	}
	wantReached := func(u string) {
		t.Helper()
		if resp := get(u); resp.Refused != "" || resp.Status != http.StatusOK {
			t.Errorf("fetch %s = %+v, want 200", u, resp)
		}
	}
	discoveryURL := issuer + "/.well-known/openid-configuration"

	call(discoveryURL, func() {
		wantRefused("https://tokens.example.test/token")
		wantReached(discoveryURL)
		wantReached("https://tokens.example.test/token")
		wantReached("https://people.example.test/userinfo")
		wantRefused("https://login.example.test/authorize")
		wantRefused("https://keys.example.test/certs")
		wantRefused("https://other.example.test/")
	})
	call(discoveryURL, func() {
		wantRefused("https://tokens.example.test/token")
	})
	call("", func() {
		wantReached(discoveryURL)
		wantRefused("https://tokens.example.test/token")
	})
	call(discoveryURL, func() {
		wantReached("https://idp.example.test/realm/other/.well-known/openid-configuration")
		wantRefused("https://tokens.example.test/token")
	})
	call("https://idp.example.test/elsewhere/.well-known/openid-configuration", func() {
		wantReached("https://idp.example.test/elsewhere/.well-known/openid-configuration")
		wantReached("https://tokens.example.test/token")
	})
	call(discoveryURL, func() {
		rt.bodies["idp.example.test/realm/.well-known/openid-configuration"] = doc("https://idp.example.test/elsewhere")
		wantReached(discoveryURL)
		wantRefused("https://tokens.example.test/token")
	})
}

// resolveAs answers every name the private-address check asks about with addr,
// and every address literal as itself, for the rest of the test.
func resolveAs(t *testing.T, addr string) {
	t.Helper()
	prev := lookupIPAddr
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP(addr)}}, nil
	}
	t.Cleanup(func() { lookupIPAddr = prev })
}

// routeTransport answers each request by host and path with a fixed status,
// body and Location, without a network; anything unrouted is answered 404.
type routeTransport map[string]routeAnswer

type routeAnswer struct {
	status   int
	body     string
	location string
}

func (rt routeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	a, ok := rt[r.URL.Hostname()+":"+r.URL.Port()+r.URL.Path]
	if !ok {
		a, ok = rt[r.URL.Hostname()+r.URL.Path]
	}
	if !ok {
		a = routeAnswer{status: http.StatusNotFound}
	}
	h := http.Header{}
	if a.location != "" {
		h.Set("Location", a.location)
	}
	return &http.Response{StatusCode: a.status, Body: io.NopCloser(strings.NewReader(a.body)), Header: h, Request: r}, nil
}

// discoveryCall reads the issuer's discovery document through fetch during one
// redirect Sign-in call to operatorHost, then fetches each of then and answers
// what each fetch was told.
func discoveryCall(t *testing.T, rt http.RoundTripper, operatorHost, issuer string, then ...string) (pluginapi.FetchResponse, []pluginapi.FetchResponse) {
	t.Helper()
	p := newPlugin("oidc", "", Options{
		Logf:             func(string, ...any) {},
		HTTPClient:       &http.Client{Transport: rt},
		FailureThreshold: 1000,
	}.withDefaults())
	p.resolveLimits()
	defer p.close(context.Background())
	h := &hostFuncs{p: p}
	if err := p.beginCall(addrOf("https://"+operatorHost), false); err != nil {
		t.Fatal(err)
	}
	defer p.endCall()
	discovery := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	defer p.beginDiscovery(discovery)()
	read := h.fetch(context.Background(), pluginapi.FetchRequest{URL: discovery})
	var out []pluginapi.FetchResponse
	for _, u := range then {
		out = append(out, h.fetch(context.Background(), pluginapi.FetchRequest{URL: u}))
	}
	return read, out
}

func discoveryDoc(issuer, token, userinfo string) string {
	raw, _ := json.Marshal(map[string]string{
		"issuer":            issuer,
		"token_endpoint":    token,
		"userinfo_endpoint": userinfo,
	})
	return string(raw)
}

// TestARedirectedDiscoveryFetchAdmitsNothing: a discovery fetch that was
// redirected — to another host serving a valid document, or even back to the
// discovery URL itself — admits nothing, because the exact URL did not answer.
func TestARedirectedDiscoveryFetchAdmitsNothing(t *testing.T) {
	resolveAs(t, "203.0.113.7")
	valid := discoveryDoc("https://idp.example.test", "https://tokens.example.test/token", "https://people.example.test/userinfo")
	rt := routeTransport{
		"idp.example.test/.well-known/openid-configuration": {status: http.StatusFound, location: "https://198.51.100.9/doc"},
		"198.51.100.9/doc":             {status: http.StatusOK, body: valid},
		"tokens.example.test/token":    {status: http.StatusOK, body: "{}"},
		"people.example.test/userinfo": {status: http.StatusOK, body: "{}"},
	}
	read, then := discoveryCall(t, rt, "idp.example.test", "https://idp.example.test",
		"https://tokens.example.test/token", "https://people.example.test/userinfo")
	if read.Status != http.StatusOK || read.Refused != "" {
		t.Fatalf("the redirected discovery fetch = %+v, want the other host's 200", read)
	}
	for i, resp := range then {
		if resp.Refused != refusedAllowlist {
			t.Errorf("endpoint %d after a redirected discovery = %+v, want refused %q", i, resp, refusedAllowlist)
		}
	}

	// Back to the very same URL: redirected all the same.
	const issuer = "https://198.51.100.20"
	hops := 0
	self := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/.well-known/openid-configuration" && hops == 0 {
			hops++
			h := http.Header{}
			h.Set("Location", issuer+"/.well-known/openid-configuration")
			return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader("")), Header: h, Request: r}, nil
		}
		return rt.RoundTrip(r)
	})
	rt["198.51.100.20/.well-known/openid-configuration"] = routeAnswer{status: http.StatusOK, body: discoveryDoc(issuer, "https://tokens.example.test/token", "")}
	read, then = discoveryCall(t, self, "198.51.100.20", issuer, "https://tokens.example.test/token")
	if read.Status != http.StatusOK || hops != 1 {
		t.Fatalf("the self-redirected discovery fetch = %+v after %d hops, want 200 after 1", read, hops)
	}
	if then[0].Refused != refusedAllowlist {
		t.Errorf("token endpoint after a self-redirected discovery = %+v, want refused %q", then[0], refusedAllowlist)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestANonOKDiscoveryDocumentAdmitsNothing: a valid document answered with any
// status but 200 admits nothing.
func TestANonOKDiscoveryDocumentAdmitsNothing(t *testing.T) {
	resolveAs(t, "203.0.113.7")
	for _, status := range []int{http.StatusNonAuthoritativeInfo, http.StatusNotFound, http.StatusInternalServerError} {
		rt := routeTransport{
			"idp.example.test/.well-known/openid-configuration": {status: status, body: discoveryDoc("https://idp.example.test", "https://tokens.example.test/token", "")},
			"tokens.example.test/token":                         {status: http.StatusOK, body: "{}"},
		}
		read, then := discoveryCall(t, rt, "idp.example.test", "https://idp.example.test", "https://tokens.example.test/token")
		if read.Status != status {
			t.Fatalf("discovery fetch = %+v, want status %d", read, status)
		}
		if then[0].Refused != refusedAllowlist {
			t.Errorf("token endpoint after a %d discovery = %+v, want refused %q", status, then[0], refusedAllowlist)
		}
	}
}

// TestADiscoveryNamedEndpointIsAdmittedByItsExactOrigin: an endpoint the
// document names is reachable at exactly its scheme, host and port, and only
// over https — not the same host on another port or scheme.
func TestADiscoveryNamedEndpointIsAdmittedByItsExactOrigin(t *testing.T) {
	resolveAs(t, "203.0.113.7")
	rt := routeTransport{
		"idp.example.test/.well-known/openid-configuration": {status: http.StatusOK, body: discoveryDoc("https://idp.example.test",
			"https://tokens.example.test/token", "http://people.example.test/userinfo")},
		"tokens.example.test/token":    {status: http.StatusOK, body: "{}"},
		"tokens.example.test/other":    {status: http.StatusOK, body: "{}"},
		"people.example.test/userinfo": {status: http.StatusOK, body: "{}"},
	}
	reached := []string{
		"https://tokens.example.test/token",
		"https://tokens.example.test:443/other",
	}
	refused := []string{
		"https://tokens.example.test:8443/token",
		"http://tokens.example.test/token",
		"http://people.example.test/userinfo",
		"https://people.example.test/userinfo",
	}
	_, then := discoveryCall(t, rt, "idp.example.test", "https://idp.example.test", append(reached, refused...)...)
	for i, u := range reached {
		if then[i].Refused != "" || then[i].Status != http.StatusOK {
			t.Errorf("fetch %s = %+v, want 200", u, then[i])
		}
	}
	for i, u := range refused {
		if resp := then[len(reached)+i]; resp.Refused != refusedAllowlist {
			t.Errorf("fetch %s = %+v, want refused %q", u, resp, refusedAllowlist)
		}
	}
}

// TestADiscoveryNamedEndpointOffTheIssuersHostIsAddressChecked: an endpoint on
// another host than the issuer's gets the private-address check a manifest host
// gets — the metadata address and private space are refused however the
// document names them — while a public token host (Google's shape) is reached
// and the issuer's own host keeps the issuer's exemption.
func TestADiscoveryNamedEndpointOffTheIssuersHostIsAddressChecked(t *testing.T) {
	resolveAs(t, "142.250.72.10")
	const issuer = "https://10.0.0.5"
	rt := routeTransport{
		"10.0.0.5/.well-known/openid-configuration": {status: http.StatusOK, body: discoveryDoc(issuer,
			"https://169.254.169.254:8080/x", "https://10.1.2.3/userinfo")},
		"169.254.169.254:8080/x":            {status: http.StatusOK, body: "{}"},
		"169.254.169.254/latest/meta-data/": {status: http.StatusOK, body: "{}"},
		"10.1.2.3/userinfo":                 {status: http.StatusOK, body: "{}"},
	}
	read, then := discoveryCall(t, rt, "10.0.0.5", issuer,
		"https://169.254.169.254:8080/x", "https://10.1.2.3/userinfo", "http://169.254.169.254/latest/meta-data/")
	if read.Status != http.StatusOK || read.Refused != "" {
		t.Fatalf("the issuer's own discovery fetch = %+v, want 200", read)
	}
	for i, u := range []string{"https://169.254.169.254:8080/x", "https://10.1.2.3/userinfo"} {
		if then[i].Refused != refusedPrivate {
			t.Errorf("fetch %s = %+v, want refused %q", u, then[i], refusedPrivate)
		}
	}
	if then[2].Refused == "" {
		t.Errorf("fetch of the metadata service = %+v, want refused", then[2])
	}

	rt["10.0.0.5/.well-known/openid-configuration"] = routeAnswer{status: http.StatusOK, body: discoveryDoc(issuer,
		"https://oauth2.googleapis.com/token", "https://10.0.0.5/userinfo")}
	rt["oauth2.googleapis.com/token"] = routeAnswer{status: http.StatusOK, body: "{}"}
	rt["10.0.0.5/userinfo"] = routeAnswer{status: http.StatusOK, body: "{}"}
	_, then = discoveryCall(t, rt, "10.0.0.5", issuer, "https://oauth2.googleapis.com/token", "https://10.0.0.5/userinfo")
	for i, u := range []string{"https://oauth2.googleapis.com/token", "https://10.0.0.5/userinfo"} {
		if then[i].Refused != "" || then[i].Status != http.StatusOK {
			t.Errorf("fetch %s = %+v, want 200", u, then[i])
		}
	}
}

// TestADiscoveryNamedEndpointOnTheIssuersHostKeepsItsExemption: the issuer is
// the operator's, on a private address and a port of its own. Its discovery
// document names a token endpoint on the same host on ANOTHER port, and that
// exact origin keeps the issuer's exemption — at the lookup and at the dial —
// while the same host on a port nobody named is the manifest's to allow, and it
// allows nothing.
func TestADiscoveryNamedEndpointOnTheIssuersHostKeepsItsExemption(t *testing.T) {
	parallel(t)
	var tokenHits, otherHits int
	tokens := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenHits++
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(tokens.Close)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits++
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(other.Close)
	var issuer string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(discoveryDoc(issuer, tokens.URL+"/token", tokens.URL+"/userinfo")))
	}))
	t.Cleanup(idp.Close)
	issuer = idp.URL

	read, then := discoveryCall(t, idp.Client().Transport, strings.TrimPrefix(issuer, "https://"), issuer,
		tokens.URL+"/token", other.URL+"/token")
	if read.Status != http.StatusOK || read.Refused != "" {
		t.Fatalf("the issuer's own discovery fetch = %+v, want 200", read)
	}
	if then[0].Refused != "" || then[0].Status != http.StatusOK || tokenHits != 1 {
		t.Errorf("the named endpoint on the issuer's host = %+v with %d requests, want 200 and 1", then[0], tokenHits)
	}
	if then[1].Refused != refusedAllowlist || otherHits != 0 {
		t.Errorf("the issuer's host on an unnamed port = %+v with %d requests, want refused %q and 0",
			then[1], otherHits, refusedAllowlist)
	}
}
