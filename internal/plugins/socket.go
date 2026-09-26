package plugins

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The socket grant (ADR-0064): the sixth host function, and the one narrow
// exception to ADR-0058's "no raw sockets, no host function that returns a
// handle".
//
// Four things make it narrow, and each is a line in this file rather than a
// promise in a manifest:
//
//   - WHO. A connection exists only inside a call made under callPolicy.socket —
//     a sign-in call — into a Plugin whose manifest declares `socket` on its
//     Sign-in provider entry. Every other call has no socket table, so every
//     request it makes is refused before anything is parsed.
//   - WHERE. The one address is the host:port the operator typed into the
//     Plugin's settings, read host-side. A guest may repeat it; any other address
//     is refused and audited.
//   - HOW. TLS is the host's: dialled into or upgraded in place, and verified
//     against the system's trust store or the operator's CA. Plaintext needs the
//     operator's opt-out; without it a connection that would carry anything
//     unencrypted — beyond the one request that asks for StartTLS — is refused.
//   - HOW LONG. A handle names a connection in THIS call's table. The table is
//     closed when the call returns and at its deadline, whichever is first, so no
//     connection outlives a call and no handle means anything in the next one.
//
// Every sentence a guest is told, logged or recorded here is a fixed one of the
// host's: a sign-in call carries a password, and nothing it supplies or causes is
// quoted (callPolicy.secret).

// The refusal sentences. Host-authored prose, like a fetch refusal: a Plugin must
// treat ANY non-empty Refused as "this server will not do this".
const (
	socketRefusedNoGrant   = "this call has no socket grant"
	socketRefusedBadOp     = "not a socket operation this server knows"
	socketRefusedNoAddress = "no address is configured for this plugin's connections"
	socketRefusedAddress   = "only the address the operator configured may be reached"
	socketRefusedPlaintext = "this server will not open an unencrypted connection for this plugin"
	socketRefusedBadCA     = "the trusted CA configured for this plugin is not a certificate"
	socketRefusedTooMany   = "this call has opened as many connections as it may"
	socketRefusedNoHandle  = "no open connection of this call has that handle"
	socketRefusedUpgrade   = "this connection must be upgraded to TLS before it carries anything more"
	socketRefusedNoUpgrade = "this connection has no upgrade pending"
	socketRefusedBigWrite  = "the write is larger than one write may carry"
)

// The error sentences: the host was willing and the network or the far end
// failed. The certificate ones name the rule the far end broke, never its text.
const (
	socketErrConnect    = "the configured address could not be reached"
	socketErrDeadline   = "the connection did not finish before this call's deadline"
	socketErrIO         = "the connection failed"
	socketErrUntrusted  = "the server's certificate is not signed by an authority this plugin trusts"
	socketErrExpired    = "the server's certificate has expired or is not yet valid"
	socketErrHostname   = "the server's certificate does not name the configured host"
	socketErrUnverified = "the server's certificate did not verify"
	socketErrHandshake  = "the TLS handshake with the configured address failed"
)

// The audit reasons an operator greps for.
const (
	auditSocketNoGrant   = "no-socket-grant"
	auditSocketAddress   = "not-the-configured-address"
	auditSocketPlaintext = "plaintext-not-allowed"
	auditSocketUpgrade   = "plaintext-before-upgrade"
)

const (
	// maxSocketsPerCall bounds the connections one call may hold open.
	maxSocketsPerCall = 4
	// maxSocketIO bounds one read's answer and one write's payload. A bind or a
	// search result is kilobytes; the bytes land in a linear memory that only
	// grows.
	maxSocketIO = 64 << 10
	// maxUpgradeRequest bounds the one plaintext write before a StartTLS upgrade:
	// room for the request asking for it (LDAP's is about 31 bytes) and none for
	// a bind sent after it.
	maxUpgradeRequest = 512
)

// grantsSocket reports whether this Plugin's manifest asks for the socket grant
// on a Sign-in provider entry — the only place validateManifest lets it appear.
func (p *Plugin) grantsSocket() bool {
	for _, e := range p.manifest.Provides {
		if e.Kind == pluginapi.ExtensionSignInProvider && e.Socket {
			return true
		}
	}
	return false
}

