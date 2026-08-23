package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/goozakdev/obelo-server/internal/config"
)

// acmeTestDomain is the name every test here obtains a certificate for. A
// `.test` name because it is reserved (RFC 6761) and can never resolve to
// anything real, and because autocert refuses a single-label name outright.
const acmeTestDomain = "obelo.test"

// TestACMEIssuesCertificateOverTLSALPN01 is the criterion this whole issue turns
// on: a certificate is obtained from an ACME directory and served, with NOTHING
// listening on or connecting to port 80 anywhere in the flow.
//
// It runs against the local CA at the bottom of this file, never against Let's
// Encrypt — not even staging. The production directory allows five failed
// authorizations per account per hostname per hour, so a test pointed at it
// would be unrunnable twice in a row and would fail for reasons that have
// nothing to do with this code. The local CA speaks enough of RFC 8555 to make
// autocert do the real thing: register an account, open an order, answer the
// challenge, finalize a CSR, and fetch the chain.
//
// The teeth are in the challenge: the CA offers BOTH tls-alpn-01 and http-01,
// dials this server back over TLS with the acme-tls/1 protocol, and checks that
// the certificate it is handed carries the id-pe-acmeIdentifier extension with
// the SHA-256 of the real key authorization. A server that dropped acme-tls/1
// from NextProtos, or that answered with an ordinary certificate, fails here.
func TestACMEIssuesCertificateOverTLSALPN01(t *testing.T) {
	ca := newTestCA(t)
	cfg := acmeConfigForTest(t, ca.URL())

	servers, err := newServers(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), func() {})
	if err != nil {
		t.Fatalf("newServers in acme mode: %v — obtaining a certificate is lazy, so nothing here may fail at boot", err)
	}
	if len(servers) != 2 {
		t.Fatalf("newServers built %d listeners in acme mode, want 2 (plain HTTP + HTTPS)", len(servers))
	}
	addrs := startServers(t, servers)
	httpAddr, httpsAddr := addrs[0], addrs[1]

	// The CA validates by connecting to the domain; the domain does not resolve,
	// so tell the CA where this server actually is. This stands in for the
	// household's DNS record plus the single forwarded port.
	ca.Resolve(acmeTestDomain, httpsAddr)

	resp, err := acmeHTTPSClient(ca.Roots(), httpsAddr).Get("https://" + acmeTestDomain + "/")
	if err != nil {
		t.Fatalf("HTTPS GET with an ACME-obtained certificate: %v; CA errors: %v", err, ca.Errors())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("HTTPS response = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}

	// The certificate verified against the CA's root — the client pool contains
	// nothing else and InsecureSkipVerify is off — so it really was issued by the
	// directory we pointed at, not fetched from anywhere else.
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate on the response")
	}
	if got := resp.TLS.PeerCertificates[0].Issuer.CommonName; !strings.Contains(got, "Obelo Test CA") {
		t.Errorf("leaf issuer = %q, want the local test CA — issuance did not go to OBELO_ACME_DIRECTORY", got)
	}

	// HTTP/2 still negotiates. NextProtos has to be set by hand in this mode (for
	// acme-tls/1), which is exactly how "h2" gets dropped; nothing about a working
	// HTTP/1.1 connection would ever reveal that.
	if resp.TLS.NegotiatedProtocol != "h2" {
		t.Errorf("ALPN = %q, want %q — setting NextProtos for acme-tls/1 must not drop h2", resp.TLS.NegotiatedProtocol, "h2")
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("resp.Proto = %q, want HTTP/2.0", resp.Proto)
	}

	// HSTS stays absent here as much as anywhere (ADR-0041). A certificate that
	// arrived automatically is no more reason to lock a household's browser onto
	// HTTPS than one that arrived by hand.
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HTTPS response carries Strict-Transport-Security = %q, want it ABSENT", got)
	}

	// The challenge that was actually used, and the one that was not.
	if got := ca.ValidatedTypes(); len(got) != 1 || got[0] != "tls-alpn-01" {
		t.Errorf("challenges validated = %v, want exactly [tls-alpn-01]", got)
	}
	if n := ca.HTTP01Attempts(); n != 0 {
		t.Errorf("the CA attempted http-01 validation %d times — TLS-ALPN-01 exists so no second port-forward is needed (ADR-0041), and Manager.HTTPHandler must never be wired in", n)
	}
	if errs := ca.Errors(); len(errs) != 0 {
		t.Errorf("the ACME CA reported protocol errors: %v", errs)
	}

	// And the plain-HTTP listener is untouched throughout, which is the point of
	// HTTPS being additive.
	plainResp, err := http.Get("http://" + httpAddr + "/")
	if err != nil {
		t.Fatalf("plain-HTTP GET: %v", err)
	}
	defer plainResp.Body.Close()
	if plainResp.StatusCode != http.StatusOK {
		t.Errorf("plain-HTTP GET = %d, want 200", plainResp.StatusCode)
	}
	if got := plainResp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("plain-HTTP response carries Strict-Transport-Security = %q, want it ABSENT", got)
	}
}

