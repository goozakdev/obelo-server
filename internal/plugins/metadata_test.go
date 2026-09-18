package plugins_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Metadata provider Extension point, driven by a REAL module built from source
// and called across the hand-rolled ABI (.scratch/plugin-system issue 11).
//
// The black-box half — that an Installed provider appears on the metadata-provider
// settings screen, leads a Library, fills what the lead left blank, and produces
// the paste box's two 400s — is in internal/api/installed_metadata_provider_test.go.
// What lives here is the loader's own policy: the registration, the per-call
// settings window, the key-value namespace and what a missing export answers.

// --- a key-value store a test can inspect ------------------------------------

// memKV is store.DB's PluginKV interface over a map. The store's own scoping test
// is in internal/store; this one exists so a Set can be built with no database and
// so the two-guest test can see what each Plugin actually wrote.
type memKV struct {
	mu   sync.Mutex
	rows map[[2]string][]byte
}

func newMemKV() *memKV { return &memKV{rows: map[[2]string][]byte{}} }

func (m *memKV) PluginKV(pluginID, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows[[2]string{pluginID, key}]
	return v, ok, nil
}

func (m *memKV) SetPluginKV(pluginID, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[[2]string{pluginID, key}] = value
	return nil
}

func (m *memKV) DeletePluginKV(pluginID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, [2]string{pluginID, key})
	return nil
}

func (m *memKV) namespaces() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for k := range m.rows {
		out[k[0]]++
	}
	return out
}

// --- helpers -----------------------------------------------------------------

// loadWithKV is `load` with a key-value store behind the kv host functions.
func loadWithKV(t *testing.T, dataDir string, log *logSink, kv plugins.PluginKV) *plugins.Set {
	t.Helper()
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout: 2 * time.Second,
		Logf:        log.logf,
		KV:          kv,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	return set
}

// providerFor registers the Set into a fresh Registry and builds the one Metadata
// provider for id, exactly as the enrichment Catalog does from a settings row.
func providerFor(t *testing.T, set *plugins.Set, id string, s pluginapi.Settings) (pluginapi.MetadataProvider, pluginapi.Descriptor) {
	t.Helper()
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.MetadataProvider(id)
	if !ok {
		t.Fatalf("the Set registered no Metadata provider for %q", id)
	}
	provider, err := registration.New(s)
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider, registration.Descriptor
}

// fullMusicProvides is the manifest entry of a Full music source that can lead.
func fullMusicProvides(caps ...pluginapi.Capability) pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds:        []string{pluginapi.KindMusic},
		Role:         pluginapi.RoleAuthoritative,
		Class:        pluginapi.ClassFull,
		Capabilities: caps,
	}
}

// --- the registration ---------------------------------------------------------

