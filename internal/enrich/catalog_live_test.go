package enrich

import (
	"fmt"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Catalog follows the registry it was derived from (.scratch/plugin-system
// issue 19). It used to copy the Descriptor list at construction, so every value
// holding a Catalog for longer than one request — the enrichment Manager, the
// GlobalEnrichment the per-Library resolver reads, a Library's cached snapshot —
// answered questions about the Plugins this server had AT BOOT. These are the unit
// half of that; internal/api/plugin_live_catalog_test.go is the operator's half.

// liveVideoPlugin is a registration with just enough on it to be a Full video
// provider the catalog will list, lead with and compose.
func liveVideoPlugin(slug string) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "Live " + slug,
			Kinds:       []string{KindVideo},
			Role:        RoleAuthoritative,
			Class:       ClassFull,
			RequiresKey: true,
			DefaultURL:  "https://" + slug + ".example.test/v1",
		},
		// It builds a source that answers "nothing here" to everything: these tests
		// are about the CATALOG following its registry, and nothing in them ever
		// looks anything up. (It used to construct an OMDb client, which stopped
		// being possible when OMDb became a Bundled plugin.)
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) {
			return silentPlugin{}, nil
		},
	}
}

// registryWith builds a fresh Registry holding exactly these Plugins, in order —
// the same act plugins.Manager.rebuild performs before it Swaps.
func registryWith(regs ...pluginapi.MetadataProviderRegistration) *pluginapi.Registry {
	reg := pluginapi.NewRegistry()
	for _, r := range regs {
		reg.RegisterMetadataProvider(r)
	}
	return reg
}

func slugsOf(entries []pluginapi.Descriptor) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Slug)
	}
	return out
}

func equalSlugs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestACatalogSeesAPluginInstalledAfterItWasDerived is the whole change in one
// assertion: one Catalog value, derived once, answering about the Plugins the
// registry holds NOW.
func TestACatalogSeesAPluginInstalledAfterItWasDerived(t *testing.T) {
	reg := registryWith(liveVideoPlugin("first"))
	cat := NewCatalog(reg)

	if got := slugsOf(cat.Entries()); !equalSlugs(got, []string{"first"}) {
		t.Fatalf("entries = %v, want the one registered Plugin", got)
	}

	// The install: a FRESH registry is built and swapped in whole, which is exactly
	// what plugins.Manager.rebuild does.
	reg.Swap(registryWith(liveVideoPlugin("first"), liveVideoPlugin("second")))

	if got := slugsOf(cat.Entries()); !equalSlugs(got, []string{"first", "second"}) {
		t.Fatalf("entries = %v after a swap, want both Plugins in registration order", got)
	}
	if got := slugsOf(cat.FullProvidersForKind(KindVideo)); !equalSlugs(got, []string{"first", "second"}) {
		t.Fatalf("Full video providers = %v, want both — a Plugin installed today can lead today", got)
	}
	if got := slugsOf(cat.SupplementProvidersForKind(KindVideo)); !equalSlugs(got, []string{"first", "second"}) {
		t.Fatalf("Supplements = %v, want both", got)
	}
	if _, ok := cat.Entry("second"); !ok {
		t.Fatal("Entry does not know a Plugin the registry holds")
	}
	if got := cat.DefaultAuthoritativeForKind(KindVideo); got != "first" {
		t.Errorf("default lead = %q, want the FIRST registered Plugin — catalog order is registration order", got)
	}

	rows := []store.MetadataProviderRow{
		{Slug: "first", Enabled: true, APIKey: "k1"},
		{Slug: "second", Enabled: true, APIKey: "k2"},
	}
	states := cat.ProviderStatesFromRows(rows)
	if !states["second"].Enabled || !states["second"].Keyed {
		t.Errorf("the per-slug state map does not cover a Plugin installed today: %+v", states)
	}
	cfg := cat.SettingsToProviderConfig(rows, "en-US", FixedProviderInputs{})
	if cfg.ProviderKeys["second"] != "k2" || !cfg.ProviderActive["second"] {
		t.Errorf("the composed config does not carry a Plugin installed today: keys=%v active=%v",
			cfg.ProviderKeys, cfg.ProviderActive)
	}
}

// TestACatalogForgetsAPluginTheRegistryDropped is the uninstall direction, which
// is what lets a Library whose lead was uninstalled fall back to the kind default
// on the next read rather than on the next restart.
func TestACatalogForgetsAPluginTheRegistryDropped(t *testing.T) {
	reg := registryWith(liveVideoPlugin("staying"), liveVideoPlugin("going"))
	cat := NewCatalog(reg)
	if _, ok := cat.Entry("going"); !ok {
		t.Fatal("the Plugin about to be uninstalled was never in the catalog")
	}

	reg.Swap(registryWith(liveVideoPlugin("staying")))

	if _, ok := cat.Entry("going"); ok {
		t.Error("an uninstalled Plugin is still a catalog entry")
	}
	if got := slugsOf(cat.FullProvidersForKind(KindVideo)); !equalSlugs(got, []string{"staying"}) {
		t.Errorf("Full video providers = %v, want only the one still installed", got)
	}
	// And the resolver, which validates a Library's lead pointer against the
	// catalog, now falls back rather than pointing at a dead Descriptor.
	global := GlobalEnrichment{
		Catalog:   cat,
		Config:    cat.SettingsToProviderConfig(nil, "en-US", FixedProviderInputs{}),
		Providers: cat.ProviderStatesFromRows(nil),
	}
	gone := "going"
	res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{AuthoritativeProvider: &gone})
	if res.Config.AuthoritativeVideo != "" {
		t.Errorf("the resolver still points a Library at the uninstalled %q", res.Config.AuthoritativeVideo)
	}
	if res.AuthoritativeFallback != "" {
		t.Errorf("an uninstalled lead was reported as an unreachable one (%q); it is simply not a provider any more",
			res.AuthoritativeFallback)
	}
}

// TestCatalogReadsAreSafeAgainstASwap is the -race half of the acceptance
// criterion: a reader never observes a half-built catalog while installs and
// uninstalls swap the registry underneath it. Every read also checks that what it
// got is a WHOLE registry's worth of Plugins in registration order, so a torn
// value would fail the assertion even on a build without the race detector.
func TestCatalogReadsAreSafeAgainstASwap(t *testing.T) {
	const generations = 200
	// Every generation is the same prefix plus one more Plugin, so a well-formed
	// read is a prefix of this sequence and nothing else.
	all := make([]pluginapi.MetadataProviderRegistration, 0, generations)
	for i := 0; i < generations; i++ {
		all = append(all, liveVideoPlugin(fmt.Sprintf("p%03d", i)))
	}

	reg := registryWith(all[0])
	cat := NewCatalog(reg)

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 1; i < generations; i++ {
			reg.Swap(registryWith(all[:i+1]...))
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				got := slugsOf(cat.FullProvidersForKind(KindVideo))
				if len(got) == 0 {
					t.Errorf("a reader saw an EMPTY catalog while the registry was being swapped")
					return
				}
				for i, slug := range got {
					if want := fmt.Sprintf("p%03d", i); slug != want {
						t.Errorf("a reader saw a half-built catalog: position %d is %q, want %q", i, slug, want)
						return
					}
				}
				// The other derivations, for the same reason.
				_ = cat.SettingsToProviderConfig(nil, "en-US", FixedProviderInputs{})
				_ = cat.ProviderStatesFromRows(nil)
				_, _ = cat.Entry(got[len(got)-1])
			}
		}()
	}
	wg.Wait()
}
