// Package sdkguesttest builds the SDK-BUILT test guest from source and places it
// on disk the way an Admin's upload does (.scratch/bundled-plugins issue 03).
//
// It is plugintest's sibling and deliberately not an addition to it, because the
// two prove opposite things and a suite that shared one guest between them would
// prove neither:
//
//   - plugintest's guest is written against the RAW ABI and imports nothing. It is
//     the proof that no SDK is required — that an author in another language has
//     the JSON schema and six function signatures and needs nothing else.
//   - this guest is written with pluginsdk. It is the proof that the SDK works:
//     that its //go:wasmexport dispatchers are spelled the way the loader looks
//     them up, that its host-function wrappers round-trip, and that a provider
//     written against pluginsdk.Host runs unchanged inside wazero.
//
// # Why from source, every time
//
// The reason plugintest gives, unchanged: a checked-in 3.7 MiB module would rot
// exactly the way internal/webui/dist/index.html did — silently, while every
// guard stayed green — and nobody reviews a binary diff. So it is compiled by the
// ordinary command, from pluginsdk/testdata/guest, when a test first asks:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared
//
// which is the command the authoring guide names and the command issue 08's
// `make plugins` will run over the seven bundled providers.
package sdkguesttest

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

var (
	buildOnce sync.Once
	guestWasm []byte
	buildErr  error
)

// Guest returns the compiled SDK guest, building it on first use in this process.
func Guest(t *testing.T) []byte {
	t.Helper()
	buildOnce.Do(build)
	if buildErr != nil {
		t.Fatalf("building the SDK test guest: %v", buildErr)
	}
	return guestWasm
}

func build() {
	dir, err := GuestDir()
	if err != nil {
		buildErr = err
		return
	}
	// GOWORK is left alone on purpose — the guest lives INSIDE the pluginsdk
	// module, whose go.mod carries a `replace` for pluginapi, so it resolves with
	// or without the workspace.
	guestWasm, buildErr = plugintest.BuildGuest(dir, false)
}

// GuestDir is pluginsdk/testdata/guest, found from this file rather than from the
// working directory, so a test can ask for the guest from any package.
func GuestDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("sdkguesttest: cannot locate its own source")
	}
	// .../internal/plugins/sdkguesttest/sdkguesttest.go -> the repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	dir := filepath.Join(root, "pluginsdk", "testdata", "guest")
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return "", fmt.Errorf("sdkguesttest: no guest source at %s: %w", dir, err)
	}
	return dir, nil
}

// BaseURL is the manifest's default endpoint, carrying the mode marker that
// selects which part the one provider plays. A real plugin's default URL is just
// its source's API; the marker is the only thing a test can set that reaches
// inside a guest.
func BaseURL(mode string) string {
	return "https://sdk-source.example.test/v1?obelo-mode=" + mode
}

// ImageHost is the second host — the image CDN a Metadata provider's artwork URLs
// point at, which the HOST downloads from.
const ImageHost = "https://sdk-images.example.test"

// Manifest is the manifest of the SDK guest as a Metadata provider: plugintest's
// document, with this guest's default URLs and whatever mode the test wants.
//
// It declares CapabilityExternalRef ON PURPOSE while the served provider does not
// implement [testprovider.Provider]'s optional half, because that pairing is one
// of issue 03's acceptance criteria: a declared-but-unimplemented optional call
// must answer "unavailable" rather than break.
func Manifest(id, mode string, p pluginapi.ManifestProvides, allowedHosts ...string) pluginapi.Manifest {
	m := plugintest.MetadataProviderManifest(id, p, allowedHosts...)
	m.Name = "SDK Source (" + id + ")"
	m.Description = "A Metadata provider built with the Obelo Go SDK, compiled from source by the test suite."
	m.Settings.DefaultURL = BaseURL(mode)
	m.Settings.DefaultURL2 = ImageHost
	return m
}

