package eventsink

import (
	"log"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/events"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

	"github.com/google/uuid"
)

// The translator: the one place the Broker's private UI snapshots become the
// curated sink events, and the reason those snapshots can keep changing shape.
//
// It subscribes to the Broker exactly as an Admin's browser does — same
// Subscribe, same audience gating, same bounded channel that drops rather than
// blocks — which is what CONTEXT.md means by "a sink sees what an Admin would
// see". It is not a privileged tap: if the Broker would not send it to an Admin,
// a sink never learns it happened.
//
// A snapshot is not an event. `scanProgress` fires many times as a Library is
// walked and says "here is the state of the bar"; `scan.completed` fires once and
// says "this finished". The derivation is this file's whole job, and it is why the
// SSE payloads stay out of the contract: a progress bar that gains a field is a UI
// change, not a Plugin-contract change.

// Lookup is everything the translator may ask the database for, and the list is
// the enforcement of "ids, names and kinds only": every method but the Library one
// answers in three columns (store.EventRef), so the translator never holds a
// password hash, a Device's client id or a Title's folder to leak.
//
// *store.DB satisfies it. It is one interface rather than four because the
// translator is one consumer with one appetite, and splitting it would suggest a
// caller might supply some of these and not others.
type Lookup interface {
	LibraryByID(id string) (store.Library, error)
	// EventTitleRef names the Title a playback event is about.
	EventTitleRef(id string) (store.EventRef, error)
	// EventDeviceRef names the Device a playback session is bound to.
	EventDeviceRef(id string) (store.EventRef, error)
	// EventUserRef names the session's owner AND its role, which is what decides
	// between a User id and a Link id (ADR-0054 §3).
	EventUserRef(id string) (store.EventRef, error)
}

// Translator derives sink events from Broker events and publishes them into the
// Dispatcher. It does NO work — no lookup, no id, no timestamp — for an event type
// no live sink subscribes to.
type Translator struct {
	disp *Dispatcher
	look Lookup

	// now and newID are seams so a test can pin a timestamp and an id. Production
	// uses the wall clock and a UUID.
	now   func() time.Time
	newID func() string

	startOnce sync.Once
	stopOnce  sync.Once
	cancelSub func()
	done      chan struct{}
}

// NewTranslator wires a Translator over the Dispatcher and the name lookups.
// look may be nil, in which case every entity is named only by its id — except an
// Actor, which is then omitted entirely (see actor: naming the wrong SIDE of a
// Link is worse than naming nobody).
func NewTranslator(disp *Dispatcher, look Lookup) *Translator {
	return &Translator{
		disp:  disp,
		look:  look,
		now:   time.Now,
		newID: uuid.NewString,
		done:  make(chan struct{}),
	}
}

// Start subscribes to the Broker and begins translating on its own goroutine.
// Subscription happens BEFORE Start returns, so a caller that starts the
// translator and then triggers a scan cannot miss that scan's terminal event.
//
// Idempotent; Stop ends it.
func (t *Translator) Start(broker *events.Broker) {
	if t == nil || broker == nil {
		return
	}
	t.startOnce.Do(func() {
		// An Admin identity: the sink is the operator's automation, so it is gated
		// exactly as the operator's own browser is — admin-only events included,
		// every Library in scope, nothing more.
		ch, cancel := broker.Subscribe(events.Identity{IsAdmin: true})
		t.cancelSub = cancel
		go t.run(ch)
	})
}

// run drains the subscription until the Broker closes the channel (Stop, or the
// Broker itself shutting down).
func (t *Translator) run(ch <-chan events.Event) {
	defer close(t.done)
	for e := range ch {
		ev, ok := t.translate(e)
		if !ok {
			continue
		}
		t.disp.Publish(ev)
	}
}

// Stop ends the subscription and waits for the goroutine to unwind. Idempotent,
// and a no-op on a Translator that was never started.
func (t *Translator) Stop() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() {
		if t.cancelSub == nil {
			return
		}
		t.cancelSub()
		<-t.done
	})
}

