package plugins_test

import (
	"context"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// webReferenceProviderFor loads one Installed Web reference provider playing
// mode, registers the Set into a fresh Registry and builds the provider exactly
// as internal/webref does.
func webReferenceProviderFor(t *testing.T, mode string) (*plugins.Set, pluginapi.WebReferenceProviderRegistration, pluginapi.WebReferenceProvider) {
	t.Helper()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.WebReferenceManifest("example-refs", mode))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"mode": mode})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.WebReferenceProvider("example-refs")
	if !ok {
		t.Fatal("the Set registered no Web reference provider for example-refs")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return set, registration, provider
}

// TestAnInstalledWebReferenceProviderRegistersAndAnswers is the loader-level
// tracer: a manifest declaring web-reference-provider reaches the registry with
// its kinds, and the one call crosses the sandbox carrying the kind and the ids.
func TestAnInstalledWebReferenceProviderRegistersAndAnswers(t *testing.T) {
	plugins.Parallel(t)
	_, registration, provider := webReferenceProviderFor(t, "https")

	d := registration.Descriptor
	if d.ExtensionPoint != pluginapi.ExtensionWebReferenceProvider || !d.Serves(pluginapi.KindVideo) {
		t.Fatalf("descriptor = %+v, want a web-reference-provider serving video", d)
	}
	resp, err := provider.Links(context.Background(), pluginapi.WebReferencesRequest{
		Kind: "movie",
		IDs:  map[string]string{"imdb": "tt1160419", "tmdb": "438631"},
	})
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	want := []pluginapi.WebReference{
		{Namespace: "imdb", ID: "tt1160419", Label: "Example imdb", URL: "https://refs.example.test/movie/imdb/tt1160419"},
		{Namespace: "tmdb", ID: "438631", Label: "Example tmdb", URL: "https://refs.example.test/movie/tmdb/438631"},
	}
	if len(resp.References) != len(want) {
		t.Fatalf("references = %+v, want %+v", resp.References, want)
	}
	for i := range want {
		if resp.References[i] != want[i] {
			t.Fatalf("reference %d = %+v, want %+v", i, resp.References[i], want[i])
		}
	}
}

// TestAWebReferenceProviderGuestCannotFetch: the guest reaches for http_fetch in
// the middle of its call and is refused by the host with the no-network
// sentence; the attempt is recorded against the Plugin.
func TestAWebReferenceProviderGuestCannotFetch(t *testing.T) {
	plugins.Parallel(t)
	set, _, provider := webReferenceProviderFor(t, "fetch")

	resp, err := provider.Links(context.Background(), pluginapi.WebReferencesRequest{
		Kind: "movie", IDs: map[string]string{"imdb": "tt1160419"},
	})
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	if len(resp.References) != 1 || resp.References[0].Label != "refused: this call has no network" {
		t.Fatalf("references = %+v, want one labelled with the host's no-network refusal", resp.References)
	}
	st, _ := set.Status("example-refs")
	if st.LastError == "" {
		t.Fatalf("status = %+v, want the refused fetch recorded as the last error", st)
	}
}

// TestAWebReferenceProviderDeclaringNoKindsIsLoggedOnceAtRegistration: a
// provider that declares no kinds serves no item, so it is never asked. That is
// said once, naming the Plugin, when it is registered — and not for one that
// declares video.
func TestAWebReferenceProviderDeclaringNoKindsIsLoggedOnceAtRegistration(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	none := plugintest.WebReferenceManifest("kindless-refs", "https")
	none.Provides[0].Kinds = nil
	plugintest.Install(t, dataDir, none)
	plugintest.Install(t, dataDir, plugintest.WebReferenceManifest("video-refs", "https"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	set.Register(pluginapi.NewRegistry())

	if n := strings.Count(log.all(), "kindless-refs declares no kinds"); n != 1 {
		t.Fatalf("logged the kindless provider %d times, want once:\n%s", n, log.all())
	}
	if strings.Contains(log.all(), "video-refs declares no kinds") {
		t.Fatalf("logged a provider that declares video:\n%s", log.all())
	}
}

// TestAnOfflineFetchDisablesUnderItsOwnName: a Web reference provider that
// reaches for the network on every call is disabled at the threshold, and the
// line saying so names what it did — reached for the network during a call that
// has none — rather than calling a no-network refusal an allowlist violation.
func TestAnOfflineFetchDisablesUnderItsOwnName(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.WebReferenceManifest("example-refs", "fetch"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"mode": "fetch"})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.WebReferenceProvider("example-refs")
	if !ok {
		t.Fatal("the Set registered no Web reference provider for example-refs")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		_, _ = provider.Links(context.Background(), pluginapi.WebReferencesRequest{
			Kind: "movie", IDs: map[string]string{"imdb": "tt1160419"},
		})
	}
	if st, _ := set.Status("example-refs"); !st.Disabled {
		t.Fatalf("status = %+v, want the provider disabled after %d refused fetches", st, plugins.DefaultFailureThreshold)
	}
	var line string
	for _, l := range strings.Split(log.all(), "\n") {
		if strings.Contains(l, "is disabled after") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no line says the provider was disabled:\n%s", log.all())
	}
	if strings.Contains(line, "allowlist") || !strings.Contains(line, "tried to fetch during a call that has no network") {
		t.Fatalf("the disable line is %q; want it to name the no-network refusal and not an allowlist", line)
	}
}
