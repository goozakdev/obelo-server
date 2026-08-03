package discovery

import (
	"net"
	"strings"
	"testing"
)

func TestPortFrom(t *testing.T) {
	// Advertising the wrong port yields a server that is discoverable and
	// unreachable — strictly worse than not being discoverable. So parse, never
	// guess.
	ok := map[string]int{
		":8080":          8080,
		"0.0.0.0:8099":   8099,
		"127.0.0.1:8080": 8080,
		"  :8080  ":      8080,
		"[::]:8080":      8080,
		"[::1]:9000":     9000,
	}
	for addr, want := range ok {
		got, err := portFrom(addr)
		if err != nil {
			t.Errorf("portFrom(%q) errored: %v", addr, err)
			continue
		}
		if got != want {
			t.Errorf("portFrom(%q) = %d, want %d", addr, got, want)
		}
	}

	for _, addr := range []string{"", "8080", "0.0.0.0", ":", ":0", ":notaport", ":99999"} {
		if got, err := portFrom(addr); err == nil {
			t.Errorf("portFrom(%q) = %d, want an error", addr, got)
		}
	}
}

func TestInstanceNameEscapesDots(t *testing.T) {
	// The instance name is assembled into <instance>.<service>.<domain>, so a raw
	// dot splits the label and produces a malformed record.
	got := instanceName("living.room")
	if strings.Contains(got, ".") {
		t.Fatalf("instanceName(%q) = %q, still contains a dot", "living.room", got)
	}
}

func TestInstanceNameNeverEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", ".", "..."} {
		if got := instanceName(in); got == "" {
			t.Errorf("instanceName(%q) = %q, want a fallback", in, got)
		}
	}
}

func TestInstanceNameFitsDNSLabel(t *testing.T) {
	// DNS labels cap at 63 octets.
	got := instanceName(strings.Repeat("a", 200))
	if len(got) > 63 {
		t.Fatalf("instanceName length = %d, want <= 63", len(got))
	}
}

func TestInstanceNameTruncatesOnRuneBoundary(t *testing.T) {
	// Cutting a multi-byte name at byte 63 would emit invalid UTF-8 into a record
	// that Bonjour then rejects — a failure that would look like "discovery just
	// doesn't work" rather than a name bug.
	got := instanceName(strings.Repeat("é", 100))
	if len(got) > 63 {
		t.Fatalf("length = %d, want <= 63", len(got))
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("invalid UTF-8 at byte %d in %q", i, got)
		}
	}
}

func TestInstanceNameKeepsOrdinaryNames(t *testing.T) {
	if got := instanceName("Living Room"); got != "Living Room" {
		t.Fatalf("instanceName(%q) = %q, want it unchanged", "Living Room", got)
	}
}

// up is the flag set of an ordinary, usable LAN interface.
const up = net.FlagUp | net.FlagMulticast | net.FlagBroadcast | net.FlagRunning

func ipsOf(t *testing.T, addrs ...string) []net.IP {
	t.Helper()
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			t.Fatalf("test setup: %q is not an IP", a)
		}
		out = append(out, ip)
	}
	return out
}

func strsOf(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func TestSelectIPsSkipsUnreachableAddresses(t *testing.T) {
	// Everything here is an address a client either cannot use or cannot route to.
	// Publishing one is not free: a client resolves to a list and tries it in
	// order, so a bad entry costs a TCP timeout and reads as "the server is
	// broken".
	links := []link{
		{name: "lo", flags: net.FlagUp | net.FlagLoopback, addrs: ipsOf(t, "127.0.0.1", "::1")},
		{name: "eth1", flags: net.FlagMulticast, addrs: ipsOf(t, "192.168.9.9")},        // down
		{name: "wg0", flags: net.FlagUp, addrs: ipsOf(t, "100.64.0.2")},                 // no multicast
		{name: "docker0", flags: up, addrs: ipsOf(t, "172.17.0.1")},                     // container bridge
		{name: "br-1a2b3c", flags: up, addrs: ipsOf(t, "172.18.0.1")},                   // docker user network
		{name: "veth7f3a", flags: up, addrs: ipsOf(t, "172.19.0.1")},                    // container pair
		{name: "eth0", flags: up, addrs: ipsOf(t, "169.254.7.7", "fe80::1", "0.0.0.0")}, // link-local/unspecified
	}
	if got := selectIPs(links, nil); len(got) != 0 {
		t.Fatalf("selectIPs = %v, want none of these advertised", strsOf(got))
	}
}

func TestSelectIPsKeepsRealLANAddresses(t *testing.T) {
	// br0 is the trap: a hand-built LAN bridge (Proxmox, KVM hosts) is a real
	// interface real clients reach the server through, and the virtual-link filter
	// must not eat it just because Docker names its own bridges br-<hash>.
	links := []link{
		{name: "eth0", flags: up, addrs: ipsOf(t, "192.168.1.50")},
		{name: "br0", flags: up, addrs: ipsOf(t, "10.0.0.5")},
		{name: "eth0.100", flags: up, addrs: ipsOf(t, "2001:db8::1")},
	}
	want := []string{"192.168.1.50", "10.0.0.5", "2001:db8::1"}
	got := strsOf(selectIPs(links, nil))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selectIPs = %v, want %v", got, want)
	}
}