// socketTable is one call's connections. It is built by callGuestUnder for a
// call whose policy grants sockets, and closed when that call ends or its
// context does; after that every handle in it names nothing.
type socketTable struct {
	mu     sync.Mutex
	conns  map[int]*socketConn
	opened int
	closed bool
}

// socketConn is one connection: the TCP connection the host dialled, the TLS
// session over it once there is one, and what the operator's settings allow.
type socketConn struct {
	raw            net.Conn
	conn           net.Conn
	tlsConfig      *tls.Config
	encrypted      bool
	upgradePending bool
	allowPlaintext bool
	// plainWrites counts writes sent before any encryption: while an upgrade is
	// pending and plaintext is not allowed, only the one asking for it may cross.
	plainWrites int
}

func (t *socketTable) add(c *socketConn) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.opened >= maxSocketsPerCall {
		return 0, false
	}
	if t.conns == nil {
		t.conns = map[int]*socketConn{}
	}
	t.opened++
	t.conns[t.opened] = c
	return t.opened, true
}

// full reports whether this call has opened as many connections as it may, so
// an open past the cap is refused before anything is dialled.
func (t *socketTable) full() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed || t.opened >= maxSocketsPerCall
}

func (t *socketTable) get(handle int) *socketConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conns[handle]
}

func (t *socketTable) drop(handle int) {
	t.mu.Lock()
	c := t.conns[handle]
	delete(t.conns, handle)
	t.mu.Unlock()
	if c != nil {
		_ = c.conn.Close()
	}
}

// closeAll closes every connection and refuses any later open. Safe to call from
// the call's deadline and from its end, in either order. It closes the TCP
// connection under any TLS session: that is the one field the call's own
// goroutine never rewrites, so the deadline can close it mid-read.
func (t *socketTable) closeAll() {
	t.mu.Lock()
	conns := t.conns
	t.conns = nil
	t.closed = true
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.raw.Close()
	}
}

// beginSockets gives the call in flight its socket table, when its policy grants
// sockets and the manifest asks for them. The returned func ends the grant and
// closes whatever is still open; it is also run at the call's deadline.
func (p *Plugin) beginSockets(callCtx context.Context, policy callPolicy) func() {
	if !policy.socket || !p.grantsSocket() {
		return func() {}
	}
	t := &socketTable{}
	p.mu.Lock()
	p.sockets = t
	p.mu.Unlock()
	stop := context.AfterFunc(callCtx, t.closeAll)
	return func() {
		stop()
		p.mu.Lock()
		p.sockets = nil
		p.mu.Unlock()
		t.closeAll()
	}
}

func (p *Plugin) socketTable() *socketTable {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sockets
}

// auditSocket writes the audit line for a refused socket request. It names the
// Plugin and the reason and never the address: the guest may have named it, and
// a sign-in call's guest may have spelled the password into it.
func (p *Plugin) auditSocket(reason string) {
	p.logf("obelo: plugin audit: refused a socket: plugin=%s reason=%s", p.id, reason)
}

// socket is the host function: a SocketRequest in guest memory, a SocketResponse
// out through the guest's allocator. A call without the grant is refused before
// its request is read out of guest memory, let alone parsed.
func (h *hostFuncs) socket(ctx context.Context, mod api.Module, ptr, n uint32) uint64 {
	if _, refused := h.socketGrant(); refused != nil {
		return h.emit(ctx, mod, *refused)
	}
	var req pluginapi.SocketRequest
	if err := h.readRequest(mod, ptr, n, &req); err != nil {
		return h.emit(ctx, mod, pluginapi.SocketResponse{Refused: socketRefusedBadOp})
	}
	return h.emit(ctx, mod, h.socketOp(ctx, req))
}

// socketGrant is the call's socket table, or the refusal for a call without the
// grant. A call with no network, and a call with no table — every call that is
// not a sign-in call into a socket-declaring Plugin — is refused and counted: a
// Plugin reaching for a socket it was not given is a Plugin doing what its
// Extension point says it does not.
func (h *hostFuncs) socketGrant() (*socketTable, *pluginapi.SocketResponse) {
	t := h.p.socketTable()
	if h.p.offlineCall() || t == nil {
		h.p.auditSocket(auditSocketNoGrant)
		h.p.recordViolation("tried to open a socket, which only a sign-in call of a plugin declaring one may")
		return nil, &pluginapi.SocketResponse{Refused: socketRefusedNoGrant}
	}
	return t, nil
}

