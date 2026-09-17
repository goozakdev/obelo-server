// Package eventsink is the HOST side of the Event sink Extension point
// (ADR-0057 decision 6): the thing that decides which curated events exist, turns
// the Broker's private UI snapshots into them, and gets them to the configured
// sinks without ever making a viewer wait.
//
// # The shape of the promise
//
// Delivery is best-effort and in memory. Each enabled sink owns a bounded queue
// and a worker goroutine; the translator enqueues and returns immediately, and a
// full queue drops its OLDEST event and counts it. The worker calls the Plugin
// under a deadline. Nothing is persisted, so a restart discards whatever was
// queued — which is a promise, not an oversight: a sink author who knows delivery
// is best-effort builds something that tolerates a gap, and an at-least-once
// outbox can later sit between the translator and the worker without a sink
// changing, because every event already carries a stable id the call is idempotent
// on.
//
// # Why it cannot slow anything down
//
// The only thing on the publish path is a non-blocking send into a buffered
// channel — the Broker's, which already drops rather than blocks (ADR-0016) — and
// then a non-blocking enqueue per sink. A target that never answers holds up its
// own worker, its own queue fills, its own dropped counter climbs, and the scan
// that produced the event finished long before any of that. That is the whole
// design: the operator's integration is never the viewer's problem.
//
// # What a sink may see
//
// What an ADMIN would see, and only through the curated types in pluginapi. The
// Broker's snapshot payloads stay private, so the five event types are the only
// thing that is ever a contract.
package eventsink

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
)

// QueueCapacity is how many undelivered events one sink may hold. Small on
// purpose: a sink that is this far behind is not going to catch up, and the
// operator is better served by a climbing dropped counter than by a queue that
// quietly grows into a memory leak.
const QueueCapacity = 64

// DeliveryTimeout bounds ONE call into a sink Plugin, including whatever retrying
// the Plugin does inside it. It is the host's deadline (ADR-0057 decision 2: the
// caller sets it), and it is what guarantees a hanging target costs one worker and
// a queue rather than a goroutine per event.
const DeliveryTimeout = 15 * time.Second

// SupportedEventTypes is what THIS server can actually derive today: the settings
// screen offers exactly this list, and the settings endpoint refuses a
// subscription to anything else.
//
// Promising an Admin an event nothing produces is worse than not offering it —
// they would configure it, see nothing, and have no way to tell a broken webhook
// from an unimplemented one. As of issue 06 the translator derives all five, so
// this answers with the whole curated set.
//
// It remains a separate function from pluginapi.AllEventTypes, and the two being
// equal today is not a reason to collapse them. They answer different questions —
// "what does the contract define" versus "what can this build produce" — and the
// next event type will be defined in the contract one commit before its
// translation exists, at which point a settings screen reading the contract's list
// would offer an Admin something nothing emits.
func SupportedEventTypes() []string {
	return pluginapi.AllEventTypes()
}

// Supported reports whether this server can derive eventType.
func Supported(eventType string) bool {
	for _, t := range SupportedEventTypes() {
		if t == eventType {
			return true
		}
	}
	return false
}

// Counters is one sink's delivery tally since this server started. It is host
// state, deliberately NOT part of the contract: a Plugin has no domain judgment to
// report about delivery, so counting is the host's job (and a Plugin that counted
// its own failures could lie about them).
//
// The counts do not survive a restart, for the same reason the queue does not.
type Counters struct {
	// Delivered is events the sink accepted.
	Delivered int64 `json:"delivered"`
	// Dropped is events discarded to make room in a full queue — the operator's
	// signal that their target cannot keep up.
	Dropped int64 `json:"dropped"`
	// Failed is delivery attempts the sink gave up on inside its deadline.
	Failed int64 `json:"failed"`
}

// counterSet is the live, slug-keyed tally behind Counters. It lives on the
// Dispatcher rather than on a worker so it SURVIVES a settings save: an Admin
// changing a URL rebuilds the sink, and resetting their failure count at that
// moment would erase the evidence they were about to act on.
type counterSet struct {
	delivered atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
}

// worker is one live sink: the built Plugin, what it subscribes to, its bounded
// queue, and the goroutine draining it.
type worker struct {
	slug   string
	plugin pluginapi.EventSink
	events map[string]struct{}
	queue  chan pluginapi.SinkEvent
	counts *counterSet

	// enqueueMu serializes the drop-oldest dance so two publishers cannot both
	// decide the queue is full and drop two events to make room for one.
	enqueueMu sync.Mutex

	quit chan struct{}
	done chan struct{}
}

// wants reports whether this sink subscribed to eventType.
func (w *worker) wants(eventType string) bool {
	_, ok := w.events[eventType]
	return ok
}

// enqueue hands the worker an event without ever blocking the caller. When the
// queue is full the OLDEST event is discarded — the newest state of the world is
// the useful one, and a sink behind by 64 events has already lost the thread.
func (w *worker) enqueue(ev pluginapi.SinkEvent) {
	w.enqueueMu.Lock()
	defer w.enqueueMu.Unlock()
	for {
		select {
		case w.queue <- ev:
			return
		default:
		}
		select {
		case <-w.queue:
			w.counts.dropped.Add(1)
		default:
			// The worker drained it between the two selects; try the send again.
		}
	}
}

