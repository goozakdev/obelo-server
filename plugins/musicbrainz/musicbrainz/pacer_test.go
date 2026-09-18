package musicbrainz

import (
	"net/http"
	"sync"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// ADR-0049's pacing property, now the plugin's own (ADR-0059 decision 5).
//
// These replace internal/enrich/hostthrottle_test.go. The Go provider needed a
// PROCESS-WIDE limiter keyed by host, because Manager.resolveLibrary builds a
// provider per Library and three Libraries kept three independent 1-req/sec
// throttles pointed at one host, each correctly believing it was well behaved.
// A guest cannot reproduce that shape: one module, one linear memory, every call
// serialized into it by the host, and one [pluginsdk.PacedHost] around its fetch.
//
// DURATIONS ARE SCALED. The shipped interval is one second; these tests use tens
// of milliseconds, because what is being asserted is the RELATIONSHIP between the
// configured interval and the observed gaps, and a test that spent real seconds
// proving it would be the same assertion at a hundred times the price. The shipped
// value itself is asserted literally in TestTheShippedIntervalIsOneRequestASecond.

// pacedProvider builds the provider the way main.go does — a PacedHost wrapped
// around the host — with the operator's interval fixed at ms milliseconds, and a
// recorder of when each request arrived.
func pacedProvider(t *testing.T, ms int) (*Provider, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var at []time.Time
	inner := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{
			Enabled: true, URL: mbHost, URL2: caaHost, RateLimitMillis: &ms,
		}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"rg-1","title":"An Album"}`))
		}),
	)
	p := New(pluginsdk.PacedHost(inner, DefaultInterval))
	return p, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), at...)
	}
}

// gaps returns the intervals between consecutive requests.
func gaps(at []time.Time) []time.Duration {
	out := make([]time.Duration, 0, len(at))
	for i := 1; i < len(at); i++ {
		out = append(out, at[i].Sub(at[i-1]))
	}
	return out
}

// THE ADR-0049 PROPERTY: two Libraries enriching CONCURRENTLY never produce two
// requests closer together than the configured interval.
//
// The goroutines are the two Libraries. On a real server they are two passes
// holding the same Plugin, and the host serializes their calls into the one guest
// instance; here they are two callers of one Provider over one PacedHost, which is
// the same single point of serialization with the sandbox taken away.
//
// This is the test the Go provider could only pass with a process-wide limiter,
// and it is why that limiter no longer has to exist.
func TestTwoLibrariesEnrichingConcurrentlyKeepTheInterval(t *testing.T) {
	const interval = 40 * time.Millisecond
	p, requests := pacedProvider(t, int(interval/time.Millisecond))

	var wg sync.WaitGroup
	for lib := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if _, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"}); err != nil {
					t.Errorf("library %d lookup: %v", lib, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	at := requests()
	if len(at) != 6 {
		t.Fatalf("the stand-in saw %d requests, want 6", len(at))
	}
	// A little slack for scheduling: the pacer reserves the slot before sleeping, so
	// a gap can only be SHORT if two requests genuinely raced past it.
	const slack = 5 * time.Millisecond
	for i, g := range gaps(at) {
		if g < interval-slack {
			t.Errorf("requests %d and %d were %v apart, want at least %v — two Libraries are "+
				"pacing themselves separately, which is exactly the shape ADR-0049 was "+
				"written about (all gaps: %v)", i, i+1, g, interval, gaps(at))
		}
	}
}

// The operator's RateLimitMillis is what decides the interval, and a DIFFERENT
// number produces a different observed spacing — the acceptance criterion in its
// own words. Both runs are the same code; only the row changed.
func TestRateLimitMillisChangesTheObservedInterval(t *testing.T) {
	for _, ms := range []int{20, 60} {
		interval := time.Duration(ms) * time.Millisecond
		p, requests := pacedProvider(t, ms)
		for range 3 {
			if _, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"}); err != nil {
				t.Fatalf("lookup: %v", err)
			}
		}
		at := requests()
		if len(at) != 3 {
			t.Fatalf("%dms: saw %d requests, want 3", ms, len(at))
		}
		const slack = 5 * time.Millisecond
		for i, g := range gaps(at) {
			if g < interval-slack {
				t.Errorf("%dms: gap %d = %v, want at least the configured %v", ms, i, g, interval)
			}
		}
		// And the pacer is actually holding the operator's number, not a default it
		// happened to agree with.
		if got := pluginsdk.PacerOf(p.Host()).Interval(); got != interval {
			t.Errorf("%dms: the pacer holds %v, want %v", ms, got, interval)
		}
	}
}

// An explicit ZERO is the operator saying their mirror has no rate policy, and it
// must not be read as "absent, use your default" (ADR-0049). The pointer is what
// keeps the two apart, and [pluginsdk.IntervalFrom] is where it is read.
func TestTheOperatorsZeroTurnsPacingOff(t *testing.T) {
	zero := 0
	if got := pluginsdk.IntervalFrom(pluginapi.Settings{RateLimitMillis: &zero}, DefaultInterval); got != 0 {
		t.Errorf("an explicit 0 gave interval %v, want none — a mirror with no policy is "+
			"throttled at one request a second", got)
	}
	if got := pluginsdk.IntervalFrom(pluginapi.Settings{}, DefaultInterval); got != DefaultInterval {
		t.Errorf("an absent rate limit gave %v, want this plugin's own default %v", got, DefaultInterval)
	}
	n := 250
	if got := pluginsdk.IntervalFrom(pluginapi.Settings{RateLimitMillis: &n}, DefaultInterval); got != 250*time.Millisecond {
		t.Errorf("250 gave %v", got)
	}
}

// And with pacing off, six requests cost no time at all — the mirror setting doing
// what it says.
func TestPacingOffCostsNothing(t *testing.T) {
	p, requests := pacedProvider(t, 0)
	start := time.Now()
	for range 6 {
		if _, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"}); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("six unpaced requests took %v", elapsed)
	}
	if n := len(requests()); n != 6 {
		t.Errorf("saw %d requests, want 6", n)
	}
}

// The SHIPPED interval, stated literally, because everything above is scaled: this
// plugin promises MusicBrainz one request a second when the operator has no
// opinion, which is the policy ADR-0049 is about.
func TestTheShippedIntervalIsOneRequestASecond(t *testing.T) {
	if DefaultInterval != time.Second {
		t.Errorf("DefaultInterval = %v, want one second — MusicBrainz answers 503 past it", DefaultInterval)
	}
}

// The pacer covers the COVER ART ARCHIVE host too: it wraps Fetch, and every fetch
// this plugin makes goes through it. One interval for everything this plugin
// touches is what the process-wide limiter effectively gave the Go provider, and
// splitting it per host would be a change rather than a port.
func TestTheCoverArtHostIsPacedToo(t *testing.T) {
	const interval = 40 * time.Millisecond
	var mu sync.Mutex
	var at []time.Time
	record := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"image":"http://img/1.jpg","front":true}]}`))
	}
	ms := int(interval / time.Millisecond)
	inner := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{
			Enabled: true, URL: mbHost, URL2: caaHost, RateLimitMillis: &ms,
		}),
		sdktest.WithHandlerFunc(record),
	)
	p := New(pluginsdk.PacedHost(inner, DefaultInterval))

	for range 3 {
		if _, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"}, "cover"); err != nil {
			t.Fatalf("artwork candidates: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Fatalf("saw %d cover-art requests, want 3", len(at))
	}
	const slack = 5 * time.Millisecond
	for i, g := range gaps(at) {
		if g < interval-slack {
			t.Errorf("cover-art requests %d and %d were %v apart, want at least %v", i, i+1, g, interval)
		}
	}
}
