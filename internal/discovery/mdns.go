// Package discovery advertises this server on the local link via mDNS/Bonjour so
// Apple clients can find it without anyone typing an IP address (ADR-0005, made
// real by ADR-0034).
//
// Advertisement only. This package announces; it never browses, pairs, or
// authenticates. A discovered server still requires a full POST /auth/login —
// discovery replaces *typing an address*, nothing more.
//
// Scope is the local link, permanently. mDNS is link-local by construction, so a
// reverse-proxied or VPN-reachable server is not discoverable and never will be:
// manual address entry stays the primary path for remote access, not a stopgap.
//
// # Why this package resolves its own addresses
//
// The obvious implementation hands the mDNS library an empty host name and a nil
// IP list and lets it "figure it out from the OS". That is what this package used
// to do, and it is silently broken in a container — the single most common way
// this server is deployed. The library resolves os.Hostname() through the normal
// resolver to decide what to put in its A records, but Docker generates the
// container's own /etc/hosts with no entry for that name, and a LAN DNS server has
// never heard of it either. Both lookups fail, NewMDNSService returns an error,
// and because advertisement is best-effort the server boots happily with no
// discovery at all. The operator sees "it works on my Mac, not in Docker" and has
// nothing to go on.
//
// So this package decides both halves itself: it publishes the addresses of the
// host's own link-capable interfaces (see selectIPs), and it publishes them under
// a name in the .local domain (see localHostName). Neither depends on name
// resolution working inside the process's network namespace.
package discovery

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/hashicorp/mdns"
)

// ServiceType is the DNS-SD service this server advertises under. Clients browse
// for exactly this string, so it is a published interface: changing it breaks
// every deployed client's discovery, silently (they simply find nothing). Treat it
// like a route, not a constant.
const ServiceType = "_obelo._tcp"

// TXT record keys. Per RFC 6763 the TXT record is a hint, not a contract — a
// client confirms everything against GET /server once it connects. Kept small and
// deliberately non-authoritative for that reason.
const (
	// txtVersion versions the TXT record's own schema, not the API.
	txtVersion = "txtvers=1"
	// txtPathKey tells a client where the API lives without hardcoding it, so a
	// future prefix change is discoverable rather than breaking.
	txtPathKey = "path="
	// txtIDKey carries the Server identity — the field that makes a DHCP lease
	// change survivable: a client rediscovers by id, updates its base URL, and
	// keeps its token (which is bound to a Device row, not to an address).
	txtIDKey = "id="
	// txtNameKey is the display name for a picker. Cosmetic.
	txtNameKey = "name="
)

// APIPath is the API root advertised in TXT. Mirrors internal/api's prefix.
const APIPath = "/api/v1"

// ipv4Group is the mDNS multicast group. Used here only as a route-lookup target
// to learn which local address the kernel would answer from; nothing is sent.
const ipv4Group = "224.0.0.251:5353"

// fallbackHost names the server when the OS hostname is unusable — a container
// with no hostname set, or a name that sanitizes down to nothing.
const fallbackHost = "obelo"

// Options tune how the advertisement reaches the link. Both exist for the same
// reason: the library's defaults assume a normally-configured host, and a
// container on a Docker host is not one. Both are empty by default and neither is
// needed on a bare-metal install.
type Options struct {
	// IPs, when non-empty, are published verbatim in the A/AAAA records instead
	// of the ones this package would discover. The escape hatch for every case
	// the interface heuristics get wrong — an unusual bridge topology, a
	// multi-homed box that should only be found on one segment, a LAN interface
	// whose name looks virtual. Wired to OBELO_ADVERTISE_IP.
	IPs []string

	// Interface, when non-empty, names the network interface the responder binds
	// its multicast listener to, and restricts the advertised addresses to that
	// interface's own.
	//
	// This is the other half of the container story, and the one that survives a
	// correct A record. The library binds with no interface, which leaves the
	// kernel to pick "the default multicast interface" from the routing table —
	// on a Docker host that is frequently docker0 rather than the LAN NIC, so the
	// group membership lands on a bridge nobody queries and the responder never
	// sees the query at all. Naming the LAN interface pins it. Wired to
	// OBELO_MDNS_INTERFACE.
	Interface string
}

// Advertiser holds a running mDNS responder.
type Advertiser struct {
	server *mdns.Server
	// Instance is the advertised instance name, for logging.
	Instance string
	// Host is the advertised .local host name, for logging.
	Host string
	// Port is the advertised port, for logging.
	Port int
	// IPs are the addresses published in the A/AAAA records, for logging. Worth
	// logging loudly: advertising an address a client cannot route to is the
	// failure this package exists to prevent, and the log line is the only place
	// an operator can catch it before a client does.
	IPs []net.IP
	// Interface is the pinned interface name, or "" when the responder listens on
	// the system default. For logging.
	Interface string
}

