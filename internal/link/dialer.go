package link

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// TailnetNode is the narrow view of the Tailnet node this package needs:
// whether it is up (and under what MagicDNS name), and the ability to open an
// outbound connection over it. *tailnet.Manager satisfies it.
//
// Two methods, because that is what the dialer choice takes. Anything wider
// would put the Tailnet state machine inside linking's reach, and this package
// has no business connecting, disconnecting or configuring anything.
type TailnetNode interface {
	Status() tailnet.Status
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

// tailscaleCGNAT is the address range Tailscale assigns its nodes
// (100.64.0.0/10, the RFC 6598 carrier-grade NAT space). An origin written as a
// literal in this range is a Tailnet address by construction and can be reached
// no other way.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// Dialer picks between ADR-0055 §5's two dialers, per origin, with nothing
// configured.
//
// # The rule
//
// The Tailnet is tried when the node is UP and the host is a name under the
// node's own MagicDNS root — the last two labels of its FQDN, so a node called
// obelo.tail1a2b.ts.net has root "ts.net" — or a literal in Tailscale's CGNAT
// range. Anything else goes to the operating system.
//
// THE ROOT IS THE LAST TWO LABELS AND NOT THE NODE'S OWN SUFFIX, and that is
// the one subtle decision here. The obvious rule — "the host ends in
// tail1a2b.ts.net, my own tailnet" — is wrong for the exact case ADR-0055 §5
// exists to serve: a friend SHARES their machine into this operator's Tailnet
// from the Tailscale console, and a shared machine keeps its owner's tailnet in
// its name. It resolves here as obelo.<sharer-tailnet>.ts.net, which the
// obvious rule refuses and which is the only name that will ever be typed.
//
// # And why a failed tailnet dial falls back
//
// The rule above cannot be exact, because one address is genuinely both: a
// Tailscale Funnel origin is the SAME name as the machine's tailnet name, and
// which of the two paths works depends on whether the friend shared the machine
// or opened a Funnel — a fact this Server cannot see. So a tailnet dial that
// fails is retried over the operating system rather than failing the origin.
// The cost is one wasted connection attempt on a path that was never going to
// work; the alternative is refusing to link over a Funnel address at all,
// silently, with a "not reachable" that names the wrong cause.
type Dialer struct {
	// Node is the Tailnet node, or nil on a deployment that has none. Nil is not
	// an error state: it is every server with the feature switched off, and every
	// origin then goes to the operating system, which is the common case.
	Node TailnetNode
	// Net is the operating-system dialer. Nil uses a default with a bounded
	// connect timeout.
	Net *net.Dialer
}

// defaultDialTimeout bounds a single connect attempt. It is short because it is
// spent per ORIGIN and an invite may carry several: an operator pasting a string
// should not sit through a minute of TCP backoff per address before being told
// the friend's server is down.
const defaultDialTimeout = 10 * time.Second

// defaultRequestTimeout bounds one whole HTTP call to a peer, connect included.
const defaultRequestTimeout = 20 * time.Second

// OverTailnet reports whether host (no port) should be dialed over the Tailnet.
// Exported because it is the decision worth testing on its own — the dial that
// follows it is net.Dial either way.
func (d *Dialer) OverTailnet(host string) bool {
	if d == nil || d.Node == nil {
		return false
	}
	st := d.Node.Status()
	if st.State != tailnet.StateRunning {
		// A node that is stopped, needs a login, or whose key has lapsed has no
		// netstack to originate from. This is what makes "nothing is configured"
		// true: turning the Tailnet off does not change one linking setting, it
		// changes what OverTailnet answers.
		return false
	}

	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if addr, err := netip.ParseAddr(h); err == nil {
		return tailscaleCGNAT.Contains(addr)
	}
	root := magicDNSRoot(st.FQDN)
	return root != "" && (h == root || strings.HasSuffix(h, "."+root))
}

// magicDNSRoot is the domain a Tailnet's MagicDNS names live under, taken as the
// last two labels of this node's own FQDN ("obelo.tail1a2b.ts.net" → "ts.net";
// a Headscale node at "obelo.home.arpa" → "home.arpa"). Empty when the node has
// no FQDN yet, which is every node that is not up.
func magicDNSRoot(fqdn string) string {
	labels := strings.Split(strings.ToLower(strings.Trim(strings.TrimSpace(fqdn), ".")), ".")
	if len(labels) < 2 || labels[len(labels)-1] == "" {
		return ""
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// DialContext opens one connection, choosing the dialer by host.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if d.OverTailnet(host) {
		conn, err := d.Node.Dial(ctx, network, addr)
		if err == nil {
			return conn, nil
		}
		// See the type comment: a Funnel origin wears a tailnet name. Falling back
		// costs one refused connection; not falling back costs the link.
	}
	return d.osDialer().DialContext(ctx, network, addr)
}

func (d *Dialer) osDialer() *net.Dialer {
	if d.Net != nil {
		return d.Net
	}
	return &net.Dialer{Timeout: defaultDialTimeout}
}

// HTTPClient builds the client this Server talks to one peer with.
//
// Redirects are REFUSED rather than followed. Every call this package makes is
// to a known path on an origin the operator was handed, and a redirect could
// only take it somewhere else — including off the Tailnet, or to a plain-HTTP
// origin carrying the bearer token in a header. There is no legitimate reason a
// peer's /server or /auth/link/redeem would 302, so a redirect is treated as
// what it is: this origin did not answer.
func (d *Dialer) HTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   defaultDialTimeout,
			ResponseHeaderTimeout: timeout,
			// One connection per peer is plenty and it is not kept: a Link is spoken to
			// in bursts minutes apart, and an idle connection over a Tailnet that comes
			// and goes is a connection that will be found dead at the worst moment.
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// originHost extracts the host of an already-validated origin.
func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
