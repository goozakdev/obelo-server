package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/config"
	"github.com/marioquake/obelo-server/internal/testharness"
)

// TestNewServersPlainHTTPOnlyByDefault: with TLS unset the command builds exactly
// what it has always built — one plain-HTTP listener, no TLS config, and no
// certificate machinery. This is the "changes nothing for everyone who did not
// ask for it" assertion, and it is the one that would fail first if the TLS path
// ever stopped being opt-in.
func TestNewServersPlainHTTPOnlyByDefault(t *testing.T) {
	cfg := config.Defaults()
	cfg.ListenAddr = ":8080"

	servers, err := newServers(cfg, http.NotFoundHandler(), func() {})
	if err != nil {
		t.Fatalf("newServers with TLS off: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("newServers built %d listeners with TLS off, want exactly 1", len(servers))
	}
	if servers[0].TLSConfig != nil {
		t.Error("the plain-HTTP listener carries a TLSConfig; want none")
	}
	if servers[0].Addr != ":8080" {
		t.Errorf("Addr = %q, want the configured ListenAddr", servers[0].Addr)
	}

	// The zero-value mode a hand-built Config carries means off too, and must not
	// send newServers looking for certificate files nobody configured.
	cfg.TLSMode = ""
	if servers, err = newServers(cfg, http.NotFoundHandler(), func() {}); err != nil || len(servers) != 1 {
		t.Fatalf("newServers with the zero-value TLS mode = %d listeners, %v; want 1, nil", len(servers), err)
	}
}

// TestNewTLSServerMirrorsHTTPPosture: the HTTPS listener must carry the same
// connection timeouts as the plain-HTTP one — it is built from newHTTPServer for
// exactly that reason — plus the TLS decisions of ADR-0041. Each assertion below
// guards a failure that is silent in production.
func TestNewTLSServerMirrorsHTTPPosture(t *testing.T) {
	cfg := config.Defaults()
	cfg.ListenAddr = ":8080"
	cfg.TLSListenAddr = ":8443"

	plain := newHTTPServer(cfg, http.NotFoundHandler())
	srv := newTLSServer(cfg, http.NotFoundHandler(), func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, nil
	})

	if srv.Addr != ":8443" {
		t.Errorf("Addr = %q, want the TLS listen address %q — the two listeners cannot share a port", srv.Addr, ":8443")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil — both listeners serve the same handler (ADR-0041)")
	}

	// Same timeouts, or the two listeners drift and a request's treatment starts
	// depending on which port it arrived on.
	if srv.ReadHeaderTimeout != plain.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want the HTTP listener's %v", srv.ReadHeaderTimeout, plain.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != plain.ReadTimeout {
		t.Errorf("ReadTimeout = %v, want the HTTP listener's %v", srv.ReadTimeout, plain.ReadTimeout)
	}
	if srv.IdleTimeout != plain.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want the HTTP listener's %v", srv.IdleTimeout, plain.IdleTimeout)
	}

	// Pinned here for the same reason TestNewHTTPServerTimeouts pins it on the
	// plain listener: WriteTimeout is an absolute deadline on the whole response,
	// so any non-zero value truncates HLS/progressive media, the direct-file
	// download, and the never-ending SSE stream — and TLS does not make that
	// tradeoff any different. See the comment block in newHTTPServer.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero value cuts off media streaming and the SSE stream mid-response", srv.WriteTimeout)
	}

	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil — newTLSServer must configure TLS")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x) — Apple's ATS requires 1.2+, and a lower floor makes Apple clients fail in a way that looks like a server bug", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}
	if srv.TLSConfig.GetCertificate == nil {
		t.Error("GetCertificate is nil — the certificate must come from the reloader on every handshake, not from a one-shot file read")
	}
	if len(srv.TLSConfig.Certificates) != 0 {
		t.Error("TLSConfig pins Certificates; the certificate must come from GetCertificate so a renewal is picked up without a restart")
	}

	// NextProtos must stay EMPTY. http.Server.ServeTLS appends "h2" itself when
	// TLSNextProto is nil; setting this here without "h2" turns HTTP/2 off
	// silently — everything still works over HTTP/1.1, and the only symptom is
	// HLS segment fetches queueing against the browser's per-origin limit months
	// later. TestTLSListenerNegotiatesHTTP2 checks the other end of this.
	if len(srv.TLSConfig.NextProtos) != 0 {
		t.Errorf("NextProtos = %v, want empty — setting it by hand silently disables the HTTP/2 that ServeTLS negotiates for us", srv.TLSConfig.NextProtos)
	}
	if srv.TLSNextProto != nil {
		t.Error("TLSNextProto is set — that is what disables net/http's automatic HTTP/2 setup")
	}
}

