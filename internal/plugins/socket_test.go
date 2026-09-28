package plugins_test

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
	"math/big"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The socket grant (ADR-0064), end to end: a real guest asks the host for
// connections to real listeners, and each test reads both sides — what the guest
// was answered, step by step, and what the far end actually received.

// --- the far end ------------------------------------------------------------------

// farEnd is a TCP listener standing in for a directory. Every connection it
// accepts is served by serve and remembered: what arrived on it in the clear, and
// when it was closed from the other side.
type farEnd struct {
	ln   net.Listener
	addr string

	mu       sync.Mutex
	accepted int
	plain    bytes.Buffer
	secured  bytes.Buffer
	closedAt []time.Time
	accepts  []time.Time
}

// newFarEnd listens on loopback and serves every connection with serve, which is
// handed the connection and the far end to record into.
func newFarEnd(t *testing.T, serve func(f *farEnd, c net.Conn)) *farEnd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &farEnd{ln: ln, addr: ln.Addr().String()}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.accepted++
			f.accepts = append(f.accepts, time.Now())
			f.mu.Unlock()
			go func() {
				defer c.Close()
				serve(f, c)
			}()
		}
	}()
	return f
}

func (f *farEnd) record(buf *bytes.Buffer, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf.Write(b)
}

func (f *farEnd) closed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedAt = append(f.closedAt, time.Now())
}

func (f *farEnd) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted
}

func (f *farEnd) received() (plain, secured string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plain.String(), f.secured.String()
}

// pingPong answers "pong" to every "ping" on c and records everything it read into
// buf, until the other side closes the connection.
func pingPong(f *farEnd, c net.Conn, buf *bytes.Buffer) {
	b := make([]byte, 4096)
	for {
		n, err := c.Read(b)
		if n > 0 {
			f.record(buf, b[:n])
			if strings.Contains(string(b[:n]), "ping") {
				_, _ = c.Write([]byte("pong"))
			}
		}
		if err != nil {
			f.closed()
			return
		}
	}
}

// plainEcho is a directory that speaks in the clear.
func plainEcho(t *testing.T) *farEnd {
	return newFarEnd(t, func(f *farEnd, c net.Conn) { pingPong(f, c, &f.plain) })
}

// tlsEcho is a directory that speaks only TLS, presenting cert.
func tlsEcho(t *testing.T, cert tls.Certificate) *farEnd {
	return newFarEnd(t, func(f *farEnd, c net.Conn) {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tc.Handshake(); err != nil {
			f.closed()
			return
		}
		pingPong(f, tc, &f.secured)
	})
}

// startTLSEcho is a directory that answers "OK" to a plaintext "STARTTLS", then
// upgrades to TLS presenting cert, and answers "pong" to "ping" inside it.
func startTLSEcho(t *testing.T, cert tls.Certificate) *farEnd {
	return newFarEnd(t, func(f *farEnd, c net.Conn) {
		b := make([]byte, 64)
		n, err := c.Read(b)
		f.record(&f.plain, b[:n])
		if err != nil || string(b[:n]) != "STARTTLS" {
			// Keep reading, so a second plaintext write would be recorded.
			pingPong(f, c, &f.plain)
			return
		}
		if _, err := c.Write([]byte("OK")); err != nil {
			return
		}
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tc.Handshake(); err != nil {
			f.closed()
			return
		}
		pingPong(f, tc, &f.secured)
	})
}