// socketOp is socket without the memory, so it can be read as the policy it is.
func (h *hostFuncs) socketOp(ctx context.Context, req pluginapi.SocketRequest) pluginapi.SocketResponse {
	// THE GRANT, first, before the request is so much as looked at.
	t, refused := h.socketGrant()
	if refused != nil {
		return *refused
	}
	switch req.Op {
	case pluginapi.SocketOpOpen:
		return h.socketOpen(ctx, t, req)
	case pluginapi.SocketOpWrite:
		return h.socketWrite(ctx, t, req)
	case pluginapi.SocketOpRead:
		return h.socketRead(ctx, t, req)
	case pluginapi.SocketOpStartTLS:
		return h.socketStartTLS(ctx, t, req)
	case pluginapi.SocketOpClose:
		if t.get(req.Handle) == nil {
			return pluginapi.SocketResponse{Refused: socketRefusedNoHandle}
		}
		t.drop(req.Handle)
		return pluginapi.SocketResponse{}
	}
	return pluginapi.SocketResponse{Refused: socketRefusedBadOp}
}

// socketSettings is what the operator saved for this Plugin's connections, read
// host-side from the values the Manager published — never from the request.
type socketSettings struct {
	host, port     string
	mode           string
	allowPlaintext bool
	trustedCA      string
}

func (p *Plugin) socketSettings() (socketSettings, string) {
	values := p.settingValues()
	address, _ := values[pluginapi.SocketAddressSetting].(string)
	host, port, ok := splitAddress(address)
	if !ok {
		return socketSettings{}, socketRefusedNoAddress
	}
	s := socketSettings{host: host, port: port}
	s.mode, _ = values[pluginapi.SocketTLSSetting].(string)
	if s.mode == "" {
		// TLS BY DEFAULT: a Plugin whose operator chose nothing is encrypted.
		s.mode = pluginapi.SocketTLSImplicit
	}
	s.allowPlaintext, _ = values[pluginapi.SocketAllowPlaintextSetting].(bool)
	s.trustedCA, _ = values[pluginapi.SocketTrustedCASetting].(string)
	return s, ""
}

// splitAddress reads a host:port, normalizing the host the way the fetch
// allowlist does so the operator's spelling and the guest's compare equal.
func splitAddress(address string) (host, port string, ok bool) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", "", false
	}
	h = normalizeHost(h)
	n, err := strconv.Atoi(p)
	if h == "" || err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	return h, strconv.Itoa(n), true
}

func (h *hostFuncs) socketOpen(ctx context.Context, t *socketTable, req pluginapi.SocketRequest) pluginapi.SocketResponse {
	s, refusal := h.p.socketSettings()
	if refusal != "" {
		return pluginapi.SocketResponse{Refused: refusal}
	}
	// THE TARGET IS THE OPERATOR'S. A guest may say where it expects to connect,
	// and anything but the address in its own settings is refused — the guest
	// never chooses where this connection goes.
	if strings.TrimSpace(req.Address) != "" {
		gh, gp, ok := splitAddress(req.Address)
		if !ok || gh != s.host || gp != s.port {
			h.p.auditSocket(auditSocketAddress)
			h.p.recordViolation("tried to open a socket to an address its operator did not configure")
			return pluginapi.SocketResponse{Refused: socketRefusedAddress}
		}
	}
	c := &socketConn{allowPlaintext: s.allowPlaintext}
	switch s.mode {
	case pluginapi.SocketTLSImplicit, pluginapi.SocketTLSStartTLS:
	case pluginapi.SocketTLSNone:
		if !s.allowPlaintext {
			h.p.auditSocket(auditSocketPlaintext)
			return pluginapi.SocketResponse{Refused: socketRefusedPlaintext}
		}
	default:
		h.p.auditSocket(auditSocketPlaintext)
		return pluginapi.SocketResponse{Refused: socketRefusedPlaintext}
	}
	if s.mode != pluginapi.SocketTLSNone {
		cfg, ok := socketTLSConfig(s)
		if !ok {
			return pluginapi.SocketResponse{Refused: socketRefusedBadCA}
		}
		c.tlsConfig = cfg
	}

	if t.full() {
		return pluginapi.SocketResponse{Refused: socketRefusedTooMany}
	}

	dctx, cancel, ok := h.deadline(ctx)
	if !ok {
		return pluginapi.SocketResponse{Error: socketErrDeadline}
	}
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(dctx, "tcp", net.JoinHostPort(s.host, s.port))
	if err != nil {
		h.socketFailed(socketErrorText(err, socketErrConnect))
		return pluginapi.SocketResponse{Error: socketErrorText(err, socketErrConnect)}
	}
	c.raw, c.conn = raw, raw
	if s.mode == pluginapi.SocketTLSImplicit {
		if msg := c.handshake(dctx); msg != "" {
			h.socketFailed(msg)
			return pluginapi.SocketResponse{Error: msg}
		}
	}
	c.upgradePending = s.mode == pluginapi.SocketTLSStartTLS
	handle, ok := t.add(c)
	if !ok {
		_ = c.conn.Close()
		return pluginapi.SocketResponse{Refused: socketRefusedTooMany}
	}
	return pluginapi.SocketResponse{Handle: handle, Encrypted: c.encrypted, UpgradePending: c.upgradePending}
}