// TestTLSListenerNegotiatesHTTP2: a real handshake against a real listener ends
// up on HTTP/2 over TLS 1.2+. HTTP/2 is the thing this listener gains for free
// and that HLS benefits from most (ADR-0041), and nothing about a working
// HTTP/1.1 connection would ever reveal its absence.
func TestTLSListenerNegotiatesHTTP2(t *testing.T) {
	certFile, keyFile, pool := writeKeyPairForTest(t, t.TempDir(), "obelo.test")
	srv := tlsServerForTest(t, certFile, keyFile, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	addr := startServer(t, srv)

	resp, err := httpsClient(pool).Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.TLS == nil {
		t.Fatal("no TLS connection state on the response")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version = %#x, want 1.2 or better", resp.TLS.Version)
	}
	if resp.TLS.NegotiatedProtocol != "h2" {
		t.Errorf("ALPN = %q, want %q — check that nothing set TLSConfig.NextProtos", resp.TLS.NegotiatedProtocol, "h2")
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("resp.Proto = %q, want HTTP/2.0", resp.Proto)
	}
}

// TestCertificateReloadedWithoutRestart: the acceptance criterion this whole
// reloader exists for. A renewal rewrites the PEM files in place; the next
// handshake must present the NEW chain with nobody restarting anything. Get this
// wrong — e.g. by using ListenAndServeTLS(certFile, keyFile) — and everything
// looks perfect until the old certificate's expiry date, months after the last
// change anyone made to the box.
func TestCertificateReloadedWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool := writeKeyPairForTest(t, dir, "before-renewal.obelo.test")
	srv := tlsServerForTest(t, certFile, keyFile, http.NotFoundHandler())
	addr := startServer(t, srv)

	if got := servedCommonName(t, addr, pool); got != "before-renewal.obelo.test" {
		t.Fatalf("served certificate CN = %q, want the one written at boot", got)
	}

	// The renewal: same paths, a different certificate, and the pool now trusts
	// both so a failure reads as "served the wrong certificate" rather than a
	// verification error.
	renewPool := writeKeyPairInto(t, dir, "after-renewal.obelo.test", pool)

	if got := servedCommonName(t, addr, renewPool); got != "after-renewal.obelo.test" {
		t.Errorf("served certificate CN = %q after the files were replaced, want the renewed one — the listener is still serving the certificate it read at boot", got)
	}
}

// TestCorruptCertificateKeepsPreviousOne: a half-written file during a renewal
// must not take the listener down. The previous chain is still valid — which is
// exactly why the renewal was routine rather than urgent — so the right answer is
// to keep serving it and complain loudly, and to retry rather than accept the
// broken content as the new state of the world.
func TestCorruptCertificateKeepsPreviousOne(t *testing.T) {
	logs := captureLog(t)

	dir := t.TempDir()
	certFile, keyFile, pool := writeKeyPairForTest(t, dir, "good.obelo.test")
	srv := tlsServerForTest(t, certFile, keyFile, http.NotFoundHandler())
	addr := startServer(t, srv)

	if got := servedCommonName(t, addr, pool); got != "good.obelo.test" {
		t.Fatalf("served certificate CN = %q, want the good one", got)
	}

	// Half a PEM file, as an interrupted or non-atomic renewal leaves behind.
	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnot a certif"), 0o644); err != nil {
		t.Fatalf("writing the truncated certificate: %v", err)
	}
	touch(t, certFile)

	if got := servedCommonName(t, addr, pool); got != "good.obelo.test" {
		t.Errorf("served certificate CN = %q after a corrupt write, want the previous good certificate still serving", got)
	}
	if !strings.Contains(logs.String(), "did not load") {
		t.Errorf("no warning logged for the failed reload; log was:\n%s", logs.String())
	}

	// And it self-heals: the failed load must NOT have recorded the broken file's
	// stamp, so the next check re-reads rather than concluding "nothing changed".
	renewPool := writeKeyPairInto(t, dir, "recovered.obelo.test", pool)
	if got := servedCommonName(t, addr, renewPool); got != "recovered.obelo.test" {
		t.Errorf("served certificate CN = %q once the renewal completed, want the new certificate — a failed reload must not stop later ones", got)
	}
}