// translate derives a sink event from one Broker event, or reports that this
// snapshot is not one.
//
// The Wants check comes FIRST in every branch. That is the point of the check:
// "the translator does no work for an event nobody subscribed to" has to mean no
// database read and no id minted, not merely no delivery.
func (t *Translator) translate(e events.Event) (pluginapi.SinkEvent, bool) {
	switch e.Type {
	case events.TypeScanProgress:
		if !t.disp.Wants(pluginapi.EventScanCompleted) {
			return pluginapi.SinkEvent{}, false
		}
		p, ok := e.Data.(events.ScanProgress)
		if !ok || !p.Complete {
			// Every snapshot but the terminal one is a progress bar advancing.
			return pluginapi.SinkEvent{}, false
		}
		return pluginapi.SinkEvent{
			ID:      t.newID(),
			Type:    pluginapi.EventScanCompleted,
			At:      t.now().UTC().Format(time.RFC3339),
			Library: t.library(p.LibraryID),
			Scan: &pluginapi.EventScan{
				TitlesFound: p.TitlesFound,
				FilesFound:  p.FilesFound,
				Added:       p.Added,
				Removed:     p.Removed,
				Scope:       p.Scope,
			},
		}, true

	case events.TypeEnrichProgress:
		if !t.disp.Wants(pluginapi.EventEnrichCompleted) {
			return pluginapi.SinkEvent{}, false
		}
		p, ok := e.Data.(events.EnrichProgress)
		if !ok || !p.Complete {
			return pluginapi.SinkEvent{}, false
		}
		return pluginapi.SinkEvent{
			ID:      t.newID(),
			Type:    pluginapi.EventEnrichCompleted,
			At:      t.now().UTC().Format(time.RFC3339),
			Library: t.library(p.LibraryID),
			Enrich: &pluginapi.EventEnrich{
				Total:     p.Total,
				Done:      p.Done,
				Matched:   p.Matched,
				Unmatched: p.Unmatched,
				Failed:    p.Failed,
				Disabled:  p.Disabled,
				Retrying:  p.Retrying,
			},
		}, true

	case events.TypeSessionStarted, events.TypeSessionEnded:
		// The two session transitions are ONE branch because they differ only in
		// which type they carry: a start and a stop name the same session, the same
		// Title, the same Actor and the same Device, and an operator's automation
		// pairs them on those. Splitting them would be two copies of the
		// attribution rule, which is the last rule in this file that should ever
		// exist twice.
		//
		// nowPlaying is deliberately NOT here. It is a position tick — a progress
		// bar by another name — and the curated set has no "still playing" event.
		sinkType := pluginapi.EventPlaybackStarted
		if e.Type == events.TypeSessionEnded {
			sinkType = pluginapi.EventPlaybackStopped
		}
		if !t.disp.Wants(sinkType) {
			return pluginapi.SinkEvent{}, false
		}
		p, ok := e.Data.(events.SessionEvent)
		if !ok {
			return pluginapi.SinkEvent{}, false
		}
		// The Actor is resolved FIRST because it decides whether there is a Device
		// to name at all. A relayed session's Device row is the linked Server
		// presenting itself as one (ADR-0055 §4), so its NAME is the other
		// household's Server name — a name from over there, which is exactly what
		// EventActor.Name promises never to carry. Naming the person's hardware
		// would be a worse version of naming the person. So a Link actor gets no
		// Device: not a masked one, not an id-only one, none.
		actor := t.actor(p.UserID)
		device := pluginapi.EventEntity{}
		if actor.LinkID == "" {
			device = t.device(p.DeviceID)
		}
		return pluginapi.SinkEvent{
			ID:     t.newID(),
			Type:   sinkType,
			At:     t.now().UTC().Format(time.RFC3339),
			Title:  t.title(p.TitleID),
			Actor:  actor,
			Device: device,
		}, true

	case events.TypeLibraryUpdated:
		if !t.disp.Wants(pluginapi.EventLibraryChanged) {
			return pluginapi.SinkEvent{}, false
		}
		p, ok := e.Data.(events.LibraryUpdated)
		if !ok {
			return pluginapi.SinkEvent{}, false
		}
		// No counts block, on purpose. The nudge is a refetch signal and carries no
		// diff (see events.LibraryUpdated), and inventing numbers for it here would
		// be a second, disagreeing copy of what the catalog already says.
		return pluginapi.SinkEvent{
			ID:      t.newID(),
			Type:    pluginapi.EventLibraryChanged,
			At:      t.now().UTC().Format(time.RFC3339),
			Library: t.library(p.LibraryID),
		}, true
	}
	// Every other Broker event is a UI snapshot with no terminal meaning — a
	// progress tick, a nowPlaying position, a Tailnet or Link nudge — and the
	// curated set says so by not having a type for it.
	return pluginapi.SinkEvent{}, false
}