// TestACMECacheSurvivesRestartWithoutReissuing: the cache directory holds the
// account key and the issued private keys, must be 0700, and must make a second
// boot free. A server that re-issues on every restart is not a performance
// problem — it is a server that runs out of certificates, because the CA's
// duplicate-certificate limit is measured in a handful per week and a household
// box restarts far more often than that.
//
// The second boot runs with the CA SHUT DOWN. Nothing short of that proves the
// certificate came from disk: with the CA reachable, a re-issue looks identical
// to a cache hit from the client's side.
func TestACMECacheSurvivesRestartWithoutReissuing(t *testing.T) {
	ca := newTestCA(t)
	cfg := acmeConfigForTest(t, ca.URL())

	firstAddr := startACMEServer(t, cfg, ca)
	firstLeaf := servedLeaf(t, firstAddr, acmeTestDomain, ca.Roots())

	cacheDir := cfg.ACMECacheDir()
	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatalf("the ACME cache directory %s was never created: %v", cacheDir, err)
	}
	// Checked as "nothing outside the owner", not as "exactly 0700", because the
	// process umask can only ever clear bits — so an exact comparison would fail
	// on a box with an unusual umask for a directory that is, if anything, MORE
	// private than asked for. The property that matters is the one asserted.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("ACME cache directory mode = %04o, want 0700 — nothing beyond the owner may read it, because it holds the ACME account key and every issued private key", perm)
	}
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) == 0 {
		t.Fatalf("the ACME cache directory is empty (%v) — nothing was persisted, so every restart would re-issue", err)
	}

	// The world goes away. Anything the second boot needs, it has on disk.
	ca.Close()

	secondAddr := startACMEServer(t, cfg, ca)
	secondLeaf := servedLeaf(t, secondAddr, acmeTestDomain, ca.Roots())

	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) != 0 {
		t.Errorf("second boot served serial %v, first served %v — the certificate was re-issued rather than read from %s", secondLeaf.SerialNumber, firstLeaf.SerialNumber, cacheDir)
	}
	if n := ca.OrdersCreated(); n != 1 {
		t.Errorf("the CA saw %d orders across two boots, want exactly 1", n)
	}
}

// TestACMEFailureDoesNotStopTheServer: the boot posture, and the deliberate
// opposite of files mode. An ACME directory that cannot be reached is a third
// party being unavailable, not a typo the operator made, and ADR-0041 is
// explicit that taking the household's media server down over it is a bad trade.
// So: the server boots, plain HTTP serves throughout, HTTPS simply has no
// certificate, and the log says so in terms someone can act on.
func TestACMEFailureDoesNotStopTheServer(t *testing.T) {
	logs := captureLog(t)

	cfg := acmeConfigForTest(t, "http://"+unreachableAddr(t)+"/directory")

	servers, err := newServers(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), func() {})
	if err != nil {
		t.Fatalf("newServers = %v with an unreachable ACME directory; want no error — a CA that cannot be reached must not stop the boot (ADR-0041)", err)
	}
	if len(servers) != 2 {
		t.Fatalf("newServers built %d listeners, want 2 — the HTTPS listener still binds, it just has no certificate yet", len(servers))
	}
	addrs := startServers(t, servers)
	httpAddr, httpsAddr := addrs[0], addrs[1]

	// Plain HTTP works before the doomed handshake...
	assertPlainHTTPServes(t, httpAddr, "before the failed handshake")

	// ...the handshake fails, because there is no certificate to present...
	conn, err := tls.Dial("tcp", httpsAddr, &tls.Config{
		ServerName:         acmeTestDomain,
		InsecureSkipVerify: true, //nolint:gosec // asserting the handshake fails for lack of a certificate, not for lack of trust
		MinVersion:         tls.VersionTLS12,
	})
	if err == nil {
		conn.Close()
		t.Error("the TLS handshake succeeded with an unreachable CA; want it to fail for want of a certificate")
	}

	// ...and plain HTTP is still there afterwards, which is the entire point.
	assertPlainHTTPServes(t, httpAddr, "after the failed handshake")

	got := logs.String()
	for _, want := range []string{"STILL RUNNING", acmeTestDomain, cfg.ListenAddr} {
		if !strings.Contains(got, want) {
			t.Errorf("the certificate failure was not logged with %q; log was:\n%s", want, got)
		}
	}
}

