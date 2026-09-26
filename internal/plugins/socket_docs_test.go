package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestThePreStartTLSCapIsTheOneTheGuideAndContractState: the one plaintext write
// a StartTLS connection carries before its upgrade is capped, and an author who
// sends a longer upgrade request is refused. The cap is stated where an author
// reads — the authoring guide and the contract's socket comment — as this
// package's own number, so changing one without the others fails here.
func TestThePreStartTLSCapIsTheOneTheGuideAndContractState(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	want := fmt.Sprintf("at most %d bytes", maxUpgradeRequest)
	for _, rel := range []string{"docs/plugins/authoring.md", "pluginapi/v1/socket.go"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		// Prose wraps and a Go comment carries its slashes, so both are read as
		// one line of words.
		words := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "//", " ")), " ")
		if !strings.Contains(words, want) {
			t.Errorf("%s does not state the pre-StartTLS cap as %q", rel, want)
		}
	}
}
