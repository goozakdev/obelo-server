package fanarttv

import (
	"context"
	"errors"
	"net/http"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// What a failed fetch becomes, and why the answer is two different things.
//
// The Go provider had ONE answer for every non-2xx that was not a 404: a Go
// error, which the fill-only chains logged and swallowed. That is preserved for
// the statuses that describe OUR REQUEST. It is deliberately NOT preserved for
// the ones that describe the SOURCE — a 408, a 429, a 5xx — because a Go error
// from a guest is a STRIKE, and three consecutive ones disable the plugin
// (.scratch/bundled-plugins issue 04's follow-up). One bad afternoon at
// fanart.tv would otherwise take artwork off the server entirely, where the
// Built-in simply retried. Those answer OutcomeUnavailable instead, which the
// host reads as "could not ask" and counts against nobody.

// statusHost builds a Provider whose host answers every request with one status
// and an empty body.
func statusHost(t *testing.T, status int) (*Provider, *sdktest.Host) {
	t.Helper()
	return fanartStub(t, `{}`, status)
}

// TestFanartTVAServerErrorIsUnavailable is the one this issue asks for by name: a
// 503 answers `unavailable`, not a Go error and not a no-match.
func TestFanartTVAServerErrorIsUnavailable(t *testing.T) {
	p, _ := statusHost(t, http.StatusServiceUnavailable)
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
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

// TestFanartTVStatusErrorsCarryTheirClassification states the whole line, in both
// directions, for the artist path.
func TestFanartTVStatusErrorsCarryTheirClassification(t *testing.T) {
	t.Run("a status describing the source is unavailable", func(t *testing.T) {
		for _, status := range []int{408, 429, 500, 502, 503, 504} {
			p, _ := statusHost(t, status)
			resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
				Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
			})
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
		// A rejected key, a forbidden request or a malformed query is not something
		// asking again will fix, and only the operator can. The item is parked where
		// they will see it, exactly as the Go provider left it.
		for _, status := range []int{400, 401, 403} {
			p, _ := statusHost(t, status)
			_, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
				Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
			})
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

	t.Run("a 404 is this source's no-match, in both kinds", func(t *testing.T) {
		// fanart.tv answers 404 for an id it has never heard of, and that is an answer
		// about the ITEM. It was ErrNoMatch before the port and it is OutcomeNoMatch now.
		p, _ := statusHost(t, http.StatusNotFound)
		for _, ref := range []pluginapi.MediaRef{
			{Kind: "artist", MusicbrainzID: mbid},
			{Kind: "movie", TMDBID: "438631"},
			{Kind: "show", TheTVDBID: "121361"},
		} {
			resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
			if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
				t.Errorf("%s 404 = (%q, %v), want no-match and no error", ref.Kind, resp.Outcome, err)
			}
		}
	})

	t.Run("a document this code cannot read stays a Go error", func(t *testing.T) {
		p, _ := fanartStub(t, `oops, not json`, 0)
		if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
		}); err == nil {
			t.Error("err = nil, want the decode failure as a real error")
		}
	})
}

// TestEveryFanartTVCallPathTreatsARetryableStatusAsUnavailable covers the calls
// the host can make — the two Lookup halves and the candidate list — so a path
// cannot be added that forgets the rule. The Go provider's
// TestFanartTVArtistCandidatesNon2xxIsError and TestFanartTVVideoNon2xxIsError
// asserted "a 500 is a real error"; this is the same assertion under the rule that
// replaced it.
func TestEveryFanartTVCallPathTreatsARetryableStatusAsUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("artist lookup", func(t *testing.T) {
		p, _ := statusHost(t, http.StatusInternalServerError)
		resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}})
		requireUnavailableLookup(t, resp, err)
	})

	t.Run("video lookup", func(t *testing.T) {
		p, _ := statusHost(t, http.StatusInternalServerError)
		resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: pluginapi.MediaRef{Kind: "movie", TMDBID: "438631"}})
		requireUnavailableLookup(t, resp, err)
	})

	t.Run("artwork candidates", func(t *testing.T) {
		p, _ := statusHost(t, http.StatusInternalServerError)
		resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
			Ref:  pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
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

// TestFanartTVARefusedFetchIsUnavailable: a host that will not reach the target at
// all is the other half of what Unavailable covers. The plugin learned nothing
// about the item, so it must not claim one.
func TestFanartTVARefusedFetchIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "k", URL: baseURL}),
		sdktest.WithAllowedHosts("example.invalid"),
	)
	p := New(host)
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid},
	})
	if err != nil {
		t.Fatalf("a refusal must not be a Go error: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
}

// TestFanartTVDoesNotCacheAFailedFetch: an outage must not poison the cache with a
// permanent "this artist has no images". The Go provider's `images` cached only a
// no-match; so does this one.
func TestFanartTVDoesNotCacheAFailedFetch(t *testing.T) {
	var status int = http.StatusServiceUnavailable
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "k", URL: baseURL}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(fanartArtistJSON))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}
	if resp, _ := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref}); resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("first outcome = %q, want unavailable", resp.Outcome)
	}
	status = 0 // the source comes back
	resp := lookup(t, p, ref)
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Record.Artwork) == 0 {
		t.Errorf("second lookup = %+v, want the artwork (a failure must not be cached)", resp)
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
