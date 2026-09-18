package eventsink

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/events"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The three things about the four events issue 06 adds that a black-box API test
// cannot reach, because each is about the translator declining to do something:
// a nowPlaying tick is not an event, an unsubscribed type costs nothing, and a
// session whose owner cannot be identified is reported with NO actor rather than a
// guessed one. Everything an Admin can observe — the documents themselves, the
// attribution over a real Link, the counters — is asserted through HTTP in
// internal/api/event_sink_events_test.go.

// stubLookup names entities from fixed maps and counts every call, so a test can
// assert that no work happened at all.
type stubLookup struct {
	calls int
	// userErr, when non-nil, is what EventUserRef returns.
	userErr error
	// userRole is the role EventUserRef reports.
	userRole string
}

func (s *stubLookup) LibraryByID(id string) (store.Library, error) {
	s.calls++
	return store.Library{ID: id, Name: "Movies", Kind: "movie"}, nil
}

func (s *stubLookup) EventTitleRef(id string) (store.EventRef, error) {
	s.calls++
	return store.EventRef{ID: id, Name: "Dune", Kind: "movie"}, nil
}

func (s *stubLookup) EventDeviceRef(id string) (store.EventRef, error) {
	s.calls++
	return store.EventRef{ID: id, Name: "Laptop", Kind: "macos"}, nil
}

func (s *stubLookup) EventUserRef(id string) (store.EventRef, error) {
	s.calls++
	if s.userErr != nil {
		return store.EventRef{}, s.userErr
	}
	role := s.userRole
	if role == "" {
		role = "admin"
	}
	return store.EventRef{ID: id, Name: "brandon", Kind: role}, nil
}

// managerSubscribedTo wires a Manager over one enabled sink subscribed to exactly
// the given event types.
func managerSubscribedTo(t *testing.T, p pluginapi.EventSink, eventTypes ...string) *Manager {
	t.Helper()
	m := NewManager(fakeStore{rows: []store.EventSinkRow{{
		Slug: "test", Enabled: true, Secret: "s", URL: "https://sink.test/hook",
		Events: eventTypes,
	}}}, registryWith(p), NewDispatcher())
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(m.Dispatcher().Close)
	return m
}

// awaitEvents waits for the sink to have received n events, then returns them.
func awaitEvents(t *testing.T, sink *recordingSink, n int) []pluginapi.SinkEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := sink.events()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d events, want %d", len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNowPlayingIsNotAnEvent: the curated set has playback.started and
// playback.stopped and deliberately nothing in between. A nowPlaying tick is a
// position report — a progress bar by another name — and a sink subscribed to both
// playback events must not be woken by one.
func TestNowPlayingIsNotAnEvent(t *testing.T) {
	sink := &recordingSink{entered: make(chan struct{}, 1)}
	m := managerSubscribedTo(t, sink, pluginapi.EventPlaybackStarted, pluginapi.EventPlaybackStopped)
	tr := NewTranslator(m.Dispatcher(), &stubLookup{})

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishSessionEvent(events.TypeSessionStarted, events.SessionEvent{
		SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
	})
	// Three position reports, which is the cheap end of what a real play produces.
	for _, pos := range []int64{1000, 2000, 3000} {
		broker.PublishSessionEvent(events.TypeNowPlaying, events.SessionEvent{
			SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
			PositionMs: pos,
		})
	}
	broker.PublishSessionEvent(events.TypeSessionEnded, events.SessionEvent{
		SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
	})

	got := awaitEvents(t, sink, 2)
	time.Sleep(100 * time.Millisecond)
	got = sink.events()
	if len(got) != 2 {
		t.Fatalf("delivered %d events for a play with three position reports, want 2: %+v", len(got), got)
	}
	if got[0].Type != pluginapi.EventPlaybackStarted || got[1].Type != pluginapi.EventPlaybackStopped {
		t.Fatalf("types = %q, %q; want started then stopped", got[0].Type, got[1].Type)
	}
	if got[0].ID == got[1].ID {
		t.Fatal("the start and the stop share an event id")
	}
}

