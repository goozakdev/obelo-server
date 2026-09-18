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
// position and, just as importantly, pin that it did not move for anything else.
//
// The fact is stated for EVERY registered provider since .scratch/bundled-plugins
// issue 01 (it used to be stated only for a Plugin this binary had no named key
// field for), so what the guard below now asserts is that stating it changed no
// answer: a key-requiring source's fact is still the key-presence question, and a
// keyless one's is its row's Enabled — which is what the MusicBrainz opt-in was.

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

// TestEveryProviderCarriesAnExplicitActiveFact is the guard on "every provider is
// treated the same", and it REPLACES the guard that used to live here
// (.scratch/bundled-plugins: issue 01). That one asserted the inverse — that no
// shipped provider ever reached the explicit map, because each had a named struct
// field that WAS its active fact — which was exactly the two-rules-for-two-kinds-of-
// Plugin this prefactor removes. The fact is now stated for every registered
// provider, from the same derivation, and what is asserted here is that stating it
// did not change any answer: for a key-requiring source the fact is still the
// key-presence question, and for a keyless one it is the row's own Enabled, which
// is precisely what the MusicBrainz opt-in meant.
func TestEveryProviderCarriesAnExplicitActiveFact(t *testing.T) {
	cat := catalogWith()
	rows := []store.MetadataProviderRow{
		{Slug: SlugTMDB, Enabled: true, APIKey: "k"},
		{Slug: SlugMusicBrainz, Enabled: true},
		{Slug: SlugOMDb, Enabled: true, APIKey: "k"},
	}
	byslug := map[string]store.MetadataProviderRow{}
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	cfg := cat.SettingsToProviderConfig(rows, "en-US", FixedProviderInputs{})

	for _, e := range cat.Entries() {
		if _, stated := cfg.ProviderActive[e.Slug]; !stated {
			t.Errorf("%s: no explicit active fact; every registered provider gets one", e.Slug)
		}
		r := byslug[e.Slug]
		want := r.Enabled
		if e.RequiresKey {
			// The pre-existing rule, unchanged: a key-requiring source is on exactly when
			// it holds a key (an enabled row with no key contributes nothing).
			want = r.Enabled && r.APIKey != ""
			if got := cfg.providerKey(e.Slug) != ""; got != want {
				t.Errorf("%s: keyed = %v, want the key-presence answer %v", e.Slug, got, want)
			}
		}
		if got := cfg.providerActive(e.Slug); got != want {
			t.Errorf("%s: providerActive = %v, want %v", e.Slug, got, want)
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
