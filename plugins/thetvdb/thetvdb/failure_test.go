package thetvdb

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

// What happens when a lookup does not produce a record, and why the halves are
// answered differently (see the package comment).
//
// The Go provider answered "a real (non-ErrNoMatch) error" to every non-2xx but a
// 404 and let internal/enrich's retryableStatus decide which ones the pass
// retried. A guest cannot wrap a server sentinel, so the same decision travels as
// the OUTCOME — and the stakes went UP with the move. A Go error from a guest is a
// STRIKE: internal/plugins drops the instance and counts a failure, and three
// consecutive failures disable the plugin. So classifying a 503 as an error would
// not merely park three shows; it would take TheTVDB off this server entirely
// until an Admin pressed Re-enable.
//
// TheTVDB has one status of its own, and it is the reason this table needs its own
// stand-in: a 401 on a DATA call is a stale token, not a failure. It is answered
// by a re-login and a retry (thetvdb_test.go), and only the SECOND one is an
// error.

// statusHost answers /login with a token and every DATA request with one status.
func statusHost(code int) *sdktest.Host {
	return sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok"}}`))
				return
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{}`))
		}),
	)
}

func TestTheTVDBStatusErrorsCarryTheirClassification(t *testing.T) {
	ref := pluginapi.MediaRef{Kind: "show", Title: "Breaking Bad", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "81189"}}

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
			t.Errorf("a %d from TheTVDB produced a Go error (%v), which the host counts as a "+
				"strike — three of them disable the plugin", code, err)
			continue
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("a %d from TheTVDB answered %q, want unavailable (never no-match: it says "+
				"nothing about the show)", code, resp.Outcome)
			continue
		}
		if !strings.Contains(resp.Detail, strconv.Itoa(code)) {
			t.Errorf("a %d from TheTVDB left the detail %q, which does not name the status an "+
				"operator would need to read", code, resp.Detail)
		}
	}

	// A status OUR REQUEST owns: still a Go error, so the item is parked where the
	// Admin will see it. The 401 is here too, and it belongs here: it reaches this
	// point only AFTER a re-login answered 401 again, which is the apikey itself
	// being rejected.
	for _, code := range []int{
		http.StatusBadRequest,   // 400
		http.StatusUnauthorized, // 401, twice over — see the re-login test
		http.StatusForbidden,    // 403
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err == nil {
			t.Errorf("a %d from TheTVDB answered %q with no error; it must reach the operator",
				code, resp.Outcome)
			continue
		}
		var fe *pluginsdk.FetchError
		if !errors.As(err, &fe) {
			t.Errorf("a %d from TheTVDB produced %v, want a *FetchError", code, err)
			continue
		}
		if fe.IsTransient() {
			t.Errorf("a %d from TheTVDB is transient: it would be retried quietly forever "+
				"instead of appearing on the attention list", code)
		}
	}

	// And the one status that is neither: a 404 is TheTVDB's "no record", which was
	// ErrNoMatch for the Go provider and is OutcomeNoMatch here.
	p := New(statusHost(http.StatusNotFound))
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("a 404 answered (%q, %v), want no-match and no error", resp.Outcome, err)
	}
}

// A retryable status on the LOGIN is unavailable too. It is worth its own case
// because the login is a step the Go provider took before every fetch on a cold
// instance, and a source that is briefly down answers it rather than the data
// endpoint — so a classifier that only looked at data calls would strike the
// plugin for exactly the outage this rule exists for.
func TestTheTVDBARetryableLoginFailureIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{}`))
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "81189"}},
	})
	if err != nil {
		t.Fatalf("a 503 on the login became a Go error: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
	}
	if !strings.Contains(resp.Detail, "503") {
		t.Errorf("detail = %q, want the login's status in it", resp.Detail)
	}
}

// A 401 on the LOGIN is the apikey being rejected on its way in, and there is no
// token to refresh — so it is a Go error at once, with no retry.
func TestTheTVDBARejectedKeyOnLoginIsAGoError(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}),
	)
	p := New(host)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "81189"}},
	}); err == nil {
		t.Fatal("a rejected apikey answered with no error; it must reach the operator")
	}
}

// A login that answers 200 with no token in it is a document this provider cannot
// use: a Go error, exactly as the Go provider's "thetvdb login returned no token"
// was, and never a claim about the show.
func TestTheTVDBALoginWithNoTokenIsAGoError(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
		}),
	)
	p := New(host)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "81189"}},
	}); err == nil {
		t.Fatal("a tokenless login answered with no error")
	}
}

// A fetch the HOST would not make is OutcomeUnavailable and NEVER OutcomeNoMatch:
// a refusal says nothing at all about the show, and reading it as "TheTVDB has no
// such record" would park a perfectly matchable Title (ADR-0048).
func TestTheTVDBAHostRefusalIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithAllowedHosts("example.test"), // not api4.thetvdb.com
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(seriesJSON))
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "121361"}},
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
func TestTheTVDBASpentBudgetIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithFetch(func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
			return pluginapi.FetchResponse{Error: deadlineSentence}, nil
		}),
	)
	p := New(host)

	for _, kind := range []string{"show", "season", "episode"} {
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: kind, Title: "Breaking Bad", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "81189"},
				SeasonNumber: 1, EpisodeNumber: 1},
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
}

// A failure is NOT cached: the Go provider stored only a parsed record or the
// negative zero value, so a source that was briefly down is asked again on the
// next pass rather than remembered as having nothing.
func TestTheTVDBDoesNotCacheAFailure(t *testing.T) {
	down := true
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok"}}`))
				return
			}
			if down {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(seriesJSON))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "show", ExternalIDs: map[string]string{pluginapi.NamespaceTheTVDB: "121361"}}

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("first lookup = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	down = false
	resp, err = p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched — an outage must not be cached as a no-match", resp.Outcome)
	}
}