func TestSelectIPsDedupes(t *testing.T) {
	// An alias or a second address family on the same wire must not publish the
	// same A record twice.
	links := []link{
		{name: "eth0", flags: up, addrs: ipsOf(t, "192.168.1.50", "192.168.1.50")},
		{name: "eth0:1", flags: up, addrs: ipsOf(t, "192.168.1.50")},
	}
	if got := selectIPs(links, nil); len(got) != 1 {
		t.Fatalf("selectIPs = %v, want one address", strsOf(got))
	}
}

func TestSelectIPsHoistsThePreferredSource(t *testing.T) {
	// The address the kernel would send from is the one most likely to work, and a
	// client tries the list in order.
	links := []link{
		{name: "eth0", flags: up, addrs: ipsOf(t, "10.0.0.5")},
		{name: "eth1", flags: up, addrs: ipsOf(t, "192.168.1.50")},
		{name: "eth2", flags: up, addrs: ipsOf(t, "172.20.0.9")},
	}
	got := strsOf(selectIPs(links, net.ParseIP("192.168.1.50")))
	want := []string{"192.168.1.50", "10.0.0.5", "172.20.0.9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selectIPs = %v, want %v (preferred first, rest in order)", got, want)
	}

	// A preferred address that did not survive the filter changes nothing.
	got = strsOf(selectIPs(links, net.ParseIP("172.17.0.1")))
	want = []string{"10.0.0.5", "192.168.1.50", "172.20.0.9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selectIPs = %v, want %v unchanged", got, want)
	}
}

func TestParseIPsRejectsGarbage(t *testing.T) {
	// An operator sets JUICEBOX_ADVERTISE_IP because discovery is already broken.
	// Dropping the entry they typo'd and advertising the rest would leave them
	// debugging a half-configured server; failing loudly puts it in the boot log.
	if _, err := parseIPs([]string{"192.168.1.50", "not-an-ip"}); err == nil {
		t.Fatal("parseIPs accepted a malformed address")
	}
	if _, err := parseIPs([]string{"", "   "}); err == nil {
		t.Fatal("parseIPs accepted a list with no addresses in it")
	}

	got, err := parseIPs([]string{" 192.168.1.50 ", "fd00::1"})
	if err != nil {
		t.Fatalf("parseIPs errored: %v", err)
	}
	if want := []string{"192.168.1.50", "fd00::1"}; strings.Join(strsOf(got), ",") != strings.Join(want, ",") {
		t.Fatalf("parseIPs = %v, want %v", strsOf(got), want)
	}
}

func TestLocalHostNameIsAlwaysInTheLocalDomain(t *testing.T) {
	// The .local suffix is load-bearing: Apple's resolver only multicasts names in
	// .local, so a bare Linux hostname ("nuc.") goes to unicast DNS, where nothing
	// answers — the service is discovered and then fails to resolve.
	cases := map[string]string{
		"nuc":             "nuc.local.",
		"nuc.lan":         "nuc.local.",
		"nuc.local":       "nuc.local.", // never nuc.local.local.
		"  Living Room  ": "Living-Room.local.",
		"media_server":    "media-server.local.",
		"":                fallbackHost + ".local.",
		"...":             fallbackHost + ".local.",
		"-":               fallbackHost + ".local.",
		"3f2a9b1c4d5e":    "3f2a9b1c4d5e.local.", // a container id is a fine label
	}
	for in, want := range cases {
		if got := localHostName(in); got != want {
			t.Errorf("localHostName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalHostNameFitsADNSLabel(t *testing.T) {
	got := localHostName(strings.Repeat("a", 100))
	label := strings.TrimSuffix(got, ".local.")
	if len(label) > 63 {
		t.Fatalf("label length = %d, want <= 63", len(label))
	}
	if !strings.HasSuffix(got, ".local.") {
		t.Fatalf("localHostName = %q, want a .local. suffix", got)
	}
}

func TestServiceTypeIsThePublishedContract(t *testing.T) {
	// Clients browse for exactly this string. Changing it breaks discovery for
	// every deployed client, silently — they just find nothing. This test exists
	// to make that change deliberate rather than incidental.
	if ServiceType != "_juicebox._tcp" {
		t.Fatalf("ServiceType = %q — this is a published interface (ADR-0034); "+
			"changing it strands every deployed client", ServiceType)
	}
	if APIPath != "/api/v1" {
		t.Fatalf("APIPath = %q, want /api/v1 (must mirror internal/api's prefix)", APIPath)
	}
}
