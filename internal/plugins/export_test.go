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

// testSlots bounds how many of this package's parallel tests run at once. Nearly
// every one calls a guest under a wall-clock budget, and under amd64 emulation
// (make check-amd64) a hundred-odd guests started together overrun those budgets.
// A few at a time keeps most of the speed-up and leaves every budget its margin.
var testSlots = make(chan struct{}, 4)

// parallel is t.Parallel holding one of testSlots until the test and its
// subtests have finished. Tests here call it instead of t.Parallel.
//
// Call it only from a top-level test, never from a subtest of one that already
// holds a slot: the subtest's t.Parallel() would pause it until its parent
// returns, but the parent is waiting on t.Cleanup to release the very slot the
// subtest is now blocked trying to acquire — a deadlock.
func parallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	testSlots <- struct{}{}
	t.Cleanup(func() { <-testSlots })
}

// Parallel is parallel for the external test package.
func Parallel(t *testing.T) { t.Helper(); parallel(t) }

// CheckUpgradeVersion is checkUpgradeVersion for the external test package: nil when
// offered is a strictly higher semantic version than installed.
func CheckUpgradeVersion(id, installed, offered string) error {
	return checkUpgradeVersion(id, installed, offered)
}

// SetUpgradeHookForTest makes every upgrade report the aside directory it uses.
func (m *Manager) SetUpgradeHookForTest(h func(aside string)) { m.onAside = h }