// TestACMEComplaintsAreRateLimited: an internet-facing port is scanned
// continuously, and every unknown SNI name is a host-policy rejection. One log
// line each would bury the one failure that is about the operator's own domain,
// and on a small box it is a way to fill a disk with someone else's port scan.
func TestACMEComplaintsAreRateLimited(t *testing.T) {
	logs := captureLog(t)

	certs := &acmeCertificates{
		plainAddr:     ":8080",
		tlsAddr:       ":8443",
		directory:     "https://ca.invalid/directory",
		complainEvery: time.Hour,
		announced:     map[string]bool{},
	}
	for i := 0; i < 25; i++ {
		certs.complain(fmt.Sprintf("scanner-%d.invalid", i), fmt.Errorf("host %q not configured", "scanner"))
	}
	if n := strings.Count(logs.String(), "no HTTPS certificate for"); n != 1 {
		t.Errorf("logged %d certificate failures for 25 handshakes, want 1 within the interval", n)
	}

	// The suppressed ones are counted, not silently dropped: an operator reading
	// one line must be able to tell "this happened once" from "this is happening
	// constantly".
	certs.complainEvery = 0
	certs.complain("media.example.com", fmt.Errorf("boom"))
	if !strings.Contains(logs.String(), "24 further certificate failures suppressed") {
		t.Errorf("the suppressed failures were not counted; log was:\n%s", logs.String())
	}
}

// TestNewACMEServerCarriesChallengeALPNAndHTTP2: the NextProtos list is the one
// thing in acme mode that must be set by hand, and both of its non-obvious
// entries fail silently if lost. Without acme-tls/1 the challenge can never
// complete and the CA reports an authorization failure that reads like a problem
// at Let's Encrypt; without h2 every request still works and only HLS gets
// quietly slower. The rest of the posture must match files mode exactly, which
// is why the server is built from newTLSServer.
func TestNewACMEServerCarriesChallengeALPNAndHTTP2(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.TLSMode = config.TLSModeACME
	cfg.TLSDomains = []string{acmeTestDomain}

	plain := newHTTPServer(cfg, http.NotFoundHandler())
	srv := newACMEServer(cfg, http.NotFoundHandler(), newACMECertificates(cfg))

	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil")
	}
	want := []string{"h2", "http/1.1", acme.ALPNProto}
	if got := srv.TLSConfig.NextProtos; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("NextProtos = %v, want %v — %q or the challenge protocol going missing are both silent failures", got, want, "h2")
	}
	if srv.TLSNextProto != nil {
		t.Error("TLSNextProto is set — that is what disables net/http's automatic HTTP/2 setup")
	}
	if srv.TLSConfig.GetCertificate == nil {
		t.Error("GetCertificate is nil — the certificate must come from the autocert manager")
	}
	if len(srv.TLSConfig.Certificates) != 0 {
		t.Error("TLSConfig pins Certificates; in acme mode everything comes from GetCertificate")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x) — Apple's ATS floor applies to both TLS modes", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}

	// Same timeouts as the plain listener, for the same reason files mode asserts
	// it: the two must not drift, and WriteTimeout in particular must stay zero or
	// media streaming and the SSE stream get cut off mid-response.
	if srv.ReadHeaderTimeout != plain.ReadHeaderTimeout || srv.ReadTimeout != plain.ReadTimeout || srv.IdleTimeout != plain.IdleTimeout {
		t.Error("the ACME listener's timeouts differ from the plain-HTTP listener's; it must be built from newHTTPServer")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero value cuts off media streaming and the SSE stream mid-response", srv.WriteTimeout)
	}
	if srv.Addr != cfg.TLSListenAddr {
		t.Errorf("Addr = %q, want the TLS listen address %q", srv.Addr, cfg.TLSListenAddr)
	}
}