// TestShutdownDrainsBothListenersWithLiveSSESubscriber: the trap the two-listener
// change introduces. GET /api/v1/events only returns when the broker closes, so
// Shutdown hangs for its whole timeout unless the close hook runs — and Shutdown
// runs only the hooks registered on the server it was called on. A subscriber
// parked on the HTTPS listener with the hook registered on the HTTP one exits
// "context deadline exceeded" ten seconds later.
func TestShutdownDrainsBothListenersWithLiveSSESubscriber(t *testing.T) {
	certFile, keyFile, pool := writeKeyPairForTest(t, t.TempDir(), "obelo.test")

	// Stands in for events.Broker: subscribers block until it closes, and closing
	// twice is a no-op — the property that makes registering the hook on every
	// server safe.
	release := make(chan struct{})
	closeBroker := sync.OnceFunc(func() { close(release) })

	subscribed := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(subscribed)
		<-release
	})

	cfg := tlsConfigForTest(certFile, keyFile)
	servers, err := newServers(cfg, handler, closeBroker)
	if err != nil {
		t.Fatalf("newServers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("newServers built %d listeners in files mode, want 2 (plain HTTP + HTTPS)", len(servers))
	}
	addrs := startServers(t, servers)

	// Park the subscriber on the HTTPS listener — the one a naive port of the
	// existing hook would forget.
	client := httpsClient(pool)
	go func() {
		resp, err := client.Get("https://" + addrs[1] + "/api/v1/events")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSE subscriber never reached the handler")
	}

	// The real timeout, so a regression fails on the clock rather than on a value
	// this test invented.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shut the HTTPS listener down ON ITS OWN first. That is the assertion with
	// teeth: shutdownAll runs both concurrently, so the HTTP server's hook would
	// release this subscriber even if the HTTPS server had none registered, and
	// the bug would sail through. Draining one server in isolation is the only
	// shape that proves its own hook exists — and it is the shape a future change
	// (shutting the listeners down in sequence, or stopping HTTPS separately)
	// would rely on.
	start := time.Now()
	if err := servers[1].Shutdown(ctx); err != nil {
		t.Errorf("HTTPS Shutdown = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutting down the HTTPS listener alone took %v with an SSE subscriber parked on it — its own close hook is missing, so Shutdown waited out the whole timeout", elapsed)
	}

	// And the real entry point drains the set without complaint.
	start = time.Now()
	if err := shutdownAll(ctx, servers); err != nil {
		t.Errorf("shutdownAll = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdownAll took %v, want a prompt drain", elapsed)
	}
}

