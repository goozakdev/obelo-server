package plugins

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/transcode"
)

// TestTheOnlineFFmpegResidualRisksAreNamedWhereAnAuthorAndADeciderRead: the ffmpeg
// path checks only the first URL (ADR-0068 decision 9), which leaves two risks the
// project accepted and promised to document. Both the authoring guide and the ADR
// must name each of them, and the protocol whitelist the guide quotes must be the
// one the code applies.
func TestTheOnlineFFmpegResidualRisksAreNamedWhereAnAuthorAndADeciderRead(t *testing.T) {
	parallel(t)
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	adrs, err := filepath.Glob(filepath.Join(root, "docs", "adr", "0068-*.md"))
	if err != nil || len(adrs) != 1 {
		t.Fatalf("ADR-0068 not found (%v, %v)", adrs, err)
	}
	for _, doc := range []string{filepath.Join(root, "docs", "plugins", "authoring.md"), adrs[0]} {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		// Prose wraps, so the document is read as one line of words.
		words := strings.Join(strings.Fields(string(raw)), " ")
		for what, want := range map[string]string{
			"the later-hop risk (a redirect or a manifest hop reaching a LAN address)": "a redirect or a manifest hop",
			"the DNS-rebinding risk of the first URL":                                  "rebinding",
			"the protocol whitelist":                                                   "-protocol_whitelist " + transcode.OnlineProtocolWhitelist,
		} {
			if !strings.Contains(words, want) {
				t.Errorf("%s does not name %s (it should contain %q)", filepath.Base(doc), what, want)
			}
		}
	}
}
