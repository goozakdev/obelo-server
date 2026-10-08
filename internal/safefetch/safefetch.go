// Package safefetch holds the ONE redirect policy every server-side fetch of a
// third-party URL runs under.
//
// # Why a package, and why only one of these
//
// Three packages fetch URLs this server did not choose: internal/api (the
// provider-image proxy), internal/enrich (artwork downloads, and the metadata JSON
// clients) and internal/subfetch (the time-limited subtitle download link). None of
// them can import another's transport helpers without a dependency knot, so the
// policy would otherwise exist three times — and the copy that drifted would be the
// one nobody was looking at. It lives here so a change to the rule is a change to
// every caller, by construction.
//
// The threat is the same everywhere: the URL, or the redirect off it, is chosen by
// a metadata/subtitle provider — a third party — not by the operator. A compromised
// or hostile provider (or a hostile DNS answer for one) that can aim a hop at
// 127.0.0.1, an RFC1918 address, or 169.254.169.254 gets this server to read those
// bytes on its behalf, and in the artwork case the bytes land in the artwork cache
// and then in an admin's browser. Nobody consented to that hop.
package safefetch

import (
	"errors"
	"net"
	"net/http"
	"time"
)

// MaxRedirects bounds the redirect chain. Refusing redirects outright is not an
// option — the Cover Art Archive answers every cover with a redirect into
// archive.org, and that is the normal, working path — so the chain is bounded
// instead of banned. Three hops covers coverartarchive → archive.org → the storage
// node it picks, which is the real chain that motivated this number. LOWERING IT
// BREAKS MUSIC ARTWORK; if a provider legitimately needs more, raise it here once
// rather than per caller.
const MaxRedirects = 3

// ErrRedirectBlocked is returned from CheckRedirect when a hop tries to land
// somewhere we will not follow. Callers get it wrapped in a *url.Error out of
// Client.Do, and most of them collapse it into whatever their ordinary "upstream
// failed" outcome is — the provider-image handler answers one 404 for every
// failure on purpose, so this is not an oracle for what some other host said.
var ErrRedirectBlocked = errors.New("safefetch: redirect blocked")

// CheckRedirect bounds the redirect chain and refuses a hop that resolves to a
// loopback/private/link-local address. It is the CheckRedirect for every client
// this package hands out.
//
// # The asymmetry, which is deliberate
//
// This checks the redirect TARGET only, and NEVER the initial request. A
// self-hosted operator may legitimately point OBELO_TMDB_IMAGE_BASE_URL, a
// per-provider imageBaseURL, or a subtitle provider's base URL at a mirror on their
// own LAN (ADR-0001 — that is the spirit of this product, not an attack), so
// validating the first hop would break exactly the deployment this product is for.
// What the operator did not choose is where that mirror then bounces us: if one
// genuinely needs an internal redirect, the fix is to configure the final URL, not
// to loosen this.
//
// DO NOT "improve" this by also validating the initial URL. The one arguable case
// is the admin-supplied URL in PUT /titles/{id}/artwork, and it is a much smaller
// escalation than the provider-initiated shape — an admin can already add any path
// on disk as a library root — so trading LAN mirrors for it would be a bad deal.
//
// A name that will not resolve is refused rather than passed to the transport:
// failing closed on a lookup error costs a thumbnail, and the alternative leaves
// the decision to a resolver whose answer we never saw.
//
// # Known limitation: DNS rebinding
//
// The lookup here and the transport's own dial are two separate resolutions, so a
// resolver that answers public for this check and private for the dial gets through
// — the classic rebinding window. Closing it properly means inspecting the address
// actually being connected to, in a net.Dialer.Control hook. That was considered
// and NOT done, because Control sees only an address: it cannot tell a first-hop
// dial (which must be allowed to be private, see above) from a redirect-hop dial,
// and the only ways to tell them apart — threading a marker through the request
// context that CheckRedirect rewrites, or pinning the target to the resolved IP
// literal and hand-setting TLS ServerName — are fragile in a way this control must
// not be. The residual risk is smaller than the one closed here: it needs the
// attacker to control the resolver's answers, not merely a Location header.
//
// Re-assessed and still NOT done (review R06-03): the dial-time check would also
// have to cover the transports behind Guard, which this package does not own (it
// copies a caller's client and shares its Transport), so it cannot be applied here
// without changing every caller's wiring.
//
// A PLUGIN's fetches do not carry this limitation. There the first hop is checked
// too, bar the operator's own host, so internal/plugins puts the check on the
// dialer (checkDialedAddress) and every other dial is judged by the address it
// connects to — except through an HTTP(S) proxy the operator configured, where
// the dial is to the proxy and the limitation above holds (dialCheckedClients).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= MaxRedirects {
		return ErrRedirectBlocked
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return ErrRedirectBlocked
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(req.Context(), req.URL.Hostname())
	if err != nil || len(addrs) == 0 {
		return ErrRedirectBlocked
	}
	for _, a := range addrs {
		if IsInternalIP(a.IP) || nonRoutableForRedirect(a.IP) {
			return ErrRedirectBlocked
		}
	}
	return nil
}

