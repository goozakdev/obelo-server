package eventsink

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/events"
	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The host side of the Event sink Extension point has three promises a black-box
// API test cannot observe, because the counters they move are not on the settings
// response until issue 06: the queue drops its OLDEST event and counts it, a
// failing delivery is counted rather than retried forever, and a stop discards
// whatever was queued. Those are tested here, against the mechanism itself. What
// an Admin CAN see — a signed document arriving, a filtered subscription, a scan
// that is no slower for having a dead webhook attached — is tested through the API
// in internal/api/event_sinks_test.go.

// recordingSink is a sink Plugin that records what it was handed and can be made
// to block or fail.
type recordingSink struct {
	mu       sync.Mutex
	received []pluginapi.SinkEvent

	// block, when non-nil, holds every Deliver until it is closed.
	block chan struct{}
	// err, when non-nil, is what every Deliver returns.
	err error
	// entered is signalled once per Deliver call that reaches the sink.
	entered chan struct{}
}

func (s *recordingSink) Deliver(ctx context.Context, ev pluginapi.SinkEvent) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.received = append(s.received, ev)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) events() []pluginapi.SinkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pluginapi.SinkEvent, len(s.received))
	copy(out, s.received)
	return out
}

// fakeStore serves one set of sink rows.
type fakeStore struct{ rows []store.EventSinkRow }

func (f fakeStore) EventSinks() ([]store.EventSinkRow, error) { return f.rows, nil }

// registryWith registers one sink Plugin under "test" that always returns p.
func registryWith(p pluginapi.EventSink) *pluginapi.Registry {
	reg := pluginapi.NewRegistry()
	reg.RegisterEventSink(pluginapi.EventSinkRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "test", Name: "Test sink"},
		New:        func(pluginapi.Settings) (pluginapi.EventSink, error) { return p, nil },
	})
	return reg
}

// liveManager wires a Manager over one enabled sink subscribed to scan.completed.
func liveManager(t *testing.T, p pluginapi.EventSink) *Manager {
	t.Helper()
	m := NewManager(fakeStore{rows: []store.EventSinkRow{{
		Slug: "test", Enabled: true, Secret: "s", URL: "https://sink.test/hook",
		Events: []string{pluginapi.EventScanCompleted},
	}}}, registryWith(p), NewDispatcher())
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(m.Dispatcher().Close)
	return m
}

func scanEvent(id string) pluginapi.SinkEvent {
	return pluginapi.SinkEvent{
		ID: id, Type: pluginapi.EventScanCompleted,
		At: "2026-09-16T12:00:00Z", Scan: &pluginapi.EventScan{},
	}
}

// TestFullQueueDropsOldestAndCounts: a sink that cannot keep up loses its OLDEST
// events and the dropped counter climbs. The newest state of the world is the
// useful one, and an operator whose target fell behind needs a number, not silence
// (PRD story 45).
func TestFullQueueDropsOldestAndCounts(t *testing.T) {
	sink := &recordingSink{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	m := liveManager(t, sink)

	// The first event is taken off the queue immediately and blocks in the sink,
	// so it is NOT one of the queued ones. Wait for it to be in flight before
	// filling the queue, so the arithmetic below is deterministic.
	m.Dispatcher().Publish(scanEvent("in-flight"))
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was never called")
	}

	// Now overfill: QueueCapacity fit, and everything past that displaces one.
	const overflow = 10
	for i := 0; i < QueueCapacity+overflow; i++ {
		m.Dispatcher().Publish(scanEvent("queued"))
	}

	got := m.Counters()["test"]
	if got.Dropped != overflow {
		t.Fatalf("dropped = %d, want %d (a full queue drops its oldest)", got.Dropped, overflow)
	}
	if got.Delivered != 0 {
		t.Fatalf("delivered = %d, want 0 — the sink is still blocked", got.Delivered)
	}
	close(sink.block)
}