// --- certificates -----------------------------------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Obelo Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// leaf issues a server certificate for the given names, valid between notBefore
// and notAfter.
func (ca testCA) leaf(t *testing.T, ips []net.IP, dns []string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "directory"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		IPAddresses:  ips,
		DNSNames:     dns,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var loopback = []net.IP{net.ParseIP("127.0.0.1")}

// goodLeaf is a certificate for 127.0.0.1, valid now.
func (ca testCA) goodLeaf(t *testing.T) tls.Certificate {
	return ca.leaf(t, loopback, nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

// --- the guest ------------------------------------------------------------------------

// socketDirectory loads the Sign-in provider "directory", declaring the socket
// grant, with the given setting values, and returns it built.
func socketDirectory(t *testing.T, values map[string]any, opts plugins.Options) (pluginapi.SignInProvider, *plugins.Set, *logSink) {
	t.Helper()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SocketSignInManifest("directory", ""))
	log := &logSink{}
	set := loadWith(t, dataDir, log, opts)
	for _, p := range set.Plugins() {
		p.SetSettingValues(values)
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("directory")
	if !ok {
		t.Fatal("no Sign-in provider registered for directory")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider, set, log
}

// settingsFor is the setting values of a directory at address running script,
// plus extra.
func socketValues(address, script string, extra map[string]any) map[string]any {
	v := map[string]any{
		"accounts":                     plugintest.SignInSocketScript + script,
		pluginapi.SocketAddressSetting: address,
	}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

// plaintextAllowed is the operator's opt-out, with encryption none.
var plaintextAllowed = map[string]any{
	pluginapi.SocketTLSSetting:            pluginapi.SocketTLSNone,
	pluginapi.SocketAllowPlaintextSetting: true,
}

// steps runs one password check against provider and answers what the guest's
// script was told, step by step.
func steps(t *testing.T, provider pluginapi.SignInProvider) []string {
	t.Helper()
	resp, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
	if err != nil {
		t.Fatalf("CheckPassword: %v", err)
	}
	if !resp.Accepted || resp.Identity == nil || resp.Identity.Subject != "socket" {
		t.Fatalf("answer = %+v, want the socket script's report", resp)
	}
	return resp.Identity.Groups
}

func wantSteps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the guest was answered %q, want %q", got, want)
	}
}

// --- where it connects ---------------------------------------------------------------

// TestASocketToAnAddressTheOperatorDidNotConfigureIsRefused is the host-owned
// judgment: the one address is the operator's. A guest naming another — even a
// live one, from a valid, enabled Sign-in provider — is refused before anything
// is dialled, audited without the address, and counted; naming the operator's
// own address is fine.
func TestASocketToAnAddressTheOperatorDidNotConfigureIsRefused(t *testing.T) {
	plugins.Parallel(t)
	configured := plainEcho(t)
	elsewhere := plainEcho(t)

	provider, set, log := socketDirectory(t,
		socketValues(configured.addr, "open="+elsewhere.addr+"|write=ping|read", plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "refused", "refused", "refused")
	if n := elsewhere.count(); n != 0 {
		t.Errorf("the address the guest named was connected to %d times, want 0", n)
	}
	if n := configured.count(); n != 0 {
		t.Errorf("the configured address was connected to %d times after a refused open, want 0", n)
	}
	if !log.contains(t, "plugin audit", "refused a socket", "plugin=directory", "reason=not-the-configured-address") {
		t.Errorf("no audit line for the refused socket:\n%s", log.all())
	}
	if strings.Contains(log.all(), elsewhere.addr) {
		t.Errorf("a log line quotes the address the guest named:\n%s", log.all())
	}
	if st, _ := set.Status("directory"); st.LastError == "" {
		t.Error("the refused socket left no last error; it is a violation")
	}

	// The operator's address, spelled by the guest or not, is reached.
	for _, script := range []string{"open=" + configured.addr + "|write=ping|read", "open|write=ping|read"} {
		provider, _, _ := socketDirectory(t, socketValues(configured.addr, script, plaintextAllowed), plugins.Options{})
		wantSteps(t, steps(t, provider), "ok:h=1", "ok:4", "ok:pong")
	}
}

// TestASocketNeedsAConfiguredAddress: no address typed, no connection.
func TestASocketNeedsAConfiguredAddress(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	values := socketValues("", "open="+far.addr, plaintextAllowed)
	provider, _, _ := socketDirectory(t, values, plugins.Options{})
	wantSteps(t, steps(t, provider), "refused")
	if n := far.count(); n != 0 {
		t.Errorf("connected %d times with no address configured, want 0", n)
	}
}

// --- who may ------------------------------------------------------------------------

// TestAPluginThatProvidesNoSignInProviderHasNoSocket: an Event sink asking for a
// socket during its delivery is refused before anything is dialled — and so is a
// sink call into a Plugin that is ALSO a socket-declaring Sign-in provider, and a
// sign-in call into a Sign-in provider that did not declare a socket.
func TestAPluginThatProvidesNoSignInProviderHasNoSocket(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)

	t.Run("an Event sink", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.Install(t, dataDir, plugintest.SinkManifest("sink"))
		log := &logSink{}
		set := load(t, dataDir, log)
		sink := sinkFor(t, set, "sink", pluginapi.Settings{
			Enabled: true, Secret: "s", URL: "http://" + far.addr + "/?obelo-mode=socket",
		})
		err := sink.Deliver(context.Background(), scanEvent())
		if err == nil || !strings.Contains(err.Error(), "refused: this call has no socket grant") {
			t.Fatalf("Deliver = %v, want the guest to have been refused a socket", err)
		}
		if !log.contains(t, "plugin audit", "refused a socket", "plugin=sink", "reason=no-socket-grant") {
			t.Errorf("no audit line for the refused socket:\n%s", log.all())
		}
	})

	t.Run("the sink half of a socket-declaring Sign-in provider", func(t *testing.T) {
		dataDir := t.TempDir()
		m := plugintest.SocketSignInManifest("both", "open")
		m.Provides = append(m.Provides, pluginapi.ManifestProvides{Kind: pluginapi.ExtensionEventSink})
		plugintest.Install(t, dataDir, m)
		set := load(t, dataDir, &logSink{})
		for _, p := range set.Plugins() {
			p.SetSettingValues(socketValues(far.addr, "open", plaintextAllowed))
		}
		sink := sinkFor(t, set, "both", pluginapi.Settings{
			Enabled: true, Secret: "s", URL: "http://" + far.addr + "/?obelo-mode=socket",
		})
		err := sink.Deliver(context.Background(), scanEvent())
		if err == nil || !strings.Contains(err.Error(), "refused: this call has no socket grant") {
			t.Fatalf("Deliver = %v, want the guest to have been refused a socket", err)
		}
	})

	t.Run("a Sign-in provider that declares no socket", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", ""))
		set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
		for _, p := range set.Plugins() {
			p.SetSettingValues(socketValues(far.addr, "open|write=ping|read", plaintextAllowed))
		}
		reg := pluginapi.NewRegistry()
		set.Register(reg)
		registration, _ := reg.SignInProvider("directory")
		provider, err := registration.New(pluginapi.Settings{Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		wantSteps(t, steps(t, provider), "refused", "refused", "refused")
	})

	if n := far.count(); n != 0 {
		t.Fatalf("a call without the grant connected %d times, want 0", n)
	}
}

// --- TLS by default -------------------------------------------------------------------

// TestAPlaintextConnectionIsRefusedWithoutTheOptOut: TLS is the default. An
// operator choosing no encryption without the opt-out gets no connection at all;
// an operator who chose nothing gets TLS, so a directory that speaks only
// plaintext is never handed anything but a TLS hello.
func TestAPlaintextConnectionIsRefusedWithoutTheOptOut(t *testing.T) {
	plugins.Parallel(t)
	t.Run("encryption none", func(t *testing.T) {
		far := plainEcho(t)
		provider, _, log := socketDirectory(t, socketValues(far.addr, "open|write=ping|read",
			map[string]any{pluginapi.SocketTLSSetting: pluginapi.SocketTLSNone}), plugins.Options{})
		wantSteps(t, steps(t, provider), "refused", "refused", "refused")
		if n := far.count(); n != 0 {
			t.Errorf("connected %d times, want 0", n)
		}
		if !log.contains(t, "refused a socket", "reason=plaintext-not-allowed") {
			t.Errorf("no audit line for the refused plaintext socket:\n%s", log.all())
		}
	})

	t.Run("nothing chosen", func(t *testing.T) {
		far := plainEcho(t)
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=ping|read", nil), plugins.Options{})
		wantSteps(t, steps(t, provider), "error", "refused", "refused")
		if plain, _ := far.received(); strings.Contains(plain, "ping") {
			t.Errorf("the directory received the guest's plaintext: %q", plain)
		}
	})
}

// TestAStartTLSConnectionCarriesOnlyTheUpgradeRequestInTheClear: before the
// upgrade the one write asking for it crosses; a second — the bind, with the
// password in it — does not, and never reaches the far end.
func TestAStartTLSConnectionCarriesOnlyTheUpgradeRequestInTheClear(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)
	far := startTLSEcho(t, ca.goodLeaf(t))
	provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=NOTSTARTTLS|write=bind-pw", map[string]any{
		pluginapi.SocketTLSSetting:       pluginapi.SocketTLSStartTLS,
		pluginapi.SocketTrustedCASetting: ca.pem,
	}), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1:upgrade", "ok:11", "refused")
	time.Sleep(50 * time.Millisecond)
	if plain, _ := far.received(); strings.Contains(plain, "bind-pw") {
		t.Fatalf("the far end received a second plaintext write before the upgrade: %q", plain)
	}
}