// TestNativeTLSSessionCookieAndPlainHTTPSideBySide: the end-to-end criterion. A
// login over the server's OWN TLS listener comes back with the ms_media cookie
// marked Secure — the r.TLS path through requestIsHTTPS, rather than the
// X-Forwarded-Proto one a proxy deployment exercises — and the same session then
// works over the plain-HTTP listener, which is the point of HTTPS being an
// addition rather than a replacement (ADR-0041). It also checks that neither
// listener emits HSTS.
func TestNativeTLSSessionCookieAndPlainHTTPSideBySide(t *testing.T) {
	h := testharness.New(t)
	status, body := h.JSON(http.MethodPost, "/api/v1/setup", "", map[string]any{
		"claimToken": h.ClaimToken(),
		"username":   "brandon",
		"password":   "hunter2hunter2",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201; body: %s", status, body)
	}

	certFile, keyFile, pool := writeKeyPairForTest(t, t.TempDir(), "obelo.test")
	servers, err := newServers(tlsConfigForTest(certFile, keyFile), h.Handler(), func() {})
	if err != nil {
		t.Fatalf("newServers: %v", err)
	}
	addrs := startServers(t, servers)
	httpAddr, httpsAddr := addrs[0], addrs[1]

	loginBody, _ := json.Marshal(map[string]any{
		"username": "brandon",
		"password": "hunter2hunter2",
		"device":   map[string]any{"name": "Browser", "platform": "web", "clientId": "native-tls-client"},
	})
	resp, err := httpsClient(pool).Post("https://"+httpsAddr+"/api/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("HTTPS login: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS login status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("login arrived over %s, want HTTP/2.0 on the TLS listener", resp.Proto)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ms_media" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("the HTTPS login set no ms_media cookie; body: %s", raw)
	}
	if !cookie.Secure {
		t.Error("ms_media Secure = false on a request that arrived over the server's own TLS listener — requestIsHTTPS is not seeing r.TLS")
	}

	// HSTS stays absent, deliberately and on BOTH listeners (ADR-0041): it is
	// sticky per hostname, and this server still answers plain HTTP on the LAN, so
	// one HSTS header could lock a household out of the only path they have to
	// their media. Do not "complete the set" now that TLS exists.
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HTTPS response carries Strict-Transport-Security = %q, want it ABSENT", got)
	}

	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &login); err != nil || login.Token == "" {
		t.Fatalf("login response has no token (%v); body: %s", err, raw)
	}

	// The same session, over the plain-HTTP listener that never stopped serving.
	req, _ := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/api/v1/libraries", nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	plainResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("plain-HTTP request with the HTTPS-issued token: %v", err)
	}
	defer plainResp.Body.Close()
	if plainResp.StatusCode != http.StatusOK {
		plainBody, _ := io.ReadAll(plainResp.Body)
		t.Errorf("GET /libraries over plain HTTP = %d, want 200 — a session must work on both listeners; body: %s", plainResp.StatusCode, plainBody)
	}
	if got := plainResp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("plain-HTTP response carries Strict-Transport-Security = %q, want it ABSENT", got)
	}
}

// TestWorldReadableKeyWarns: a key anyone on the box can read is a mistake that
// never announces itself — everything works — so it earns a line in the log. A
// warning and not a refusal: a group-readable key shared with a certbot deploy
// hook is a normal arrangement, and this server does not get to overrule the
// operator's filesystem.
func TestWorldReadableKeyWarns(t *testing.T) {
	logs := captureLog(t)

	dir := t.TempDir()
	certFile, keyFile, _ := writeKeyPairForTest(t, dir, "obelo.test")
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := newCertReloader(certFile, keyFile, defaultCertCheckInterval); err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	if !strings.Contains(logs.String(), "readable beyond its owner") {
		t.Errorf("no warning for a world-readable private key; log was:\n%s", logs.String())
	}
}

// TestNewCertReloaderRefusesABadPair: the boot posture. With TLS explicitly asked
// for and the certificate unusable, this fails rather than returning a server
// that would quietly serve nothing but plain HTTP.
func TestNewCertReloaderRefusesABadPair(t *testing.T) {
	dir := t.TempDir()
	certFile, _, _ := writeKeyPairForTest(t, dir, "obelo.test")
	if _, err := newCertReloader(certFile, filepath.Join(dir, "absent-key.pem"), 0); err == nil {
		t.Error("newCertReloader accepted a missing key file; want a refusal")
	}
}

// --- helpers ---------------------------------------------------------------