// TestAManifestBecomesAMetadataProviderRegistration: the four facts that decide
// where a source sits in the enrichment chain cross from the manifest onto the
// Descriptor a Built-in would have registered, unchanged. Everything downstream —
// the settings screen, the Authoritative-provider candidate list, the builder —
// reads only the Descriptor, so this is the whole of "it is indistinguishable".
func TestAManifestBecomesAMetadataProviderRegistration(t *testing.T) {
	dataDir := t.TempDir()
	m := plugintest.MetadataProviderManifest("example-source",
		fullMusicProvides(pluginapi.CapabilitySearch, pluginapi.CapabilityExternalRef))
	m.Settings.DefaultURL = "https://source.example.test/v1"
	plugintest.Install(t, dataDir, m)

	set := load(t, dataDir, &logSink{})
	_, d := providerFor(t, set, "example-source", pluginapi.Settings{Enabled: true})

	if d.Slug != "example-source" || d.Name != "Test Source (example-source)" {
		t.Errorf("descriptor identity = %q/%q, want the directory and the manifest name", d.Slug, d.Name)
	}
	if d.ExtensionPoint != pluginapi.ExtensionMetadataProvider {
		t.Errorf("extension point = %q, want metadata-provider", d.ExtensionPoint)
	}
	if !d.Serves(pluginapi.KindMusic) || d.Serves(pluginapi.KindVideo) {
		t.Errorf("kinds = %v, want music only", d.Kinds)
	}
	if d.Role != pluginapi.RoleAuthoritative || d.Class != pluginapi.ClassFull {
		t.Errorf("role/class = %q/%q, want authoritative/full — this is what lets it LEAD", d.Role, d.Class)
	}
	if !d.RequiresKey {
		t.Error("requiresKey = false; the manifest said it needs a secret")
	}
	if !d.HasCapability(pluginapi.CapabilitySearch) || !d.HasCapability(pluginapi.CapabilityExternalRef) {
		t.Errorf("capabilities = %v, want the two the manifest declared", d.Capabilities)
	}
	if d.HasCapability(pluginapi.CapabilityEpisodeList) || d.HasCapability(pluginapi.CapabilityAlbumTracklist) {
		t.Errorf("capabilities = %v, want NOTHING the manifest did not declare", d.Capabilities)
	}
	if d.DefaultURL != "https://source.example.test/v1" {
		t.Errorf("defaultUrl = %q, want the manifest's", d.DefaultURL)
	}
}

// TestAGuestAnswersALookup is the tracer: a module on disk, loaded, registered,
// built from Settings, and asked what a Track is.
func TestAGuestAnswersALookup(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source", fullMusicProvides()))

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true, Secret: "k", URL: "https://source.example.test/v1",
	})

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "track", Title: "Blue Monday", Artist: "New Order"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if resp.Record.Name != "Blue Monday" || resp.Record.Overview == "" {
		t.Fatalf("record = %+v, want the guest's answer about the ref it was given", resp.Record)
	}
	// The artwork is a URL and only a URL. The guest never returns bytes: the host's
	// fetcher downloads these into the identity-keyed cache under its own policy.
	if len(resp.Record.Artwork) != 1 || resp.Record.Artwork[0].Role != "poster" {
		t.Fatalf("artwork = %+v, want one poster URL", resp.Record.Artwork)
	}

	// A kind this source does not serve is a NO-MATCH, not a failure, and not a
	// guess. The host settles the item; the guest just says it has nothing. (The
	// per-kind chains mean a music-only Plugin is never offered a movie in the
	// first place — this is the guest declining a fine kind it does not know.)
	resp, err = provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "collection", Title: "Dune"},
	})
	if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Fatalf("a movie lookup on a music source = (%q, %v), want no-match", resp.Outcome, err)
	}
}

// --- settings_get -------------------------------------------------------------

// TestSettingsReachTheGuestOnlyInsideACall is ADR-0058 decision 5's "secrets at
// call time only", checked from both sides: the guest can read the resolved
// Settings while it is answering, and the Plugin holds nothing readable once the
// call is over.
//
// The guest proves it by echoing its own secret back through the kv namespace —
// it could only have got that string from settings_get.
func TestSettingsReachTheGuestOnlyInsideACall(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source", fullMusicProvides()))

	kv := newMemKV()
	set := loadWithKV(t, dataDir, &logSink{}, kv)
	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true,
		Secret:  "the-operators-api-key",
		URL:     "https://source.example.test/v1?obelo-mode=kv",
	})

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Record.Overview != "the-operators-api-key" {
		t.Fatalf("the guest read %q through settings_get, want the secret the host resolved", resp.Record.Overview)
	}

	// And the value it stored is in ITS namespace, under its own id.
	if v, found, _ := kv.PluginKV("example-source", "shared-key"); !found || string(v) != "the-operators-api-key" {
		t.Fatalf("the plugin's namespace holds (%q, found=%v), want its own write", v, found)
	}
}

