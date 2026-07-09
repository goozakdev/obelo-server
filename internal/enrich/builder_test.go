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
		provider, en := BuildProvider(ProviderConfig{
			TMDBAPIKey:           "tmdb-key",
			MusicBrainzEnabled:   true,
			MusicBrainzRateLimit: 2 * time.Second,
		})
		if en != (Enablement{Video: true, Music: true}) {
			t.Errorf("enablement = %+v, want video+music on", en)
		}
		comp, ok := provider.(CompositeProvider)
		if !ok {
			t.Fatalf("provider = %T, want CompositeProvider", provider)
		}
		if _, ok := comp.Video.(*TMDBProvider); !ok {
			t.Errorf("video = %T, want *TMDBProvider", comp.Video)
		}
		// No image key: Music stays plain MusicBrainz, NOT wrapped in the chain.
		mb, ok := comp.Music.(*MusicBrainzProvider)
		if !ok {
			t.Fatalf("music = %T, want plain *MusicBrainzProvider", comp.Music)
		}
		// The operator's throttle policy is threaded through to the host.
		if mb.MinInterval != 2*time.Second {
			t.Errorf("MinInterval = %v, want 2s (honoring MusicBrainzRateLimit)", mb.MinInterval)
		}
	})

	t.Run("image key + music => MusicChain", func(t *testing.T) {
		provider, en := BuildProvider(ProviderConfig{
			TMDBAPIKey:     "tmdb-key",
			FanartTVAPIKey: "fanart-key",
		})
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
		provider, en := BuildProvider(ProviderConfig{FanartTVAPIKey: "fanart-key"})
		if en != (Enablement{Video: false, Music: false}) {
			t.Errorf("enablement = %+v, want both off", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Music.(*MusicBrainzProvider); !ok {
			t.Errorf("music = %T, want plain *MusicBrainzProvider (music off)", comp.Music)
		}
	})

	t.Run("omdb + tmdb => Video is the chain", func(t *testing.T) {
		provider, en := BuildProvider(ProviderConfig{
			TMDBAPIKey: "tmdb-key",
			OMDbAPIKey: "omdb-key",
		})
		if !en.Video {
			t.Errorf("enablement = %+v, want video on", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*VideoChainProvider); !ok {
			t.Errorf("video = %T, want *VideoChainProvider (OMDb supplement configured)", comp.Video)
		}
	})

	t.Run("thetvdb + tmdb => Video is the chain", func(t *testing.T) {
		provider, en := BuildProvider(ProviderConfig{
			TMDBAPIKey:    "tmdb-key",
			TheTVDBAPIKey: "tvdb-key",
		})
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
		provider, en := BuildProvider(ProviderConfig{TheTVDBAPIKey: "tvdb-key"})
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*TMDBProvider); !ok {
			t.Errorf("video = %T, want plain *TMDBProvider (video off)", comp.Video)
		}
	})

	t.Run("omdb + thetvdb + tmdb => both supplements in the chain", func(t *testing.T) {
		provider, _ := BuildProvider(ProviderConfig{
			TMDBAPIKey:    "tmdb-key",
			OMDbAPIKey:    "omdb-key",
			TheTVDBAPIKey: "tvdb-key",
		})
		comp := provider.(CompositeProvider)
		chain, ok := comp.Video.(*VideoChainProvider)
		if !ok {
			t.Fatalf("video = %T, want *VideoChainProvider", comp.Video)
		}
		var haveOMDb, haveTVDB bool
		for _, s := range chain.Supplements {
			switch s.(type) {
			case *OMDbProvider:
				haveOMDb = true
			case *TheTVDBProvider:
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
		provider, en := BuildProvider(ProviderConfig{OMDbAPIKey: "omdb-key"})
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*TMDBProvider); !ok {
			t.Errorf("video = %T, want plain *TMDBProvider (video off)", comp.Video)
		}
	})

	t.Run("fanarttv + tmdb => fanart.tv wired into BOTH the video and music chains", func(t *testing.T) {
		// The same fanart.tv key feeds both chains: it supplies artist images in the
		// music chain AND movie/show artwork in the video chain.
		provider, en := BuildProvider(ProviderConfig{
			TMDBAPIKey:     "tmdb-key",
			FanartTVAPIKey: "fanart-key",
		})
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
			if _, ok := s.(*FanartTVProvider); ok {
				haveFanart = true
			}
		}
		if !haveFanart {
			t.Errorf("video supplements = %+v, want a *FanartTVProvider", chain.Supplements)
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
		provider, en := BuildProvider(ProviderConfig{FanartTVAPIKey: "fanart-key"})
		if en.Video {
			t.Errorf("enablement = %+v, want video off (supplement can't enable a kind)", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*TMDBProvider); !ok {
			t.Errorf("video = %T, want plain *TMDBProvider (video off)", comp.Video)
		}
	})

	t.Run("omdb disabled => plain TMDB (no chain)", func(t *testing.T) {
		provider, en := BuildProvider(ProviderConfig{TMDBAPIKey: "tmdb-key"})
		if !en.Video {
			t.Errorf("enablement = %+v, want video on", en)
		}
		comp := provider.(CompositeProvider)
		if _, ok := comp.Video.(*TMDBProvider); !ok {
			t.Errorf("video = %T, want plain *TMDBProvider (no supplement)", comp.Video)
		}
	})

	t.Run("nothing configured => both kinds disabled", func(t *testing.T) {
		provider, en := BuildProvider(ProviderConfig{})
		if en != (Enablement{Video: false, Music: false}) {
			t.Errorf("enablement = %+v, want both kinds disabled", en)
		}
		// The composite is still wired (with an unconfigured TMDB + plain
		// MusicBrainz); enablement — not a nil provider — is what gates the calls.
		comp, ok := provider.(CompositeProvider)
		if !ok {
			t.Fatalf("provider = %T, want CompositeProvider", provider)
		}
		if _, ok := comp.Music.(*MusicBrainzProvider); !ok {
			t.Errorf("music = %T, want plain *MusicBrainzProvider", comp.Music)
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