// TestACMEHostPolicyIsNeverNil: the failure this issue warns loudest about. A
// manager with no host policy obtains a certificate for whatever name a stranger
// puts in an SNI, which spends the account's rate limit on domains this server
// has nothing to do with. The config layer refuses to boot without a domain list;
// this checks the other half — that the list actually reaches autocert, and that
// a name outside it is turned away before any CA is contacted.
func TestACMEHostPolicyIsNeverNil(t *testing.T) {
	cfg := acmeConfigForTest(t, "http://"+unreachableAddr(t)+"/directory")
	certs := newACMECertificates(cfg)

	if certs.manager.HostPolicy == nil {
		t.Fatal("HostPolicy is nil — autocert would then obtain a certificate for any SNI name a client offers")
	}
	if err := certs.manager.HostPolicy(t.Context(), "someone-elses.example.com"); err == nil {
		t.Error("the host policy accepted a name that is not in OBELO_TLS_DOMAINS")
	}
	if err := certs.manager.HostPolicy(t.Context(), acmeTestDomain); err != nil {
		t.Errorf("the host policy rejected %q, which IS in OBELO_TLS_DOMAINS: %v", acmeTestDomain, err)
	}

	// A rejected name never reaches the CA: the directory here is unreachable, so
	// a policy that let it through would fail slowly on a dial rather than
	// immediately on the policy.
	start := time.Now()
	if _, err := certs.GetCertificate(&tls.ClientHelloInfo{ServerName: "someone-elses.example.com"}); err == nil {
		t.Error("GetCertificate served a name outside the whitelist")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("rejecting an unlisted name took %v — the host policy must be consulted before the CA is contacted", elapsed)
	}
}

// --- helpers ---------------------------------------------------------------

// acmeConfigForTest is an acme-mode config with its own data directory, so the
// certificate cache of one test cannot satisfy another.
func acmeConfigForTest(t *testing.T, directoryURL string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.TLSMode = config.TLSModeACME
	cfg.TLSDomains = []string{acmeTestDomain}
	cfg.ACMEEmail = "operator@obelo.test"
	cfg.ACMEDirectoryURL = directoryURL
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test config does not validate: %v", err)
	}
	return cfg
}

// startACMEServer boots one HTTPS listener from cfg and points the CA at it,
// returning its address. Used by the restart test, which does this twice against
// the same data directory.
func startACMEServer(t *testing.T, cfg config.Config, ca *testCA) string {
	t.Helper()
	servers, err := newServers(cfg, http.NotFoundHandler(), func() {})
	if err != nil {
		t.Fatalf("newServers: %v", err)
	}
	addr := startServer(t, servers[1])
	ca.Resolve(acmeTestDomain, addr)
	return addr
}