// TestUnidentifiableSessionCarriesNoActor: if the owner of a session cannot be
// read, the event goes out with NO actor rather than a bare user id.
//
// This is the attribution rule being honoured in the one case where it would be
// easy not to. Without the role there is no way to know which SIDE of a Link the
// session is on, and putting the id in `userId` would tell a receiver that a linked
// Server is a person — the exact claim ADR-0054 §3 exists to prevent. Unattributed
// is a smaller lie than misattributed.
func TestUnidentifiableSessionCarriesNoActor(t *testing.T) {
	sink := &recordingSink{entered: make(chan struct{}, 1)}
	m := managerSubscribedTo(t, sink, pluginapi.EventPlaybackStarted)
	tr := NewTranslator(m.Dispatcher(), &stubLookup{userErr: errors.New("database is locked")})

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishSessionEvent(events.TypeSessionStarted, events.SessionEvent{
		SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
	})

	ev := awaitEvents(t, sink, 1)[0]
	if ev.Actor != (pluginapi.EventActor{}) {
		t.Fatalf("actor = %+v, want no actor at all", ev.Actor)
	}
	// The event still happens, and still names the Title: an automation that keys
	// on "something is playing" is better served by that than by silence.
	if ev.Title.ID != "title-1" {
		t.Fatalf("title = %+v, want the Title named anyway", ev.Title)
	}
}

// TestRemoteSessionNamesTheLinkAndOmitsTheDevice: the rule at the level it is
// written, so it is pinned even for a `remote` User whose session never went
// through a real Link (a re-keyed Link, a revoked one mid-session).
//
// The Device is omitted because a relayed session's Device row IS the other
// household's Server presenting itself as one (ADR-0055 §4) — its name is a name
// from over there, and naming the far household's hardware would be a worse
// version of naming the person.
func TestRemoteSessionNamesTheLinkAndOmitsTheDevice(t *testing.T) {
	sink := &recordingSink{entered: make(chan struct{}, 1)}
	m := managerSubscribedTo(t, sink, pluginapi.EventPlaybackStarted)
	tr := NewTranslator(m.Dispatcher(), &stubLookup{userRole: "remote"})

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishSessionEvent(events.TypeSessionStarted, events.SessionEvent{
		SessionID: "sess-1", UserID: "peer-9", DeviceID: "dev-9", TitleID: "title-1",
	})

	ev := awaitEvents(t, sink, 1)[0]
	if ev.Actor.LinkID != "peer-9" || ev.Actor.UserID != "" {
		t.Fatalf("actor = %+v, want the Link named and no User", ev.Actor)
	}
	if ev.Device != (pluginapi.EventEntity{}) {
		t.Fatalf("device = %+v, want none — it would name the other household's Server", ev.Device)
	}
}

// TestEachEventTypeCostsNothingWhenUnsubscribed: "the translator does no work for
// an event nobody subscribed to" has to hold for every type, not just the first
// one that was implemented. A sink subscribed to scan.completed alone must cost
// nothing at all for a finished pass, a play or a Library change.
func TestEachEventTypeCostsNothingWhenUnsubscribed(t *testing.T) {
	sink := &recordingSink{entered: make(chan struct{}, 1)}
	m := managerSubscribedTo(t, sink, pluginapi.EventScanCompleted)
	look := &stubLookup{}
	tr := NewTranslator(m.Dispatcher(), look)

	broker := events.NewBroker()
	defer broker.Close()
	tr.Start(broker)
	defer tr.Stop()

	broker.PublishEnrichProgress(events.EnrichProgress{LibraryID: "lib-1", Complete: true})
	broker.PublishSessionEvent(events.TypeSessionStarted, events.SessionEvent{
		SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
	})
	broker.PublishSessionEvent(events.TypeSessionEnded, events.SessionEvent{
		SessionID: "sess-1", UserID: "user-1", DeviceID: "dev-1", TitleID: "title-1",
	})
	broker.PublishLibraryUpdated("lib-1")

	// Let the translator goroutine drain everything above.
	time.Sleep(300 * time.Millisecond)
	if look.calls != 0 {
		t.Fatalf("lookups = %d, want 0 — none of those four types is subscribed", look.calls)
	}
	if got := sink.events(); len(got) != 0 {
		t.Fatalf("delivered %d events, want 0: %+v", len(got), got)
	}
}

// TestSupportedEventTypesIsTheWholeCuratedSet: the settings screen offers
// SupportedEventTypes and the settings endpoint validates against it, so a type
// the translator derives but this list omits is one an Admin cannot subscribe to.
// As of issue 06 every curated type has a translation, so the two lists agree —
// and the day they stop agreeing, that is the bug this pins.
func TestSupportedEventTypesIsTheWholeCuratedSet(t *testing.T) {
	for _, ev := range pluginapi.AllEventTypes() {
		if !Supported(ev) {
			t.Fatalf("the contract defines %q but this server does not offer it", ev)
		}
	}
	if len(SupportedEventTypes()) != len(pluginapi.AllEventTypes()) {
		t.Fatalf("SupportedEventTypes() = %v, want the curated set %v",
			SupportedEventTypes(), pluginapi.AllEventTypes())
	}
}
