package plugins_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/sdkguesttest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The SDK-built guest, across the real ABI (.scratch/bundled-plugins issue 03).
//
// Everything here is already proved for the SDK-FREE guest in metadata_test.go.
// It is proved again for a module built with pluginsdk because the two modules
// share nothing: the SDK writes its own obelo_alloc, its own packed-i64 returns,
// its own //go:wasmexport names and its own host-function wrappers, and a bug in
// any of them is a bug in every plugin the maintainer ships after issue 04.
//
// What is NOT re-proved here is the contract's behaviour — the chain, the
// acceptance test, the settings screen. Those are the host's and they do not care
// which toolchain built the guest.

// The strings the SDK guest writes. They live in the provider's source
// (pluginsdk/internal/testprovider), which this module may not import because
// `internal` there means internal to the SDK — so they are restated here exactly
// as internal/api restates the SDK-free guest's. Nothing else in this server
// writes them, so a record carrying one came from the guest.
const (
	sdkGuestOverview    = "Filled from inside the sandbox by a plugin built with the Obelo Go SDK."
	sdkGuestSource      = "sdkguest"
	sdkGuestKVKey       = "sdk-guest-marker"
	sdkGuestArtworkPath = "/art/poster.jpg"
)

// sdkFullVideoProvides is the manifest entry of a Full video source that can lead
// and that DECLARES the external-ref capability its provider does not implement —
// the pairing this issue has to prove degrades rather than breaks.
func sdkFullVideoProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindVideo, pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			pluginapi.CapabilityArtworkCandidates,
			pluginapi.CapabilityExternalRef,
		},
	}
}

// sdkInstall places the SDK guest and its manifest and returns the Settings the
// host would resolve for it.
func sdkInstall(t *testing.T, dataDir, id, mode string, allowedHosts ...string) pluginapi.Settings {
	t.Helper()
	m := sdkguesttest.Manifest(id, mode, sdkFullVideoProvides(), allowedHosts...)
	sdkguesttest.Install(t, dataDir, m)
	return pluginapi.Settings{
		Enabled: true,
		Secret:  "an-operator-key",
		URL:     m.Settings.DefaultURL,
		URL2:    m.Settings.DefaultURL2,
	}
}

// TestASDKBuiltGuestAnswersTheThreeMandatoryCalls is the first claim: a provider
// written as three Go methods against pluginsdk.Host, compiled to wasm by the
// ordinary command, answers lookup, search and artwork candidates across the
// sandbox with the contract's own shapes.
func TestASDKBuiltGuestAnswersTheThreeMandatoryCalls(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "")

	set := load(t, dataDir, &logSink{})
	provider, d := providerFor(t, set, "sdk-source", s)

	if d.Role != pluginapi.RoleAuthoritative || d.Class != pluginapi.ClassFull {
		t.Fatalf("role/class = %q/%q, want authoritative/full", d.Role, d.Class)
	}

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("lookup outcome = %q, want matched", resp.Outcome)
	}
	if resp.Record.Name != "Dune" || resp.Record.Overview != sdkGuestOverview {
		t.Errorf("record = %q/%q, want the ref's title and the SDK guest's overview",
			resp.Record.Name, resp.Record.Overview)
	}
	if resp.Record.Source != sdkGuestSource || resp.Record.ExternalID != sdkGuestSource+"-movie" {
		t.Errorf("record identity = %q/%q", resp.Record.Source, resp.Record.ExternalID)
	}
	// The artwork is a URL on the image host the operator configured. The HOST
	// downloads it; a plugin returns URLs and never bytes.
	if len(resp.Record.Artwork) != 1 || resp.Record.Artwork[0].URL != sdkguesttest.ImageHost+sdkGuestArtworkPath {
		t.Errorf("artwork = %+v, want one poster on the configured image host", resp.Record.Artwork)
	}

	// A kind this source does not serve is a settled no-match, not a guess.
	none, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "sandwich", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup of an unserved kind: %v", err)
	}
	if none.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("unserved kind outcome = %q, want no-match", none.Outcome)
	}

	search, err := provider.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "Dune"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if search.Outcome != pluginapi.OutcomeMatched || len(search.Candidates) != 1 {
		t.Fatalf("search = %q with %d candidates, want matched with one", search.Outcome, len(search.Candidates))
	}
	if search.Candidates[0].Title != "Dune" || search.Candidates[0].Kind != "movie" {
		t.Errorf("candidate = %+v, want the query echoed with its kind", search.Candidates[0])
	}

	art, err := provider.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceTMDB: "1"}}, Role: "poster",
	})
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if art.Outcome != pluginapi.OutcomeMatched || len(art.Candidates) != 1 {
		t.Fatalf("artwork candidates = %q with %d, want matched with one", art.Outcome, len(art.Candidates))
	}
	if art.Candidates[0].Width != 600 || art.Candidates[0].Height != 900 {
		t.Errorf("candidate dimensions = %dx%d, want 600x900", art.Candidates[0].Width, art.Candidates[0].Height)
	}
}

