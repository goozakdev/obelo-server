package enrich

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The catalog these tests run against. Since ADR-0057 the Metadata provider
// catalog is a VALUE the composition root derives from the Plugin registry, so a
// test that wants the shipped providers has to compose them the same way app.New
// does — register into a Registry and derive a Catalog from it. That is the point:
// there is no package-level catalog to reach for any more, and what these suites
// assert is what a server composed with THESE Plugins does.
//
// It is built per call rather than once in a package variable so no test can leave
// a mutated catalog behind for the next one.

func builtinCatalog() Catalog {
	reg := pluginapi.NewRegistry()
	// THE BUNDLED PLUGINS FIRST, in the order this server ships them, because
	// ORDER IS THE CATALOG ORDER: on a real server they register ahead of the
	// Built-ins (plugins.Set.RegisterEnabledAround), and that is what makes TMDB
	// the default video lead and MusicBrainz the default music lead.
	for _, id := range bundledStandIns() {
		reg.RegisterMetadataProvider(bundledStandIn(id))
	}
	for _, plugin := range MetadataPlugins() {
		reg.RegisterMetadataProvider(plugin)
	}
	return NewCatalog(reg)
}

// bundledStandIns is every provider that has LEFT this package for plugins/<id>/,
// in shipped order. It is the list these suites compose a server from beside
// whatever Built-ins are still here.
//
// ISSUES 05-07: adding a plugin is adding ONE LINE here, in the order
// internal/bundled.IDs() ships it. Nothing else in this file changes.
func bundledStandIns() []string {
	return []string{
		SlugTMDB,
		SlugOMDb,
		SlugTheTVDB,
		SlugAniDB,
		SlugMusicBrainz,
		SlugFanartTV,
		SlugTheAudioDB,
	}
}

// bundledStandIn is the stand-in for one Bundled plugin (ADR-0059,
// .scratch/bundled-plugins issue 04).
//
// On a real server these are WebAssembly modules: internal/bundled installs one,
// the loader compiles it, and it reaches the registry carrying the Descriptor its
// manifest declares. These are white-box tests of the enrichment domain's
// COMPOSITION — which source leads, which fills, what a Settings value does to a
// chain — and none of them is about wazero, so a real module would buy nothing
// here and cost a toolchain.
//
// What they DO depend on is the registration facts being the ones the real plugin
// registers with, and TestEveryBundledStandInMatchesItsShippedManifest below is
// what keeps this honest: it reads each shipped manifest off disk and holds the
// Descriptor to it. A change to a manifest fails that test rather than silently
// changing what these suites believe about a server.
//
// The factory builds NOTHING that talks to anything — a guest is what builds on a
// real server, and there is no guest here — so it answers a Plugin that has
// nothing for every call. The tests that need a source which ANSWERS inject their
// own fake; the tests that need one to EXIST need only this.
func bundledStandIn(id string) pluginapi.MetadataProviderRegistration {
	m := shippedManifest(id)
	p := m.Provides[0]
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:         m.ID,
			Name:         m.Name,
			Kinds:        p.Kinds,
			Role:         p.Role,
			Class:        p.Class,
			RequiresKey:  p.RequiresSecret || m.Settings.RequiresSecret,
			Capabilities: p.Capabilities,
			DefaultURL:   m.Settings.DefaultURL,
			DefaultURL2:  m.Settings.DefaultURL2,
			Description:  m.Description,
			DocsURL:      m.DocsURL,
			Probe:        p.Probe,
		},
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) {
			return silentPlugin{}, nil
		},
	}
}

// shippedManifest reads plugins/<id>/manifest.json — the document the real server
// installs and registers from. Read from disk rather than restated, so a stand-in
// cannot drift from what ships.
func shippedManifest(id string) pluginapi.Manifest {
	path := filepath.Join("..", "..", "plugins", id, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		panic("enrich tests: cannot read " + path + ": " + err.Error())
	}
	var m pluginapi.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		panic("enrich tests: " + path + " is not valid JSON: " + err.Error())
	}
	if len(m.Provides) == 0 {
		panic("enrich tests: " + path + " provides nothing")
	}
	return m
}

// silentPlugin is a Plugin that answers "nothing here" to everything. It stands
// where a compiled guest stands on a real server.
type silentPlugin struct{}