// Advertise announces this server on the local link and returns a handle to stop
// it. listenAddr is the server's bind address (e.g. ":8080", "0.0.0.0:8099").
//
// A discovered server is always plain http: the server binds plain HTTP and a
// TLS-terminating reverse proxy is, by definition, not on the local link — so no
// scheme is advertised, and clients assume http for what they discover.
func Advertise(identity server.Identity, listenAddr string, opts Options) (*Advertiser, error) {
	port, err := portFrom(listenAddr)
	if err != nil {
		return nil, err
	}

	iface, err := resolveInterface(opts.Interface)
	if err != nil {
		return nil, err
	}

	ips, err := advertisedIPs(opts.IPs, iface)
	if err != nil {
		return nil, err
	}

	instance := instanceName(identity.Name)
	host := localHostName(osHostname())
	txt := []string{
		txtVersion,
		txtIDKey + identity.ID,
		txtNameKey + identity.Name,
		txtPathKey + APIPath,
	}

	svc, err := mdns.NewMDNSService(instance, ServiceType, "", host, port, ips, txt)
	if err != nil {
		return nil, fmt.Errorf("discovery: building service: %w", err)
	}

	srv, err := mdns.NewServer(&mdns.Config{Zone: svc, Iface: iface, Logger: responderLogger()})
	if err != nil {
		return nil, fmt.Errorf("discovery: starting responder: %w", err)
	}
	return &Advertiser{
		server:    srv,
		Instance:  instance,
		Host:      host,
		Port:      port,
		IPs:       ips,
		Interface: opts.Interface,
	}, nil
}

// Close stops advertising. Safe on a nil Advertiser so callers that never started
// one (advertisement is best-effort) can defer unconditionally.
func (a *Advertiser) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Shutdown()
}

// truncatedBitNotice is the library's complaint about a query it will not parse.
// Matched as a substring because the rest of the line is the entire dumped
// message struct — every record the querier attached, on one line.
const truncatedBitNotice = "support for DNS requests with high truncated bit not implemented"

// responderLogger is the logger the mDNS library logs through. Without it the
// library logs to log.Default(), i.e. straight into the server's own log, where
// its noise reads like Obelo failing.
//
// The noise in question: RFC 6762 §7.2 lets a querier set the TC bit on a query
// to mean "more Known-Answer records are following shortly" — nothing to do with
// the unicast DNS meaning of "this message was cut short". It is ordinary,
// correct behaviour, emitted by any host browsing a link with more known answers
// than fit in one packet (a Mac that can see a chatty AirPrint printer will do it
// all day). hashicorp/mdns has a standing TODO for it and returns an error
// instead, which its receive loop logs at [ERR], once per query, forever.
//
// So it is dropped rather than reworded. Nothing is lost: those queries are other
// devices' business — the questions in them are for services like _ipp._tcp, not
// _obelo._tcp — and a browse for this server that ever did arrive truncated gets
// answered on the querier's next attempt, because browsers re-query.
//
// Everything else the library says is kept and prefixed, so a real responder
// problem still surfaces and is still attributable to mDNS rather than to us.
func responderLogger() *log.Logger {
	return log.New(dropTruncatedBitNotice{out: log.Writer()}, "obelo: mdns: ", log.LstdFlags)
}

// dropTruncatedBitNotice discards whole log lines matching truncatedBitNotice.
// Filtering at the writer is safe because log.Logger emits one Write per Printf,
// so a line is never split across calls and never partially suppressed.
type dropTruncatedBitNotice struct{ out io.Writer }

func (w dropTruncatedBitNotice) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(truncatedBitNotice)) {
		// Report the full write as accepted: a short count is an io.Writer error
		// to the caller, and log.Logger would print *that* to stderr — swapping
		// one line of noise for another.
		return len(p), nil
	}
	return w.out.Write(p)
}

// resolveInterface looks up a configured interface by name. An unknown name is an
// error rather than a silent fallback to the system default: an operator who names
// an interface is working around a responder that is already not being seen, and
// "your typo was ignored, discovery is still broken" is the worst possible answer.
func resolveInterface(name string) (*net.Interface, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("discovery: no network interface named %q: %w", name, err)
	}
	return iface, nil
}

