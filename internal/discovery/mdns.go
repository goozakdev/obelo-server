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
package discovery

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/mdns"
	"github.com/marioquake/juicebox/internal/server"
)

// ServiceType is the DNS-SD service this server advertises under. Clients browse
// for exactly this string, so it is a published interface: changing it breaks
// every deployed client's discovery, silently (they simply find nothing). Treat it
// like a route, not a constant.
const ServiceType = "_juicebox._tcp"

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

// Advertiser holds a running mDNS responder.
type Advertiser struct {
	server *mdns.Server
	// Instance is the advertised instance name, for logging.
	Instance string
	// Port is the advertised port, for logging.
	Port int
}

// Advertise announces this server on the local link and returns a handle to stop
// it. listenAddr is the server's bind address (e.g. ":8080", "0.0.0.0:8099").
//
// A discovered server is always plain http: the server binds plain HTTP and a
// TLS-terminating reverse proxy is, by definition, not on the local link — so no
// scheme is advertised, and clients assume http for what they discover.
func Advertise(identity server.Identity, listenAddr string) (*Advertiser, error) {
	port, err := portFrom(listenAddr)
	if err != nil {
		return nil, err
	}

	instance := instanceName(identity.Name)
	txt := []string{
		txtVersion,
		txtIDKey + identity.ID,
		txtNameKey + identity.Name,
		txtPathKey + APIPath,
	}

	// Empty hostName and nil IPs let the library use the host's name and resolve
	// its own addresses — correct for a multi-homed box, where hardcoding one
	// interface's IP would advertise an address some clients cannot route to.
	svc, err := mdns.NewMDNSService(instance, ServiceType, "", "", port, nil, txt)
	if err != nil {
		return nil, fmt.Errorf("discovery: building service: %w", err)
	}

	srv, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		return nil, fmt.Errorf("discovery: starting responder: %w", err)
	}
	return &Advertiser{server: srv, Instance: instance, Port: port}, nil
}

// Close stops advertising. Safe on a nil Advertiser so callers that never started
// one (advertisement is best-effort) can defer unconditionally.
func (a *Advertiser) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Shutdown()
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
		return "Juice Box"
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
