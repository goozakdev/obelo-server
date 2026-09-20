package musicbrainz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// Telling "you are rate-limited" apart from "the host is shedding load for
// everyone" (ADR-0049), and the 503 ladder built on that distinction. Ported from
// internal/enrich's refusal_test.go and musicbrainz_retry_test.go.
//
// They arrive as the same three digits and have opposite remedies — slow down,
// versus wait, because slowing down cannot help. What is NEW here is the last
// section: whichever way the ladder ends, the answer is `unavailable` and never a
// Go error, because a Go error from a guest is a strike and three disable the
// plugin (ADR-0059 decision 6).

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
	shed := refusal{
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

	ours := refusal{
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
	if !(refusal{Status: 503}).ourQuota() {
		t.Fatal("a refusal with no headers and no message is not treated as ours")
	}
}

// The whole point is that the operator can read the verdict. The old log line was
// "status 503" and nothing else, which is what sent this investigation looking for
// a block that did not exist.
//
// A GUEST CANNOT PUT THAT VERDICT IN ITS ERROR, and this is the one place the port
// changed shape. The sentence the host retries on is [pluginsdk.Unavailable]'s, so
// the diagnosis goes where an operator actually reads it — the server log, through
// the host's own log function, prefixed with this plugin's id.
func TestTheLogLineQuotesTheHost(t *testing.T) {
	p, host := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		mbShedHeaders(w)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error": "The MusicBrainz web server is currently busy. Please try again later."}`)
	}, noPacing())

	_, err := lookup(p, pluginapi.MediaRef{Kind: "track", Track: "Time", Artist: "Pink Floyd"})
	assertUnavailable(t, err, "a shed 503")

	logs := host.Logs()
	if len(logs) != 1 {
		t.Fatalf("wrote %d log lines, want the one refusal diagnosis: %+v", len(logs), logs)
	}
	if logs[0].Level != pluginsdk.LevelWarn {
		t.Errorf("logged at level %v, want warn", logs[0].Level)
	}
	for _, want := range []string{"currently busy", "search-shed", "676/1200", "not our rate limit"} {
		if !strings.Contains(logs[0].Message, want) {
			t.Errorf("the log line omits %q, so it cannot answer 'am I blocked?': %s",
				want, logs[0].Message)
		}
	}
}

// In-request retries are for waits that can plausibly work. Against a shed they
// cannot: the shed outlives the pass, so retrying three more times just adds
// requests to a struggling host and delays every track by seconds.
func TestAShedIsNotRetriedInsideOneLookup(t *testing.T) {
	fastBackoff(t)
	var calls int
	var mu sync.Mutex
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		mbShedHeaders(w)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error": "The MusicBrainz web server is currently busy. Please try again later."}`)
	}, noPacing())

	_, err := lookup(p, pluginapi.MediaRef{Kind: "track", Track: "T", Artist: "A"})
	assertUnavailable(t, err, "a shed lookup")
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
	fastBackoff(t)
	var calls int
	var mu sync.Mutex
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
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
	}, noPacing())

	meta, err := lookup(p, pluginapi.MediaRef{Kind: "track", Track: "T", Artist: "A"})
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
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
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
	}, noPacing())

	if _, err := lookup(p, pluginapi.MediaRef{Kind: "track", Track: "T", Artist: "A"}); err != nil {
		t.Fatalf("a named Retry-After was not honored: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("made %d requests, want 2", got)
	}
}

// --- the ladder itself ---------------------------------------------------------