// handshake runs TLS over the raw connection, verified by the host. On failure
// the connection is closed and the answer is the rule the far end broke.
func (c *socketConn) handshake(ctx context.Context) string {
	tc := tls.Client(c.raw, c.tlsConfig)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = c.raw.Close()
		return tlsErrorText(err)
	}
	c.conn = tc
	c.encrypted = true
	c.upgradePending = false
	return ""
}

// socketTLSConfig is the verification the host does: the operator's host name,
// against the operator's CA when there is one and the system's when there is not.
func socketTLSConfig(s socketSettings) (*tls.Config, bool) {
	cfg := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	if strings.TrimSpace(s.trustedCA) != "" {
		pool, ok := parseTrustedCA(s.trustedCA)
		if !ok {
			return nil, false
		}
		cfg.RootCAs = pool
	}
	return cfg, true
}

// parseTrustedCA reads every certificate in a PEM document. It is tolerant of
// what a single-line form box does to a paste — line breaks turned into spaces
// or dropped — because the base64 between the markers is the whole of it.
func parseTrustedCA(text string) (*x509.CertPool, bool) {
	const begin, end = "-----BEGIN CERTIFICATE-----", "-----END CERTIFICATE-----"
	pool := x509.NewCertPool()
	found := false
	for {
		i := strings.Index(text, begin)
		if i < 0 {
			break
		}
		rest := text[i+len(begin):]
		j := strings.Index(rest, end)
		if j < 0 {
			return nil, false
		}
		body := strings.Join(strings.Fields(rest[:j]), "")
		der, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, false
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, false
		}
		pool.AddCert(cert)
		found = true
		text = rest[j+len(end):]
	}
	return pool, found
}

func (h *hostFuncs) socketWrite(ctx context.Context, t *socketTable, req pluginapi.SocketRequest) pluginapi.SocketResponse {
	c := t.get(req.Handle)
	if c == nil {
		return pluginapi.SocketResponse{Refused: socketRefusedNoHandle}
	}
	if len(req.Data) > maxSocketIO {
		return pluginapi.SocketResponse{Refused: socketRefusedBigWrite}
	}
	// Before a StartTLS upgrade, the one small write that asks for it may cross in
	// the clear; a second, or one big enough to carry a bind — a password — after
	// the request, may not, unless the operator allowed plaintext.
	if c.upgradePending && !c.allowPlaintext && (c.plainWrites >= 1 || len(req.Data) > maxUpgradeRequest) {
		h.p.auditSocket(auditSocketUpgrade)
		return pluginapi.SocketResponse{Refused: socketRefusedUpgrade}
	}
	end, ok := h.ioDeadline(ctx)
	if !ok {
		return pluginapi.SocketResponse{Error: socketErrDeadline}
	}
	_ = c.conn.SetWriteDeadline(end)
	n, err := c.conn.Write(req.Data)
	if !c.encrypted {
		c.plainWrites++
	}
	if err != nil {
		return pluginapi.SocketResponse{Written: n, Encrypted: c.encrypted, Error: socketErrorText(err, socketErrIO)}
	}
	return pluginapi.SocketResponse{Written: n, Encrypted: c.encrypted}
}