// TestThePlaintextOptOutAllowsAPlaintextConnection: with the opt-out on, a
// connection with no encryption is opened and carries the protocol as is.
func TestThePlaintextOptOutAllowsAPlaintextConnection(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=ping|read|close", plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "ok:4", "ok:pong", "ok")
	if plain, _ := far.received(); plain != "ping" {
		t.Fatalf("the far end received %q, want ping", plain)
	}
}

// --- verification -----------------------------------------------------------------

// TestACertificateThatFailsVerificationIsRefused: an untrusted chain, an expired
// certificate and one naming another host each answer an error to open — never a
// handle — and the rule broken is in the log, in the host's words.
func TestACertificateThatFailsVerificationIsRefused(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)
	other := newTestCA(t)
	now := time.Now()
	for _, tc := range []struct {
		name    string
		cert    tls.Certificate
		trusted string
		want    string
	}{
		{"untrusted chain", other.goodLeaf(t), ca.pem, "is not signed by an authority this plugin trusts"},
		{"untrusted chain, system store", ca.goodLeaf(t), "", "the server's certificate"},
		{"expired", ca.leaf(t, loopback, nil, now.Add(-48*time.Hour), now.Add(-24*time.Hour)), ca.pem, "has expired or is not yet valid"},
		{"hostname mismatch", ca.leaf(t, nil, []string{"other.example.test"}, now.Add(-time.Hour), now.Add(time.Hour)), ca.pem, "does not name the configured host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			far := tlsEcho(t, tc.cert)
			provider, _, log := socketDirectory(t, socketValues(far.addr, "open|write=ping|read", map[string]any{
				pluginapi.SocketTrustedCASetting: tc.trusted,
			}), plugins.Options{})
			wantSteps(t, steps(t, provider), "error", "refused", "refused")
			if _, secured := far.received(); secured != "" {
				t.Errorf("the far end received %q over a connection that failed verification", secured)
			}
			if !log.contains(t, "plugin directory", tc.want) {
				t.Errorf("no log line naming the broken rule %q:\n%s", tc.want, log.all())
			}
		})
	}
}