// TestTwoGuestsDoNotShareAKey is the isolation criterion, through two real
// sandboxes rather than through the store: both write the SAME key, each reads
// back its OWN value, and neither can see the other's.
func TestTwoGuestsDoNotShareAKey(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("source-alpha", fullMusicProvides()))
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("source-beta", fullMusicProvides()))

	kv := newMemKV()
	set := loadWithKV(t, dataDir, &logSink{}, kv)

	ask := func(id, secret string) string {
		t.Helper()
		provider, _ := providerFor(t, set, id, pluginapi.Settings{
			Enabled: true, Secret: secret,
			URL: "https://source.example.test/v1?obelo-mode=kv",
		})
		resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
		})
		if err != nil {
			t.Fatalf("Lookup on %s: %v", id, err)
		}
		return resp.Record.Overview
	}

	if got := ask("source-alpha", "alpha-value"); got != "alpha-value" {
		t.Fatalf("alpha read back %q, want its own value", got)
	}
	if got := ask("source-beta", "beta-value"); got != "beta-value" {
		t.Fatalf("beta read back %q, want its own value", got)
	}
	// Beta wrote the same key after alpha did. Alpha still reads its own.
	if got := ask("source-alpha", "alpha-value"); got != "alpha-value" {
		t.Fatalf("after beta wrote the same key, alpha read %q — the namespace leaked", got)
	}

	// Two namespaces, one key each, and the store was never asked for a third.
	spaces := kv.namespaces()
	if len(spaces) != 2 || spaces["source-alpha"] != 1 || spaces["source-beta"] != 1 {
		t.Fatalf("the store holds %v, want one key in each of two namespaces", spaces)
	}
}

// TestAnOversizeValueIsRefusedNotTruncated: the cap answers a refusal, for the
// reason an oversize fetch does — a guest handed back a shortened value would
// parse it as its whole cursor.
func TestAnOversizeValueIsRefusedNotTruncated(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source", fullMusicProvides()))

	kv := newMemKV()
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:     2 * time.Second,
		Logf:            (&logSink{}).logf,
		KV:              kv,
		MaxKVValueBytes: 4, // smaller than the secret the guest will try to store
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true, Secret: "far-too-long-for-this-cap",
		URL: "https://source.example.test/v1?obelo-mode=kv",
	})
	_, err = provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	})
	if err == nil {
		t.Fatal("Lookup succeeded; the guest's oversize write should have been refused")
	}
	if !strings.Contains(err.Error(), "kv_set was refused") {
		t.Fatalf("error = %v, want the guest's report that the write was refused", err)
	}
	// Nothing was stored — not even a shortened version of it.
	if len(kv.namespaces()) != 0 {
		t.Fatalf("the store holds %v after a refused write, want nothing", kv.namespaces())
	}
}

// TestAServerWithNoKeyValueStoreStillCallsItsPlugins: KV is optional, and a
// Plugin that cannot cache a cursor is a slower Plugin rather than a broken server
// (ADR-0001). The guest is told so and says so; nothing panics and nothing is
// disabled.
func TestAServerWithNoKeyValueStoreStillCallsItsPlugins(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source", fullMusicProvides()))

	set := load(t, dataDir, &logSink{}) // no KV at all
	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true, Secret: "k",
		URL: "https://source.example.test/v1?obelo-mode=kv",
	})
	if _, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	}); err == nil {
		t.Fatal("the kv-mode guest reported success with no store behind it")
	}
	// The ordinary path is untouched: a second provider built from the same Plugin
	// answers normally.
	plain, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true, Secret: "k", URL: "https://source.example.test/v1",
	})
	resp, err := plain.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	})
	if err != nil || resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("a plain lookup = (%q, %v), want matched", resp.Outcome, err)
	}
	if st, _ := set.Status("example-source"); st.Disabled {
		t.Fatalf("the Plugin was disabled by a kv refusal: %+v", st)
	}
}

// --- the optional capabilities -------------------------------------------------

