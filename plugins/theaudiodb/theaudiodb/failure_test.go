package theaudiodb

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// What a failed fetch becomes, and why the answer is two different things.
//
// The Go provider had ONE answer for every non-2xx that was not a 404: a Go
// error, which the fill-only music chain logged and swallowed. That is preserved
// for the statuses that describe OUR REQUEST. It is deliberately NOT preserved
// for the ones that describe the SOURCE — a 408, a 429, a 5xx — because a Go
// error from a guest is a STRIKE, and three consecutive ones disable the plugin
// (.scratch/bundled-plugins issue 04's follow-up). One bad afternoon at
// TheAudioDB would otherwise take artist images and biographies off the server
// entirely, where the Built-in simply retried. Those answer OutcomeUnavailable
// instead, which the host reads as "could not ask" and counts against nobody.

// TestTheAudioDBAServerErrorIsUnavailable is the one this issue asks for by name:
// a 503 answers `unavailable`, not a Go error and not a no-match.
func TestTheAudioDBAServerErrorIsUnavailable(t *testing.T) {
	p, _ := audiodbStub(t, `{}`, http.StatusServiceUnavailable)
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
	})
	if err != nil {
		t.Fatalf("a 503 must not be a Go error (it is a strike against the plugin): %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
	}
	if resp.Detail == "" {
		t.Error("Detail is empty; the operator's log line has nothing to say")
	}
}

// TestTheAudioDBStatusErrorsCarryTheirClassification states the whole line, in
// both directions.
func TestTheAudioDBStatusErrorsCarryTheirClassification(t *testing.T) {
	ref := pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}}

	t.Run("a status describing the source is unavailable", func(t *testing.T) {
		for _, status := range []int{408, 429, 500, 502, 503, 504} {
			p, _ := audiodbStub(t, `{}`, status)
			resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
			if err != nil {
				t.Errorf("status %d: err = %v, want the unavailable answer", status, err)
				continue
			}
			if resp.Outcome != pluginapi.OutcomeUnavailable {
				t.Errorf("status %d: outcome = %q, want unavailable", status, resp.Outcome)
			}
		}
	})

	t.Run("a status describing our request stays a Go error", func(t *testing.T) {
		for _, status := range []int{400, 401, 403} {
			p, _ := audiodbStub(t, `{}`, status)
			_, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
			if err == nil {
				t.Errorf("status %d: err = nil, want a real error", status)
				continue
			}
			var fe *pluginsdk.FetchError
			if !errors.As(err, &fe) || fe.IsTransient() {
				t.Errorf("status %d: err = %v, want a non-transient *FetchError", status, err)
			}
		}
	})

	t.Run("a 404 is this source's no-match", func(t *testing.T) {
		// A 404 is an answer about the ITEM. It was ErrNoMatch before the port and it
		// is OutcomeNoMatch now, on both the artist and the track path.
		p, _ := audiodbStub(t, `{}`, http.StatusNotFound)
		for _, r := range []pluginapi.MediaRef{
			{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
			{Kind: "track", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
		} {
			resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: r})
			if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
				t.Errorf("%s 404 = (%q, %v), want no-match and no error", r.Kind, resp.Outcome, err)
			}
		}
	})

	t.Run("a document this code cannot read stays a Go error", func(t *testing.T) {
		p, _ := audiodbStub(t, `oops, not json`, 0)
		if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref}); err == nil {
			t.Error("err = nil, want the decode failure as a real error")
		}
	})
}

// TestEveryTheAudioDBCallPathTreatsARetryableStatusAsUnavailable covers the three
// calls the host can make, so a path cannot be added that forgets the rule.
func TestEveryTheAudioDBCallPathTreatsARetryableStatusAsUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("artist lookup", func(t *testing.T) {
		p, _ := audiodbStub(t, `{}`, http.StatusInternalServerError)
		resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}}})
		requireUnavailableLookup(t, resp, err)
	})

	t.Run("track lookup", func(t *testing.T) {
		p, _ := audiodbStub(t, `{}`, http.StatusInternalServerError)
		resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: pluginapi.MediaRef{Kind: "track", Track: "Creep", Artist: "Radiohead"}})
		requireUnavailableLookup(t, resp, err)
	})

	t.Run("artwork candidates", func(t *testing.T) {
		p, _ := audiodbStub(t, `{}`, http.StatusInternalServerError)
		resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
			Ref:  pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
			Role: "poster",
		})
		if err != nil {
			t.Fatalf("err = %v, want the unavailable answer", err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable || resp.Detail == "" {
			t.Errorf("resp = %+v, want unavailable with a detail", resp)
		}
	})
}

// TestTheAudioDBARefusedFetchIsUnavailable: a host that will not reach the target
// at all is the other half of what Unavailable covers. The plugin learned nothing
// about the item, so it must not claim one.
func TestTheAudioDBARefusedFetchIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "k", URL: baseURL}),
		sdktest.WithAllowedHosts("example.invalid"),
	)
	p := New(host)
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}},
	})
	if err != nil {
		t.Fatalf("a refusal must not be a Go error: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
}

// TestTheAudioDBDoesNotCacheAFailedFetch: an outage must not poison the cache with
// a permanent "this artist has no data". The Go provider's `artist` cached only a
// no-match; so does this one.
func TestTheAudioDBDoesNotCacheAFailedFetch(t *testing.T) {
	status := http.StatusServiceUnavailable
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "k", URL: baseURL, Language: "en-US"}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(audiodbArtistJSON))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: mbid}}
	if resp, _ := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref}); resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("first outcome = %q, want unavailable", resp.Outcome)
	}
	status = 0 // the source comes back
	resp := lookup(t, p, ref)
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Record.Artwork) == 0 {
		t.Errorf("second lookup = %+v, want the artwork (a failure must not be cached)", resp)
	}
}

// TestTheAudioDBPacesItsFetches: pacing is the plugin's now (ADR-0059 decision 5)
// and main.go is where it is wired, so this test wires it the same way and asserts
// that two fetches out of one instance are one interval apart — the Go provider's
// 250 ms throttle, with the operator's own override in force.
func TestTheAudioDBPacesItsFetches(t *testing.T) {
	const interval = 60 * time.Millisecond
	ms := int(interval / time.Millisecond)
	var mu sync.Mutex
	var arrivals []time.Time
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{
			Enabled:         true,
			Secret:          "k",
			URL:             baseURL,
			Language:        "en-US",
			RateLimitMillis: &ms,
		}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
			_, _ = w.Write([]byte(audiodbArtistJSON))
		}),
	)
	p := New(pluginsdk.PacedHost(host, DefaultInterval))

	// Two DIFFERENT artists, so neither is served from the cache.
	var wg sync.WaitGroup
	wg.Add(2)
	for _, id := range []string{"one", "two"} {
		go func() {
			defer wg.Done()
			_, _ = p.Lookup(context.Background(), pluginapi.LookupRequest{
				Ref: pluginapi.MediaRef{Kind: "artist", ExternalIDs: map[string]string{pluginapi.NamespaceMusicBrainz: id}},
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	seen := append([]time.Time(nil), arrivals...)
	mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the host saw %d requests, want 2", len(seen))
	}
	if gap := seen[1].Sub(seen[0]); gap < interval-10*time.Millisecond {
		t.Errorf("the two requests were %v apart, want at least the %v interval", gap, interval)
	}
}

func requireUnavailableLookup(t *testing.T, resp pluginapi.LookupResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("err = %v, want the unavailable answer rather than a strike", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
	if resp.Detail == "" {
		t.Error("Detail is empty; the operator's log line has nothing to say")
	}
}
