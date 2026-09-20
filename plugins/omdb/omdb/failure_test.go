package omdb

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
//
// The Go provider answered "a real (non-ErrNoMatch) error" to every non-2xx and
// let internal/enrich's retryableStatus decide which ones the pass retried. A
// guest cannot wrap a server sentinel, so the same decision travels as the
// OUTCOME — and the stakes went UP with the move, which is why this is a table. A
// Go error from a guest is a STRIKE: internal/plugins drops the instance and
// counts a failure, and three consecutive failures disable the plugin. So
// classifying a 503 as an error would not merely park three movies; it would take
// OMDb off this server entirely until an Admin pressed Re-enable.

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

func TestOMDbStatusErrorsCarryTheirClassification(t *testing.T) {
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
			t.Errorf("a %d from OMDb produced a Go error (%v), which the host counts as a strike "+
				"— three of them disable the plugin", code, err)
			continue
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("a %d from OMDb answered %q, want unavailable (never no-match: it says "+
				"nothing about the movie)", code, resp.Outcome)
			continue
		}
		if !strings.Contains(resp.Detail, strconv.Itoa(code)) {
			t.Errorf("a %d from OMDb left the detail %q, which does not name the status an "+
				"operator would need to read", code, resp.Detail)
		}
	}

	// A status OUR REQUEST owns: still a Go error, so the item is parked where the
	// Admin will see it. OMDb rejects a bad apikey with a 401, and retrying that
	// quietly forever is how a rejected credential stays invisible.
	for _, code := range []int{
		http.StatusBadRequest,   // 400
		http.StatusUnauthorized, // 401
		http.StatusForbidden,    // 403
		http.StatusNotFound,     // 404
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err == nil {
			t.Errorf("a %d from OMDb answered %q with no error; it must reach the operator",
				code, resp.Outcome)
			continue
		}
		var fe *pluginsdk.FetchError
		if !errors.As(err, &fe) {
			t.Errorf("a %d from OMDb produced %v, want a *FetchError", code, err)
			continue
		}
		if fe.IsTransient() {
			t.Errorf("a %d from OMDb is transient: it would be retried quietly forever instead "+
				"of appearing on the attention list", code)
		}
	}
}

// A fetch the HOST would not make is OutcomeUnavailable and NEVER OutcomeNoMatch:
// a refusal says nothing at all about the movie, and reading it as "OMDb has no
// such record" would park a perfectly matchable Title (ADR-0048).
func TestOMDbAHostRefusalIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithAllowedHosts("example.test"), // not www.omdbapi.com
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(movieJSON))
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceIMDB: "tt0111161"}},
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
func TestOMDbASpentBudgetIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithFetch(func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
			return pluginapi.FetchResponse{Error: deadlineSentence}, nil
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021},
	})
	if err != nil {
		t.Fatalf("a spent budget became a Go error: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable (never no-match)", resp.Outcome)
	}
	if !strings.Contains(resp.Detail, deadlineSentence) {
		t.Errorf("detail = %q, want the host's deadline sentence in it", resp.Detail)
	}
}

// A failure is NOT cached: the Go provider stored only a parsed record or the
// negative zero value, so a source that was briefly down is asked again on the
// next pass rather than remembered as having nothing.
func TestOMDbDoesNotCacheAFailure(t *testing.T) {
	var status int = http.StatusServiceUnavailable
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(movieJSON))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceIMDB: "tt0111161"}}

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("first lookup = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	status = http.StatusOK
	resp, err = p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched — an outage must not be cached as a no-match", resp.Outcome)
	}
	if got := len(host.Requests()); got != 2 {
		t.Errorf("the source was hit %d times, want 2 (the failure was not cached)", got)
	}
}
