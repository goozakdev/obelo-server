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

// TestTheGuidesMinimalOnlineSourceManifestIsOneTheServerAccepts: §2a shows a
// complete minimal manifest for an Online source, marked `<!-- online-manifest -->`.
// An author copies it, so it must pass the same decode-and-validate the installer
// runs, and it must declare what the section says it declares.
func TestTheGuidesMinimalOnlineSourceManifestIsOneTheServerAccepts(t *testing.T) {
	parallel(t)
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	raw, err := os.ReadFile(filepath.Join(root, "docs", "plugins", "authoring.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var body []string
	found := false
	for i, l := range lines {
		if strings.TrimSpace(l) != "<!-- online-manifest -->" {
			continue
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "```") {
			t.Fatalf("line %d: the online-manifest marker is not followed by a fenced block", i+1)
		}
		for _, b := range lines[i+2:] {
			if strings.HasPrefix(b, "```") {
				found = true
				break
			}
			body = append(body, b)
		}
		break
	}
	if !found {
		t.Fatal("authoring.md carries no <!-- online-manifest --> block")
	}
	m, err := decodeManifest([]byte(strings.Join(body, "\n")))
	if err != nil {
		t.Fatalf("the guide's minimal Online source manifest is refused: %v", err)
	}
	if len(m.Provides) != 1 || m.Provides[0].Kind != "online-source-provider" {
		t.Errorf("provides = %+v, want exactly one online-source-provider", m.Provides)
	}
	if m.Settings.DefaultURL == "" || len(m.Network.Hosts) == 0 {
		t.Errorf("the manifest should carry a defaultUrl and network.hosts: %+v", m)
	}
	var urlField bool
	for _, f := range settingsFields(m) {
		if f.Key == "instance" && f.Type == "url" {
			urlField = true
		}
	}
	if !urlField {
		t.Error("the manifest should declare a url-typed `instance` settings field")
	}
}