// TestATLSConnectionHandsTheGuestOnlyPlaintext: over implicit TLS and over
// StartTLS, the guest writes and reads the protocol's own bytes — "ping" arrives
// at the far end decrypted, and "pong" is exactly what the guest reads back.
func TestATLSConnectionHandsTheGuestOnlyPlaintext(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)

	t.Run("implicit", func(t *testing.T) {
		far := tlsEcho(t, ca.goodLeaf(t))
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=ping|read", map[string]any{
			pluginapi.SocketTrustedCASetting: ca.pem,
		}), plugins.Options{})
		wantSteps(t, steps(t, provider), "ok:h=1:tls", "ok:4:tls", "ok:pong:tls")
		if _, secured := far.received(); secured != "ping" {
			t.Fatalf("the far end decrypted %q, want ping", secured)
		}
	})

	t.Run("starttls", func(t *testing.T) {
		far := startTLSEcho(t, ca.goodLeaf(t))
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=STARTTLS|read|starttls|write=ping|read", map[string]any{
			pluginapi.SocketTLSSetting:       pluginapi.SocketTLSStartTLS,
			pluginapi.SocketTrustedCASetting: ca.pem,
		}), plugins.Options{})
		wantSteps(t, steps(t, provider), "ok:h=1:upgrade", "ok:8", "ok:OK", "ok:tls", "ok:4:tls", "ok:pong:tls")
		plain, secured := far.received()
		if plain != "STARTTLS" || secured != "ping" {
			t.Fatalf("the far end received %q in the clear and %q inside TLS, want STARTTLS and ping", plain, secured)
		}
	})

	t.Run("a failed upgrade does not fall back", func(t *testing.T) {
		far := startTLSEcho(t, newTestCA(t).goodLeaf(t))
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=STARTTLS|read|starttls|write=ping", map[string]any{
			pluginapi.SocketTLSSetting:       pluginapi.SocketTLSStartTLS,
			pluginapi.SocketTrustedCASetting: ca.pem,
		}), plugins.Options{})
		wantSteps(t, steps(t, provider), "ok:h=1:upgrade", "ok:8", "ok:OK", "error", "refused")
	})
}