// SubtitleManifest is the manifest of the SDK guest as a Subtitle provider:
// plugintest's document, naming this compiled module. It exists beside
// Manifest for the same reason plugintest.SubtitleManifest exists beside
// SinkManifest — one module CAN fill two seams, and testprovider.Provider now
// does (SearchSubtitles). The caller sets Settings.URL to the source it wants
// SearchSubtitles to ask, exactly as a Metadata provider test does.
func SubtitleManifest(id string, allowedHosts ...string) pluginapi.Manifest {
	m := plugintest.SubtitleManifest(id, allowedHosts...)
	m.Name = "SDK Source (" + id + ")"
	m.Description = "A Subtitle provider built with the Obelo Go SDK, compiled from source by the test suite."
	return m
}

// SinkManifest is the manifest of the SDK guest as an Event sink: plugintest's
// document, naming this compiled module. It exists beside Manifest and
// SubtitleManifest for the reason both do — one module fills a third seam
// (testprovider.Sink) — and the caller sets Settings.RateLimitMillis to pace it,
// exactly as a Subtitle provider test does.
func SinkManifest(id string, allowedHosts ...string) pluginapi.Manifest {
	m := plugintest.SinkManifest(id, allowedHosts...)
	m.Name = "SDK Source (" + id + ")"
	m.Description = "An Event sink built with the Obelo Go SDK, compiled from source by the test suite."
	return m
}

// WebReferenceManifest is the manifest of the SDK guest as a Web reference
// provider: plugintest's document, naming this compiled module. The guest's
// provider ignores plugintest's `mode` setting; it links an item's IMDb id and
// nothing else.
func WebReferenceManifest(id string) pluginapi.Manifest {
	m := plugintest.WebReferenceManifest(id, "https")
	m.Name = "SDK Source (" + id + ")"
	m.Description = "A Web reference provider built with the Obelo Go SDK, compiled from source by the test suite."
	return m
}

// LyricManifest is the manifest of the SDK guest as a Lyric provider whose
// source is sourceURL: plugintest's document, naming this compiled module.
func LyricManifest(id, sourceURL string) pluginapi.Manifest {
	m := plugintest.LyricManifest(id, sourceURL)
	m.Name = "SDK Source (" + id + ")"
	m.Description = "A Lyric provider built with the Obelo Go SDK, compiled from source by the test suite."
	return m
}

// MarkerManifest is the manifest of the SDK guest as a Marker provider whose
// source is sourceURL: plugintest's document, naming this compiled module.
func MarkerManifest(id, sourceURL string) pluginapi.Manifest {
	m := plugintest.MarkerManifest(id, sourceURL)
	m.Name = "SDK Source (" + id + ")"
	m.Description = "A Marker provider built with the Obelo Go SDK, compiled from source by the test suite."
	return m
}

// SignInManifest is the manifest of the SDK guest as a password-flow Sign-in
// provider that also answers lookup(subject): a directory reached over HTTP at
// directoryURL, which is the one declared setting, `directory`, and the one host
// the manifest allows.
func SignInManifest(id, directoryURL string) pluginapi.Manifest {
	host := directoryURL
	if u, err := url.Parse(directoryURL); err == nil {
		host = u.Hostname()
	}
	return pluginapi.Manifest{
		ID:         id,
		Name:       "SDK Source (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:         pluginapi.ExtensionSignInProvider,
			Capabilities: []pluginapi.Capability{pluginapi.CapabilityPasswordSignIn, pluginapi.CapabilitySignInLookup},
		}},
		Network: pluginapi.ManifestNetwork{Hosts: []string{host}},
		Settings: pluginapi.ManifestSettings{Fields: []pluginapi.SettingsField{{
			Key:     "directory",
			Type:    pluginapi.FieldURL,
			Label:   "Directory",
			Default: json.RawMessage(strconv.Quote(directoryURL)),
		}}},
		Description: "A Sign-in provider built with the Obelo Go SDK, compiled from source by the test suite.",
	}
}

// Install places the manifest and the compiled SDK guest under
// <dataDir>/plugins/<id>/, the way an Admin does by hand.
func Install(t *testing.T, dataDir string, m pluginapi.Manifest) string {
	t.Helper()
	return plugintest.InstallModule(t, dataDir, m, Guest(t))
}