// The base delay is never zero, however this plugin is configured. The bug: the
// delay used to be attempt x the pacing interval, and an interval of 0 is the
// DOCUMENTED setting for a mirror with no rate policy — so exactly the operator who
// disabled client-side pacing got four back-to-back requests at a host that had
// just asked for a pause. It is now an independent constant.
func TestRetryBackoffIsIndependentOfPacingAndNeverZero(t *testing.T) {
	if defaultRetryBackoff <= 0 {
		t.Fatal("a zero backoff retries instantly at a host that just said 503")
	}
	ours := refusal{Status: 503, Message: "Your requests are exceeding the allowable rate limit."}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		got, ok := inRequestRetry(ours, attempt, maxAttempts)
		if !ok {
			t.Fatalf("attempt %d: our own quota was not retried", attempt)
		}
		if want := time.Duration(attempt) * retryBackoffBase; got != want {
			t.Errorf("attempt %d waits %v, want %v — the delay must GROW", attempt, got, want)
		}
	}
	if _, ok := inRequestRetry(ours, maxAttempts, maxAttempts); ok {
		t.Errorf("attempt %d was retried; the ladder has %d rungs", maxAttempts, maxAttempts)
	}
	// A status that describes OUR REQUEST is never retried in place, whatever the
	// headers say.
	for _, code := range []int{400, 401, 403, 404, 422} {
		if _, ok := inRequestRetry(refusal{Status: code}, 1, maxAttempts); ok {
			t.Errorf("status %d was retried in place; asking again sends the same bad request", code)
		}
	}
}

// End to end with pacing OFF, the retries are actually spaced — and the delay grows
// per attempt.
func TestMusicBrainz503BacksOffWithPacingDisabled(t *testing.T) {
	const base = 40 * time.Millisecond
	prev := retryBackoffBase
	retryBackoffBase = base
	t.Cleanup(func() { retryBackoffBase = prev })

	var mu sync.Mutex
	var at []time.Time
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n <= 2 { // two throttle answers, then success
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mbid-1","title":"Doolittle"}`))
	}, noPacing())

	md, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "mbid-1"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !md.Matched {
		t.Error("the retry should have recovered the lookup")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Fatalf("made %d requests, want 3 (two 503s then a success)", len(at))
	}
	// Attempt N waits N x base, so the second gap must exceed the first.
	first, second := at[1].Sub(at[0]), at[2].Sub(at[1])
	if first < base {
		t.Errorf("first retry came after %v, want at least %v", first, base)
	}
	if second < 2*base {
		t.Errorf("second retry came after %v, want at least %v (the delay must grow)", second, 2*base)
	}
}

// A host that stays down surfaces as the UNAVAILABLE answer rather than retrying
// forever — and, critically, not as a Go error: three of those disable the plugin,
// and a source having a bad afternoon must not take MusicBrainz off the server.
func TestMusicBrainz503GivesUpAfterMaxAttemptsAndIsUnavailable(t *testing.T) {
	fastBackoff(t)
	var calls int
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}, noPacing())

	_, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "mbid-1"})
	assertUnavailable(t, err, "a persistent 503")
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("the detail %q should name the status", err)
	}
	if calls != maxAttempts {
		t.Errorf("made %d requests, want %d (maxAttempts)", calls, maxAttempts)
	}
}

// TestRetryAfterHeaderWins: the host's own Retry-After beats our computed delay;
// anything unusable falls back to it.
func TestRetryAfterHeaderWins(t *testing.T) {
	const fallback = 7 * time.Second
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"whole seconds are honoured", "2", 2 * time.Second},
		{"absent falls back", "", fallback},
		{"zero is not a licence to retry now", "0", fallback},
		{"negative falls back", "-5", fallback},
		{"an HTTP-date we cannot parse falls back", "Wed, 21 Oct 2026 07:28:00 GMT", fallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryAfter(c.header, fallback); got != c.want {
				t.Errorf("retryAfter(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// --- the budget the ladder now runs inside (ADR-0059 decision 6) ---------------

// A Retry-After the call's budget cannot pay for is NOT slept through. Sleeping
// past the host's deadline gets the guest unwound, which counts a failure against
// the plugin — so the ladder stops and answers `unavailable`, which is what an
// outage is.
func TestARetryAfterTheBudgetCannotPayForIsNotTaken(t *testing.T) {
	var calls int
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Who", "203.0.113.9")
		w.Header().Set("Retry-After", "3600") // an hour: no call budget can pay for it
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error": "Your requests are exceeding the allowable rate limit."}`)
	}, noPacing())

	start := time.Now()
	_, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "mbid-1"})
	elapsed := time.Since(start)

	assertUnavailable(t, err, "a Retry-After longer than the budget")
	if calls != 1 {
		t.Errorf("made %d requests, want 1 — the wait was refused, so there is no second attempt", calls)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the call took %v; it slept through a Retry-After the budget could not pay for", elapsed)
	}
}