// acmeHTTPSClient dials addr but presents serverName in the SNI, which is what a
// household client does after DNS resolves the name to the house. It trusts only
// the test CA's root, so a certificate from anywhere else fails verification.
func acmeHTTPSClient(pool *x509.CertPool, addr string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

// servedLeaf opens a fresh TLS connection and returns the leaf the server
// presented. A raw handshake, not an HTTP request, because a pooled connection
// would answer with an earlier handshake's certificate and make a re-issue look
// like a cache hit.
func servedLeaf(t *testing.T, addr, serverName string, pool *x509.CertPool) *x509.Certificate {
	t.Helper()
	dialer := &tls.Dialer{Config: &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("TLS handshake with %s for %q: %v", addr, serverName, err)
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatal("the server presented no certificate")
	}
	return certs[0]
}

// assertPlainHTTPServes checks the LAN listener is answering. Called on both
// sides of a deliberately-failing HTTPS handshake, because "HTTPS is broken but
// the house still works" is the property under test.
func assertPlainHTTPServes(t *testing.T, addr, when string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("plain-HTTP GET %s: %v", when, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("plain-HTTP GET %s = %d, want 200", when, resp.StatusCode)
	}
}

// unreachableAddr returns a loopback address nothing is listening on: a listener
// is opened to reserve a port and closed again. A refused connection rather than
// a black hole, so the test fails fast instead of waiting out a dial timeout.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// --- a local ACME CA -------------------------------------------------------
//
// testCA speaks the slice of RFC 8555 that autocert's TLS-ALPN-01 flow uses:
// directory, new-nonce, new-account, new-order, authorization, challenge,
// finalize, and certificate download. It exists so issuance can be exercised end
// to end without pointing anything at a real CA — see the note on rate limits at
// the top of this file. It is modelled on x/crypto's own acme/autocert test CA,
// which is an internal package and so cannot be imported.
//
// It is deliberately stricter than that one in the place that matters: the
// TLS-ALPN-01 verification below checks the id-pe-acmeIdentifier extension
// actually contains SHA-256 of the key authorization derived from the account
// key's JWK thumbprint. Without that check a server could answer the challenge
// with any old certificate and the test would still pass, which would leave the
// acme-tls/1 wiring unproven — the exact failure this issue calls out as looking
// like a CA problem rather than a config one.
//
// It records rather than reports its own errors (no t.Errorf inside the
// handler): autocert deactivates spent authorizations from a background
// goroutine that can outlive the test, and logging from there panics the suite.
type testCA struct {
	server   *httptest.Server
	closeOne sync.Once

	rootKey      *ecdsa.PrivateKey
	rootDER      []byte
	rootTemplate *x509.Certificate
	roots        *x509.CertPool

	mu             sync.Mutex
	accountKey     *ecdsa.PublicKey
	domainAddr     map[string]string
	authorizations []*caAuthz
	orders         []*caOrder
	certCount      int
	validatedTypes []string
	http01Attempts int
	errs           []string
}

type caChallenge struct {
	URI   string `json:"url"`
	Type  string `json:"type"`
	Token string `json:"token"`
}

type caAuthz struct {
	Status     string        `json:"status"`
	Challenges []caChallenge `json:"challenges"`

	domain string
	id     int
}

type caOrder struct {
	Status      string   `json:"status"`
	AuthzURLs   []string `json:"authorizations"`
	FinalizeURL string   `json:"finalize"`
	CertURL     string   `json:"certificate"`

	leaf []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	ca := &testCA{domainAddr: map[string]string{}}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Obelo Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the CA certificate: %v", err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the CA certificate: %v", err)
	}
	ca.rootKey, ca.rootDER, ca.rootTemplate = key, der, tmpl
	ca.roots = x509.NewCertPool()
	ca.roots.AddCert(root)

	ca.server = httptest.NewServer(http.HandlerFunc(ca.handle))
	t.Cleanup(ca.Close)
	return ca
}

func (ca *testCA) URL() string           { return ca.server.URL }
func (ca *testCA) Roots() *x509.CertPool { return ca.roots }
func (ca *testCA) Close()                { ca.closeOne.Do(ca.server.Close) }
func (ca *testCA) serverURL(f string, a ...any) string {
	return ca.server.URL + fmt.Sprintf(f, a...)
}

func (ca *testCA) Resolve(domain, addr string) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.domainAddr[domain] = addr
}

func (ca *testCA) ValidatedTypes() []string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return append([]string(nil), ca.validatedTypes...)
}

func (ca *testCA) HTTP01Attempts() int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return ca.http01Attempts
}

func (ca *testCA) OrdersCreated() int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return len(ca.orders)
}

func (ca *testCA) Errors() []string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return append([]string(nil), ca.errs...)
}

func (ca *testCA) fail(w http.ResponseWriter, code int, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	ca.mu.Lock()
	ca.errs = append(ca.errs, msg)
	ca.mu.Unlock()
	http.Error(w, msg, code)
}