// advertisedIPs decides what goes into the A/AAAA records.
func advertisedIPs(configured []string, iface *net.Interface) ([]net.IP, error) {
	if len(configured) > 0 {
		return parseIPs(configured)
	}

	links, err := readLinks(iface)
	if err != nil {
		return nil, err
	}
	ips := selectIPs(links, preferredSourceIP())
	if len(ips) == 0 {
		return nil, fmt.Errorf("discovery: no usable interface address found%s "+
			"— set OBELO_ADVERTISE_IP to this host's LAN address", forInterface(iface))
	}
	return ips, nil
}

func forInterface(iface *net.Interface) string {
	if iface == nil {
		return ""
	}
	return " on " + iface.Name
}

// parseIPs validates operator-supplied addresses. A malformed entry fails the
// whole advertisement instead of being dropped: the operator set this knob to fix
// discovery, so a typo must surface in the boot log rather than produce a server
// that is advertised at three of the four addresses they listed.
func parseIPs(values []string) ([]net.IP, error) {
	var ips []net.IP
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		ip := net.ParseIP(v)
		if ip == nil {
			return nil, fmt.Errorf("discovery: %q is not a valid IP address", v)
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("discovery: no valid IP address configured")
	}
	return ips, nil
}

// link is one network interface, reduced to what address selection needs. The
// indirection exists so selectIPs is testable: the interesting cases are a Docker
// host's bridges and a downed NIC, neither of which a test can conjure.
type link struct {
	name  string
	flags net.Flags
	addrs []net.IP
}

// readLinks reads the host's interfaces, or just the one that was pinned.
func readLinks(iface *net.Interface) ([]link, error) {
	ifaces := []net.Interface{}
	if iface != nil {
		ifaces = append(ifaces, *iface)
	} else {
		all, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("discovery: listing network interfaces: %w", err)
		}
		ifaces = all
	}

	links := make([]link, 0, len(ifaces))
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			// One unreadable interface must not blind the others.
			continue
		}
		l := link{name: i.Name, flags: i.Flags}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				l.addrs = append(l.addrs, v.IP)
			case *net.IPAddr:
				l.addrs = append(l.addrs, v.IP)
			}
		}
		links = append(links, l)
	}
	return links, nil
}

// selectIPs picks the addresses worth publishing, most-likely-reachable first.
//
// preferred, when non-nil, is the address the kernel would send from to reach the
// mDNS group; it is hoisted to the front because a client resolves to a list and
// tries it in order. An address that is merely *wrong* is not free — a client on
// another subnet that tries 172.17.0.1 first waits out a full TCP timeout before
// falling through, which reads as "the server is broken", not "one record was
// stale". Hence also the virtual-link filter: publishing a Docker bridge address
// to the LAN can only cost a client time.
func selectIPs(links []link, preferred net.IP) []net.IP {
	var ips []net.IP
	seen := map[string]bool{}
	for _, l := range links {
		if !usableLink(l) {
			continue
		}
		for _, ip := range l.addrs {
			// IsGlobalUnicast rejects loopback, multicast, unspecified, and
			// link-local (fe80::/169.254) in one predicate. A link-local address
			// in an A record is useless to a client anyway: it carries no zone,
			// so the client cannot tell which of its own interfaces it belongs to.
			if ip == nil || !ip.IsGlobalUnicast() {
				continue
			}
			if key := ip.String(); !seen[key] {
				seen[key] = true
				ips = append(ips, ip)
			}
		}
	}

	if preferred != nil {
		for i, ip := range ips {
			if ip.Equal(preferred) {
				copy(ips[1:i+1], ips[:i])
				ips[0] = ip
				break
			}
		}
	}
	return ips
}

// usableLink reports whether an interface can carry mDNS at all.
func usableLink(l link) bool {
	if l.flags&net.FlagUp == 0 {
		return false
	}
	if l.flags&net.FlagLoopback != 0 {
		return false
	}
	// No multicast, no mDNS. This alone excludes most VPN tunnels (wg0, tailscale0,
	// utun*), which is correct — they are not a local link.
	if l.flags&net.FlagMulticast == 0 {
		return false
	}
	return !isVirtualLink(l.name)
}

// virtualLinkPrefixes name interfaces that are up, multicast-capable, and still
// not somewhere a client lives: container and VM bridges.
//
// This is a heuristic on names, which is exactly as fragile as it sounds — hence
// OBELO_ADVERTISE_IP, which bypasses it entirely. The prefixes are chosen to be
// unambiguous: "br-" is Docker's user-defined-network bridge naming and does NOT
// match a hand-built LAN bridge like br0 (Proxmox, KVM hosts), which is a real
// interface a real client is reachable through.
var virtualLinkPrefixes = []string{
	"docker",    // docker0
	"br-",       // docker user-defined networks
	"veth",      // container side of a bridge pair
	"virbr",     // libvirt
	"vmnet",     // VMware
	"vmenet",    // macOS virtualization framework
	"bridge",    // macOS bridge100 (Docker Desktop, VM host networking)
	"podman",    // podman0
	"cni",       // CNI plugins
	"flannel",   // k8s overlay
	"cali",      // Calico
	"kube",      // kube-ipvs0 and friends
	"tailscale", // tailscale0, if it ever reports multicast
	"zt",        // ZeroTier
}

