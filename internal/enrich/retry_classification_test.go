package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Which provider failures are worth trying again (ADR-0048). The distinction is
// the whole feature: classify too little and a provider outage still parks a
// library's worth of Titles; classify too much and a rejected API key becomes an
// invisible retry loop nobody is ever told about.

func TestStatusErrorClassifiesRetryableStatuses(t *testing.T) {
	transientCodes := []int{
		http.StatusRequestTimeout,      // 408 — the provider gave up waiting
		http.StatusTooManyRequests,     // 429 — throttled; the item is fine
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503 — the classic metadata-provider blip
		http.StatusGatewayTimeout,      // 504
	}
	for _, code := range transientCodes {
		if err := statusError("tmdb", "/movie/550", code); !IsTransient(err) {
			t.Errorf("status %d classified permanent (%v) — a Title lost to it would park "+
				"on the attention list until somebody ran a full pass", code, err)
		}
	}

	permanentCodes := []int{
		http.StatusBadRequest,          // 400 — our query is wrong; resending it changes nothing
		http.StatusUnauthorized,        // 401 — the key is rejected; only the Admin can fix it
		http.StatusForbidden,           // 403
		http.StatusUnprocessableEntity, // 422
	}
	for _, code := range permanentCodes {
		if err := statusError("tmdb", "/movie/550", code); IsTransient(err) {
			t.Errorf("status %d classified transient — it would be retried forever instead of "+
				"telling the Admin their credentials or query are wrong", code)
		}
	}
}

// An unclassified error must NOT be retried. The marker is opt-in precisely so a
// failure mode nobody has thought about degrades to the old visible behavior.
func TestUnmarkedErrorIsPermanent(t *testing.T) {
	if IsTransient(errors.New("something nobody classified")) {
		t.Fatal("a bare error reports transient: the classification has become opt-out, so any " +
			"new failure mode silently starts retrying instead of surfacing")
	}
	if IsTransient(nil) {
		t.Fatal("nil reports transient")
	}
}

// Marking an error must not rewrite what it says: the operator reads the log, and
// the retry machinery is not what they are trying to diagnose.
func TestTransientPreservesMessageAndChain(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	err := requestError("tmdb", inner)

	if want := "enrich: tmdb request: dial tcp: connection refused"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, inner) {
		t.Error("the underlying cause is no longer reachable through errors.Is")
	}
	if !IsTransient(err) {
		t.Error("a failed round-trip is not transient")
	}
}

// A no-match must never be mistaken for a connectivity problem: it is a real
// answer about the item, and retrying it wastes a call per pass forever.
func TestNoMatchIsNotTransient(t *testing.T) {
	if IsTransient(ErrNoMatch) {
		t.Fatal("ErrNoMatch reports transient — every genuinely unmatched Title would be " +
			"re-asked on every pass and never reach the Admin's hand-match list")
	}
}

// The backoff climbs and then holds. There is no attempt cap: "we could not reach
// the provider" never becomes evidence about the item, however often it is said.
func TestRetryDelayClimbsToACeilingAndStays(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= len(retryBackoff); attempt++ {
		got := retryDelay(attempt)
		if got <= prev {
			t.Fatalf("retryDelay(%d) = %s, not longer than the previous %s — the backoff is "+
				"not backing off", attempt, got, prev)
		}
		prev = got
	}
	ceiling := retryDelay(len(retryBackoff))
	for _, attempt := range []int{len(retryBackoff) + 1, 50, 10000} {
		if got := retryDelay(attempt); got != ceiling {
			t.Errorf("retryDelay(%d) = %s, want the ceiling %s", attempt, got, ceiling)
		}
	}
	// A zero/negative streak is the first attempt, not a zero wait.
	if retryDelay(0) != retryBackoff[0] {
		t.Errorf("retryDelay(0) = %s, want the first step %s", retryDelay(0), retryBackoff[0])
	}
}

// The escalation threshold and the schedule are one decision: an item reaches the
// Admin's attention list exactly when its backoff reaches the daily ceiling. They
// live in different packages (the store renders the list, enrich owns the
// schedule), so nothing but this test stops them drifting apart.
func TestEscalationMatchesTheBackoffCeiling(t *testing.T) {
	if len(retryBackoff) != store.EnrichRetryEscalateAfter {
		t.Fatalf("backoff schedule has %d steps but store.EnrichRetryEscalateAfter is %d — "+
			"items now either escalate before the backoff has topped out (noise on the "+
			"attention list) or keep retrying past it with nobody told",
			len(retryBackoff), store.EnrichRetryEscalateAfter)
	}
}

// End-to-end through a real provider: an httptest server returning 503 must produce
// a transient error at the seam the pass actually reads, not merely in the helper.
// This is the wiring half — the helpers can be perfect and still be unused.
func TestTMDBProviderMarksServerErrorsTransient(t *testing.T) {
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	p := NewTMDBProvider("key", "en-US", srv.URL, srv.URL)

	code = http.StatusServiceUnavailable
	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Inception", Year: 2010})
	if err == nil || !IsTransient(err) {
		t.Fatalf("a 503 from TMDB produced %v, want a transient error — the classification "+
			"never reaches the pass", err)
	}

	code = http.StatusUnauthorized
	_, err = p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Inception", Year: 2010})
	if err == nil {
		t.Fatal("a 401 from TMDB produced no error at all")
	}
	if IsTransient(err) {
		t.Fatal("a 401 from TMDB is transient: a wrong API key would be retried quietly forever " +
			"instead of appearing on the attention list")
	}
}