func (ca *testCA) handle(w http.ResponseWriter, r *http.Request) {
	// A fixed nonce. Replay protection is the one part of RFC 8555 a test CA
	// gains nothing from implementing, and acme.Client only needs the header to
	// be present.
	w.Header().Set("Replay-Nonce", "nonce")

	switch {
	case r.URL.Path == "/":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"newNonce":   ca.serverURL("/new-nonce"),
			"newAccount": ca.serverURL("/new-account"),
			"newOrder":   ca.serverURL("/new-order"),
			"meta":       map[string]any{"termsOfService": ca.serverURL("/terms")},
		})

	case r.URL.Path == "/new-nonce":
		// The header above is the whole response.

	case r.URL.Path == "/new-account":
		ca.newAccount(w, r)

	case r.URL.Path == "/new-order":
		ca.newOrder(w, r)

	case strings.HasPrefix(r.URL.Path, "/orders/"):
		ca.mu.Lock()
		defer ca.mu.Unlock()
		o, err := ca.orderAt(strings.TrimPrefix(r.URL.Path, "/orders/"))
		if err != nil {
			ca.fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(o)

	case strings.HasPrefix(r.URL.Path, "/challenge/"):
		ca.acceptChallenge(w, r)

	case strings.HasPrefix(r.URL.Path, "/authz/"):
		ca.mu.Lock()
		defer ca.mu.Unlock()
		z, err := ca.authzAt(strings.TrimPrefix(r.URL.Path, "/authz/"))
		if err != nil {
			ca.fail(w, http.StatusNotFound, "%v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(z)

	case strings.HasPrefix(r.URL.Path, "/finalize/"):
		ca.finalize(w, r)

	case strings.HasPrefix(r.URL.Path, "/issued-cert/"):
		ca.mu.Lock()
		defer ca.mu.Unlock()
		o, err := ca.orderAt(strings.TrimPrefix(r.URL.Path, "/issued-cert/"))
		if err != nil {
			ca.fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: o.leaf})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: ca.rootDER})

	default:
		ca.fail(w, http.StatusBadRequest, "unrecognized path %s", r.URL.Path)
	}
}

// newAccount registers the account and, crucially, keeps its public key. The
// TLS-ALPN-01 key authorization is derived from that key's JWK thumbprint, so
// without it the challenge check below could not be a real check.
func (ca *testCA) newAccount(w http.ResponseWriter, r *http.Request) {
	pub, err := accountKeyFromJWS(r)
	if err != nil {
		ca.fail(w, http.StatusBadRequest, "new-account: %v", err)
		return
	}
	ca.mu.Lock()
	ca.accountKey = pub
	ca.mu.Unlock()

	w.Header().Set("Location", ca.serverURL("/accounts/1"))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte("{}"))
}

func (ca *testCA) newOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifiers []struct{ Value string }
	}
	if err := decodeJWSPayload(&req, r); err != nil {
		ca.fail(w, http.StatusBadRequest, "new-order: %v", err)
		return
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()

	o := &caOrder{Status: acme.StatusPending}
	for _, id := range req.Identifiers {
		z := ca.newAuthz(id.Value)
		o.AuthzURLs = append(o.AuthzURLs, ca.serverURL("/authz/%d", z.id))
	}
	id := len(ca.orders)
	o.FinalizeURL = ca.serverURL("/finalize/%d", id)
	ca.orders = append(ca.orders, o)

	w.Header().Set("Location", ca.serverURL("/orders/%d", id))
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(o)
}

// newAuthz offers BOTH challenge types, exactly as Let's Encrypt does. That is
// what makes "no port 80 anywhere" an assertion rather than an assumption: the
// client is free to pick http-01 and must not.
func (ca *testCA) newAuthz(domain string) *caAuthz {
	id := len(ca.authorizations)
	z := &caAuthz{Status: acme.StatusPending, domain: domain, id: id}
	for _, typ := range []string{"tls-alpn-01", "http-01"} {
		z.Challenges = append(z.Challenges, caChallenge{
			Type:  typ,
			URI:   ca.serverURL("/challenge/%s/%d", typ, id),
			Token: fmt.Sprintf("token-%s-%d", typ, id),
		})
	}
	ca.authorizations = append(ca.authorizations, z)
	return z
}