// library names a Library id. A lookup that fails still yields the id, because an
// event that names the thing that happened is more useful than no event at all —
// the id is what an automation keys on, and the name is the part a human reads.
func (t *Translator) library(id string) pluginapi.EventEntity {
	entity := pluginapi.EventEntity{ID: id}
	if t.look == nil || id == "" {
		return entity
	}
	lib, err := t.look.LibraryByID(id)
	if err != nil {
		log.Printf("obelo: event sink: naming library %q: %v", id, err)
		return entity
	}
	entity.Name = lib.Name
	entity.Kind = lib.Kind
	return entity
}

// title names the Title a playback event is about, on library's terms: the id
// always, the name and kind when they can be read.
func (t *Translator) title(id string) pluginapi.EventEntity {
	return t.entity(id, "title", func(ref string) (store.EventRef, error) {
		return t.look.EventTitleRef(ref)
	})
}

// device names the Device a playback session is bound to (ADR-0015), on the same
// terms. Its "kind" is the Device's platform.
func (t *Translator) device(id string) pluginapi.EventEntity {
	return t.entity(id, "device", func(ref string) (store.EventRef, error) {
		return t.look.EventDeviceRef(ref)
	})
}

// entity is the shared id-always-name-if-possible shape behind title and device.
func (t *Translator) entity(id, what string, lookup func(string) (store.EventRef, error)) pluginapi.EventEntity {
	entity := pluginapi.EventEntity{ID: id}
	if t.look == nil || id == "" {
		return entity
	}
	ref, err := lookup(id)
	if err != nil {
		log.Printf("obelo: event sink: naming %s %q: %v", what, id, err)
		return entity
	}
	entity.Name = ref.Name
	entity.Kind = ref.Kind
	return entity
}

// actor decides WHOSE play this is, and it is the one derivation in this package
// with a rule rather than a mapping (ADR-0054 section 3, ADR-0056 section 5).
//
// A session under a `remote` User is a relayed play from another household. The
// sharing Server is entitled to know that its friend's Server is streaming — that
// is load, and content, and its own Playback ceiling being spent — and is entitled
// to nothing about the person on the far sofa, who is not its User and whose name
// it does not even hold. So such a session names the LINK: the id of this server's
// own `remote` User record, the label the sharing Admin typed for it, and NO user
// id. Every other session names the User.
//
// A lookup failure yields NO actor at all rather than a bare user id. That is the
// deliberate choice: without the role there is no way to know which side of a Link
// this session is on, and an id in the wrong field is a receiver being told that a
// linked Server is a person. Unattributed is a smaller lie than misattributed.
func (t *Translator) actor(userID string) pluginapi.EventActor {
	if t.look == nil || userID == "" {
		return pluginapi.EventActor{}
	}
	ref, err := t.look.EventUserRef(userID)
	if err != nil {
		log.Printf("obelo: event sink: naming the actor of a session: %v", err)
		return pluginapi.EventActor{}
	}
	if ref.Kind == auth.RoleRemote {
		return pluginapi.EventActor{LinkID: ref.ID, Name: ref.Name}
	}
	return pluginapi.EventActor{UserID: ref.ID, Name: ref.Name}
}
