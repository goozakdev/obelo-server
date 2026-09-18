package tmdb

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// What happens when a lookup does not produce a record, and why the two halves
// are answered differently (see the package comment).

// The port of internal/enrich's TestTMDBProviderMarksServerErrorsTransient — the
// same line, one layer over.
//
// It used to be `enrich.ErrTransient` wrapped around the error the Go provider
// returned, and the pass read it to decide whether to retry the item or park it.
// A guest cannot wrap a server sentinel, so the same decision now travels as the
// OUTCOME: a status that describes the SOURCE is OutcomeUnavailable, and a status
// that describes OUR REQUEST is still a Go error.
//
// The stakes went UP with the move, which is why this is worth a table. A Go error
// from a guest is a STRIKE: internal/plugins drops the instance and counts a
// failure, and three consecutive failures disable the plugin. So classifying a 503
// as an error would not merely park three Titles — it would take TMDB off this
// server entirely until an Admin pressed Re-enable.
func TestTMDBStatusErrorsCarryTheirClassification(t *testing.T) {
	ref := pluginapi.MediaRef{Kind: "movie", Title: "Inception", Year: 2010}

	// A status the SOURCE owns: unavailable, with the status in the Detail, and no
	// Go error for the host to count against the plugin.
	for _, code := range []int{
		http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err != nil {
			t.Errorf("a %d from TMDB produced a Go error (%v), which the host counts as a strike "+
				"— three of them disable the plugin", code, err)
			continue
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("a %d from TMDB answered %q, want unavailable (never no-match: it says "+
				"nothing about the item)", code, resp.Outcome)
			continue
		}
		if !strings.Contains(resp.Detail, strconv.Itoa(code)) {
			t.Errorf("a %d from TMDB left the detail %q, which does not name the status an "+
				"operator would need to read", code, resp.Detail)
		}
	}

	// A status OUR REQUEST owns: still a Go error, so the item is parked where the
	// Admin will see it. Asking again with the same key or the same id gets the
	// same answer, and retrying it quietly forever is how a rejected credential
	// stays invisible.
	for _, code := range []int{
		http.StatusBadRequest,   // 400
		http.StatusUnauthorized, // 401
		http.StatusForbidden,    // 403
		http.StatusNotFound,     // 404
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err == nil {
			t.Errorf("a %d from TMDB answered %q with no error; it must reach the operator",
				code, resp.Outcome)
			continue
		}
		var fe *pluginsdk.FetchError
		if !errors.As(err, &fe) {
			t.Errorf("a %d from TMDB produced %v, want a *FetchError", code, err)
			continue
		}
		if fe.IsTransient() {
			t.Errorf("a %d from TMDB is transient: it would be retried quietly forever instead "+
				"of appearing on the attention list", code)
		}
	}
}

// Every call path applies the same rule, not only Lookup. The picker saying "this
// record has no images" because TMDB was briefly down is the same mistake in a
// place a human is watching.
func TestEveryTMDBCallPathTreatsARetryableStatusAsUnavailable(t *testing.T) {
	p := New(statusHost(http.StatusServiceUnavailable))
	ctx := context.Background()

	if resp, err := p.Lookup(ctx, pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"},
	}); err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("lookup = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.Search(ctx, pluginapi.SearchRequest{Kind: "movie", Query: "Dune"}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("search = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"}, Role: "poster",
	}); err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("artwork candidates = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.SeriesSeasons(ctx, pluginapi.SeriesSeasonsRequest{SeriesID: "1399"}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("series seasons = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.SeasonEpisodes(ctx, pluginapi.SeasonEpisodesRequest{SeriesID: "1399", Season: 1}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("season episodes = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
}

// statusHost answers every fetch with one status and an empty JSON document.
func statusHost(code int) *sdktest.Host {
	return sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{}`))
		}),
	)
}

// A fetch the HOST would not make is OutcomeUnavailable and NEVER OutcomeNoMatch:
// a refusal says nothing at all about the item, and reading it as "this source
// has no such movie" would park a perfectly matchable Title (ADR-0048).
func TestTMDBAHostRefusalIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithAllowedHosts("example.test"), // not api.themoviedb.org
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(movieDetailsJSON))
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"},
	})
	if err != nil {
		t.Fatalf("a refusal became a Go error, which the host counts against the plugin: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
	}
	if !strings.Contains(resp.Detail, sdktest.RefusedAllowlist) {
		t.Errorf("detail = %q, want the host's own refusal in it", resp.Detail)
	}
}

// deadlineSentence is what the host answers a fetch that ran out of the call's
// budget (ADR-0059 decision 6; internal/plugins owns the string). It is restated
// here rather than imported because this module does not — and must not — depend
// on the server.
const deadlineSentence = "the fetch did not finish before this call's deadline"

// A slow upstream is the case ADR-0059 decision 6 exists for: the host hands the
// guest a fetch error instead of killing it, and the guest turns that into
// unavailable, so the item takes ADR-0048's backoff and NO failure is counted.
func TestTMDBASpentBudgetIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithFetch(func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
			return pluginapi.FetchResponse{Error: deadlineSentence}, nil
		}),
	)
	p := New(host)

	for _, kind := range []string{"movie", "show"} {
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: kind, Title: "Dune", Year: 2021},
		})
		if err != nil {
			t.Fatalf("%s: a spent budget became a Go error: %v", kind, err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Fatalf("%s: outcome = %q, want unavailable (never no-match)", kind, resp.Outcome)
		}
		if !strings.Contains(resp.Detail, deadlineSentence) {
			t.Errorf("%s: detail = %q, want the host's deadline sentence in it", kind, resp.Detail)
		}
	}

	// The same rule on the two list calls and on search, because the picker must
	// say "not now" rather than "this record has no images".
	sr, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "Dune"})
	if err != nil || sr.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("search = (%q, %v), want unavailable and no error", sr.Outcome, err)
	}
	ar, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"}, Role: "poster",
	})
	if err != nil || ar.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("artwork candidates = (%q, %v), want unavailable and no error", ar.Outcome, err)
	}
	ss, err := p.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "1399"})
	if err != nil || ss.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("series seasons = (%q, %v), want unavailable and no error", ss.Outcome, err)
	}
	se, err := p.SeasonEpisodes(context.Background(), pluginapi.SeasonEpisodesRequest{SeriesID: "1399", Season: 1})
	if err != nil || se.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("season episodes = (%q, %v), want unavailable and no error", se.Outcome, err)
	}
}