// TestASDKBuiltGuestReadsTheSettingsTheHostResolved: settings_get, through the
// SDK's Host.Settings, inside one call. The guest writes what it saw into the
// record's overview, which is somewhere a test can read it.
func TestASDKBuiltGuestReadsTheSettingsTheHostResolved(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "settings")
	s.Language = "de-DE"
	limit := 250
	s.RateLimitMillis = &limit

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	var got struct {
		Enabled         bool   `json:"enabled"`
		HasSecret       bool   `json:"hasSecret"`
		URL             string `json:"url"`
		URL2            string `json:"url2"`
		Language        string `json:"language"`
		RateLimitMillis *int   `json:"rateLimitMillis"`
	}
	if err := json.Unmarshal([]byte(resp.Record.Overview), &got); err != nil {
		t.Fatalf("the guest did not echo its settings as JSON: %v\noverview: %s", err, resp.Record.Overview)
	}
	if !got.Enabled || !got.HasSecret {
		t.Errorf("the guest saw enabled=%v hasSecret=%v, want both true", got.Enabled, got.HasSecret)
	}
	if got.URL != s.URL || got.URL2 != s.URL2 {
		t.Errorf("the guest saw %q/%q, want the resolved URLs %q/%q", got.URL, got.URL2, s.URL, s.URL2)
	}
	if got.Language != "de-DE" {
		t.Errorf("the guest saw language %q, want de-DE", got.Language)
	}
	// The pointer is the point: absent and zero are different settings, and the
	// operator's 250 has to arrive as 250 rather than as a missing key.
	if got.RateLimitMillis == nil || *got.RateLimitMillis != 250 {
		t.Errorf("the guest saw rateLimitMillis %v, want 250", got.RateLimitMillis)
	}
}

// TestASDKBuiltGuestRoundTripsItsOwnKeyValueNamespace: kv_set and kv_get through
// the SDK's wrappers, landing in the store under the plugin's own id.
func TestASDKBuiltGuestRoundTripsItsOwnKeyValueNamespace(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "kv")

	kv := newMemKV()
	set := loadWithKV(t, dataDir, &logSink{}, kv)
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "album", Title: "Kid A", Artist: "Radiohead"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (%s), want matched", resp.Outcome, resp.Detail)
	}
	if resp.Record.Overview != s.Secret {
		t.Errorf("the guest read back %q, want the value it just wrote (%q)", resp.Record.Overview, s.Secret)
	}
	// The namespace is the host's doing, from the manifest on disk: the guest
	// never spells its own id and has no field to put one in.
	value, found, _ := kv.PluginKV("sdk-source", sdkGuestKVKey)
	if !found || string(value) != s.Secret {
		t.Errorf("the store holds %q/%v under the plugin's id, want the secret", value, found)
	}
}

// TestASDKBuiltGuestFetchesThroughTheHost: the only way out of the sandbox,
// reached through pluginsdk.Host.Fetch and the SDK's GetJSON helper.
//
// The target is the OPERATOR's own URL, which the host allows beside the manifest
// allowlist — an author cannot know which mirror an operator points at — so a
// local httptest server stands in for the source without the manifest naming it.
func TestASDKBuiltGuestFetchesThroughTheHost(t *testing.T) {
	var seen []string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "fetch")
	s.URL = source.URL + "?obelo-mode=fetch"

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (%s), want matched", resp.Outcome, resp.Detail)
	}
	if resp.Record.Overview != "from the source itself" {
		t.Errorf("overview = %q, want the body the source served", resp.Record.Overview)
	}
	if len(seen) != 1 || seen[0] != "/record" {
		t.Errorf("the source saw %v, want exactly one GET of /record", seen)
	}
}

// TestASDKBuiltGuestAnswersUnavailableForACapabilityItDoesNotImplement is the
// degradation ADR-0057 decision 3 asks for, at the SDK's seam rather than the
// host's.
//
// The manifest DECLARES external-ref, so the host makes the call — it consults
// the declaration, not the module. The served provider implements no
// ExternalRefParser, so the SDK's dispatcher answers "unavailable", which is the
// same known state an undeclared capability produces and is NOT an error: nothing
// is counted against the plugin and the host is free to read the paste itself.
func TestASDKBuiltGuestAnswersUnavailableForACapabilityItDoesNotImplement(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "")

	log := &logSink{}
	set := load(t, dataDir, log)
	provider, d := providerFor(t, set, "sdk-source", s)

	if !d.HasCapability(pluginapi.CapabilityExternalRef) {
		t.Fatalf("the manifest did not declare external-ref, so this test proves nothing")
	}
	parser, ok := provider.(pluginapi.ExternalRefParser)
	if !ok {
		t.Fatalf("an Installed provider must implement every optional interface unconditionally")
	}
	resp, err := parser.ParseExternalRef(context.Background(), pluginapi.ExternalRefRequest{
		Kind: "movie", Pasted: "https://sdk-source.example.test/movie/1",
	})
	if err != nil {
		t.Fatalf("ParseExternalRef: %v — an unimplemented capability is a known state, not a failure", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
	if !strings.Contains(resp.Detail, "external-ref") {
		t.Errorf("detail = %q, want it to name the capability the manifest over-claimed", resp.Detail)
	}

	// The same for an optional call whose capability was never declared at all:
	// the host would not make it, but nothing about the module breaks if it does.
	lister, ok := provider.(pluginapi.EpisodeLister)
	if !ok {
		t.Fatalf("an Installed provider must implement every optional interface unconditionally")
	}
	seasons, err := lister.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "1"})
	if err != nil {
		t.Fatalf("SeriesSeasons: %v", err)
	}
	if seasons.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("seasons outcome = %q, want unavailable", seasons.Outcome)
	}
}
