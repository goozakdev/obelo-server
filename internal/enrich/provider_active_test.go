package enrich

import (
	"context"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The explicit per-provider ACTIVE fact (.scratch/plugin-system issue 13).
//
// Every composition gate in this package used to ask one question — "is this
// provider's key non-empty?" — and a source that honestly declares it needs no
// credential could therefore never be on. These tests pin the gate in its new
// position and, just as importantly, pin that it did not move for anything else:
// the eight Built-ins carry no explicit fact at all, so key presence still
// answers for every one of them, which is why none of their behaviour changed.

// keylessFullMusic is a registration exactly like an Installed plugin's: a Full
// music source that declares it needs no secret.
func keylessFullMusic(slug string) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "Keyless Source",
			Kinds:       []string{KindMusic},
			Role:        RoleAuthoritative,
			Class:       ClassFull,
			RequiresKey: false,
			DefaultURL:  "https://keyless.example.test/v1",
		},
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) {
			return stubPlugin{}, nil
		},
	}
}

// keyedFullMusic is its twin, differing only in the declaration under test.
func keyedFullMusic(slug string) pluginapi.MetadataProviderRegistration {
	r := keylessFullMusic(slug)
	r.Descriptor.RequiresKey = true
	r.Descriptor.Name = "Keyed Source"
	return r
}

type stubPlugin struct{}

func (stubPlugin) Lookup(_ context.Context, _ pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}
func (stubPlugin) Search(_ context.Context, _ pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}
func (stubPlugin) ArtworkCandidates(_ context.Context, _ pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

func catalogWith(regs ...pluginapi.MetadataProviderRegistration) Catalog {
	reg := pluginapi.NewRegistry()
	for _, r := range MetadataPlugins() {
		reg.RegisterMetadataProvider(r)
	}
	for _, r := range regs {
		reg.RegisterMetadataProvider(r)
	}
	return NewCatalog(reg)
}

// TestAKeylessProviderIsActiveWhenEnabledAndOffWhenNot is the whole change, at
// the level the change was made: an enabled keyless source turns its kind on with
// no key at all, and a disabled one does not.
func TestAKeylessProviderIsActiveWhenEnabledAndOffWhenNot(t *testing.T) {
	cat := catalogWith(keylessFullMusic("keyless-source"))

	on := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "keyless-source", Enabled: true},
	}, "en-US", FixedProviderInputs{})
	on.AuthoritativeMusic = "keyless-source"
	if !on.providerActive("keyless-source") {
		t.Fatal("an enabled keyless provider is not active; it can never be composed")
	}
	if !on.musicEnabled() {
		t.Fatal("music is off for a Library led by an enabled keyless provider")
	}

	off := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "keyless-source", Enabled: false},
	}, "en-US", FixedProviderInputs{})
	off.AuthoritativeMusic = "keyless-source"
	if off.providerActive("keyless-source") {
		t.Fatal("a switched-off keyless provider is still active")
	}
	if off.musicEnabled() {
		t.Fatal("music is on for a Library led by a switched-off provider")
	}
}

// TestAnUnkeyedKeyRequiringProviderIsStillInactive is issue 11's negative at the
// level it is decided. The fix must not have been "stop asking".
func TestAnUnkeyedKeyRequiringProviderIsStillInactive(t *testing.T) {
	cat := catalogWith(keyedFullMusic("keyed-source"))

	cfg := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "keyed-source", Enabled: true},
	}, "en-US", FixedProviderInputs{})
	cfg.AuthoritativeMusic = "keyed-source"
	if cfg.providerActive("keyed-source") {
		t.Fatal("a key-requiring provider with no key on file is active")
	}
	if cfg.musicEnabled() {
		t.Fatal("music is on for a Library led by an unkeyed key-requiring provider")
	}

	keyed := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "keyed-source", Enabled: true, APIKey: "an-operator-key"},
	}, "en-US", FixedProviderInputs{})
	keyed.AuthoritativeMusic = "keyed-source"
	if !keyed.providerActive("keyed-source") || !keyed.musicEnabled() {
		t.Fatal("keying it did not turn it on")
	}
}

// TestTheBuiltInsCarryNoExplicitActiveFact is the guard on "Built-ins are
// unchanged". None of the eight ever reaches the explicit map, so for every one of
// them providerActive is still exactly the key-presence question it always was —
// which is why no Built-in's behaviour could have moved.
func TestTheBuiltInsCarryNoExplicitActiveFact(t *testing.T) {
	cat := catalogWith()
	cfg := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: SlugTMDB, Enabled: true, APIKey: "k"},
		{Slug: SlugMusicBrainz, Enabled: true},
		{Slug: SlugCoverArt, Enabled: true},
		{Slug: SlugOMDb, Enabled: true, APIKey: "k"},
	}, "en-US", FixedProviderInputs{})

	if len(cfg.ProviderActive) != 0 {
		t.Fatalf("a Built-in recorded an explicit active fact: %+v", cfg.ProviderActive)
	}
	for _, e := range cat.Entries() {
		if got, want := cfg.providerActive(e.Slug), cfg.providerKey(e.Slug) != ""; got != want {
			t.Errorf("%s: providerActive = %v, want the key-presence answer %v", e.Slug, got, want)
		}
	}
}

// TestASupplementOverrideStillMutesAKeylessProvider: the per-Library force-off has
// to keep working for a source whose key was never what was holding it up. Before
// the explicit fact, clearing an empty key was a no-op that read as "still on".
func TestASupplementOverrideStillMutesAKeylessProvider(t *testing.T) {
	cat := catalogWith(keylessFullMusic("keyless-source"))
	global := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "keyless-source", Enabled: true},
	}, "en-US", FixedProviderInputs{})
	global.AuthoritativeMusic = "keyless-source"

	cfg := global
	// Not the current authoritative, so the override is not a no-op.
	cfg.AuthoritativeMusic = SlugMusicBrainz
	applySupplementOverrides(&cfg, map[string]ProviderState{
		"keyless-source": {Enabled: true, Keyed: true},
	}, map[string]bool{"keyless-source": false})

	if cfg.providerActive("keyless-source") {
		t.Fatal("a force-off left a keyless provider active")
	}
	// COPY-ON-WRITE: the global config the resolver copied from is untouched, or
	// one Library's policy would rewrite every other Library's.
	if !global.providerActive("keyless-source") {
		t.Fatal("one Library's force-off reached the global config")
	}
}