func (h *hostFuncs) socketRead(ctx context.Context, t *socketTable, req pluginapi.SocketRequest) pluginapi.SocketResponse {
	c := t.get(req.Handle)
	if c == nil {
		return pluginapi.SocketResponse{Refused: socketRefusedNoHandle}
	}
	limit := req.Max
	if limit <= 0 || limit > maxSocketIO {
		limit = maxSocketIO
	}
	end, ok := h.ioDeadline(ctx)
	if !ok {
		return pluginapi.SocketResponse{Error: socketErrDeadline}
	}
	_ = c.conn.SetReadDeadline(end)
	buf := make([]byte, limit)
	n, err := c.conn.Read(buf)
	out := pluginapi.SocketResponse{Data: buf[:n], Encrypted: c.encrypted}
	switch {
	case errors.Is(err, io.EOF):
		out.EOF = true
	case err != nil && n == 0:
		out.Error = socketErrorText(err, socketErrIO)
	}
	return out
}

func (h *hostFuncs) socketStartTLS(ctx context.Context, t *socketTable, req pluginapi.SocketRequest) pluginapi.SocketResponse {
	c := t.get(req.Handle)
	if c == nil {
		return pluginapi.SocketResponse{Refused: socketRefusedNoHandle}
	}
	if !c.upgradePending {
		return pluginapi.SocketResponse{Refused: socketRefusedNoUpgrade, Encrypted: c.encrypted}
	}
	dctx, cancel, ok := h.deadline(ctx)
	if !ok {
		return pluginapi.SocketResponse{Error: socketErrDeadline}
	}
	defer cancel()
	_ = c.raw.SetDeadline(time.Time{})
	if msg := c.handshake(dctx); msg != "" {
		// The connection is closed and gone: a failed upgrade never falls back to
		// the plaintext it was meant to replace.
		t.drop(req.Handle)
		h.socketFailed(msg)
		return pluginapi.SocketResponse{Error: msg}
	}
	return pluginapi.SocketResponse{Encrypted: true}
}

// ioDeadline is the deadline one read or write runs under: the fetch rule, so it
// ends before the call's own deadline and the guest can still answer.
func (h *hostFuncs) ioDeadline(ctx context.Context) (time.Time, bool) {
	dctx, cancel, ok := h.deadline(ctx)
	defer cancel()
	if !ok {
		return time.Time{}, false
	}
	end, _ := dctx.Deadline()
	return end, true
}

// socketFailed logs a connection that failed, in the host's own sentence, so an
// operator whose directory will not verify can read which rule it broke.
func (h *hostFuncs) socketFailed(msg string) {
	h.p.logf("obelo: plugin %s: a connection to its configured address failed: %s", h.p.id, msg)
}

// socketErrorText turns a network error into the host's sentence: the deadline's,
// or the fallback. Never the error's own text.
func socketErrorText(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return socketErrDeadline
	}
	return fallback
}

// tlsErrorText names which verification rule a handshake failed, as a fixed
// sentence.
func tlsErrorText(err error) string {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &unknown):
		return socketErrUntrusted
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return socketErrExpired
	case errors.As(err, &hostname):
		return socketErrHostname
	case errors.As(err, &verify):
		// A platform verifier (macOS's) may refuse without saying which rule.
		return socketErrUnverified
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return socketErrDeadline
	}
	return socketErrHandshake
}

// --- the settings the host adds ------------------------------------------------

// socketWarning is shown beside the plaintext opt-out while it is on.
const socketWarning = "Passwords typed at the login form will cross the network unencrypted to this address, " +
	"where anyone who can watch that traffic can read them. Leave this off unless the connection never " +
	"leaves a network you trust."

