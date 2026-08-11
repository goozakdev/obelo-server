package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The trusted-proxy allowlist: where every X-Forwarded-* header this server
// reads is decided (ADR-0041, OBELO_TRUSTED_PROXIES).
//
// A forwarded header is a claim, and the only question worth asking about a
// claim is who made it. While a TLS-terminating reverse proxy was assumed
// (ADR-0005) the answer was "the proxy, necessarily" — nothing else could reach
// the socket — so reading X-Forwarded-Proto was safe by construction. The
// server now terminates TLS itself and clients connect directly, which retires
// that construction: on a port-forwarded box the header is written by whoever
// dialled the port, and the honest name for that party is "anybody".
//
// So the claim is read ONLY when the immediate peer — r.RemoteAddr, the address
// this process genuinely received the bytes from and the one thing on a request
// nobody can forge — is inside a prefix the operator listed. With the list empty,
// which is every deployment that has not opted in, nothing here changes any
// answer: the peer is the client and r.TLS is the scheme, exactly as before.
//
// Resolution happens ONCE per request, in resolveRequestOrigin below, and the
// answer rides the context. That is deliberate: it means there is exactly one
// place in this server where an attacker-supplied header becomes a trusted fact,
// and a reviewer who wants to know whether a given header can be believed has one
// function to read rather than a grep across handlers.

// trustedProxies is the parsed OBELO_TRUSTED_PROXIES allowlist. Nil — the
// default — trusts nothing, which is why every zero value in this package
// (a Deps built by a narrow unit test, a request that never met the middleware)
// fails in the safe direction.
type trustedProxies []netip.Prefix

// has reports whether addr is inside any listed prefix. An address that did not
// parse is never trusted: it cannot be inside a prefix, and answering anything
// else for an input we failed to understand is how trust checks get bypassed.
func (t trustedProxies) has(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	for _, p := range t {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// requestOrigin is what the allowlist resolves a request down to: who the client
// really is, and whether their hop to us was encrypted. Both are derived facts —
// the whole point is that no handler re-derives them from raw headers.
type requestOrigin struct {
	// ClientIP keys the per-source rate limits (auth/login_limit.go,
	// auth/device_auth.go). It is the peer address unless a trusted proxy vouched
	// for something else.
	ClientIP string
	// HTTPS decides the media cookie's Secure flag (cookie.go).
	HTTPS bool
}

// resolveRequestOrigin is the middleware that performs the trust decision once
// per request and attaches the result. api.Handler wraps the whole API in it, so
// every route below is served a request whose origin has already been settled.
func resolveRequestOrigin(trusted trustedProxies, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := resolveOrigin(r, trusted)
		h.ServeHTTP(w, r.WithContext(withRequestOrigin(r.Context(), o)))
	})
}

// resolveOrigin is the trust decision itself, split out from the middleware so
// it is directly testable and so the fallback path (a handler reached without the
// middleware) runs the same code with an empty allowlist.
func resolveOrigin(r *http.Request, trusted trustedProxies) requestOrigin {
	peer := peerHost(r)
	o := requestOrigin{ClientIP: peer, HTTPS: r.TLS != nil}

	// The gate. Everything below this line reads attacker-supplied bytes; nothing
	// below it runs unless an operator named this peer.
	if len(trusted) == 0 || !trusted.has(parseHostAddr(peer)) {
		return o
	}

	if ip, ok := forwardedFor(r, trusted); ok {
		o.ClientIP = ip
	}
	// r.TLS wins outright and is never turned off by a header: we terminated that
	// handshake ourselves, so it is the one scheme signal that is not hearsay. The
	// header can only ever add HTTPS, never remove it.
	if !o.HTTPS && forwardedProtoIsHTTPS(r) {
		o.HTTPS = true
	}
	return o
}

