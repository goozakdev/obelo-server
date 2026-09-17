package eventsink

import (
	"log"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/events"
	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/store"

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

// LibraryLookup is how a Library id becomes a name and a kind for the event
// payload. *store.DB satisfies it. Narrow on purpose — a sink event carries ids,
// names and kinds and never a catalog row, so this is the most the translator is
// ever allowed to ask for.
type LibraryLookup interface {
	LibraryByID(id string) (store.Library, error)
}

// Translator derives sink events from Broker events and publishes them into the
// Dispatcher. It does NO work — no lookup, no id, no timestamp — for an event type
// no live sink subscribes to.
type Translator struct {
	disp *Dispatcher
	libs LibraryLookup

	// now and newID are seams so a test can pin a timestamp and an id. Production
	// uses the wall clock and a UUID.
	now   func() time.Time
	newID func() string

	startOnce sync.Once
	stopOnce  sync.Once
	cancelSub func()
	done      chan struct{}
}

// NewTranslator wires a Translator over the Dispatcher and the Library lookup.
// libs may be nil, in which case a Library is named only by its id.
func NewTranslator(disp *Dispatcher, libs LibraryLookup) *Translator {
	return &Translator{
		disp:  disp,
		libs:  libs,
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
	}
	// Every other Broker event is either a UI snapshot with no terminal meaning or
	// one of the four types issue 06 translates.
	return pluginapi.SinkEvent{}, false
}

// library names a Library id. A lookup that fails still yields the id, because an
// event that names the thing that happened is more useful than no event at all —
// the id is what an automation keys on, and the name is the part a human reads.
func (t *Translator) library(id string) pluginapi.EventEntity {
	entity := pluginapi.EventEntity{ID: id}
	if t.libs == nil || id == "" {
		return entity
	}
	lib, err := t.libs.LibraryByID(id)
	if err != nil {
		log.Printf("obelo: event sink: naming library %q: %v", id, err)
		return entity
	}
	entity.Name = lib.Name
	entity.Kind = lib.Kind
	return entity
}