// redirectOnlyDenied are ranges a redirect hop must not land in but IsInternalIP
// (shared with the plugin host) deliberately does not cover: 0.0.0.0/8 ("this
// network", which some stacks route to loopback), 100.64.0.0/10 (the CGNAT space
// Tailscale hands its nodes) and 240.0.0.0/4 (reserved, incl. broadcast).
//
// The Tailnet this server runs is a userspace tsnet node: its peers are dialled
// through the node itself (link.Dialer), never through the OS stack and never
// through a safefetch client, so no legitimate fetch needs 100.x here. On a host
// that ALSO runs Tailscale, though, the OS stack does route 100.x, and a provider
// redirect into it would reach the operator's tailnet peers; that is the case this
// closes. The first hop is still unchecked by design (see CheckRedirect).
var redirectOnlyDenied = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "240.0.0.0/4"} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("safefetch: bad CIDR " + c)
		}
		nets = append(nets, n)
	}
	return nets
}()

func nonRoutableForRedirect(ip net.IP) bool {
	for _, n := range redirectOnlyDenied {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsBlockedAddress reports whether ip is an address a third-party URL must not
// resolve to: IsInternalIP plus the redirect-only ranges (this-network, CGNAT,
// reserved). It is the judgment CheckRedirect applies to a hop, exported for a
// caller that applies it to a FIRST URL a Plugin supplied (Online source media).
func IsBlockedAddress(ip net.IP) bool {
	return IsInternalIP(ip) || nonRoutableForRedirect(ip)
}

// IsInternalIP reports whether ip is somewhere a redirect must not take us: the
// loopback, RFC1918/ULA private space, link-local (which includes the cloud
// metadata address 169.254.169.254), the unspecified address, and multicast.
func IsInternalIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// Client builds an http.Client that carries the policy, with the timeout the caller
// needs. This is the sanctioned way to construct a client for a third-party fetch.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: CheckRedirect,
	}
}

// Guard returns a COPY of c carrying the policy, for the callers that accept an
// injectable *http.Client (enrich.HTTPArtworkFetcher.HTTPClient, every provider's
// HTTPClient field) and therefore cannot rely on having built it themselves.
//
// A copy, never a mutation, for two reasons that pull in the same direction.
// Reaching into a caller-supplied client and overwriting a field it set is rude and
// surprising — and one of the values that reaches here is http.DefaultClient, which
// is process-global, so mutating it would silently change the redirect behaviour of
// every unrelated HTTP call in the process. The copy shares the Transport, so
// connection pooling and any injected RoundTripper are preserved; only the redirect
// decision is ours.
//
// The consequence worth stating plainly: applying this at the point of USE rather
// than only at construction is what makes the policy impossible for the production
// wiring to bypass. app.go builds enrich.HTTPArtworkFetcher{} with a nil client
// today, but a future line assigning a bare &http.Client{} would otherwise disarm
// the whole control with every test still green.
//
// Guard(nil) yields a guarded stand-in for http.DefaultClient — same zero Transport
// and zero Timeout, which is what the nil-client callers used before.
func Guard(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{CheckRedirect: CheckRedirect}
	}
	guarded := *c
	guarded.CheckRedirect = CheckRedirect
	return &guarded
}
