package plugins

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// ResolveAs is resolveAs for the external test package: every name the
// private-address check asks about resolves to addr for the rest of the test.
func ResolveAs(t *testing.T, addr string) { resolveAs(t, addr) }

// CheckDialedAddress is checkDialedAddress for the external test package: the
// judgement the fetch dialer makes of the address it is about to connect to.
func CheckDialedAddress(address string) error { return checkDialedAddress("tcp", address, nil) }

// DialCheckedClients is dialCheckedClients for the external test package: the
// checked and operator clients a plugin fetch is made with.
func DialCheckedClients(c *http.Client) (checked, operator *http.Client) {
	return dialCheckedClients(c)
}

// ExemptDialTo is ctx as a fetch to the operator's host and port carries it: the
// one address the operator client dials unchecked.
func ExemptDialTo(ctx context.Context, host, port string) context.Context {
	return context.WithValue(ctx, exemptAddrKey{}, dialKey(host, port))
}

// LookupIPAddr is the resolver the private-address check asks, ResolveAs's
// answer included.
func LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return lookupIPAddr(ctx, host)
}