func (ca *testCA) acceptChallenge(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	typ, id := parts[len(parts)-2], parts[len(parts)-1]

	ca.mu.Lock()
	z, err := ca.authzAt(id)
	accountKey := ca.accountKey
	addr := ""
	if z != nil {
		addr = ca.domainAddr[z.domain]
	}
	if typ == "http-01" {
		ca.http01Attempts++
	}
	ca.mu.Unlock()

	if err != nil {
		ca.fail(w, http.StatusBadRequest, "challenge accept: %v", err)
		return
	}

	// The dial happens with the lock released: it comes back into this same
	// process, to a TLS listener whose handshake will ask the manager for a token
	// certificate. Holding ca.mu across it would deadlock the moment the CA needed
	// its own state again.
	verr := ca.verifyALPN(z, addr, accountKey, typ)

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if verr != nil {
		z.Status = acme.StatusInvalid
		ca.errs = append(ca.errs, fmt.Sprintf("challenge %s for %s: %v", typ, z.domain, verr))
	} else {
		z.Status = acme.StatusValid
		ca.validatedTypes = append(ca.validatedTypes, typ)
	}
	ca.updatePendingOrders()
	_, _ = w.Write([]byte("{}"))
}

// verifyALPN performs the TLS-ALPN-01 check of RFC 8737: connect to the domain
// over TLS offering only acme-tls/1, and require the certificate presented to
// carry a critical id-pe-acmeIdentifier extension holding SHA-256 of the key
// authorization.
func (ca *testCA) verifyALPN(z *caAuthz, addr string, accountKey *ecdsa.PublicKey, typ string) error {
	if typ != "tls-alpn-01" {
		return fmt.Errorf("this CA only validates tls-alpn-01 in tests; %q was attempted, which would need port 80", typ)
	}
	if addr == "" {
		return fmt.Errorf("no address registered for %q", z.domain)
	}
	if accountKey == nil {
		return fmt.Errorf("no account was registered before the challenge")
	}
	var token string
	for _, c := range z.Challenges {
		if c.Type == typ {
			token = c.Token
		}
	}

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName:         z.domain,
		InsecureSkipVerify: true, //nolint:gosec // the challenge certificate is self-signed by design (RFC 8737 §3)
		NextProtos:         []string{acme.ALPNProto},
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return fmt.Errorf("dialing %s: %w", addr, err)
	}
	defer conn.Close()

	if got := conn.ConnectionState().NegotiatedProtocol; got != acme.ALPNProto {
		return fmt.Errorf("negotiated protocol %q, want %q — the server does not offer the challenge protocol in NextProtos", got, acme.ALPNProto)
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) != 1 {
		return fmt.Errorf("got %d certificates, want 1", len(certs))
	}
	leaf := certs[0]
	if err := leaf.VerifyHostname(z.domain); err != nil {
		return fmt.Errorf("challenge certificate is not valid for %q: %w", z.domain, err)
	}

	thumb, err := acme.JWKThumbprint(accountKey)
	if err != nil {
		return fmt.Errorf("account key thumbprint: %w", err)
	}
	want := sha256.Sum256([]byte(token + "." + thumb))

	// RFC 8737 §3: OID 1.3.6.1.5.5.7.1.31, critical, an ASN.1 OCTET STRING.
	oid := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oid) {
			continue
		}
		if !ext.Critical {
			return fmt.Errorf("the id-pe-acmeIdentifier extension is not marked critical")
		}
		var got []byte
		if _, err := asn1.Unmarshal(ext.Value, &got); err != nil {
			return fmt.Errorf("decoding the id-pe-acmeIdentifier extension: %w", err)
		}
		if string(got) != string(want[:]) {
			return fmt.Errorf("the challenge response does not match the key authorization")
		}
		return nil
	}
	return fmt.Errorf("no id-pe-acmeIdentifier extension in the challenge certificate")
}

