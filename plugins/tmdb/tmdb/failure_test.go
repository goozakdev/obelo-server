package tmdb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// What happens when a lookup does not produce a record, and why the two halves
// are answered differently (see the package comment).

// The port of internal/enrich's TestTMDBProviderMarksServerErrorsTransient. The
// classification moved: it used to be enrich.ErrTransient wrapped around the
// error this provider returned, and it is now the SDK's *FetchError, which tells
// "the source is briefly unwell" from "the source refused this request". The
// property under test is the same one, and it is the one that decides whether a
// wrong API key is retried quietly forever instead of reaching the Admin.
func TestTMDBStatusErrorsCarryTheirClassification(t *testing.T) {
	var code int
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{}`))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "movie", Title: "Inception", Year: 2010}

	code = http.StatusServiceUnavailable
	_, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	var fe *pluginsdk.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("a 503 from TMDB produced %v, want a *FetchError", err)
	}
	if !fe.IsTransient() {
		t.Fatalf("a 503 from TMDB is not transient — a brief outage would park the item")
	}

	code = http.StatusUnauthorized
	_, err = p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if !errors.As(err, &fe) {
		t.Fatalf("a 401 from TMDB produced %v, want a *FetchError", err)
	}
	if fe.IsTransient() {
		t.Fatal("a 401 from TMDB is transient: a wrong API key would be retried quietly forever " +
			"instead of appearing on the attention list")
	}
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