// TestTheTrustedCASettingIsHonoured: a directory whose certificate a private CA
// issued is refused against the system's trusted certificates and reached with
// that CA configured — and a DIFFERENT configured CA does not reach it.
func TestTheTrustedCASettingIsHonoured(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)
	far := tlsEcho(t, ca.goodLeaf(t))
	for _, tc := range []struct {
		name    string
		trusted string
		want    string
	}{
		{"the system's", "", "error"},
		{"another CA", newTestCA(t).pem, "error"},
		{"the directory's CA", ca.pem, "ok:h=1:tls"},
		{"the directory's CA pasted onto one line", strings.ReplaceAll(ca.pem, "\n", " "), "ok:h=1:tls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _, _ := socketDirectory(t, socketValues(far.addr, "open", map[string]any{
				pluginapi.SocketTrustedCASetting: tc.trusted,
			}), plugins.Options{})
			wantSteps(t, steps(t, provider), tc.want)
		})
	}
}

// --- how long ---------------------------------------------------------------------------

// TestASocketIsClosedWhenItsCallEnds: a guest that opens a connection and never
// closes it has it closed for it when the call returns — and one whose call is
// killed at its deadline has it closed no later than that deadline.
func TestASocketIsClosedWhenItsCallEnds(t *testing.T) {
	plugins.Parallel(t)
	t.Run("the call returns", func(t *testing.T) {
		far := plainEcho(t)
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=ping|read", plaintextAllowed), plugins.Options{})
		wantSteps(t, steps(t, provider), "ok:h=1", "ok:4", "ok:pong")
		waitClosed(t, far, 1, time.Second)
	})

	t.Run("the deadline", func(t *testing.T) {
		far := plainEcho(t)
		// Long enough that the call has time left, after its FetchGrace (1s), to open.
		const budget = 1300 * time.Millisecond
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write=ping|read|hang", plaintextAllowed),
			plugins.Options{CallTimeout: budget})
		start := time.Now()
		if _, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"}); err == nil {
			t.Fatal("a guest that hangs answered no error")
		}
		if n := far.count(); n != 1 {
			t.Fatalf("the far end accepted %d connections, want the one the guest opened", n)
		}
		waitClosed(t, far, 1, 2*budget)
		far.mu.Lock()
		closedAt := far.closedAt[0]
		far.mu.Unlock()
		if late := closedAt.Sub(start); late > budget+100*time.Millisecond {
			t.Fatalf("the connection was closed %s after the call began, past its %s deadline", late, budget)
		}
	})

	t.Run("the caller gives up mid-read", func(t *testing.T) {
		// The far end never answers, so the guest's read would wait out its own
		// deadline, seconds away; the caller's cancel must close it now. Cancel
		// once the guest has actually opened its socket, rather than after a fixed
		// delay, so a slow scheduler can't fire the cancel before the socket exists.
		far := plainEcho(t)
		provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|read", plaintextAllowed),
			plugins.Options{CallTimeout: 2 * time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			deadline := time.Now().Add(800 * time.Millisecond)
			for far.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		start := time.Now()
		_, _ = provider.CheckPassword(ctx, pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
		// A correct cancel closes the socket in well under a second (observed:
		// single-digit ms even loaded); a cancel that failed to close it falls
		// through to the fetch grace instead, which alone takes ~1s here (the
		// call's own CallTimeout minus DefaultFetchGrace).
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("the call took %s after its caller gave up", took)
		}
		waitClosed(t, far, 1, time.Second)
	})
}

func waitClosed(t *testing.T, far *farEnd, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		far.mu.Lock()
		got := len(far.closedAt)
		far.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the far end saw %d of %d connections closed within %s", len(far.closedAt), n, within)
}

