package link

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// ADR-0055 §5's two dialers. The interesting property is not that either one
// opens a socket — net.Dial does that — it is WHICH ONE an origin gets, with
// nothing configured, and the fact that a node which is not up takes every
// origin back to the operating system.

func runningNode(fqdn string) *tailnet.Fake {
	st := tailnet.Status{State: tailnet.StateRunning, FQDN: fqdn}
	f := &tailnet.Fake{Fresh: st, Returning: st}
	f.Transition(st)
	return f
}

func TestDialerChoosesTheTailnetForMagicDNSNames(t *testing.T) {
	node := runningNode("obelo.tail1a2b.ts.net")
	d := &Dialer{Node: node}

	cases := []struct {
		name string
		host string
		want bool
	}{
		// The node's own tailnet.
		{"a peer on this operator's own tailnet", "nas.tail1a2b.ts.net", true},
		// THE CASE THE RULE EXISTS FOR (ADR-0055 §5). A friend shares their Obelo
		// machine into this operator's Tailnet from the Tailscale console, and a
		// shared machine keeps its OWNER's tailnet in its name. A rule written as
		// "ends in my own tailnet suffix" refuses exactly the address that will be
		// typed, which is why the root is the last two labels.
		{"a machine shared in from another tailnet", "obelo.tailf00d.ts.net", true},
		{"the root itself", "ts.net", true},
		{"case and a trailing dot are not a different name", "OBELO.TailF00D.ts.net.", true},
		// A Tailscale address literal (100.64.0.0/10) can be reached no other way.
		{"a tailnet address literal", "100.101.102.103", true},
		{"a literal just outside the range", "100.63.255.255", false},
		// Everything else is an origin on the internet: a port-forward with an ACME
		// certificate, a reverse proxy, a Funnel address. One HTTP client, no new
		// code (ADR-0055 §5).
		{"a public hostname", "media.example.org", false},
		{"a LAN address", "192.168.1.10", false},
		{"a name that merely contains the root", "ts.net.example.org", false},
		{"a name that ends in the root as a substring, not a label", "notts.net", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.OverTailnet(tc.host); got != tc.want {
				t.Errorf("OverTailnet(%q) = %t, want %t", tc.host, got, tc.want)
			}
		})
	}
}

// TestDialerFallsBackToTheOSWhenTheNodeIsNotUp: nothing is configured, so
// turning the Tailnet off does not change a linking setting — it changes what
// this answers. Every state but running has no netstack to originate from.
func TestDialerFallsBackToTheOSWhenTheNodeIsNotUp(t *testing.T) {
	host := "obelo.tail1a2b.ts.net"

	t.Run("no node at all (a build or a household without one)", func(t *testing.T) {
		d := &Dialer{}
		if d.OverTailnet(host) {
			t.Error("a Server with no Tailnet node chose the tailnet dialer")
		}
	})

	for _, st := range []tailnet.Status{
		{State: tailnet.StateStopped},
		{State: tailnet.StateStarting},
		{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/x"},
		{State: tailnet.StateKeyExpired, FQDN: host},
		{State: tailnet.StateError, LastError: "boom"},
	} {
		t.Run(string(st.State), func(t *testing.T) {
			node := &tailnet.Fake{}
			node.Transition(st)
			d := &Dialer{Node: node}
			if d.OverTailnet(host) {
				t.Errorf("a node in state %q chose the tailnet dialer", st.State)
			}
		})
	}

	t.Run("running but with no FQDN yet", func(t *testing.T) {
		node := &tailnet.Fake{}
		node.Transition(tailnet.Status{State: tailnet.StateRunning})
		d := &Dialer{Node: node}
		if d.OverTailnet(host) {
			t.Error("a node with no MagicDNS name of its own has no root to compare against")
		}
	})
}

// TestDialContextRoutesEachOriginThroughTheChosenDialer is the end of the
// argument: the choice above actually changes which socket opens. One HTTP
// server, two names for it, and the Fake counting what went over the Tailnet.
func TestDialContextRoutesEachOriginThroughTheChosenDialer(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer peer.Close()

	node := runningNode("obelo.tail1a2b.ts.net")
	// The MagicDNS name resolves nowhere on this machine; the node is what makes
	// it reachable, which is exactly the production situation.
	node.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, peer.Listener.Addr().String())
	}
	client := (&Dialer{Node: node}).HTTPClient(0)

	resp, err := client.Get("http://obelo.tailf00d.ts.net/")
	if err != nil {
		t.Fatalf("fetching a tailnet origin: %v", err)
	}
	closeBody(resp)
	if node.Dials() != 1 {
		t.Fatalf("tailnet dials = %d, want 1 — that origin did not go over the Tailnet", node.Dials())
	}
	if addrs := node.DialAddrs(); len(addrs) != 1 || addrs[0] != "obelo.tailf00d.ts.net:80" {
		t.Errorf("tailnet dialed %v, want the origin's host and port", addrs)
	}

	// The same server, addressed as an ordinary origin. The Fake must not see it:
	// a count that does not move is the only way to prove a connection went out
	// over the operating system.
	resp, err = client.Get(peer.URL + "/")
	if err != nil {
		t.Fatalf("fetching a plain origin: %v", err)
	}
	closeBody(resp)
	if node.Dials() != 1 {
		t.Errorf("tailnet dials = %d after a plain origin, want it unchanged at 1", node.Dials())
	}
}

// TestATailnetDialThatFailsFallsBackToTheOS is the one impurity in the rule,
// and it is deliberate: a Tailscale Funnel origin is the SAME name as the
// machine's tailnet name, and which of the two paths works depends on a fact
// this Server cannot see. Refusing to fall back would make a Funnel address
// unlinkable, silently, with a "not reachable" naming the wrong cause.
func TestATailnetDialThatFailsFallsBackToTheOS(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer peer.Close()

	node := runningNode("obelo.tail1a2b.ts.net")
	node.DialErr = errors.New("no route to that peer: it was never shared with you")

	// The host is a MagicDNS name that ALSO resolves through the OS — which is
	// what a Funnel address is. Loopback stands in for that resolution.
	_, port, err := net.SplitHostPort(peer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	d := &Dialer{Node: node}
	conn, err := d.DialContext(context.Background(), "tcp", "obelo.tailf00d.ts.net:"+port)
	if err == nil {
		conn.Close()
		t.Fatal("expected the fallback to try the OS resolver and fail on an unresolvable name")
	}
	if node.Dials() != 1 {
		t.Errorf("tailnet dials = %d, want 1 — the tailnet was not tried first", node.Dials())
	}
	// The fallback happened: what failed is the OS lookup of a name that resolves
	// nowhere, NOT the scripted tailnet error.
	if errors.Is(err, node.DialErr) {
		t.Error("the tailnet error was returned; the OS dialer was never tried")
	}
}