// socketFields are the HOST's settings for a Plugin that declares a socket: the
// address, how it is encrypted, the plaintext opt-out and the trusted CA. The
// labels and the warning are the host's, not the author's, because they describe
// what the host does with the operator's passwords.
func socketFields() []pluginapi.SettingsField {
	return []pluginapi.SettingsField{
		{
			Key:      pluginapi.SocketAddressSetting,
			Type:     pluginapi.FieldString,
			Label:    "Directory address",
			Help:     "The host:port this plugin connects to, such as ldap.example.lan:636. It is the only address it can reach.",
			Required: true,
		},
		{
			Key:     pluginapi.SocketTLSSetting,
			Type:    pluginapi.FieldEnum,
			Label:   "Encryption",
			Help:    "tls connects straight into TLS (LDAPS, usually port 636); starttls connects and upgrades in place (usually port 389); none never encrypts. The server verifies the certificate either way.",
			Options: []string{pluginapi.SocketTLSImplicit, pluginapi.SocketTLSStartTLS, pluginapi.SocketTLSNone},
			Default: json.RawMessage(strconv.Quote(pluginapi.SocketTLSImplicit)),
		},
		{
			Key:     pluginapi.SocketAllowPlaintextSetting,
			Type:    pluginapi.FieldBool,
			Label:   "Allow unencrypted connection (plaintext passwords)",
			Help:    "Needed for Encryption none, and to skip a StartTLS upgrade.",
			Warning: socketWarning,
			Default: json.RawMessage("false"),
		},
		{
			Key:   pluginapi.SocketTrustedCASetting,
			Type:  pluginapi.FieldString,
			Label: "Trusted CA certificate (PEM)",
			Help:  "Verify the directory's certificate against this CA instead of the system's trusted certificates. Empty uses the system's.",
		},
	}
}

// isSocketSettingKey reports whether key is one of the host's socket settings,
// which a manifest may not declare for itself.
func isSocketSettingKey(key string) bool {
	switch key {
	case pluginapi.SocketAddressSetting, pluginapi.SocketTLSSetting,
		pluginapi.SocketAllowPlaintextSetting, pluginapi.SocketTrustedCASetting:
		return true
	}
	return false
}

// settingsFields is a Plugin's whole settings schema: what its manifest declares,
// and — for a Plugin asking for the socket grant — the host's socket settings
// after it.
func settingsFields(m pluginapi.Manifest) []pluginapi.SettingsField {
	fields := m.Settings.Fields
	for _, e := range m.Provides {
		if e.Kind == pluginapi.ExtensionSignInProvider && e.Socket {
			return append(append([]pluginapi.SettingsField(nil), fields...), socketFields()...)
		}
	}
	return fields
}

// SettingsFields is settingsFields for a caller outside this package.
func SettingsFields(m pluginapi.Manifest) []pluginapi.SettingsField { return settingsFields(m) }

// checkSocketSettings judges the host's socket settings in a document about to be
// saved: an address that is a host:port, a trusted CA that is a certificate, and
// no "none" without the opt-out. Nothing for a schema without them.
func checkSocketSettings(rows []store.PluginSetting, fields []pluginapi.SettingsField) []FieldError {
	if _, ok := declaredField(fields, pluginapi.SocketAddressSetting); !ok {
		return nil
	}
	values := map[string]any{}
	for _, r := range rows {
		var v any
		if json.Unmarshal([]byte(r.Value), &v) == nil {
			values[r.Key] = v
		}
	}
	var errs []FieldError
	if address, _ := values[pluginapi.SocketAddressSetting].(string); address != "" {
		if _, _, ok := splitAddress(address); !ok {
			errs = append(errs, FieldError{Key: pluginapi.SocketAddressSetting,
				Message: "Directory address must be a host and a port, such as ldap.example.lan:636"})
		}
	}
	if ca, _ := values[pluginapi.SocketTrustedCASetting].(string); strings.TrimSpace(ca) != "" {
		if _, ok := parseTrustedCA(ca); !ok {
			errs = append(errs, FieldError{Key: pluginapi.SocketTrustedCASetting,
				Message: "Trusted CA certificate must be a PEM certificate"})
		}
	}
	mode, _ := values[pluginapi.SocketTLSSetting].(string)
	allow, _ := values[pluginapi.SocketAllowPlaintextSetting].(bool)
	if mode == pluginapi.SocketTLSNone && !allow {
		errs = append(errs, FieldError{Key: pluginapi.SocketTLSSetting,
			Message: "Encryption none needs Allow unencrypted connection (plaintext passwords) turned on"})
	}
	return errs
}
