package fanarttv

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// ONE INSTANCE, BOTH KINDS — the property this plugin exists to hold.
//
// fanart.tv is the one shipped source serving `kinds: [video, music]`. On a real
// server the host's factory is called once per chain and each call returns a view
// over the SAME module instance, with the host serializing every call into it
// (internal/plugins/metadata.go: metaState.mu, whose own comment names fanart.tv
// as the reason it exists). The Go provider was two instances sharing one
// process-wide throttle; a plugin is one instance, which is the same arrangement
// with the sharing made structural rather than remembered.
//
// These tests are the native half of that claim: one *Provider, driven from both
// chains' worth of calls, keeping its two caches apart and spacing its fetches by
// one interval however they arrive.

// pacedStub is fanartStub with the host wrapped the way main.go wraps the sandbox,
// and with the operator's own pacing override in force so the test runs in
// milliseconds rather than in quarter-seconds. It also stamps the arrival time of
// every request, which is what "paced by one interval" is asserted against.
func pacedStub(t *testing.T, interval time.Duration, body func(path string) string) (*Provider, *[]time.Time, *sync.Mutex) {
	t.Helper()
	ms := int(interval / time.Millisecond)
	var mu sync.Mutex
	var arrivals []time.Time
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{
			Enabled:         true,
			Secret:          "k",
			URL:             baseURL,
			RateLimitMillis: &ms,
		}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body(r.URL.Path)))
		}),
	)
	return New(pluginsdk.PacedHost(host, DefaultInterval)), &arrivals, &mu
}

// TestAConcurrentVideoAndMusicCallArePacedByOneInterval is the acceptance
// criterion: a video call and a music call against fanart.tv, issued at the same
// moment down the two chains, are both answered correctly and the stand-in sees
// them one interval apart — because there is one instance with one pacer, not two
// instances racing each other at the source.
func TestAConcurrentVideoAndMusicCallArePacedByOneInterval(t *testing.T) {
	const interval = 60 * time.Millisecond
	p, arrivals, mu := pacedStub(t, interval, func(path string) string {
		if strings.Contains(path, "/music/") {
			return fanartArtistJSON
		}
		return fanartMovieJSON
	})

	type answer struct {
		artwork []pluginapi.ArtworkRef
		cands   []pluginapi.ArtworkCandidate
		outcome pluginapi.Outcome
		err     error
	}
	var video, music answer

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	// The VIDEO chain's call: fanart.tv's movie/show artwork arrives through Lookup,
	// which is the only call the video chain makes of an artwork-only supplement.
	go func() {
		defer wg.Done()
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceTMDB: "438631"}},
		})
		video = answer{artwork: resp.Record.Artwork, outcome: resp.Outcome, err: err}
	}()
	// The MUSIC chain's call: the artist artwork-candidates list behind the picker.
	go func() {
		defer wg.Done()
		resp, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
			Ref:  pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
			Role: "poster",
		})
		music = answer{cands: resp.Candidates, outcome: resp.Outcome, err: err}
	}()
	wg.Wait()
	elapsed := time.Since(start)

	// Both are answered CORRECTLY — the concurrency does not cross the two kinds'
	// parses over, which is what one instance with two namespaced caches buys.
	if video.err != nil || video.outcome != pluginapi.OutcomeMatched {
		t.Fatalf("video = (%q, %v), want matched", video.outcome, video.err)
	}
	if len(video.artwork) != 2 || video.artwork[0].URL != "https://assets.fanart.tv/movie-poster-best.jpg" {
		t.Errorf("video artwork = %+v, want the best movieposter + moviebackground", video.artwork)
	}
	if music.err != nil || music.outcome != pluginapi.OutcomeMatched {
		t.Fatalf("music = (%q, %v), want matched", music.outcome, music.err)
	}
	if len(music.cands) != 3 || music.cands[0].URL != "https://assets.fanart.tv/thumb-best.jpg" {
		t.Errorf("music candidates = %+v, want the three artistthumb[] by likes", music.cands)
	}

	// And the stand-in saw exactly two requests, one interval apart.
	mu.Lock()
	seen := append([]time.Time(nil), *arrivals...)
	mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the host saw %d requests, want 2 (one per kind)", len(seen))
	}
	gap := seen[1].Sub(seen[0])
	if gap < interval-10*time.Millisecond {
		t.Errorf("the two requests were %v apart, want at least the %v interval — "+
			"a video call and a music call must share ONE pacer", gap, interval)
	}
	if elapsed < interval-10*time.Millisecond {
		t.Errorf("both calls finished in %v, which is less than one %v interval", elapsed, interval)
	}
}

