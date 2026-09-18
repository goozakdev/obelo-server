package plugins_test

import (
	"context"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A MULTI-KIND METADATA PROVIDER: one module, one instance, a view per chain.
//
// fanart.tv is the one shipped source that serves both media kinds, and as a
// Bundled plugin it declares `kinds: [video, music]` in a single `provides` entry
// (.scratch/bundled-plugins issue 07). What that has to produce is:
//
//   - ONE registration, whose Descriptor serves both kinds — so the enrichment
//     Catalog lists it once, the settings screen shows one row with one key, and
//     both chains find it by the same id.
//   - a factory the host calls ONCE PER CHAIN, each call returning a view over the
//     SAME Plugin. The video chain and the music chain each hold their own
//     resolved Settings; there is still one module, one linear memory and one set
//     of caches behind them, which is what makes "one instance serving both
//     chains" true rather than aspirational.
//   - NO INTERLEAVING. Each view carries its own settings window, and settings_get
//     answers whichever call is on the stack — so two calls overlapping inside one
//     instance would let one read the other's secret. metaState.mu is what stops
//     that, taken before callMu and never by a host function.
//
// These are that, driven through a real module across the real ABI. They were
// written to CONFIRM the behaviour rather than to change it: nothing in
// internal/plugins needed fixing for a multi-kind manifest, and this file is the
// evidence for that sentence.

// bothKindsProvides is the manifest entry of an artwork-only supplement serving
// BOTH kinds — fanart.tv's shape.
func bothKindsProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds:        []string{pluginapi.KindVideo, pluginapi.KindMusic},
		Role:         pluginapi.RoleSupplement,
		Class:        pluginapi.ClassArtworkOnly,
		Capabilities: []pluginapi.Capability{pluginapi.CapabilityArtworkCandidates},
	}
}

// TestAMultiKindManifestIsOneRegistrationServingBothKinds: the Descriptor carries
// both kinds, and the registry holds exactly one entry for the id.
func TestAMultiKindManifestIsOneRegistrationServingBothKinds(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("both-kinds", bothKindsProvides()))

	set := load(t, dataDir, &logSink{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)

	var seen int
	for _, r := range reg.MetadataProviders() {
		if r.Descriptor.Slug == "both-kinds" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the registry holds %d entries for a two-kind manifest, want exactly 1 — "+
			"two would be two settings rows and two API keys for one source", seen)
	}

	registration, ok := reg.MetadataProvider("both-kinds")
	if !ok {
		t.Fatal("the Set registered no Metadata provider for a two-kind manifest")
	}
	d := registration.Descriptor
	if !d.Serves(pluginapi.KindVideo) || !d.Serves(pluginapi.KindMusic) {
		t.Errorf("kinds = %v, want both video and music — losing one takes a whole chain's artwork away", d.Kinds)
	}
	if d.Role != pluginapi.RoleSupplement || d.Class != pluginapi.ClassArtworkOnly {
		t.Errorf("role/class = %q/%q, want supplement/artwork", d.Role, d.Class)
	}
}

// TestTwoChainsGetTwoViewsOverOneInstance: the factory answers a DISTINCT provider
// per call (each chain holds its own Settings) over ONE Plugin — proved by the
// plugin-scoped key-value namespace, which is the instance's own state: what the
// video view writes, the music view reads.
func TestTwoChainsGetTwoViewsOverOneInstance(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("both-kinds", bothKindsProvides()))

	kv := newMemKV()
	set := loadWithKV(t, dataDir, &logSink{}, kv)
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.MetadataProvider("both-kinds")
	if !ok {
		t.Fatal("the Set registered no Metadata provider")
	}

	// The host builds one per chain, from the same settings row, exactly as
	// Catalog.newProvider does for the video chain and again for the music chain.
	const kvMode = "https://source.example.test/v1?obelo-mode=kv"
	video, err := registration.New(pluginapi.Settings{Enabled: true, Secret: "video-view", URL: kvMode})
	if err != nil {
		t.Fatalf("building the video view: %v", err)
	}
	music, err := registration.New(pluginapi.Settings{Enabled: true, Secret: "music-view", URL: kvMode})
	if err != nil {
		t.Fatalf("building the music view: %v", err)
	}
	if video == music {
		t.Error("the factory answered the same value twice; each chain carries its own Settings")
	}

	ctx := context.Background()
	// The "kv" mode writes this call's secret under one shared key and reads it
	// back. Run each view once: each must read its OWN secret, because the host
	// serializes the two calls.
	for _, tc := range []struct {
		name     string
		provider pluginapi.MetadataProvider
		ref      pluginapi.MediaRef
		want     string
	}{
		{"video", video, pluginapi.MediaRef{Kind: "movie", Title: "Dune"}, "video-view"},
		{"music", music, pluginapi.MediaRef{Kind: "artist", Title: "Radiohead"}, "music-view"},
	} {
		resp, err := tc.provider.Lookup(ctx, pluginapi.LookupRequest{Ref: tc.ref})
		if err != nil {
			t.Fatalf("%s Lookup: %v", tc.name, err)
		}
		if resp.Outcome != pluginapi.OutcomeMatched {
			t.Fatalf("%s outcome = %q, want matched", tc.name, resp.Outcome)
		}
		if resp.Record.Overview != tc.want {
			t.Errorf("%s view read %q out of the instance's own namespace, want %q",
				tc.name, resp.Record.Overview, tc.want)
		}
	}

	// ONE namespace, because there is one Plugin. Two instances would be two.
	if ns := kv.namespaces(); len(ns) != 1 || ns["both-kinds"] == 0 {
		t.Errorf("key-value namespaces = %v, want exactly one (both-kinds) — "+
			"two views over one Plugin share one instance's state", ns)
	}
}