// budgetAllows' own table: with no deadline every wait is allowed (a caller's plain
// context), and with one the wait plus the headroom for the request after it must
// fit.
func TestBudgetAllows(t *testing.T) {
	if !budgetAllows(context.Background(), time.Hour) {
		t.Error("a context with no deadline must not refuse a wait; there is nothing to overrun")
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryHeadroom+2*time.Second)
	defer cancel()
	if !budgetAllows(ctx, time.Second) {
		t.Error("a wait that fits was refused")
	}
	if budgetAllows(ctx, 10*time.Second) {
		t.Error("a wait that does not fit was allowed; the guest would be unwound mid-sleep")
	}
}

// Every exported call path answers `unavailable` for a retryable status, and a Go
// error for one that describes our request. This is the table the whole error
// policy rests on, asked of all six calls at once so a new one cannot quietly opt
// out (ADR-0059 decision 6).
func TestEveryCallPathTreatsARetryableStatusAsUnavailable(t *testing.T) {
	paths := map[string]func(*Provider) error{
		"lookup": func(p *Provider) error {
			_, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"})
			return err
		},
		"search": func(p *Provider) error {
			_, err := search(p, "track", "Time", pluginapi.Page{}, "", "")
			return err
		},
		"artwork-candidates": func(p *Provider) error {
			_, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"}, "cover")
			return err
		},
		"album-tracklist": func(p *Provider) error {
			_, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-1", LocalTrackCount: 3})
			return err
		},
		"release-editions": func(p *Provider) error {
			_, err := editions(p, "rg-1")
			return err
		},
	}
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		for name, call := range paths {
			t.Run(fmt.Sprintf("%s/%d", name, code), func(t *testing.T) {
				fastBackoff(t)
				p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
					// A shed, so the ladder does not spend four attempts on every cell.
					w.Header().Set("X-RateLimit-Who", "search-shed")
					w.WriteHeader(code)
					fmt.Fprint(w, `{"error": "The MusicBrainz web server is currently busy."}`)
				}, noPacing())
				assertUnavailable(t, call(p), name)
			})
		}
	}
	for _, code := range []int{400, 403, 422} {
		for name, call := range paths {
			t.Run(fmt.Sprintf("%s/%d", name, code), func(t *testing.T) {
				p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(code)
				}, noPacing())
				assertGoError(t, call(p), name)
			})
		}
	}
}

// A host REFUSAL — the allowlist, an address this server will not talk to — is the
// unavailable answer too. The plugin learned nothing about the item, and a refusal
// retried unchanged will be refused again, so it is the pass's problem and not a
// fact about the album.
func TestAHostRefusalIsUnavailable(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, URL: "http://elsewhere.test", URL2: caaHost}),
		sdktest.WithAllowedHosts("musicbrainz.org"),
		sdktest.WithHandlerFunc(jsonHandler(`{}`)),
	)
	p := New(host)
	_, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"})
	assertUnavailable(t, err, "a refused fetch")
}

// A 200 carrying something this code cannot read stays a Go error. It is not an
// outage: the source answered, and a document that will not parse is a
// disagreement an operator has to see.
func TestAnUnreadableAnswerStaysAGoError(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`<html>this is not JSON</html>`))
	}, noPacing())

	_, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-1"})
	assertGoError(t, err, "an unparseable body")
	var fe *pluginsdk.FetchError
	if !errors.As(err, &fe) || fe.Decode == nil {
		t.Errorf("err = %v, want a decode failure", err)
	}
}