// run drains the queue one event at a time, each under the host's deadline.
// Serial by design: a sink is an outbound integration, not a load generator, and
// one event at a time is what keeps the ordering an operator expects.
func (w *worker) run() {
	defer close(w.done)
	for {
		// A stop WINS over a queued event. Without this first, non-blocking check
		// Go would pick either ready case at random, and "a stop discards what was
		// queued" would be true only most of the time.
		select {
		case <-w.quit:
			return
		default:
		}
		select {
		case <-w.quit:
			return
		case ev := <-w.queue:
			w.deliver(ev)
		}
	}
}

// deliver makes one call into the Plugin under DeliveryTimeout, counting the
// outcome. A stop cancels the in-flight call rather than waiting it out, so
// shutting the server down never waits on somebody else's web server.
func (w *worker) deliver(ev pluginapi.SinkEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), DeliveryTimeout)
	defer cancel()
	go func() {
		select {
		case <-w.quit:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := w.plugin.Deliver(ctx, ev); err != nil {
		w.counts.failed.Add(1)
		log.Printf("obelo: event sink %q: delivering %s (%s): %v", w.slug, ev.Type, ev.ID, err)
		return
	}
	w.counts.delivered.Add(1)
}

// stop ends the worker and waits for it, discarding anything still queued. That
// discard is the documented behavior, not a shortcut: delivery is in memory, so
// the events a stopped sink was holding are gone.
func (w *worker) stop() {
	close(w.quit)
	<-w.done
}

// Dispatcher is the live set of sinks and the one thing the translator talks to.
// It is swapped whole on a settings save (see Manager), which is how a sink an
// Admin just disabled stops receiving events without a restart.
type Dispatcher struct {
	mu      sync.RWMutex
	workers []*worker
	// counters is slug-keyed and OUTLIVES the workers, so a rebuild does not reset
	// an operator's evidence.
	counters map[string]*counterSet
	closed   bool
}

// NewDispatcher returns an empty Dispatcher. Nothing is delivered until a Manager
// reloads settings into it.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{counters: make(map[string]*counterSet)}
}

// Wants reports whether ANY live sink subscribes to eventType. The translator asks
// before it does any work — before it looks up a Library's name, before it mints
// an event id — so a server with no sinks, or with a sink that asked for something
// else, pays nothing for events nobody wants.
func (d *Dispatcher) Wants(eventType string) bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, w := range d.workers {
		if w.wants(eventType) {
			return true
		}
	}
	return false
}

// Publish hands ev to every subscribed sink and returns immediately. It is the
// only thing the translator calls, and it never blocks, never fails and never
// reports anything back — a sink is the operator's business, not the caller's.
func (d *Dispatcher) Publish(ev pluginapi.SinkEvent) {
	if d == nil {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, w := range d.workers {
		if w.wants(ev.Type) {
			w.enqueue(ev)
		}
	}
}

// Counters reports each configured sink's tally, keyed by slug. Issue 06 puts
// these on the settings response; they are counted from this slice so that when
// they do surface they describe a mechanism that has been running all along.
func (d *Dispatcher) Counters() map[string]Counters {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]Counters, len(d.counters))
	for slug, c := range d.counters {
		out[slug] = Counters{
			Delivered: c.delivered.Load(),
			Dropped:   c.dropped.Load(),
			Failed:    c.failed.Load(),
		}
	}
	return out
}

// counterSetFor returns the tally for a slug, creating it on first use.
// Called with d.mu held for writing.
func (d *Dispatcher) counterSetFor(slug string) *counterSet {
	c, ok := d.counters[slug]
	if !ok {
		c = &counterSet{}
		d.counters[slug] = c
	}
	return c
}

// swap replaces the live sinks with next, stopping the previous workers. A stopped
// worker's queue is discarded — see stop.
func (d *Dispatcher) swap(next []sinkSpec) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	previous := d.workers
	workers := make([]*worker, 0, len(next))
	for _, spec := range next {
		w := &worker{
			slug:   spec.slug,
			plugin: spec.plugin,
			events: spec.events,
			queue:  make(chan pluginapi.SinkEvent, QueueCapacity),
			counts: d.counterSetFor(spec.slug),
			quit:   make(chan struct{}),
			done:   make(chan struct{}),
		}
		workers = append(workers, w)
	}
	d.workers = workers
	d.mu.Unlock()

	// Start and stop OUTSIDE the lock: stop waits for an in-flight delivery to
	// unwind, and holding the lock through that would make a settings save wait on
	// a stranger's web server.
	for _, w := range workers {
		go w.run()
	}
	for _, w := range previous {
		w.stop()
	}
}

// Close stops every sink and leaves the Dispatcher inert. Idempotent; called on
// app shutdown, before the Broker goes.
func (d *Dispatcher) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	previous := d.workers
	d.workers = nil
	d.mu.Unlock()

	for _, w := range previous {
		w.stop()
	}
}

// sinkSpec is one built sink on its way into the Dispatcher: the Plugin, its slug,
// and the event types it subscribes to.
type sinkSpec struct {
	slug   string
	plugin pluginapi.EventSink
	events map[string]struct{}
}
