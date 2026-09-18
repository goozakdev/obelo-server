package enrich

import (
	"context"
	"testing"
	"time"
)

// TestBuildProviderComposition asserts BuildProvider reproduces the boot-time
// composition + per-kind enablement for representative configs — the byte-for-
// byte behavior the prefactor must preserve.
func TestBuildProviderComposition(t *testing.T) {
	t.Run("no image key => plain MusicBrainz (no chain)", func(t *testing.T) {
		provider, en := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withActive(SlugMusicBrainz, true),
			withRateLimit(2*time.Second),
			withURLs(SlugMusicBrainz, registryMusicBrainzBaseURL, "https://cover.test"),
		))
		if en != (Enablement{Video: true, Music: true}) {
			t.Errorf("enablement = %+v, want video+music on", en)
		}
		comp, ok := provider.(CompositeProvider)
		if !ok {
			t.Fatalf("provider = %T, want CompositeProvider", provider)
		}
		if got := pluginSlug(comp.Video); got != SlugTMDB {
			t.Errorf("video = %q (%T), want the tmdb Plugin", got, comp.Video)
		}
		// No image key: Music stays the plain lead Plugin, NOT wrapped in the chain.
		if got := pluginSlug(comp.Music); got != SlugMusicBrainz {
			t.Fatalf("music = %q (%T), want the plain musicbrainz Plugin", got, comp.Music)
		}
		// The operator's throttle policy is threaded through to the host — now through
		// the Plugin's Settings, so this asserts the whole path the setting takes.
		mb := musicBrainzBehind(comp.Music)
		if mb == nil {
			t.Fatalf("music Plugin is not built around a *MusicBrainzProvider: %T", comp.Music)
		}
		if mb.MinInterval != 2*time.Second {
			t.Errorf("MinInterval = %v, want 2s (honoring MusicBrainzRateLimit)", mb.MinInterval)
		}
		// And the Cover Art Archive host reaches it as the Plugin's SECOND url, which
		// is the whole of what Cover Art Archive's factory-less registration does.
		if mb.CoverArtURL != "https://cover.test" {
			t.Errorf("CoverArtURL = %q, want the configured Cover Art host", mb.CoverArtURL)
		}
	})

	t.Run("image key + music => MusicChain", func(t *testing.T) {
		provider, en := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugFanartTV, "fanart-key"),
		))
		if en != (Enablement{Video: true, Music: true}) {
			t.Errorf("enablement = %+v, want video+music on", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Music.(*MusicChainProvider); !ok {
			t.Errorf("music = %T, want *MusicChainProvider (image source configured)", comp.Music)
		}
	})

	t.Run("music image key but music off => no chain", func(t *testing.T) {
		// An image key alone must NOT turn Music on, and must NOT wrap the chain
		// (MusicImageEnabled && MusicEnrichmentEnabled — both required).
		provider, en := buildProvider(testConfig(withKey(SlugFanartTV, "fanart-key")))
		if en != (Enablement{Video: false, Music: false}) {
			t.Errorf("enablement = %+v, want both off", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Music); got != SlugMusicBrainz {
			t.Errorf("music = %q (%T), want the plain musicbrainz Plugin (music off)", got, comp.Music)
		}
	})

	t.Run("omdb + tmdb => Video is the chain", func(t *testing.T) {
		provider, en := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugOMDb, "omdb-key"),
		))
		if !en.Video {
			t.Errorf("enablement = %+v, want video on", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*VideoChainProvider); !ok {
			t.Errorf("video = %T, want *VideoChainProvider (OMDb supplement configured)", comp.Video)
		}
	})

	t.Run("thetvdb + tmdb => Video is the chain", func(t *testing.T) {
		provider, en := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugTheTVDB, "tvdb-key"),
		))
		if !en.Video {
			t.Errorf("enablement = %+v, want video on", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*VideoChainProvider); !ok {
			t.Errorf("video = %T, want *VideoChainProvider (TheTVDB supplement configured)", comp.Video)
		}
	})

	t.Run("thetvdb key but tmdb off => video off, plain (no chain)", func(t *testing.T) {
		// A supplement can't enable the video kinds on its own; with no TMDB key
		// video stays off and Video stays plain TMDB (zero calls to TheTVDB).
		provider, en := buildProvider(testConfig(withKey(SlugTheTVDB, "tvdb-key")))
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Video); got != SlugTMDB {
			t.Errorf("video = %q (%T), want the plain tmdb Plugin (video off)", got, comp.Video)
		}
	})

	t.Run("omdb + thetvdb + tmdb => both supplements in the chain", func(t *testing.T) {
		provider, _ := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugOMDb, "omdb-key"),
			withKey(SlugTheTVDB, "tvdb-key"),
		))
		comp := provider.(CompositeProvider)
		chain, ok := comp.Video.(*VideoChainProvider)
		if !ok {
			t.Fatalf("video = %T, want *VideoChainProvider", comp.Video)
		}
		var haveOMDb, haveTVDB bool
		for _, s := range chain.Supplements {
			switch pluginSlug(s) {
			case SlugOMDb:
				haveOMDb = true
			case SlugTheTVDB:
				haveTVDB = true
			}
		}
		if !haveOMDb || !haveTVDB {
			t.Errorf("supplements = %+v, want both OMDb and TheTVDB", chain.Supplements)
		}
	})

	t.Run("omdb key but tmdb off => video still off, plain (no chain)", func(t *testing.T) {
		// A supplement can't enable the video kinds on its own; with no TMDB key
		// video stays off and Video stays plain TMDB (zero calls to OMDb).
		provider, en := buildProvider(testConfig(withKey(SlugOMDb, "omdb-key")))
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Video); got != SlugTMDB {
			t.Errorf("video = %q (%T), want the plain tmdb Plugin (video off)", got, comp.Video)
		}
	})

	t.Run("fanarttv + tmdb => fanart.tv wired into BOTH the video and music chains", func(t *testing.T) {
		// The same fanart.tv key feeds both chains: it supplies artist images in the
		// music chain AND movie/show artwork in the video chain.
		provider, en := buildProvider(testConfig(
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugFanartTV, "fanart-key"),
		))
		if !en.Video || !en.Music {
			t.Errorf("enablement = %+v, want video+music on", en)
		}
		comp := provider.(CompositeProvider)
		// Video: fanart.tv is a supplement in the video chain.
		chain, ok := comp.Video.(*VideoChainProvider)
		if !ok {
			t.Fatalf("video = %T, want *VideoChainProvider (fanart.tv video supplement)", comp.Video)
		}
		var haveFanart bool
		for _, s := range chain.Supplements {
			if pluginSlug(s) == SlugFanartTV {
				haveFanart = true
			}
		}
		if !haveFanart {
			t.Errorf("video supplements = %+v, want the fanarttv Plugin", chain.Supplements)
		}
		// Music: fanart.tv remains wired into the music chain (unchanged).
		if _, ok := comp.Music.(*MusicChainProvider); !ok {
			t.Errorf("music = %T, want *MusicChainProvider (fanart.tv still the artist-image source)", comp.Music)
		}
	})

	t.Run("fanarttv key but tmdb off => video off, plain (no chain)", func(t *testing.T) {
		// A supplement (even fanart.tv, which now serves video) can't enable the video
		// kinds on its own; with no TMDB key video stays off and Video stays plain TMDB
		// (zero calls to fanart.tv on the video side).
		provider, en := buildProvider(testConfig(withKey(SlugFanartTV, "fanart-key")))
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Video); got != SlugTMDB {
			t.Errorf("video = %q (%T), want the plain tmdb Plugin (video off)", got, comp.Video)
		}
	})

	t.Run("omdb disabled => plain TMDB (no chain)", func(t *testing.T) {
		provider, en := buildProvider(testConfig(withKey(SlugTMDB, "tmdb-key")))
		if !en.Video {
			t.Errorf("enablement = %+v, want video on", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Video); got != SlugTMDB {
			t.Errorf("video = %q (%T), want the plain tmdb Plugin (no supplement)", got, comp.Video)
		}
	})

	t.Run("authoritative repointed to OMDb => OMDb leads, TMDB is a supplement", func(t *testing.T) {
		// A Library led by a keyed OMDb: OMDb is the chain's authoritative, and the
		// remaining keyed video providers (TMDB, TheTVDB) run as fill-only supplements
		// in registry order — the anime-swap mechanism, demoable without AniDB.
		provider, en := buildProvider(testConfig(
			withVideoLead(SlugOMDb),
			withKey(SlugTMDB, "tmdb-key"),
			withKey(SlugOMDb, "omdb-key"),
			withKey(SlugTheTVDB, "tvdb-key"),
		))
		if !en.Video {
			t.Errorf("enablement = %+v, want video on (OMDb keyed)", en)
		}
		comp := provider.(CompositeProvider)
		chain, ok := comp.Video.(*VideoChainProvider)
		if !ok {
			t.Fatalf("video = %T, want *VideoChainProvider", comp.Video)
		}
		if got := pluginSlug(chain.Authoritative); got != SlugOMDb {
			t.Errorf("authoritative = %q (%T), want the omdb Plugin (repointed lead)", got, chain.Authoritative)
		}
		// TMDB and TheTVDB are the supplements; OMDb is NOT among them (it leads).
		var haveTMDB, haveTVDB, haveOMDb bool
		for _, s := range chain.Supplements {
			switch pluginSlug(s) {
			case SlugTMDB:
				haveTMDB = true
			case SlugTheTVDB:
				haveTVDB = true
			case SlugOMDb:
				haveOMDb = true
			}
		}
		if !haveTMDB || !haveTVDB || haveOMDb {
			t.Errorf("supplements wrong: tmdb=%v tvdb=%v omdb=%v (want tmdb+tvdb, not omdb)", haveTMDB, haveTVDB, haveOMDb)
		}
	})

	t.Run("authoritative OMDb keyed but TMDB unkeyed => video on, OMDb leads alone", func(t *testing.T) {
		// A globally-disabled-but-keyed authoritative leads even when TMDB is unkeyed:
		// video is on because the AUTHORITATIVE is keyed, not because TMDB is. With no
		// other keyed source it is a plain OMDb lead (no chain wrap).
		provider, en := buildProvider(testConfig(
			withVideoLead(SlugOMDb),
			withKey(SlugOMDb, "omdb-key"),
		))
		if !en.Video {
			t.Errorf("enablement = %+v, want video on (authoritative OMDb keyed)", en)
		}
		comp := provider.(CompositeProvider)
		if got := pluginSlug(comp.Video); got != SlugOMDb {
			t.Errorf("video = %q (%T), want a plain omdb Plugin lead (no other keyed source)", got, comp.Video)
		}
	})

	t.Run("nothing configured => both kinds disabled", func(t *testing.T) {
		provider, en := buildProvider(ProviderConfig{})
		if en != (Enablement{Video: false, Music: false}) {
			t.Errorf("enablement = %+v, want both kinds disabled", en)
		}
		// The composite is still wired (with an unconfigured TMDB + plain
		// MusicBrainz); enablement — not a nil provider — is what gates the calls.
		comp, ok := provider.(CompositeProvider)
		if !ok {
			t.Fatalf("provider = %T, want CompositeProvider", provider)
		}
		if got := pluginSlug(comp.Music); got != SlugMusicBrainz {
			t.Errorf("music = %q (%T), want the plain musicbrainz Plugin", got, comp.Music)
		}
	})
}

