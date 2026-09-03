package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Telling "you are rate-limited" apart from "the host is shedding load for
// everyone" (ADR-0049). They arrive as the same three digits and have opposite
// remedies — slow down, versus wait, because slowing down cannot help.

// mbShedHeaders is what musicbrainz.org actually returns while its search cluster
// sheds load: a quota that is not exhausted, and a `who` that is a shed bucket
// rather than a client address.
func mbShedHeaders(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Zone", "search-global")
	w.Header().Set("X-RateLimit-Who", "search-shed")
	w.Header().Set("X-RateLimit-Limit", "1200")
	w.Header().Set("X-RateLimit-Remaining", "676")
	w.Header().Set("Retry-After", "0")
}

func TestRefusalTellsAGlobalShedFromOurOwnQuota(t *testing.T) {
	shed := ProviderRefusal{
		Status: 503, Zone: "search-global", Who: "search-shed",
		Limit: "1200", Remaining: "676",
		Message: "The MusicBrainz web server is currently busy. Please try again later.",
	}
	if shed.ourQuota() {
		t.Error("a global load shed reads as our own quota — the operator is told to throttle, " +
			"which cannot help and slows the pass through the outage")
	}
	if got := shed.String(); !strings.Contains(got, "not our rate limit") {
		t.Errorf("the log line does not say whose problem this is: %q", got)
	}

	ours := ProviderRefusal{
		Status: 503, Zone: "ws", Who: "203.0.113.9",
		Message: "Your requests are exceeding the allowable rate limit.",
	}
	if !ours.ourQuota() {
		t.Error("a per-client rate limit does not read as ours — we would not back off when " +
			"backing off is exactly the fix")
	}
	if got := ours.String(); !strings.Contains(got, "OUR usage") {
		t.Errorf("the log line does not say this one IS ours: %q", got)
	}
}

// A refusal we cannot attribute is treated as ours: backing off unnecessarily
// costs a little time, while hammering a host that is actually counting us costs
// a block.
func TestUnattributableRefusalIsTreatedAsOurs(t *testing.T) {
	if !(ProviderRefusal{Status: 503}).ourQuota() {
		t.Fatal("a refusal with no headers and no message is not treated as ours")
	}
}

// The whole point is that the operator can read the verdict. The old log line was
// "status 503" and nothing else, which is what sent this investigation looking for
// a block that did not exist.
func TestTheLogLineQuotesTheHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mbShedHeaders(w)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error": "The MusicBrainz web server is currently busy. Please try again later."}`)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0
	_, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Time", Artist: "Pink Floyd"})
	if err == nil {
		t.Fatal("no error from a 503")
	}
	msg := err.Error()
	for _, want := range []string{"currently busy", "search-shed", "676/1200", "not our rate limit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error omits %q, so the log cannot answer 'am I blocked?': %s", want, msg)
		}
	}
	if !IsTransient(err) {
		t.Error("a shed 503 is not transient, so the Track parks instead of being retried")
	}
}

// In-request retries are for waits that can plausibly work. Against a shed they
// cannot: the shed outlives the pass, so retrying three more times just adds
// requests to a struggling host and delays every track by seconds.
func TestAShedIsNotRetriedInsideOneLookup(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		mbShedHeaders(w)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error": "The MusicBrainz web server is currently busy. Please try again later."}`)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0
	p.RetryBackoff = time.Millisecond
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "T", Artist: "A"}); err == nil {
		t.Fatal("no error")
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("made %d requests into a global shed, want 1 — the extra attempts cannot "+
			"succeed and are load added to a host that is already dropping it", got)
	}
}

// Our OWN rate limit is the case where waiting works, so that one still retries.
func TestOurOwnRateLimitIsStillRetriedInPlace(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.Header().Set("X-RateLimit-Who", "203.0.113.9")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error": "Your requests are exceeding the allowable rate limit."}`)
			return
		}
		fmt.Fprint(w, `{"recordings":[{"id":"b9ad642e-b012-41c7-b72a-42cf4911a0f1","title":"T"}]}`)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0
	p.RetryBackoff = time.Millisecond
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "T", Artist: "A"})
	if err != nil {
		t.Fatalf("a self-inflicted rate limit was not ridden out: %v", err)
	}
	if !meta.Matched {
		t.Fatal("no match after the retry succeeded")
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 3 {
		t.Fatalf("made %d requests, want 3 (two throttled, one good)", got)
	}
}

// An explicit Retry-After is the host naming its own terms; honor it even when the
// refusal is not attributable to us.
func TestRetryAfterIsHonoredEvenForAShed(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("X-RateLimit-Who", "search-shed")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error": "currently busy"}`)
			return
		}
		fmt.Fprint(w, `{"recordings":[{"id":"b9ad642e-b012-41c7-b72a-42cf4911a0f1","title":"T"}]}`)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "T", Artist: "A"}); err != nil {
		t.Fatalf("a named Retry-After was not honored: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("made %d requests, want 2", got)
	}
}