// TestAVideoCallAndAMusicCallCannotInterleaveInOneInstance is the one the issue
// asks for by name.
//
// Each view has its own settings window and its own secret. The guest writes this
// call's secret into the plugin's key-value namespace and reads the SAME key back
// in the same call. If the two views' calls ever overlapped inside the instance,
// one would read the other's secret — and with the two running concurrently in a
// tight loop, it would happen. metaState.mu is what makes it impossible, and this
// fails loudly if it is ever removed.
func TestAVideoCallAndAMusicCallCannotInterleaveInOneInstance(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("both-kinds", bothKindsProvides()))

	set := loadWithKV(t, dataDir, &logSink{}, newMemKV())
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, _ := reg.MetadataProvider("both-kinds")

	const kvMode = "https://source.example.test/v1?obelo-mode=kv"
	views := []struct {
		name   string
		secret string
		ref    pluginapi.MediaRef
	}{
		{"video", "video-view", pluginapi.MediaRef{Kind: "movie", Title: "Dune"}},
		{"music", "music-view", pluginapi.MediaRef{Kind: "artist", Title: "Radiohead"}},
	}

	const rounds = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	var crossed []string

	for _, v := range views {
		provider, err := registration.New(pluginapi.Settings{Enabled: true, Secret: v.secret, URL: kvMode})
		if err != nil {
			t.Fatalf("building the %s view: %v", v.name, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{Ref: v.ref})
				if err != nil {
					mu.Lock()
					crossed = append(crossed, v.name+" Lookup failed: "+err.Error())
					mu.Unlock()
					return
				}
				if resp.Record.Overview != v.secret {
					mu.Lock()
					crossed = append(crossed, v.name+" read "+resp.Record.Overview+", want "+v.secret)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if len(crossed) > 0 {
		t.Errorf("a video call and a music call interleaved inside one instance (%d of %d round trips): %v",
			len(crossed), 2*rounds, crossed)
	}
}

// TestAMultiKindGuestAnswersBothKinds: the same instance answers a video ref and a
// music ref, each on its own terms. A supplement that quietly stopped serving one
// kind would still pass every registration test above.
func TestAMultiKindGuestAnswersBothKinds(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("both-kinds", bothKindsProvides()))

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "both-kinds", pluginapi.Settings{
		Enabled: true, Secret: "k",
		URL:  "https://source.example.test/v1",
		URL2: "https://images.example.test",
	})

	ctx := context.Background()
	for _, ref := range []pluginapi.MediaRef{
		{Kind: "movie", Title: "Dune", Year: 2021},
		{Kind: "artist", Title: "Radiohead"},
	} {
		resp, err := provider.Lookup(ctx, pluginapi.LookupRequest{Ref: ref})
		if err != nil {
			t.Fatalf("%s Lookup: %v", ref.Kind, err)
		}
		if resp.Outcome != pluginapi.OutcomeMatched {
			t.Errorf("%s outcome = %q, want matched — one instance serves both kinds", ref.Kind, resp.Outcome)
		}
		if len(resp.Record.Artwork) == 0 {
			t.Errorf("%s record carried no artwork: %+v", ref.Kind, resp.Record)
		}
	}
}