// TestAMissingOptionalExportIsUnavailableAndNotAFailure: the test guest declares
// album-tracklist and episode-list in this manifest but exports neither, which is
// the manifest claiming something the module does not do.
//
// Two things have to be true of that. The answer is the same "unavailable" an
// UNDECLARED capability produces, because that is what the declaration would have
// given had it been honest. And it is not counted as a failure, because nothing
// ran: a Plugin whose optional call is missing must not be disabled out of a
// working chain over it.
func TestAMissingOptionalExportIsUnavailableAndNotAFailure(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source",
		fullMusicProvides(pluginapi.CapabilityAlbumTracklist, pluginapi.CapabilityEpisodeList)))

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true, Secret: "k", URL: "https://source.example.test/v1",
	})

	lister, ok := provider.(pluginapi.EpisodeLister)
	if !ok {
		t.Fatal("the adapter does not satisfy EpisodeLister; the chains' assertions would fail")
	}
	// Three misses in a row — one more than the failure threshold — and the Plugin
	// is still healthy afterwards.
	for i := 0; i < 3; i++ {
		resp, err := lister.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "x"})
		if err != nil {
			t.Fatalf("SeriesSeasons: %v", err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
		}
	}

	// The album half is the pair the test guest splits: it EXPORTS the tracklist
	// call (which answers its own no-match — "this album has no tracklist", the
	// call-scoped reading issue 04 pinned) and does NOT export the editions call,
	// which answers unavailable — the picker's "not now". The two absent-answers
	// differ on purpose: an album with no editions to choose from is a real,
	// matched answer, so "I have none" and "I cannot say" must stay distinguishable.
	tracklister, ok := provider.(pluginapi.AlbumTracklister)
	if !ok {
		t.Fatal("the adapter does not satisfy AlbumTracklister")
	}
	tl, err := tracklister.AlbumTracklist(context.Background(), pluginapi.TracklistRequest{ReleaseGroupID: "rg"})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	if tl.Outcome != pluginapi.OutcomeNoMatch {
		t.Fatalf("tracklist outcome = %q, want no-match", tl.Outcome)
	}
	ed, err := tracklister.ReleaseGroupEditions(context.Background(), pluginapi.ReleaseEditionsRequest{ReleaseGroupID: "rg"})
	if err != nil {
		t.Fatalf("ReleaseGroupEditions: %v", err)
	}
	if ed.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("editions outcome = %q, want unavailable", ed.Outcome)
	}

	if st, _ := set.Status("example-source"); st.Disabled || st.LastError != "" {
		t.Fatalf("status = %+v; a missing optional export is not a failure", st)
	}
	// And the Plugin still answers the call it DOES export.
	if resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "album", Title: "Power, Corruption & Lies"},
	}); err != nil || resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("after three missing-export calls, Lookup = (%q, %v), want matched", resp.Outcome, err)
	}
}

// TestARefusedPluginIsRegisteredAndItsFactoryRefuses: a Plugin an operator placed
// that this server will not run is still ON the settings screen, with the sentence
// that says why — and the builder composes a chain WITHOUT it rather than with a
// source that fails every call (ADR-0001).
func TestARefusedPluginIsRegisteredAndItsFactoryRefuses(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.InstallModule(t, dataDir,
		plugintest.MetadataProviderManifest("broken-source", fullMusicProvides()),
		[]byte("this is not a WebAssembly module"))

	set := load(t, dataDir, &logSink{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)

	registration, ok := reg.MetadataProvider("broken-source")
	if !ok {
		t.Fatal("a Plugin that would not compile was not registered; an operator would see nothing at all")
	}
	if _, err := registration.New(pluginapi.Settings{Enabled: true}); err == nil {
		t.Fatal("the factory of a refused Plugin built a provider")
	}
	st, _ := set.Status("broken-source")
	if !st.Disabled || st.LastError == "" {
		t.Fatalf("status = %+v, want disabled with a reason", st)
	}
}
