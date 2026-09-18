package enrich

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The host stops naming the seven (.scratch/bundled-plugins: issue 01).
//
// Two host rules used to be true only of the providers this binary was written
// around, and both of them are what this issue makes true of every Plugin: the
// operator's pacing policy reaches a source, and "Test connection" knows what to
// look up. These tests are the pair of proofs, driven through a registration
// shaped exactly like an Installed plugin's — which is what issues 04–08 turn the
// shipped seven into.

// settingsSpy is a registration that records the Settings the host resolved for it
// and answers a scripted outcome. It stands in for a guest: nothing about it is
// known to this package beyond its Descriptor.
type settingsSpy struct {
	got     pluginapi.Settings
	outcome pluginapi.Outcome
	err     error
	ref     pluginapi.MediaRef
}

func (s *settingsSpy) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	s.ref = req.Ref
	if s.err != nil {
		return pluginapi.LookupResponse{}, s.err
	}
	return pluginapi.LookupResponse{
		Outcome: s.outcome,
		Record:  pluginapi.MetadataRecord{Matched: s.outcome == pluginapi.OutcomeMatched, Name: req.Ref.Title},
	}, nil
}

func (s *settingsSpy) Search(_ context.Context, _ pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

func (s *settingsSpy) ArtworkCandidates(_ context.Context, _ pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

// guestLike registers the spy under a slug with the Descriptor an Installed
// plugin's manifest would produce.
func guestLike(slug string, spy *settingsSpy, probe *pluginapi.MediaRef) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "A Guest",
			Kinds:       []string{KindVideo},
			Role:        RoleSupplement,
			Class:       ClassFull,
			RequiresKey: true,
			DefaultURL:  "https://guest.example.test/v1",
			Probe:       probe,
		},
		New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
			spy.got = s
			return spy, nil
		},
	}
}

// TestTheOperatorsRateLimitReachesEveryProvider is ADR-0059 decision 5 at the seam
// it was broken at. providerSettings used to hand RateLimitMillis to MusicBrainz by
// name and to withhold it from everything else — the `default:` case stated no rate
// limit at all — so an Installed provider was paced by nothing the operator could
// set. It is now stated for whoever is built.
func TestTheOperatorsRateLimitReachesEveryProvider(t *testing.T) {
	spy := &settingsSpy{outcome: pluginapi.OutcomeNoMatch}
	cat := catalogWith(guestLike("a-guest", spy, nil))

	cfg := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "a-guest", Enabled: true, APIKey: "k"},
	}, "en-US", FixedProviderInputs{MusicBrainzRateLimit: 750 * time.Millisecond})

	if p := cat.newProvider(cfg, "a-guest", KindVideo); p == nil {
		t.Fatal("the guest was not built")
	}
	if spy.got.RateLimitMillis == nil {
		t.Fatal("the guest was handed no rate limit; before this issue that was every provider but one")
	}
	if *spy.got.RateLimitMillis != 750 {
		t.Errorf("rate limit = %d ms, want the operator's 750", *spy.got.RateLimitMillis)
	}

	// An explicit 0 is "do not throttle" and must still be STATED — it is a saved
	// setting, and absent would silently substitute the Plugin's own default.
	zero := cat.SettingsToProviderConfig([]store.MetadataProviderRow{
		{Slug: "a-guest", Enabled: true, APIKey: "k"},
	}, "en-US", FixedProviderInputs{})
	cat.newProvider(zero, "a-guest", KindVideo)
	if spy.got.RateLimitMillis == nil || *spy.got.RateLimitMillis != 0 {
		t.Errorf("rate limit = %v, want a stated 0 (the operator's explicit no-throttle)", spy.got.RateLimitMillis)
	}

	// A config that states no policy at all leaves it ABSENT: the guest paces itself.
	if s := testConfig(withKey("a-guest", "k")).providerSettings("a-guest"); s.RateLimitMillis != nil {
		t.Errorf("rate limit = %v, want absent when this server holds no policy", *s.RateLimitMillis)
	}
}

