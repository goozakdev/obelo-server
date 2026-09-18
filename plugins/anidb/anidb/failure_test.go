package anidb

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
// The Go provider answered "a real (non-ErrNoMatch) error" to every non-2xx and
// let internal/enrich's retryableStatus decide which ones the pass retried. A
// guest cannot wrap a server sentinel, so the same decision travels as the
// OUTCOME — and the stakes went UP with the move. A Go error from a guest is a
// STRIKE: internal/plugins drops the instance and counts a failure, and three
// consecutive failures disable the plugin. AniDB is the provider where that
// matters most, because the thing AniDB does to a client it dislikes is a
// temporary BAN, and taking the provider off the server for it would turn a
// two-hour timeout into an outage nobody notices ending.

// statusHost answers every fetch with one status and an empty document.
func statusHost(code int) *sdktest.Host {
	return sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`<error>server</error>`))
		}),
	)
}

func TestAniDBStatusErrorsCarryTheirClassification(t *testing.T) {
	ref := pluginapi.MediaRef{Kind: "show", Title: "Cowboy Bebop", AniDBID: "1"}

	// A status the SOURCE owns: unavailable, with the status in the Detail, and no
	// Go error for the host to count against the plugin.
	for _, code := range []int{
		http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,     // 429 — AniDB's own rate answer
		http.StatusInternalServerError, // 500
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err != nil {
			t.Errorf("a %d from AniDB produced a Go error (%v), which the host counts as a strike "+
				"— three of them disable the plugin", code, err)
			continue
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("a %d from AniDB answered %q, want unavailable (never no-match: it says "+
				"nothing about the anime)", code, resp.Outcome)
			continue
		}
		if !strings.Contains(resp.Detail, strconv.Itoa(code)) {
			t.Errorf("a %d from AniDB left the detail %q, which does not name the status an "+
				"operator would need to read", code, resp.Detail)
		}
	}

	// A status OUR REQUEST owns: still a Go error, so the item is parked where the
	// Admin will see it. An unregistered or banned client name is what AniDB
	// answers 401/403 to, and retrying that quietly forever is how a bad client
	// name stays invisible.
	for _, code := range []int{
		http.StatusBadRequest,   // 400
		http.StatusUnauthorized, // 401
		http.StatusForbidden,    // 403
		http.StatusNotFound,     // 404
	} {
		p := New(statusHost(code))
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
		if err == nil {
			t.Errorf("a %d from AniDB answered %q with no error; it must reach the operator",
				code, resp.Outcome)
			continue
		}
		var fe *pluginsdk.FetchError
		if !errors.As(err, &fe) {
			t.Errorf("a %d from AniDB produced %v, want a *FetchError", code, err)
			continue
		}
		if fe.IsTransient() {
			t.Errorf("a %d from AniDB is transient: it would be retried quietly forever instead "+
				"of appearing on the attention list", code)
		}
	}
}

// Both call paths apply the same rule, not only Lookup. The picker saying "this
// anime has no cover art" because AniDB was briefly down is the same mistake in a
// place a human is watching.
func TestEveryAniDBCallPathTreatsARetryableStatusAsUnavailable(t *testing.T) {
	p := New(statusHost(http.StatusServiceUnavailable))
	ctx := context.Background()
	ref := pluginapi.MediaRef{Kind: "show", AniDBID: "1"}

	if resp, err := p.Lookup(ctx, pluginapi.LookupRequest{Ref: ref}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("lookup = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
		Ref: ref, Role: "poster",
	}); err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("artwork candidates = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	// Search makes no call at all, so it has nothing to be unavailable about: it
	// answers its empty candidate list either way. That is the manifest's
	// declaration doing its job and is asserted in anidb_test.go.
}

// A fetch the HOST would not make is OutcomeUnavailable and NEVER OutcomeNoMatch:
// a refusal says nothing at all about the anime, and reading it as "AniDB has no
// such record" would park a perfectly matchable Title (ADR-0048).
//
// It is worth its own test here rather than only in the TMDB port, because AniDB
// is the one provider whose configured host carries an explicit port: the host's
// allowlist matches on the HOSTNAME with the port stripped, and a refusal has to
// stay a refusal rather than becoming a verdict about the anime either way.
func TestAniDBAHostRefusalIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithAllowedHosts("example.test"), // not api.anidb.net
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(animeXML))
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
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

// The mirror image: an allowlist naming api.anidb.net covers the configured
// `:9001` endpoint, because the host matches on the hostname with the port
// stripped. Without this the whole provider would be refused on every call.
func TestAniDBsAllowlistedHostCoversItsExplicitPort(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithAllowedHosts("api.anidb.net"), // no port, as a manifest spells it
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(animeXML))
		}),
	)

	resp, err := New(host).Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (detail %q), want matched — `api.anidb.net` must cover :9001",
			resp.Outcome, resp.Detail)
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
func TestAniDBASpentBudgetIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithFetch(func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
			return pluginapi.FetchResponse{Error: deadlineSentence}, nil
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
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

// A body that is not XML at all is a document this provider cannot read: a Go
// error, never a claim about the anime.
func TestAniDBAnUnreadableBodyIsAGoError(t *testing.T) {
	p, _ := stub(t, `{"this":"is json, not the http api"}`)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	}); err == nil {
		t.Fatal("an unreadable body answered with no error")
	}
}

// A failure is NOT cached: the Go provider stored only a parsed record or the
// negative zero value, so a source that was briefly down is asked again on the
// next pass rather than remembered as having nothing.
func TestAniDBDoesNotCacheAFailure(t *testing.T) {
	down := true
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if down {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`<error>down</error>`))
				return
			}
			_, _ = w.Write([]byte(animeXML))
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "show", AniDBID: "1"}

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
