package v1

// The socket grant, on the wire (ADR-0064): the sixth host function, `socket`.
//
// It exists for ONE Extension point. A Plugin providing a Sign-in provider that
// declares `socket: true` on its provides entry may open a raw TCP connection
// during a sign-in call — an LDAP-style directory is not an HTTP protocol, and
// http_fetch cannot reach one. Every other Plugin, and every other call, is
// refused by the host whatever it asks.
//
// # Where it connects
//
// To the host:port the OPERATOR typed into the Plugin's settings
// (SocketAddressSetting), and nowhere else. A request may repeat that address in
// Address, and a request naming any other address is refused: the target is never
// the manifest's and never the guest's.
//
// # TLS is the host's
//
// The host dials, terminates and verifies TLS itself — at the dial
// (SocketTLSImplicit) or by an in-place upgrade (SocketTLSStartTLS), as the
// operator's SocketTLSSetting says — against the system's trusted certificates or
// the operator's SocketTrustedCASetting. A guest never holds key material, never
// sees a TLS record, and makes no TLS decision: it reads and writes the plaintext
// of the protocol it speaks. A connection that is not encrypted at all
// (SocketTLSNone) is refused unless the operator turned on
// SocketAllowPlaintextSetting.
//
// For StartTLS the guest sends its protocol's own upgrade request — LDAP's
// StartTLS extended operation — reads the answer, and then asks the host for
// SocketOpStartTLS. Until the upgrade, the connection carries that one write and
// nothing else.
//
// # A handle lives for one call
//
// Open answers a Handle. It is valid only inside the call that opened it: the
// host closes every connection when the call returns or at its deadline,
// whichever is first, and a handle kept for a later call names nothing. Every
// read and write ends before the call's own deadline, like a fetch.

// The settings the host adds to a socket-declaring Plugin's form, by key. They are
// the HOST's fields — its labels, its warning, its validation — stored beside the
// Plugin's own and handed to the guest in Settings.Values like them. A manifest may
// not declare a field with one of these keys.
const (
	// SocketAddressSetting is the host:port the operator typed: the one address
	// this Plugin may connect to.
	SocketAddressSetting = "socket_address"
	// SocketTLSSetting is how the connection is encrypted: SocketTLSImplicit (the
	// default), SocketTLSStartTLS, or SocketTLSNone.
	SocketTLSSetting = "socket_tls"
	// SocketAllowPlaintextSetting is the operator's opt-out: true lets a
	// connection carry bytes unencrypted. Off by default.
	SocketAllowPlaintextSetting = "socket_allow_plaintext"
	// SocketTrustedCASetting is a PEM certificate the host verifies the far end
	// against INSTEAD of the system's trusted certificates. Empty means the
	// system's.
	SocketTrustedCASetting = "socket_trusted_ca"
)

// The values of SocketTLSSetting.
const (
	// SocketTLSImplicit dials straight into TLS (LDAPS).
	SocketTLSImplicit = "tls"
	// SocketTLSStartTLS dials in the clear and upgrades in place.
	SocketTLSStartTLS = "starttls"
	// SocketTLSNone never encrypts, and needs SocketAllowPlaintextSetting.
	SocketTLSNone = "none"
)

// SocketOp is what one socket request asks for.
type SocketOp string

const (
	// SocketOpOpen connects to the operator's address and answers a Handle.
	SocketOpOpen SocketOp = "open"
	// SocketOpWrite sends Data on Handle.
	SocketOpWrite SocketOp = "write"
	// SocketOpRead answers what has arrived on Handle, at most Max bytes.
	SocketOpRead SocketOp = "read"
	// SocketOpStartTLS upgrades Handle to TLS in place, after the guest's own
	// protocol has asked the far end to.
	SocketOpStartTLS SocketOp = "starttls"
	// SocketOpClose closes Handle.
	SocketOpClose SocketOp = "close"
)

// AllSocketOps is every SocketOp, in declaration order.
func AllSocketOps() []SocketOp {
	return []SocketOp{SocketOpOpen, SocketOpWrite, SocketOpRead, SocketOpStartTLS, SocketOpClose}
}

// SocketRequest is one socket operation.
type SocketRequest struct {
	Op SocketOp `json:"op"`
	// Address is, for open, the address the guest expects to reach. Empty means
	// the operator's; anything but the operator's is refused.
	Address string `json:"address,omitempty"`
	// Handle names the connection, for every op but open.
	Handle int `json:"handle,omitempty"`
	// Data is what a write sends.
	Data []byte `json:"data,omitempty"`
	// Max is the most bytes a read may answer. Zero means the host's cap.
	Max int `json:"max,omitempty"`
}

// SocketResponse is what one socket operation answers. As with a fetch, exactly
// one of three things is true: Refused is set (the host will not do this), Error
// is set (it tried and the network or the far end failed), or neither is and the
// op did what it asked. Both sentences are the host's own and a Plugin must not
// branch on their text.
type SocketResponse struct {
	// Handle is the connection open answered.
	Handle int `json:"handle,omitempty"`
	// Encrypted is whether the connection is now TLS, verified by the host.
	Encrypted bool `json:"encrypted,omitempty"`
	// UpgradePending is open saying the operator chose StartTLS: send your
	// protocol's upgrade request, then SocketOpStartTLS.
	UpgradePending bool `json:"upgradePending,omitempty"`
	// Written is how many bytes a write sent.
	Written int `json:"written,omitempty"`
	// Data is what a read received: the protocol's plaintext, never a TLS record.
	Data []byte `json:"data,omitempty"`
	// EOF is a read finding the far end closed the connection.
	EOF     bool   `json:"eof,omitempty"`
	Refused string `json:"refused,omitempty"`
	Error   string `json:"error,omitempty"`
}