// TestFailedDeliveryIsCountedNotRetriedForever: a sink that returns an error has
// its failure counted once and the event is dropped on the floor. Delivery is
// best-effort; whatever retrying is worth doing happens inside the Plugin's own
// deadline, not in an unbounded host loop.
func TestFailedDeliveryIsCountedNotRetriedForever(t *testing.T) {
	sink := &recordingSink{err: errors.New("target refused"), entered: make(chan struct{}, 1)}
	m := liveManager(t, sink)

	m.Dispatcher().Publish(scanEvent("doomed"))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := m.Counters()["test"]; got.Failed == 1 {
			if got.Delivered != 0 {
				t.Fatalf("delivered = %d, want 0", got.Delivered)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure never counted; counters = %+v", m.Counters()["test"])
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give a retry loop, if one existed, time to spin.
	time.Sleep(100 * time.Millisecond)
	if got := m.Counters()["test"].Failed; got != 1 {
		t.Fatalf("failed = %d, want 1 — a failed delivery must not be retried by the host", got)
	}
}

// TestStopDiscardsQueuedEvents documents the intended best-effort behavior: sink
// delivery is IN MEMORY, so shutting the server down (or swapping a sink out on a
// settings save) discards whatever was queued. A sink author who knows this builds
// something that tolerates a gap; one who assumed durability would build a
// scrobbler that silently loses plays.
func TestStopDiscardsQueuedEvents(t *testing.T) {
	sink := &recordingSink{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	m := liveManager(t, sink)

	m.Dispatcher().Publish(scanEvent("in-flight"))
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was never called")
	}
	for i := 0; i < 5; i++ {
		m.Dispatcher().Publish(scanEvent("queued"))
	}

	// Close cancels the in-flight call rather than waiting it out, and the five
	// queued events are simply gone.
	done := make(chan struct{})
	go func() { m.Dispatcher().Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — a hanging sink must not hold shutdown open")
	}
	close(sink.block)

	// Nothing was delivered, and nothing survives the stop to be delivered later.
	time.Sleep(50 * time.Millisecond)
	if got := sink.events(); len(got) != 0 {
		t.Fatalf("delivered %d events after stop, want 0", len(got))
	}
	if got := m.Counters()["test"].Delivered; got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}

// TestCountersSurviveASettingsSave: a rebuild swaps the workers, but the tally is
// the operator's evidence and must not be wiped by the save they made in response
// to it.
func TestCountersSurviveASettingsSave(t *testing.T) {
	sink := &recordingSink{err: errors.New("target refused"), entered: make(chan struct{}, 1)}
	m := liveManager(t, sink)

	m.Dispatcher().Publish(scanEvent("doomed"))
	deadline := time.Now().Add(5 * time.Second)
	for m.Counters()["test"].Failed == 0 {
		if time.Now().After(deadline) {
			t.Fatal("failure never counted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Counters()["test"].Failed; got != 1 {
		t.Fatalf("failed = %d after a settings save, want 1", got)
	}
}

// countingLookup records how often a Library was named.
type countingLookup struct{ calls int }

func (c *countingLookup) LibraryByID(id string) (store.Library, error) {
	c.calls++
	return store.Library{ID: id, Name: "Movies", Kind: "movie"}, nil
}

// TestTranslatorDoesNoWorkForAnUnsubscribedEvent: with no sink subscribed to
// scan.completed, a terminal scan snapshot costs NOTHING — no Library lookup, no
// event minted. "The translator does no work for it" has to mean no work, not
// merely no delivery (PRD story 42).
func TestTranslatorDoesNoWorkForAnUnsubscribedEvent(t *testing.T) {
	// A Dispatcher with no sinks at all: nobody wants anything.
	disp := NewDispatcher()
	defer disp.Close()
	lookup := &countingLookup{}
	tr := NewTranslator(disp, lookup)

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishScanProgress(events.ScanProgress{LibraryID: "lib-1", Complete: true})

	// Let the translator goroutine drain.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lookup.calls != 0 {
		t.Fatalf("library lookups = %d, want 0 — nothing subscribed to scan.completed", lookup.calls)
	}
}

// TestTranslatorDerivesOnlyTheTerminalSnapshot: scanProgress fires repeatedly as a
// Library is walked; exactly one of those snapshots is an event. This is why the
// Broker's payloads stay private — a progress bar is not a contract.
func TestTranslatorDerivesOnlyTheTerminalSnapshot(t *testing.T) {
	sink := &recordingSink{entered: make(chan struct{}, 1)}
	m := liveManager(t, sink)
	tr := NewTranslator(m.Dispatcher(), &countingLookup{})

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishScanProgress(events.ScanProgress{LibraryID: "lib-1", TitlesFound: 1})
	broker.PublishScanProgress(events.ScanProgress{LibraryID: "lib-1", TitlesFound: 2})
	broker.PublishScanProgress(events.ScanProgress{LibraryID: "lib-1", TitlesFound: 3, FilesFound: 4, Complete: true})
	// A library-updated nudge is not one of this slice's events either.
	broker.PublishLibraryUpdated("lib-1")

	deadline := time.Now().Add(5 * time.Second)
	var got []pluginapi.SinkEvent
	for {
		got = sink.events()
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d events, want exactly 1 (the terminal snapshot)", len(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := got[0]
	if ev.Type != pluginapi.EventScanCompleted {
		t.Fatalf("type = %q, want %q", ev.Type, pluginapi.EventScanCompleted)
	}
	if ev.ID == "" {
		t.Fatal("event carries no id — a sink cannot be idempotent without one")
	}
	if _, err := time.Parse(time.RFC3339, ev.At); err != nil {
		t.Fatalf("at = %q, not RFC 3339: %v", ev.At, err)
	}
	if ev.Library.ID != "lib-1" || ev.Library.Name != "Movies" || ev.Library.Kind != "movie" {
		t.Fatalf("library = %+v, want the named lib-1", ev.Library)
	}
	if ev.Scan == nil || ev.Scan.TitlesFound != 3 || ev.Scan.FilesFound != 4 {
		t.Fatalf("scan = %+v, want the terminal counts 3/4", ev.Scan)
	}
	// Give the libraryUpdated nudge time to be (not) translated.
	time.Sleep(100 * time.Millisecond)
	if len(sink.events()) != 1 {
		t.Fatalf("delivered %d events, want 1 — libraryUpdated is issue 06's", len(sink.events()))
	}
}

// TestManagerSkipsSinksThatCannotRun: the four ordinary ways a settings row is not
// a live sink. None of them is an error — a misconfigured outbound integration
// must never stop a boot (ADR-0001).
func TestManagerSkipsSinksThatCannotRun(t *testing.T) {
	cases := []struct {
		name string
		row  store.EventSinkRow
	}{
		{"disabled", store.EventSinkRow{Slug: "test", Secret: "s", URL: "https://x.test", Events: []string{pluginapi.EventScanCompleted}}},
		{"no url", store.EventSinkRow{Slug: "test", Enabled: true, Secret: "s", Events: []string{pluginapi.EventScanCompleted}}},
		{"subscribed to nothing", store.EventSinkRow{Slug: "test", Enabled: true, Secret: "s", URL: "https://x.test"}},
		{"unknown slug", store.EventSinkRow{Slug: "gone", Enabled: true, Secret: "s", URL: "https://x.test", Events: []string{pluginapi.EventScanCompleted}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{entered: make(chan struct{}, 1)}
			m := NewManager(fakeStore{rows: []store.EventSinkRow{tc.row}}, registryWith(sink), NewDispatcher())
			if err := m.Reload(context.Background()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			defer m.Dispatcher().Close()
			if m.Dispatcher().Wants(pluginapi.EventScanCompleted) {
				t.Fatal("a sink that cannot run is still subscribed")
			}
			m.Dispatcher().Publish(scanEvent("x"))
			time.Sleep(50 * time.Millisecond)
			if got := sink.events(); len(got) != 0 {
				t.Fatalf("delivered %d events to a sink that should not be live", len(got))
			}
		})
	}
}
