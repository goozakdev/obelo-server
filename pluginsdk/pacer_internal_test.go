package pluginsdk

import (
	"context"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The pacer, tested WITHOUT sleeping (.scratch/bundled-plugins issue 03).
//
// It is an internal test so it can replace the sleep with a recorder. A pacer
// test that actually waited would be a test that is slow when it passes and flaky
// when the machine is busy, and what is worth asserting is the DECISION — how
// long it decided to wait, and whose number it used — not the wall clock.

// sdkStubHost is a Host that records what it was asked and answers fixed
// settings. The `sdk` prefix keeps it from colliding with anything another slice
// adds to this module.
type sdkStubHost struct {
	settings pluginapi.Settings
	fetches  int
}

func (h *sdkStubHost) Fetch(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	h.fetches++
	return pluginapi.FetchResponse{Status: 200}, nil
}
func (h *sdkStubHost) Log(Level, string)                  {}
func (h *sdkStubHost) KVGet(string) ([]byte, bool, error) { return nil, false, nil }
func (h *sdkStubHost) KVSet(string, []byte) error         { return nil }
func (h *sdkStubHost) KVDelete(string) error              { return nil }
func (h *sdkStubHost) Settings() pluginapi.Settings       { return h.settings }

// TestIntervalFromTellsAbsentApartFromZero is the whole reason
// Settings.RateLimitMillis is a pointer, asserted where a plugin reads it.
//
// Absent means "use your own default pacing", which is what a source the operator
// has no opinion about must get. Zero means the explicit "do not throttle at all"
// an operator sets for a self-hosted mirror. An int with 0 meaning "off" would
// make a host that simply forgot the field hammer MusicBrainz — which is the
// failure ADR-0049 records.
func TestIntervalFromTellsAbsentApartFromZero(t *testing.T) {
	zero, five := 0, 5000
	for _, tc := range []struct {
		name string
		set  *int
		want time.Duration
	}{
		{"absent falls back to the plugin's own default", nil, time.Second},
		{"zero is the operator switching pacing off", &zero, 0},
		{"a number is the operator's own interval", &five, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IntervalFrom(pluginapi.Settings{RateLimitMillis: tc.set}, time.Second)
			if got != tc.want {
				t.Errorf("IntervalFrom = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAPacerSpacesCallsByTheInterval: the first call never waits, the second
// waits out the remainder, and a pacer with no interval never waits at all.
func TestAPacerSpacesCallsByTheInterval(t *testing.T) {
	var slept []time.Duration
	p := NewPacer(time.Second)
	p.sleep = func(d time.Duration) { slept = append(slept, d) }

	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if len(slept) != 0 {
		t.Fatalf("the first call waited %v; nothing has been sent yet", slept)
	}
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	if len(slept) != 1 || slept[0] <= 0 || slept[0] > time.Second {
		t.Fatalf("the second call waited %v, want something up to the one-second interval", slept)
	}

	p.SetInterval(0)
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("third Wait: %v", err)
	}
	if len(slept) != 1 {
		t.Fatalf("an unpaced pacer waited: %v", slept)
	}
}

// TestAPacerGivesUpWhenTheCallsDeadlineHasPassed: the host's deadline is real, and
// a plugin asked to wait longer than it has left should answer rather than spin
// until the runtime unwinds it.
func TestAPacerGivesUpWhenTheCallsDeadlineHasPassed(t *testing.T) {
	p := NewPacer(time.Hour)
	p.sleep = func(time.Duration) {}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); err == nil {
		t.Fatal("Wait on a cancelled context returned nil")
	}
	// And it did not mark a call that never happened: the next Wait on a live
	// context is the FIRST one and waits for nothing.
	var slept []time.Duration
	p.sleep = func(d time.Duration) { slept = append(slept, d) }
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(slept) != 0 {
		t.Fatalf("a cancelled call left the pacer thinking it had sent something: %v", slept)
	}
}

// TestAPacedHostReadsTheOperatorsIntervalOnEveryCall: a settings save is not a
// plugin rebuild from inside the sandbox, so a host that read RateLimitMillis once
// at construction would honour a number the Admin has since changed.
func TestAPacedHostReadsTheOperatorsIntervalOnEveryCall(t *testing.T) {
	stub := &sdkStubHost{}
	h := PacedHost(stub, 250*time.Millisecond)
	pacer := PacerOf(h)
	if pacer == nil {
		t.Fatal("PacedHost produced a Host with no pacer in it")
	}
	pacer.sleep = func(time.Duration) {}

	if _, err := h.Fetch(context.Background(), pluginapi.FetchRequest{URL: "https://example.test/a"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := pacer.Interval(); got != 250*time.Millisecond {
		t.Errorf("interval = %v, want the plugin's own default while the operator has set none", got)
	}

	// The Admin saves a slower policy between two calls.
	ms := 2000
	stub.settings.RateLimitMillis = &ms
	if _, err := h.Fetch(context.Background(), pluginapi.FetchRequest{URL: "https://example.test/b"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := pacer.Interval(); got != 2*time.Second {
		t.Errorf("interval = %v, want the operator's 2s", got)
	}
	if stub.fetches != 2 {
		t.Errorf("the wrapped host saw %d fetches, want 2 — pacing must not swallow a request", stub.fetches)
	}
}
