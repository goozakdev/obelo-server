package plugins

import "testing"

// ResolveAs is resolveAs for the external test package: every name the
// private-address check asks about resolves to addr for the rest of the test.
func ResolveAs(t *testing.T, addr string) { resolveAs(t, addr) }
