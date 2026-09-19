package enrich

import (
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// REGISTRATION ORDER DECIDES THE MUSIC CHAIN, and after
// .scratch/bundled-plugins issue 07 nothing in this package holds it any more.
//
// Catalog.musicImageSupplements fills the music chain's two fill-only image slots
// from the catalog in REGISTRATION order: the first active artwork-only music
// source becomes Image (the PREFERRED, strictly-MBID-keyed artist photo) and the
// second becomes ImageBio (the FALLBACK image plus the real biography). While
// fanart.tv and TheAudioDB were Built-ins, the order was two adjacent literals in
// MetadataPlugins(). Now they are guests, and what puts fanart.tv first is
// internal/bundled's ordered id list — a JSON-and-slice fact two packages away
// from the composition it decides.
//
// So it is asserted here. If someone reorders internal/bundled.ids, or registers
// the Bundled plugins after the Built-ins, TheAudioDB silently becomes the
// preferred artist image: every artist with an MBID would get TheAudioDB's single
// name-matched thumb where fanart.tv's best-liked photo used to win, and no other
// test in this repository would notice.

// TestTheMusicChainKeepsFanartTVAheadOfTheAudioDB is that assertion at the level
// the slots are filled.
func TestTheMusicChainKeepsFanartTVAheadOfTheAudioDB(t *testing.T) {
	cat := shippedCatalog()
	cfg := testConfig(
		withActive(SlugMusicBrainz, true),
		withKey(SlugFanartTV, "fanart-key"),
		withKey(SlugTheAudioDB, "audiodb-key"),
	)

	supplements := cat.musicImageSupplements(cfg, SlugMusicBrainz)
	if len(supplements) != 2 {
		t.Fatalf("music image supplements = %d, want 2 (the preferred image and the image+bio fallback)", len(supplements))
	}
	if got := pluginSlug(supplements[0]); got != SlugFanartTV {
		t.Errorf("the PREFERRED artist image source is %q, want fanarttv — "+
			"registration order decides this, and internal/bundled's ordered ids are what hold it", got)
	}
	if got := pluginSlug(supplements[1]); got != SlugTheAudioDB {
		t.Errorf("the image+bio FALLBACK is %q, want theaudiodb", got)
	}
}

// TestTheComposedMusicChainTakesBothGuestsInOrder is the same fact one level up,
// where it is visible: the chain the Service actually runs.
func TestTheComposedMusicChainTakesBothGuestsInOrder(t *testing.T) {
	provider, en := buildProvider(testConfig(
		withKey(SlugTMDB, "tmdb-key"),
		withKey(SlugFanartTV, "fanart-key"),
		withKey(SlugTheAudioDB, "audiodb-key"),
	))
	if !en.Music {
		t.Fatalf("enablement = %+v, want music on", en)
	}
	chain, ok := provider.(CompositeProvider).Music.(*MusicChainProvider)
	if !ok {
		t.Fatalf("music = %T, want *MusicChainProvider", provider.(CompositeProvider).Music)
	}
	if got := pluginSlug(chain.MusicBrainz); got != SlugMusicBrainz {
		t.Errorf("the music lead is %q, want musicbrainz", got)
	}
	if got := pluginSlug(chain.Image); got != SlugFanartTV {
		t.Errorf("chain.Image = %q, want fanarttv (the preferred, MBID-keyed artist photo)", got)
	}
	if got := pluginSlug(chain.ImageBio); got != SlugTheAudioDB {
		t.Errorf("chain.ImageBio = %q, want theaudiodb (the fallback image and the real biography)", got)
	}
}

// TestFanartTVStillSupplementsBothChains: fanart.tv is the one source serving two
// kinds, and losing either half is a silent regression. As a Bundled plugin it is
// ONE instance with a view per chain, so both slots must still be filled from the
// one registration.
func TestFanartTVStillSupplementsBothChains(t *testing.T) {
	provider, en := buildProvider(testConfig(
		withKey(SlugTMDB, "tmdb-key"),
		withKey(SlugFanartTV, "fanart-key"),
	))
	if !en.Video || !en.Music {
		t.Fatalf("enablement = %+v, want video+music on", en)
	}
	comp := provider.(CompositeProvider)

	video, ok := comp.Video.(*VideoChainProvider)
	if !ok {
		t.Fatalf("video = %T, want *VideoChainProvider (fanart.tv is a video supplement)", comp.Video)
	}
	var inVideo bool
	for _, s := range video.Supplements {
		if pluginSlug(s) == SlugFanartTV {
			inVideo = true
		}
	}
	if !inVideo {
		t.Errorf("video supplements = %+v, want the fanarttv Plugin among them", video.Supplements)
	}

	music, ok := comp.Music.(*MusicChainProvider)
	if !ok {
		t.Fatalf("music = %T, want *MusicChainProvider (fanart.tv is the artist-image source)", comp.Music)
	}
	if got := pluginSlug(music.Image); got != SlugFanartTV {
		t.Errorf("chain.Image = %q, want fanarttv", got)
	}
}

// TestTheBundledArtworkStandInsMatchTheirShippedManifests is the guard on what
// these suites believe about the two artwork sources, now that every one of those
// facts lives in a JSON file rather than in this package: fanart.tv serves BOTH
// kinds, TheAudioDB serves music, both are artwork-only supplements that need a
// key and offer artwork candidates, and both declare a probe.
func TestTheBundledArtworkStandInsMatchTheirShippedManifests(t *testing.T) {
	t.Run("fanarttv", func(t *testing.T) {
		d := bundledStandIn(SlugFanartTV).Descriptor
		if d.Slug != SlugFanartTV {
			t.Fatalf("the shipped manifest declares the id %q, but this package still calls it %q", d.Slug, SlugFanartTV)
		}
		// BOTH kinds from ONE registration — the whole reason fanart.tv is the
		// awkward one. Losing the video half takes movie and show artwork away.
		if !d.Serves(KindVideo) || !d.Serves(KindMusic) {
			t.Errorf("kinds = %v, want both video and music", d.Kinds)
		}
		requireArtworkSupplement(t, d)
	})

	t.Run("theaudiodb", func(t *testing.T) {
		d := bundledStandIn(SlugTheAudioDB).Descriptor
		if d.Slug != SlugTheAudioDB {
			t.Fatalf("the shipped manifest declares the id %q, but this package still calls it %q", d.Slug, SlugTheAudioDB)
		}
		if !d.Serves(KindMusic) || d.Serves(KindVideo) {
			t.Errorf("kinds = %v, want music only", d.Kinds)
		}
		requireArtworkSupplement(t, d)
	})
}

// requireArtworkSupplement holds a Descriptor to the four facts that decide where
// an artwork source sits: it can never LEAD (artwork-only), it fills behind the
// lead (supplement), it cannot be enabled without a key, and it offers a candidate
// list to the Edit-item picker.
func requireArtworkSupplement(t *testing.T, d pluginapi.Descriptor) {
	t.Helper()
	if d.Role != RoleSupplement || d.Class != ClassArtworkOnly {
		t.Errorf("role/class = %q/%q, want supplement/artwork — anything else could lead a Library (ADR-0027)",
			d.Role, d.Class)
	}
	if !d.RequiresKey {
		t.Error("requiresSecret is false; the settings API would let an unkeyed source be enabled")
	}
	if !d.HasCapability(pluginapi.CapabilityArtworkCandidates) {
		t.Error("the shipped manifest does not declare artwork-candidates, so the picker would never ask it")
	}
	if d.DefaultURL == "" {
		t.Error("the shipped manifest declares no default URL, so a fresh row has nowhere to point")
	}
	if d.Probe == nil {
		t.Error(`the shipped manifest declares no probe, so "Test connection" cannot work (ADR-0059 decision 8)`)
	}
}