func isVirtualLink(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualLinkPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// preferredSourceIP asks the kernel which local address it would use to reach the
// mDNS group. A connected UDP socket performs the route lookup and binds a local
// address without sending a packet, so this is a cheap question with no wire
// effect. Best-effort: a nil answer just means no reordering.
func preferredSourceIP() net.IP {
	c, err := net.Dial("udp4", ipv4Group)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	if addr, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP
	}
	return nil
}

// osHostname returns the host's name, or "" if the OS will not say.
func osHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// localHostName turns an OS hostname into the fully-qualified .local name the
// SRV record points at, and the A/AAAA records answer for.
//
// The .local suffix is load-bearing, not decoration. Apple's resolver only sends
// names in the .local domain to multicast; a bare "nuc." — which is what a Linux
// hostname yields — is a single-label name it will hand to unicast DNS instead,
// where nothing can answer. The service would be discovered and then fail to
// resolve, which looks like a client bug. macOS hosts happen to escape this
// because their hostname is already "something.local"; Linux hosts do not, which
// is one more reason this used to work on a Mac and not on a server.
func localHostName(raw string) string {
	name := DNSLabel(raw)
	if name == "" {
		name = fallbackHost
	}
	return name + ".local."
}

// DNSLabel reduces an arbitrary operator-supplied name to a single legal DNS
// label: the first dotted component, with every character that is not a letter,
// digit, or hyphen replaced by a hyphen, stripped of leading/trailing hyphens and
// capped at the 63-octet label limit. It returns "" when nothing legal survives —
// the CALLER supplies the fallback, because two callers that want the same
// sanitizing rule are not obliged to want the same replacement name.
//
// It is exported for the Tailnet node's MagicDNS hostname (ADR-0043), which is
// the same problem this function already solved for the `.local` name: an
// operator-typed server name becoming a DNS label. A second, subtly different
// copy of this rule is precisely how the two names drift apart — and the symptom
// is an operator who corrected their server's name in one place and is now
// debugging why the house reaches it under two spellings.
func DNSLabel(raw string) string {
	// Keep only the first label: "nuc.lan" and "nuc.local" are both the host "nuc"
	// as far as the link is concerned, and re-appending .local to an existing
	// suffix would publish "nuc.local.local.".
	name, _, _ := strings.Cut(strings.TrimSpace(raw), ".")

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			// Anything else (underscores, spaces, non-ASCII) is not a legal host
			// label. Replace rather than drop so "media server" stays two words.
			b.WriteRune('-')
		}
	}
	name = strings.Trim(b.String(), "-")

	// DNS labels cap at 63 octets; every kept rune here is one octet.
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// portFrom extracts the port from a bind address. ":8080" and "0.0.0.0:8099" both
// parse; a missing or non-numeric port is an error rather than a guess, because
// advertising the wrong port produces a server that is discoverable and
// unreachable — worse than not being discoverable at all.
func portFrom(listenAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return 0, fmt.Errorf("discovery: parsing listen address %q: %w", listenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("discovery: listen address %q has no usable port", listenAddr)
	}
	return port, nil
}

// instanceName makes a display name safe to embed in a DNS-SD instance label.
//
// DNS-SD instance names are rich UTF-8, but they are assembled into a dotted FQDN
// (<instance>.<service>.<domain>), so an unescaped dot splits the label and
// produces a malformed record. Replace rather than reject: an operator who names
// their server "living.room" should get a working server, not a boot failure.
func instanceName(name string) string {
	n := strings.TrimSpace(name)
	n = strings.ReplaceAll(n, ".", " ")
	n = strings.Join(strings.Fields(n), " ") // collapse whitespace runs
	if n == "" {
		return "Obelo"
	}
	// DNS labels cap at 63 octets. Truncate on a rune boundary so a multi-byte
	// name doesn't get cut mid-character into invalid UTF-8.
	if len(n) > 63 {
		trimmed := []rune(n)
		for len(string(trimmed)) > 63 {
			trimmed = trimmed[:len(trimmed)-1]
		}
		n = strings.TrimSpace(string(trimmed))
	}
	return n
}