// TestAVideoArtworkCandidatesCallIsUnavailableAndCostsNoRequest: the candidate
// LIST is the artist path's alone. fanart.tv owns no listable set for a movie or a
// show — the video picker lists through the video lead — so the same instance that
// happily serves a video Lookup answers the picker's "not now" for a video role,
// and makes no request doing it. That was ErrSearchUnavailable before the port.
func TestAVideoArtworkCandidatesCallIsUnavailableAndCostsNoRequest(t *testing.T) {
	p, host := fanartStub(t, fanartMovieJSON, 0)
	for _, ref := range []pluginapi.MediaRef{
		{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceTMDB: "438631"}},
		{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "121361"}},
	} {
		resp := candidates(t, p, ref, "poster")
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("%s candidates outcome = %q, want unavailable", ref.Kind, resp.Outcome)
		}
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("a video candidate list made %v requests, want none", seen)
	}
}

// TestOneInstanceServesBothChainsFromOneCachePair: the video chain's lookup and
// the music chain's lookup land in the SAME provider, and each is served from its
// own namespace on re-ask — one instance, two caches, no crossing.
func TestOneInstanceServesBothChainsFromOneCachePair(t *testing.T) {
	p, host := fanartStub(t, `{
      "artistthumb": [{"url":"https://x/artist.jpg","likes":"5"}],
      "movieposter": [{"url":"https://x/movie-poster.jpg","likes":"5"}]
    }`, 0)
	ctx := context.Background()
	refs := []pluginapi.MediaRef{
		{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "shared-id"}},
		{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceTMDB: "shared-id"}},
	}
	// Two passes: the second must be served entirely from the caches.
	for pass := 0; pass < 2; pass++ {
		for _, ref := range refs {
			resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: ref})
			if err != nil || resp.Outcome != pluginapi.OutcomeMatched {
				t.Fatalf("pass %d %s = (%q, %v), want matched", pass, ref.Kind, resp.Outcome, err)
			}
		}
	}
	seen := host.Paths()
	if len(seen) != 2 {
		t.Fatalf("request count = %d, want 2 (one per namespace, both cached after); paths=%v", len(seen), seen)
	}
	if !strings.HasSuffix(seen[0], "/music/shared-id") || !strings.HasSuffix(seen[1], "/movies/shared-id") {
		t.Errorf("paths = %v, want the artist endpoint then the movie one", seen)
	}
}

// TestFanartTVReadsItsSettingsPerCall: the key and the base URL are not fields on
// the provider — they are read from the Host on every call, so an operator's save
// takes effect on the next call rather than on the next rebuild.
func TestFanartTVReadsItsSettingsPerCall(t *testing.T) {
	var keys []string
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "first", URL: baseURL}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keys = append(keys, r.URL.Query().Get("api_key"))
			_, _ = w.Write([]byte(fanartArtistJSON))
		}),
	)
	p := New(host)
	lookup(t, p, pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "one"}})
	host.SetSettings(pluginapi.Settings{Enabled: true, Secret: "second", URL: baseURL})
	lookup(t, p, pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: "two"}})

	if len(keys) != 2 || keys[0] != "first" || keys[1] != "second" {
		t.Errorf("api keys sent = %v, want [first second] — the key is read per call", keys)
	}
}
