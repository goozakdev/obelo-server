package enrich

import "testing"

// What an anime Library gets by pointing its Authoritative provider at AniDB
// (ADR-0027) — the two tests from anidb_test.go that were never about AniDB's
// HTTP client.
//
// The client itself left this package with .scratch/bundled-plugins issue 05:
// AniDB is a Bundled plugin now, its XML parsing is tested natively under
// plugins/anidb/anidb/, and what is left here is the enrichment domain's own
// question — does the catalog offer AniDB as a Full video candidate, does it
// stay off until an operator configures it, and does a Library that repoints at
// it get a chain with AniDB leading and the other keyed video sources filling.
// Those are answered against the catalog the stand-ins compose (catalog_test.go),
// which reads the shipped manifest off disk, so they are still answers about the
// server that ships.

// TestAniDBRegistryEntry asserts AniDB's registry facts (ADR-0027): a Full video
// provider, RequiresKey, and shipped globally DISABLED (no seed row) so it appears
// as a usable authoritative candidate only once keyed.
func TestAniDBRegistryEntry(t *testing.T) {
	e, ok := shippedCatalog().Entry(SlugAniDB)
	if !ok {
		t.Fatalf("AniDB not registered")
	}
	if e.Class != ClassFull {
		t.Errorf("AniDB class = %q, want full (leadable)", e.Class)
	}
	if !e.Serves(KindVideo) || e.Serves(KindMusic) {
		t.Errorf("AniDB kinds = %v, want video only", e.Kinds)
	}
	if !e.RequiresKey {
		t.Errorf("AniDB RequiresKey = false, want true (needs a registered client)")
	}

	// It is a Full VIDEO candidate...
	found := false
	for _, c := range shippedCatalog().FullProvidersForKind(KindVideo) {
		if c.Slug == SlugAniDB {
			found = true
		}
	}
	if !found {
		t.Errorf("AniDB missing from the Full video candidates")
	}
	// ...but NOT the kind default (TMDB is), so adding it changes no existing Library.
	if shippedCatalog().DefaultAuthoritativeForKind(KindVideo) == SlugAniDB {
		t.Errorf("AniDB became the video default; want TMDB unchanged")
	}
	// Shipped disabled: with NO provider rows, AniDB is neither enabled nor keyed, so
	// it is not a usable authoritative until configured.
	states := shippedCatalog().ProviderStatesFromRows(nil)
	if s := states[SlugAniDB]; s.Enabled || s.Keyed {
		t.Errorf("AniDB default state = %+v, want disabled + unkeyed (ships off)", s)
	}
}

// TestBuildAniDBLeads asserts a keyed AniDB pointed as the video authoritative
// composes as the chain's lead (with TMDB, if keyed, a fill-only supplement).
func TestBuildAniDBLeads(t *testing.T) {
	provider, en := buildProvider(testConfig(
		withVideoLead(SlugAniDB),
		withKey(SlugAniDB, "anidb-client"),
		withKey(SlugTMDB, "tmdb-key"),
	))
	if !en.Video {
		t.Errorf("enablement = %+v, want video on (AniDB keyed authoritative)", en)
	}
	comp := provider.(CompositeProvider)
	chain, ok := comp.Video.(*VideoChainProvider)
	if !ok {
		t.Fatalf("video = %T, want *VideoChainProvider (AniDB leads, TMDB supplements)", comp.Video)
	}
	if got := pluginSlug(chain.Authoritative); got != SlugAniDB {
		t.Errorf("authoritative = %q (%T), want the anidb Plugin", got, chain.Authoritative)
	}
	var haveTMDB bool
	for _, s := range chain.Supplements {
		if pluginSlug(s) == SlugTMDB {
			haveTMDB = true
		}
	}
	if !haveTMDB {
		t.Errorf("supplements = %+v, want TMDB as a fill-only supplement", chain.Supplements)
	}
}
