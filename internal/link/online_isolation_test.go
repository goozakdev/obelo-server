package link

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLinkSyncDoesNotDependOnOnlineSources: the code that mirrors and relays a
// linked server's catalog does not import the Online source package, directly or
// through anything it uses (ADR-0068 decision 5: a source is never over a Link).
func TestLinkSyncDoesNotDependOnOnlineSources(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasSuffix(pkg, "/onlinesource") || strings.Contains(pkg, "/onlinesource/") {
			t.Fatalf("internal/link depends on %s", pkg)
		}
	}
}