// TestConnectionRunsTheDeclaredProbe is ADR-0059 decision 8: the host runs an
// ordinary lookup with the reference the Plugin declared and keeps the judgment.
// A guest used to fall off the end of a switch over eight slugs into "unknown
// provider" — the settings screen's most operator-visible feature, working only
// for the providers the host was written around.
func TestConnectionRunsTheDeclaredProbe(t *testing.T) {
	probe := &pluginapi.MediaRef{Kind: "movie", Title: "A Guest Probe", Year: 1999}

	t.Run("a matched lookup passes, with the declared reference", func(t *testing.T) {
		spy := &settingsSpy{outcome: pluginapi.OutcomeMatched}
		cat := catalogWith(guestLike("a-guest", spy, probe))
		ok, detail := TestConnection(context.Background(), cat, "a-guest", "k", "", "", "en-US")
		if !ok {
			t.Fatalf("matched probe = ok:false (%q), want a pass", detail)
		}
		if spy.ref.Title != probe.Title || spy.ref.Year != probe.Year || spy.ref.Kind != probe.Kind {
			t.Errorf("looked up %+v, want the manifest-declared probe %+v", spy.ref, *probe)
		}
	})

	t.Run("a no-match passes too — the host answered and the key was accepted", func(t *testing.T) {
		spy := &settingsSpy{outcome: pluginapi.OutcomeNoMatch}
		cat := catalogWith(guestLike("a-guest", spy, probe))
		if ok, detail := TestConnection(context.Background(), cat, "a-guest", "k", "", "", "en-US"); !ok {
			t.Fatalf("no-match probe = ok:false (%q), want a pass", detail)
		}
	})

	t.Run("a refused fetch fails with a sentence", func(t *testing.T) {
		// What a guest reaching outside its manifest allowlist looks like at this seam:
		// the call comes back with an error, and the operator is told what it said.
		spy := &settingsSpy{err: errors.New("plugin: fetch refused: host not in allowlist")}
		cat := catalogWith(guestLike("a-guest", spy, probe))
		ok, detail := TestConnection(context.Background(), cat, "a-guest", "k", "", "", "en-US")
		if ok {
			t.Fatal("a refused probe reported a successful connection")
		}
		if detail == "" || detail == "unknown provider" {
			t.Errorf("detail = %q, want the refusal as a sentence", detail)
		}
	})

	t.Run("an unavailable outcome fails", func(t *testing.T) {
		spy := &settingsSpy{outcome: pluginapi.OutcomeUnavailable}
		cat := catalogWith(guestLike("a-guest", spy, probe))
		if ok, _ := TestConnection(context.Background(), cat, "a-guest", "k", "", "", "en-US"); ok {
			t.Error("an unavailable probe reported a successful connection")
		}
	})

	t.Run("no declared probe says so", func(t *testing.T) {
		spy := &settingsSpy{outcome: pluginapi.OutcomeMatched}
		cat := catalogWith(guestLike("a-guest", spy, nil))
		ok, detail := TestConnection(context.Background(), cat, "a-guest", "k", "", "", "en-US")
		if ok {
			t.Fatal("a Plugin with no probe reported a successful connection")
		}
		if detail != "this provider declares no connection probe" {
			t.Errorf("detail = %q, want the no-probe sentence", detail)
		}
	})
}

// TestEveryBuiltInDeclaresItsProbe: the switch connectivity.go used to hold is now
// eight declarations, and the guard is that none of them went missing in the move.
func TestEveryBuiltInDeclaresItsProbe(t *testing.T) {
	for _, r := range MetadataPlugins() {
		if r.Descriptor.Probe == nil {
			t.Errorf("%s declares no connection probe; its Test connection would refuse to run", r.Descriptor.Slug)
			continue
		}
		if r.Descriptor.Probe.Kind == "" {
			t.Errorf("%s declares a probe with no kind", r.Descriptor.Slug)
		}
	}
}