func (silentPlugin) Lookup(context.Context, pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

func (silentPlugin) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (silentPlugin) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

// TestEveryBundledStandInMatchesItsShippedManifest is the guard on the stand-ins
// above: what each one claims is read off the manifest that ships, so a manifest
// that stops declaring a probe, or a name, or a chain position, fails HERE rather
// than quietly changing what these suites believe about a server.
func TestEveryBundledStandInMatchesItsShippedManifest(t *testing.T) {
	for _, id := range bundledStandIns() {
		d := bundledStandIn(id).Descriptor
		if d.Slug != id {
			t.Errorf("plugins/%s/manifest.json declares the id %q", id, d.Slug)
		}
		if d.Name == "" {
			t.Errorf("%s: the manifest has no name, and the name is what an operator reads", id)
		}
		if d.Role == "" || d.Class == "" {
			t.Errorf("%s: role/class = %q/%q, want both — they are its position in the chain", id, d.Role, d.Class)
		}
		if len(d.Kinds) == 0 {
			t.Errorf("%s: the manifest declares no kinds, so it would serve nothing", id)
		}
		if d.Probe == nil {
			t.Errorf(`%s: the manifest declares no probe, so "Test connection" cannot work `+
				`(ADR-0059 decision 8)`, id)
		}
	}
}

// TestTheBundledTMDBStandInMatchesTheShippedManifest is the TMDB half of the same
// guard, and it is specific because these suites believe specific things about
// TMDB: that it leads video, that it is Full and authoritative, that it needs a
// key, that it can search, offer artwork and list episodes, and that it has two
// default URLs — and every one of those facts now lives in a JSON file rather than
// in this package.
func TestTheBundledTMDBStandInMatchesTheShippedManifest(t *testing.T) {
	d := bundledStandIn(SlugTMDB).Descriptor
	if d.Slug != SlugTMDB {
		t.Fatalf("the shipped manifest declares the id %q, but this package still calls it %q", d.Slug, SlugTMDB)
	}
	if d.Role != RoleAuthoritative || d.Class != ClassFull {
		t.Errorf("role/class = %q/%q, want authoritative/full — TMDB would stop leading video", d.Role, d.Class)
	}
	if !d.Serves(KindVideo) {
		t.Errorf("kinds = %v, want video", d.Kinds)
	}
	if !d.RequiresKey {
		t.Error("requiresSecret is false; the settings API would let an unkeyed TMDB be enabled")
	}
	for _, c := range []pluginapi.Capability{
		pluginapi.CapabilitySearch,
		pluginapi.CapabilityArtworkCandidates,
		pluginapi.CapabilityEpisodeList,
	} {
		if !d.HasCapability(c) {
			t.Errorf("the shipped manifest does not declare %q", c)
		}
	}
	if d.DefaultURL == "" || d.DefaultURL2 == "" {
		t.Errorf("default urls = %q / %q, want both (TMDB serves images from a second host)", d.DefaultURL, d.DefaultURL2)
	}
	if d.Probe == nil {
		t.Error(`the shipped manifest declares no probe, so "Test connection" cannot work (ADR-0059 decision 8)`)
	}
}

// buildProvider composes the chain from the Built-in catalog — the production
// path (BuilderFor's BuildFunc) with the catalog spelled out.
func buildProvider(cfg ProviderConfig) (MetadataProvider, Enablement) {
	return builtinCatalog().BuildProvider(cfg)
}

// pluginSlug reports which registered Plugin a composed video provider IS, or ""
// for anything that did not come through the contract. Since ADR-0057 the chain
// holds adapted Plugins rather than a source's concrete Go type, so "TMDB leads
// and OMDb supplements" is asserted by SLUG — which is also the honest question,
// because the whole point of the contract is that the chain cannot tell a Built-in
// from an Installed plugin by its type.
func pluginSlug(p MetadataProvider) string {
	adapted, ok := p.(pluginProvider)
	if !ok {
		return ""
	}
	return adapted.desc.Slug
}

// shippedMusicBrainzBaseURL is the MusicBrainz web service as the shipped manifest
// declares it. It was a constant in registry.go until MusicBrainz stopped being a
// Built-in; a test that wants "the default base URL" now reads the value the
// Descriptor actually carries.
func shippedMusicBrainzBaseURL(t *testing.T) string {
	t.Helper()
	return shippedManifest(SlugMusicBrainz).Settings.DefaultURL
}

// shippedCoverArtBaseURL is the COVER ART ARCHIVE as the shipped MusicBrainz
// manifest declares it: that plugin's SECOND url. It used to be a registration of
// its own with its own default (.scratch/bundled-plugins: issue 06), and the whole
// point of reading it from here is that there is now exactly one place it is
// written down.
func shippedCoverArtBaseURL(t *testing.T) string {
	t.Helper()
	return shippedManifest(SlugMusicBrainz).Settings.DefaultURL2
}

// musicGuestLike registers a settings-recording stand-in under a MUSIC slug, with
// the Descriptor the MusicBrainz manifest produces — a keyless, authoritative, Full
// music source with two default URLs. It is guestLike's music twin (prefactor_test.go),
// and it exists because the only honest way to assert what a Settings VALUE does to
// a guest is to read the Settings the host resolved for it.
func musicGuestLike(slug string, spy *settingsSpy) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "A Music Guest",
			Kinds:       []string{KindMusic},
			Role:        RoleAuthoritative,
			Class:       ClassFull,
			RequiresKey: false,
			DefaultURL:  "https://guest.example.test/ws/2",
			DefaultURL2: "https://images.guest.example.test",
			Probe:       &pluginapi.MediaRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"},
		},
		New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
			spy.got = s
			return spy, nil
		},
	}
}

// musicBrainzBehind IS GONE (.scratch/bundled-plugins: issue 06). It unwrapped a
// composed provider back through both halves of the adapter to the concrete
// *MusicBrainzProvider, for the handful of assertions about what a Settings VALUE
// did to the source it constructed — the operator's rate policy, the Cover Art
// Archive host.
//
// There is no concrete type to reach any more: MusicBrainz is a WebAssembly module
// behind the contract, and looking through the contract at a guest is not something
// a test can do or should want to. Each of those assertions moved to where it can
// be made honestly — the SETTINGS the host resolves are asserted here by
// composition (see the Settings-shaped tests in builder_test.go), and what the
// PROVIDER does with them is asserted natively in plugins/musicbrainz/musicbrainz.

// shippedTMDBImageBaseURL is the TMDB image host as the shipped manifest declares
// it. It was a constant in registry.go until TMDB stopped being a Built-in; the
// two tests that assert "a row with no image-host override falls back to the
// Descriptor default" need the same value the Descriptor now carries, and reading
// it off the manifest is how they keep needing only one.
var shippedTMDBImageBaseURL = shippedManifest(SlugTMDB).Settings.DefaultURL2