func (ca *testCA) finalize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CSR string `json:"csr"`
	}
	if err := decodeJWSPayload(&req, r); err != nil {
		ca.fail(w, http.StatusBadRequest, "finalize: %v", err)
		return
	}
	der, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		ca.fail(w, http.StatusBadRequest, "finalize: decoding the CSR: %v", err)
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		ca.fail(w, http.StatusBadRequest, "finalize: parsing the CSR: %v", err)
		return
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	id := strings.TrimPrefix(r.URL.Path, "/finalize/")
	o, err := ca.orderAt(id)
	if err != nil {
		ca.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if o.Status != acme.StatusReady {
		ca.fail(w, http.StatusForbidden, "finalize: order status is %q, want ready", o.Status)
		return
	}

	ca.certCount++
	names := csr.DNSNames
	if len(names) == 0 {
		names = []string{csr.Subject.CommonName}
	}
	leaf := &x509.Certificate{
		SerialNumber:          big.NewInt(int64(1000 + ca.certCount)),
		Subject:               pkix.Name{CommonName: names[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              names,
		BasicConstraintsValid: true,
	}
	issued, err := x509.CreateCertificate(rand.Reader, leaf, ca.rootTemplate, csr.PublicKey, ca.rootKey)
	if err != nil {
		ca.fail(w, http.StatusInternalServerError, "finalize: issuing: %v", err)
		return
	}
	o.leaf = issued
	o.CertURL = ca.serverURL("/issued-cert/%s", id)
	o.Status = acme.StatusValid
	_ = json.NewEncoder(w).Encode(o)
}

// updatePendingOrders moves an order to "ready" once all its authorizations are
// valid, per RFC 8555 §7.1.6. Caller holds ca.mu.
func (ca *testCA) updatePendingOrders() {
	for i, o := range ca.orders {
		if o.Status != acme.StatusPending {
			continue
		}
		valid, invalid := 0, 0
		for _, u := range o.AuthzURLs {
			z, err := ca.authzAt(u[strings.LastIndex(u, "/")+1:])
			if err != nil {
				continue
			}
			switch z.Status {
			case acme.StatusValid:
				valid++
			case acme.StatusInvalid:
				invalid++
			}
		}
		switch {
		case invalid > 0:
			o.Status = acme.StatusInvalid
		case valid == len(o.AuthzURLs):
			o.Status = acme.StatusReady
			o.FinalizeURL = ca.serverURL("/finalize/%d", i)
		}
	}
}

// orderAt and authzAt look up by index. Caller holds ca.mu.
func (ca *testCA) orderAt(id string) (*caOrder, error) {
	i, err := strconv.Atoi(id)
	if err != nil || i < 0 || i >= len(ca.orders) {
		return nil, fmt.Errorf("no such order %q", id)
	}
	ca.updatePendingOrders()
	return ca.orders[i], nil
}

func (ca *testCA) authzAt(id string) (*caAuthz, error) {
	i, err := strconv.Atoi(id)
	if err != nil || i < 0 || i >= len(ca.authorizations) {
		return nil, fmt.Errorf("no such authorization %q", id)
	}
	return ca.authorizations[i], nil
}

// decodeJWSPayload unwraps the flattened JWS the ACME client sends. The
// signature is not verified: this CA proves the SERVER's behaviour, and a
// signature check would only prove x/crypto signs its own requests correctly.
// A POST-as-GET carries an empty payload, which is not an error.
func decodeJWSPayload(v any, r *http.Request) error {
	var jws struct{ Payload string }
	if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
		return fmt.Errorf("decoding the JWS: %w", err)
	}
	if jws.Payload == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return fmt.Errorf("decoding the JWS payload: %w", err)
	}
	return json.Unmarshal(payload, v)
}

// accountKeyFromJWS rebuilds the account's public key from the "jwk" member of
// the protected header, which RFC 8555 requires on newAccount. autocert always
// generates an ECDSA P-256 account key.
func accountKeyFromJWS(r *http.Request) (*ecdsa.PublicKey, error) {
	var jws struct{ Protected string }
	if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
		return nil, fmt.Errorf("decoding the JWS: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(jws.Protected)
	if err != nil {
		return nil, fmt.Errorf("decoding the protected header: %w", err)
	}
	var hdr struct {
		JWK struct {
			Crv string `json:"crv"`
			Kty string `json:"kty"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"jwk"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil, fmt.Errorf("parsing the protected header: %w", err)
	}
	if hdr.JWK.Kty != "EC" || hdr.JWK.Crv != "P-256" {
		return nil, fmt.Errorf("account key is %q/%q, want EC/P-256", hdr.JWK.Kty, hdr.JWK.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(hdr.JWK.X)
	if err != nil {
		return nil, fmt.Errorf("decoding the account key x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(hdr.JWK.Y)
	if err != nil {
		return nil, fmt.Errorf("decoding the account key y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}