// forwardedFor walks X-Forwarded-For RIGHT TO LEFT and returns the first address
// that is NOT in the trusted set — the left-most untrusted hop.
//
// THE DIRECTION IS THE ENTIRE SECURITY PROPERTY. The header is built by
// appending: each proxy adds the address it received the connection from, so the
// RIGHT-most entry was written by our own trusted peer and everything to its left
// was written by somebody further out — ending, at the left edge, with whatever
// the client itself sent before any proxy touched it. A client that prepends
// "X-Forwarded-For: 1.2.3.4" simply gets its own address appended after it. So
// walking from the right steps back through hops we trust and stops at the first
// one we do not, which is the earliest party whose word we have no reason to take
// — the real client. Reading the left-most entry instead, which is what "the
// client IP is the first entry" folklore says, reads exactly the bytes the
// attacker chose, and hands anyone who can type a header a fresh rate-limit
// budget per request. That is the bypass this whole file exists to prevent.
//
// Two edges, both resolved toward the peer rather than toward the header:
//
//   - An entry that is not an address at all (an obfuscated identifier, "unknown",
//     a proxy configured to pass the client's header through verbatim instead of
//     appending to it) ends the walk with the PEER as the answer. We cannot reason
//     about the rest of a chain we failed to parse, and collapsing to the proxy's
//     own address is the strict direction to fail — one shared bucket, never a
//     fresh one per request.
//
//   - Every entry trusted, i.e. the walk falls off the left end, returns the
//     left-most entry. That is the operator having listed a range wide enough to
//     contain their own clients, and the left-most entry is then the client as a
//     trusted proxy reported it. Returning the peer instead would silently pool
//     every LAN client into one bucket, which is the misconfiguration this reading
//     avoids rather than the one it creates.
//
// X-Real-IP is deliberately NOT consulted, from any peer. It carries no chain, so
// a proxy that sets it and a proxy that passes the client's own copy of it
// through are indistinguishable on the wire — there is no right-to-left walk to
// perform and therefore no way to tell the two apart. X-Forwarded-For answers the
// same question and can be reasoned about, so it is the only one read.
func forwardedFor(r *http.Request, trusted trustedProxies) (string, bool) {
	entries := forwardedForEntries(r)
	if len(entries) == 0 {
		return "", false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		addr := parseHostAddr(entries[i])
		if !addr.IsValid() {
			return "", false
		}
		if !trusted.has(addr) {
			return addr.String(), true
		}
	}
	// Every hop was trusted; the left-most entry is the client.
	return parseHostAddr(entries[0]).String(), true
}

// forwardedForEntries flattens X-Forwarded-For into one ordered list. The header
// may appear as several lines as well as one comma-separated line — Go keeps them
// separate — and RFC 7230 says the two are equivalent, so joining them in header
// order reconstructs the chain a single line would have carried.
func forwardedForEntries(r *http.Request) []string {
	var out []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(line, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// forwardedProtoIsHTTPS reports whether X-Forwarded-Proto names https. Only
// reached for a trusted peer (see resolveOrigin).
//
// It reads the LEFT-most entry, which is the opposite end from forwardedFor and
// is not an inconsistency: this header records the scheme of the ORIGINAL request,
// so the earliest hop's value is the one that answers the question, whereas
// X-Forwarded-For's earliest entry is the one nobody vouched for. The worst a
// client behind a trusted, appending proxy can do by prepending its own value is
// claim "http" and lose the Secure flag on its own cookie.
func forwardedProtoIsHTTPS(r *http.Request) bool {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		return false
	}
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// peerHost returns the host portion of r.RemoteAddr — the address this process
// actually received the bytes from, before any header is considered.
//
// A RemoteAddr with no parseable port is returned whole rather than dropped: an
// unusual transport should still get its own bucket, and returning "" would pool
// it with every other unparseable caller.
func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// parseHostAddr parses one address out of a RemoteAddr host or an X-Forwarded-For
// entry, returning the zero Addr (which trustedProxies.has never trusts) for
// anything it cannot read. It accepts the three spellings that turn up in the
// wild — a bare address, a bracketed IPv6 literal, and an address:port, which
// some proxies write — and unmaps ::ffff:a.b.c.d so a dual-stack listener keys
// and matches a v4 client identically however the kernel reported it.
func parseHostAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap()
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}