// TestASecondCallOpensAFreshConnection: a handle the guest kept from its last
// call names nothing in the next one, even though the instance — and the number
// in its memory — survived; the next call opens a connection of its own.
func TestASecondCallOpensAFreshConnection(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	values := socketValues(far.addr, "open|keep|write=ping|read", plaintextAllowed)
	provider, set, _ := socketDirectory(t, values, plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "ok", "ok:4", "ok:pong")

	for _, p := range set.Plugins() {
		p.SetSettingValues(socketValues(far.addr, "kept|write=ping|read|open|write=ping|read", plaintextAllowed))
	}
	wantSteps(t, steps(t, provider), "ok", "refused", "refused", "ok:h=1", "ok:4", "ok:pong")
	if n := far.count(); n != 2 {
		t.Fatalf("the far end accepted %d connections over two calls, want 2", n)
	}
}

// --- the settings -------------------------------------------------------------------

// TestTheSocketSettingsAreTheHosts: a Plugin declaring a socket gets the host's
// four settings after its own — the plaintext opt-out carrying the host's
// warning — and a save is judged by the host: a host:port, a certificate, and no
// "none" without the opt-out.
func TestTheSocketSettingsAreTheHosts(t *testing.T) {
	plugins.Parallel(t)
	fields := plugins.SettingsFields(plugintest.SocketSignInManifest("directory", ""))
	var keys []string
	var optOut pluginapi.SettingsField
	for _, f := range fields {
		keys = append(keys, f.Key)
		if f.Key == pluginapi.SocketAllowPlaintextSetting {
			optOut = f
		}
	}
	wantKeys := []string{"accounts", pluginapi.SocketAddressSetting, pluginapi.SocketTLSSetting,
		pluginapi.SocketAllowPlaintextSetting, pluginapi.SocketTrustedCASetting}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("fields = %q, want %q", keys, wantKeys)
	}
	if optOut.Type != pluginapi.FieldBool || optOut.Label != "Allow unencrypted connection (plaintext passwords)" ||
		!strings.Contains(optOut.Warning, "unencrypted") {
		t.Fatalf("the opt-out is %+v, want the host's bool with its warning", optOut)
	}
	if got := plugins.SettingsFields(plugintest.SignInManifest("directory", "")); len(got) != 1 {
		t.Fatalf("a Sign-in provider declaring no socket has %d fields, want only its own", len(got))
	}

	ca := newTestCA(t)
	for _, tc := range []struct {
		name    string
		values  map[string]string
		wantKey string
	}{
		{"a good address", map[string]string{"socket_address": `"ldap.example.test:636"`}, ""},
		{"no address", map[string]string{}, pluginapi.SocketAddressSetting},
		{"no port", map[string]string{"socket_address": `"ldap.example.test"`}, pluginapi.SocketAddressSetting},
		{"a URL", map[string]string{"socket_address": `"ldaps://ldap.example.test:636"`}, pluginapi.SocketAddressSetting},
		{"none without the opt-out", map[string]string{"socket_address": `"l:389"`, "socket_tls": `"none"`}, pluginapi.SocketTLSSetting},
		{"none with the opt-out", map[string]string{"socket_address": `"l:389"`, "socket_tls": `"none"`, "socket_allow_plaintext": "true"}, ""},
		{"a CA that is not one", map[string]string{"socket_address": `"l:636"`, "socket_trusted_ca": `"hello"`}, pluginapi.SocketTrustedCASetting},
		{"a CA", map[string]string{"socket_address": `"l:636"`, "socket_trusted_ca": mustQuote(ca.pem)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := plugins.PrepareSettings(fields, submitted(tc.values), nil)
			if tc.wantKey == "" {
				if len(errs) != 0 {
					t.Fatalf("refused %v, want it saved", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Key != tc.wantKey {
				t.Fatalf("refusals = %v, want one on %s", errs, tc.wantKey)
			}
		})
	}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestASocketDeclarationIsRefusedWhereItDoesNotBelong: socket on another
// Extension point, a manifest field wearing one of the host's keys, and a warning
// on a control that is not a switch are each refused at load.
func TestASocketDeclarationIsRefusedWhereItDoesNotBelong(t *testing.T) {
	plugins.Parallel(t)
	onASink := plugintest.SinkManifest("sink")
	onASink.Provides[0].Socket = true

	ownAddress := plugintest.SignInManifest("directory", "")
	ownAddress.Settings.Fields = append(ownAddress.Settings.Fields,
		pluginapi.SettingsField{Key: pluginapi.SocketAddressSetting, Type: pluginapi.FieldString})

	warnedText := plugintest.SignInManifest("directory", "")
	warnedText.Settings.Fields[0].Warning = "careful"

	for _, tc := range []struct {
		name string
		m    pluginapi.Manifest
		want string
	}{
		{"socket on an Event sink", onASink, "a socket declaration belongs on a sign-in-provider entry"},
		{"the host's key", ownAddress, "one of the host's socket settings"},
		{"a warning on text", warnedText, "declares a warning, which only a bool has"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			plugintest.Install(t, dataDir, tc.m)
			set := load(t, dataDir, &logSink{})
			st := mustStatus(t, set, tc.m.ID)
			if !st.Disabled || !strings.Contains(st.LastError, tc.want) {
				t.Fatalf("status = %+v, want it refused with %q", st, tc.want)
			}
		})
	}
}

// --- the limits ---------------------------------------------------------------------------

// TestTheWriteBeforeAStartTLSUpgradeIsSmall: the one plaintext write a StartTLS
// connection may carry is the request asking for the upgrade — LDAP's is about 31
// bytes. A write large enough to carry a bind after it is refused whole, and not
// a byte of it reaches the far end.
func TestTheWriteBeforeAStartTLSUpgradeIsSmall(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)
	settings := map[string]any{
		pluginapi.SocketTLSSetting:       pluginapi.SocketTLSStartTLS,
		pluginapi.SocketTrustedCASetting: ca.pem,
	}

	far := startTLSEcho(t, ca.goodLeaf(t))
	big := "STARTTLS" + strings.Repeat("x", 600) + "bind-pw"
	provider, _, log := socketDirectory(t, socketValues(far.addr, "open|write="+big, settings), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1:upgrade", "refused")
	time.Sleep(50 * time.Millisecond)
	if plain, _ := far.received(); plain != "" {
		t.Fatalf("the far end received %d bytes of a refused write in the clear", len(plain))
	}
	if !log.contains(t, "refused a socket", "reason=plaintext-before-upgrade") {
		t.Errorf("no audit line for the refused write:\n%s", log.all())
	}

	small := startTLSEcho(t, ca.goodLeaf(t))
	provider, _, _ = socketDirectory(t, socketValues(small.addr, "open|write="+strings.Repeat("x", 512), settings), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1:upgrade", "ok:512")
}

// TestACallMayOpenOnlySoManySockets: the fifth open in one call is refused.
func TestACallMayOpenOnlySoManySockets(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|open|open|open|open", plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "ok:h=2", "ok:h=3", "ok:h=4", "refused")
}

// TestOneReadOrWriteCarriesAtMost64KiB: a write over 64 KiB is refused whole, one
// of exactly 64 KiB is carried, and a read asking for more than 64 KiB is
// answered no more than that.
func TestOneReadOrWriteCarriesAtMost64KiB(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|write="+strings.Repeat("a", 64<<10+1), plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "refused")
	provider, _, _ = socketDirectory(t, socketValues(far.addr, "open|write="+strings.Repeat("a", 64<<10), plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "ok:65536")

	talker := newFarEnd(t, func(f *farEnd, c net.Conn) {
		_, _ = c.Write(bytes.Repeat([]byte("b"), 100000))
		pingPong(f, c, &f.plain)
	})
	provider, _, _ = socketDirectory(t, socketValues(talker.addr, "open|write=x|read=200000", plaintextAllowed), plugins.Options{})
	got := steps(t, provider)
	if len(got) != 3 || !strings.HasPrefix(got[2], "ok:b") {
		t.Fatalf("the guest was answered %d steps, want open, write and a read of the far end's bytes", len(got))
	}
	if n := len(strings.TrimPrefix(got[2], "ok:")); n > 64<<10 {
		t.Fatalf("one read answered %d bytes, want at most %d", n, 64<<10)
	}
}

// TestAFailedHandshakeIsLoggedInTheHostsWords: a far end that does not speak TLS
// at all fails the handshake with a Go error of its own; the log carries the
// host's sentence, never that error's text.
func TestAFailedHandshakeIsLoggedInTheHostsWords(t *testing.T) {
	plugins.Parallel(t)
	far := newFarEnd(t, func(f *farEnd, c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.0 400 this is not TLS\r\n\r\n"))
		pingPong(f, c, &f.plain)
	})
	provider, _, log := socketDirectory(t, socketValues(far.addr, "open", nil), plugins.Options{})
	wantSteps(t, steps(t, provider), "error")
	if !log.contains(t, "plugin directory", "the TLS handshake with the configured address failed") {
		t.Errorf("no log line in the host's words:\n%s", log.all())
	}
	if strings.Contains(log.all(), "tls:") || strings.Contains(log.all(), "first record") {
		t.Errorf("a log line quotes the handshake's own error:\n%s", log.all())
	}
}

// TestASocketGrantEndsWithItsCall: a Plugin that is both a socket-declaring
// Sign-in provider and an Event sink makes a sign-in call, then a delivery; the
// delivery has no grant — its open is refused before anything is dialled, so the
// far end sees only the sign-in call's one connection.
func TestASocketGrantEndsWithItsCall(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	dataDir := t.TempDir()
	m := plugintest.SocketSignInManifest("both", "")
	m.Provides = append(m.Provides, pluginapi.ManifestProvides{Kind: pluginapi.ExtensionEventSink})
	plugintest.Install(t, dataDir, m)
	set := load(t, dataDir, &logSink{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(socketValues(far.addr, "open|close", plaintextAllowed))
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("both")
	if !ok {
		t.Fatal("no Sign-in provider registered for both")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	wantSteps(t, steps(t, provider), "ok:h=1", "ok")

	sink := sinkFor(t, set, "both", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: "http://" + far.addr + "/?obelo-mode=socket",
	})
	err = sink.Deliver(context.Background(), scanEvent())
	if err == nil || !strings.Contains(err.Error(), "refused: this call has no socket grant") {
		t.Fatalf("Deliver = %v, want the guest to have been refused a socket", err)
	}
	// The sign-in call's connection closes on its own ("open|close"); the accept
	// counter is bumped from the listener's goroutine (newFarEnd), so wait for the
	// close before reading it rather than racing that goroutine.
	waitClosed(t, far, 1, time.Second)
	time.Sleep(100 * time.Millisecond)
	if n := far.count(); n != 1 {
		t.Fatalf("the far end accepted %d connections, want only the sign-in call's one", n)
	}
}

// TestAFifthOpenIsRefusedBeforeItConnects: the fifth open in one call is refused
// before anything is dialled — the far end sees the four connections the call
// was allowed and no fifth, not a fifth that was connected and then dropped.
func TestAFifthOpenIsRefusedBeforeItConnects(t *testing.T) {
	plugins.Parallel(t)
	far := plainEcho(t)
	provider, _, _ := socketDirectory(t, socketValues(far.addr, "open|open|open|open|open", plaintextAllowed), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1", "ok:h=2", "ok:h=3", "ok:h=4", "refused")
	waitClosed(t, far, 4, time.Second)
	time.Sleep(100 * time.Millisecond)
	if n := far.count(); n != 4 {
		t.Fatalf("the far end accepted %d connections, want 4: the fifth open must not connect", n)
	}
}

// TestTheSocketTLSFloorHoldsInAHandshake: a directory that speaks no TLS newer
// than 1.1 is refused in the handshake itself, while the same directory offering
// TLS 1.2 is reached — the floor as a connection meets it, not as a field in a
// configuration.
func TestTheSocketTLSFloorHoldsInAHandshake(t *testing.T) {
	plugins.Parallel(t)
	ca := newTestCA(t)
	cert := ca.goodLeaf(t)
	legacy := func(maxVersion uint16) *farEnd {
		return newFarEnd(t, func(f *farEnd, c net.Conn) {
			tc := tls.Server(c, &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS10,
				MaxVersion:   maxVersion,
			})
			if err := tc.Handshake(); err != nil {
				f.closed()
				return
			}
			pingPong(f, tc, &f.secured)
		})
	}
	trusted := map[string]any{pluginapi.SocketTrustedCASetting: ca.pem}

	control := legacy(tls.VersionTLS12)
	provider, _, _ := socketDirectory(t, socketValues(control.addr, "open", trusted), plugins.Options{})
	wantSteps(t, steps(t, provider), "ok:h=1:tls")

	for name, version := range map[string]uint16{"TLS 1.1": tls.VersionTLS11, "TLS 1.0": tls.VersionTLS10} {
		t.Run(name, func(t *testing.T) {
			far := legacy(version)
			provider, _, _ := socketDirectory(t, socketValues(far.addr, "open", trusted), plugins.Options{})
			wantSteps(t, steps(t, provider), "error")
		})
	}
}