// TestServiceSetProviderSwap proves SetProvider changes which provider a
// subsequent pass consults WITHOUT reconstructing the Service — the runtime
// hot-swap seam. ResolveIdentity is the read-only pass entrypoint (no Store
// writes), so it exercises the swap directly.
func TestServiceSetProviderSwap(t *testing.T) {
	first := &stubProvider{meta: TitleMetadata{Matched: true, Name: "First"}}
	second := &stubProvider{meta: TitleMetadata{Matched: true, Name: "Second"}}

	svc := NewService(nil, first, nil, Enablement{Video: true}, "", 0)

	ref := TitleRef{Kind: "movie", Title: "x"}
	name, _, matched, err := svc.ResolveIdentity(context.Background(), ref)
	if err != nil || !matched || name != "First" {
		t.Fatalf("before swap: got (%q, matched=%v, err=%v), want First", name, matched, err)
	}

	// Swap in a different provider (same Service instance).
	svc.SetProvider(second, Enablement{Video: true})

	name, _, matched, err = svc.ResolveIdentity(context.Background(), ref)
	if err != nil || !matched || name != "Second" {
		t.Fatalf("after swap: got (%q, matched=%v, err=%v), want Second", name, matched, err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("call counts = first:%d second:%d, want 1 and 1", first.calls, second.calls)
	}

	// Swapping enablement off makes the same kind report disabled (no lookup).
	svc.SetProvider(second, Enablement{})
	_, _, matched, err = svc.ResolveIdentity(context.Background(), ref)
	if err != nil || matched {
		t.Fatalf("after disabling: matched=%v err=%v, want not matched", matched, err)
	}
	if second.calls != 1 {
		t.Errorf("disabled kind consulted provider: second.calls = %d, want still 1", second.calls)
	}
}