// tlsConfigForTest is a files-mode config pointed at a generated key pair. The
// listen addresses are placeholders: these tests serve on their own ephemeral
// listeners so they know the port, which is also why they never bind :8080.
func tlsConfigForTest(certFile, keyFile string) config.Config {
	cfg := config.Defaults()
	cfg.TLSMode = config.TLSModeFiles
	cfg.TLSCertFile = certFile
	cfg.TLSKeyFile = keyFile
	return cfg
}

// tlsServerForTest builds the HTTPS server around a reloader that re-checks the
// files on every handshake, so a reload test observes the change immediately
// rather than sleeping out defaultCertCheckInterval.
func tlsServerForTest(t *testing.T, certFile, keyFile string, h http.Handler) *http.Server {
	t.Helper()
	reloader, err := newCertReloader(certFile, keyFile, 0)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	return newTLSServer(tlsConfigForTest(certFile, keyFile), h, reloader.GetCertificate)
}

// startServer serves one server on an ephemeral loopback port and returns its
// address. The server's own Addr is left alone: a test needs to know the port it
// got, which only the listener can tell it.
func startServer(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		if srv.TLSConfig != nil {
			_ = srv.ServeTLS(ln, "", "")
			return
		}
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// startServers starts each server in order and returns their addresses in the
// same order (plain HTTP first, HTTPS second, as newServers builds them).
func startServers(t *testing.T, servers []*http.Server) []string {
	t.Helper()
	addrs := make([]string, 0, len(servers))
	for _, srv := range servers {
		addrs = append(addrs, startServer(t, srv))
	}
	return addrs
}

// httpsClient trusts exactly the generated certificates in pool — no
// InsecureSkipVerify, so a broken chain fails as a verification error rather than
// passing unnoticed. ForceAttemptHTTP2 is required because setting TLSClientConfig
// otherwise disables the transport's automatic HTTP/2.
func httpsClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: true,
		},
	}
}

// servedCommonName opens a fresh TLS connection and reports the leaf certificate
// the server presented. A raw handshake rather than an HTTP request because it
// cannot accidentally reuse a pooled connection — which would answer with the
// certificate from an earlier handshake and make a broken reloader look fine.
func servedCommonName(t *testing.T, addr string, pool *x509.CertPool) string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs:    pool,
		ServerName: "obelo.test",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS handshake with %s: %v", addr, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatalf("the server presented no certificate")
	}
	return certs[0].Subject.CommonName
}

// writeKeyPairForTest generates a throwaway certificate into dir and returns the
// two paths plus a pool trusting it. Generated in-process (crypto/x509) rather
// than committed as fixtures: a fixture certificate expires, and the test that
// depends on it then fails on a date nobody chose.
func writeKeyPairForTest(t *testing.T, dir, commonName string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	pool = x509.NewCertPool()
	pool = writeKeyPairInto(t, dir, commonName, pool)
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), pool
}

// writeKeyPairInto writes a fresh certificate over dir's cert.pem/key.pem — the
// renewal a running server has to notice — and returns a pool trusting it in
// addition to whatever pool already trusted. Trusting both certificates keeps a
// wrong answer legible as "served the old certificate" instead of collapsing
// into a verification failure.
func writeKeyPairInto(t *testing.T, dir, commonName string, pool *x509.CertPool) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		t.Fatalf("generating a serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Both names: the HTTP tests dial 127.0.0.1, the handshake tests use
		// obelo.test as the SNI name.
		DNSNames:              []string{"obelo.test", commonName},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling the key: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	touch(t, certFile)
	touch(t, keyFile)
	pool.AddCert(leaf)
	return pool
}

// touch pushes a file's modification time a second into the future. The reloader
// compares mtime and size, and a test rewrites both files within microseconds —
// on a filesystem with coarse timestamps (some still have one-second
// granularity) the rewrite would land in the same tick as the previous read and
// the change would be invisible. This removes that from the test rather than
// papering over anything real: a certificate renewal happens weeks after the
// previous read, so no production timestamp is ever that close.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
}

// captureLog redirects the standard logger into a buffer for the duration of one
// test, so an assertion can check that a failure was actually reported. The
// buffer is mutex-guarded because the writes come from handshake goroutines.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
