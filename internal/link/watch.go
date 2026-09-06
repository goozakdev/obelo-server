package link

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Staying fresh (ADR-0056 §4, §6): the goroutines, and nothing else.
//
// Every decision about what a sweep DOES is in sync.go. This file owns only when
// one happens, and there are exactly three answers:
//
//   - the sharer said something changed — a `libraryUpdated` on the `/events`
//     stream this Server holds open under the Link's token, debounced;
//   - the timer came round — OBELO_LINK_SYNC_INTERVAL, the poll fallback;
//   - an operator pressed "sync now" — POST /links/{id}/sync.
//
// The subscription is an OPTIMISATION OVER THE TIMER AND NEVER A REQUIREMENT
// (ADR-0016's bargain, restated for a server-to-server stream): a Server whose
// subscription is down is a Server that is one interval behind, not one that is
// wrong. Nothing about a failed subscription touches the Link's state — only a
// sweep decides that — with the single exception of a 401, which is the sharer
// telling this Server the credential is dead however it heard it.

// linkBackoffMin / linkBackoffMax bound the retry of an `unreachable` Link
// (ADR-0056 §6: "retried with backoff"). It doubles from a minute to half an
// hour and stays there: a friend's server that is off for a week costs 48
// requests a day, and one that reboots is picked up within a minute.
const (
	linkBackoffMin = time.Minute
	linkBackoffMax = 30 * time.Minute
)

// nudgeDebounce is how long a `libraryUpdated` waits for its friends before a
// sweep. A scan on the sharer's side publishes one per Library it finishes and an
// enrichment pass publishes more; sweeping on each would walk the same feed a
// dozen times for one evening's additions.
const nudgeDebounce = 2 * time.Second

// Syncer keeps every Link's mirror fresh. One per Server, owned by the app: it
// is started after the API is wired and stopped on shutdown, and it holds one
// goroutine per Link plus that Link's subscription.
type Syncer struct {
	svc      *Service
	interval time.Duration
	debounce time.Duration
	// after is the only clock this file has. Injected so a test drives the timer
	// and the backoff without waiting for either.
	after func(d time.Duration) <-chan time.Time
	// subscribe holds one Link's /events stream open. Injected for the same reason
	// as after; nil in a test that is not exercising the nudge path.
	subscribe func(ctx context.Context, l store.Link, onUpdate func(remoteLibraryID string)) error
	// onSweep, when set, is called after every sweep with its result. It exists for
	// tests, which otherwise have to guess when a background goroutine has finished.
	onSweep func(linkID string, err error)

	mu      sync.Mutex
	workers map[string]*linkWorker
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

// SyncerOptions are the Syncer's injectable seams. The zero value of each is
// what production uses.
type SyncerOptions struct {
	// Interval is the poll fallback's cadence (config.LinkSyncInterval), and it is
	// the switch for the whole background half: 0 starts NO per-Link goroutine, so
	// there is no timer, no backoff and no subscription, and the mirror refreshes
	// only when a Link is created or re-keyed or an operator asks
	// (POST /links/{id}/sync). Start still parks every Link as `unreachable`,
	// because that is a statement about this boot and not about the schedule.
	//
	// One knob for both because they are one feature — "does this Server keep its
	// friends' libraries fresh on its own" — and a Server that has stopped pulling
	// on a timer has no use for a stream telling it to.
	Interval time.Duration
	// Debounce is how long a nudge waits for more of its kind. 0 uses
	// nudgeDebounce; a negative value sweeps on every nudge.
	Debounce time.Duration
	// After replaces time.After for the timer, the backoff and the debounce.
	After func(d time.Duration) <-chan time.Time
	// Subscribe replaces the /events subscription. A test that wants no outbound
	// stream sets it to a function that blocks on ctx.
	Subscribe func(ctx context.Context, l store.Link, onUpdate func(remoteLibraryID string)) error
	// OnSweep is called after each sweep with its result — a test seam, nil in
	// production.
	OnSweep func(linkID string, err error)
}

// linkWorker is one Link's goroutine and the two ways to wake it.
type linkWorker struct {
	id string
	// nudge is coalescing (buffered 1): ten libraryUpdated events between two
	// sweeps are one reason to sweep.
	nudge chan struct{}
	// force carries a reply channel, so POST /links/{id}/sync waits for the sweep
	// it asked for rather than for whatever sweep happens next.
	force  chan chan error
	done   chan struct{}
	cancel context.CancelFunc
}

// NewSyncer wires a Syncer over a Service. It starts nothing; Start does.
func NewSyncer(svc *Service, opts SyncerOptions) *Syncer {
	sy := &Syncer{
		svc:       svc,
		interval:  opts.Interval,
		debounce:  opts.Debounce,
		after:     opts.After,
		subscribe: opts.Subscribe,
		onSweep:   opts.OnSweep,
		workers:   map[string]*linkWorker{},
	}
	if sy.debounce == 0 {
		sy.debounce = nudgeDebounce
	}
	if sy.after == nil {
		sy.after = time.After
	}
	if sy.subscribe == nil {
		sy.subscribe = svc.watchEvents
	}
	return sy
}

// Start brings every Link under the Syncer's care.
//
// EVERY LINK BEGINS `unreachable`, whatever it was when this Server last shut
// down, and stays there until a call to that household actually succeeds
// (ADR-0056 §6). A Server that boots with no network then says so — its friends'
// libraries are there and badged unavailable — instead of showing a `connected`
// it inherited from a week ago and only discovering the truth when somebody
// presses play. A `revoked` Link is left alone: that state is a fact about the
// sharer's User table, not about this boot.
//
// It returns an error only if the Links cannot be read at all. Everything after
// that is a goroutine's problem, and none of it may fail a boot.
func (sy *Syncer) Start(ctx context.Context) error {
	// Reap any linked Library whose Link is gone before anything reads the table —
	// the debris a partial unlink can leave (issue 17). No requests are served yet
	// and no worker is running, so this is the one moment it races nothing.
	if n, err := sy.svc.store.DeleteOrphanLinkedLibraries(); err != nil {
		log.Printf("obelo: link: could not remove orphaned linked libraries at boot: %v", err)
	} else if n > 0 {
		log.Printf("obelo: link: removed %d orphaned linked librar%s left by an interrupted unlink", n, plural(n))
	}

	links, err := sy.svc.store.Links()
	if err != nil {
		return err
	}

	sy.mu.Lock()
	if sy.cancel != nil {
		sy.mu.Unlock()
		return nil
	}
	sy.ctx, sy.cancel = context.WithCancel(ctx)
	sy.mu.Unlock()

	sy.svc.attach(sy)
	sy.svc.OnLinked = sy.LinkAdded
	sy.svc.OnUnlinked = sy.LinkRemoved

	for _, l := range links {
		if l.State != store.LinkStateRevoked && l.State != store.LinkStateUnreachable {
			if err := sy.svc.store.SetLinkState(l.ID, store.LinkStateUnreachable,
				"not contacted since this server started"); err != nil {
				log.Printf("obelo: link: could not park %q as unreachable at boot: %v", l.ServerName, err)
			}
		}
		sy.start(l.ID)
	}
	return nil
}

// Stop cancels every goroutine and waits for them, so a closed App leaves none
// behind and no publish reaches a closed Broker.
func (sy *Syncer) Stop() {
	sy.mu.Lock()
	cancel := sy.cancel
	sy.cancel, sy.stopped = nil, true
	sy.workers = map[string]*linkWorker{}
	sy.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	sy.wg.Wait()
}

// LinkAdded starts (or restarts) a Link's worker. It is Service.OnLinked, so it
// runs inside the request that pasted the invite and must not block: a re-key
// restarts the worker precisely because the addresses moved and the credential
// may have come back from the dead, and the restart is two channel operations.
func (sy *Syncer) LinkAdded(l store.Link) {
	sy.stopWorker(l.ID)
	sy.start(l.ID)
}

// LinkRemoved stops a Link's worker after the row is gone.
func (sy *Syncer) LinkRemoved(l store.Link) { sy.stopWorker(l.ID) }

// SweepNow forces one sweep of a Link and waits for it — the "try again now" the
// Linked servers page calls. It goes through the Link's own worker, so it cannot
// race the timer's sweep, and its result resets the backoff exactly as a
// scheduled sweep's would.
func (sy *Syncer) SweepNow(ctx context.Context, linkID string) error {
	w := sy.worker(linkID)
	if w == nil {
		return sy.svc.sweep(ctx, linkID, true)
	}
	reply := make(chan error, 1)
	select {
	case w.force <- reply:
	case <-w.done:
		// The worker went away under us (an unlink, or shutdown). Sweeping inline is
		// still the right answer to the request that is in flight.
		return sy.svc.sweep(ctx, linkID, true)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sy *Syncer) start(linkID string) {
	if sy.interval <= 0 {
		// Background freshness is off (SyncerOptions.Interval). Nothing runs, and
		// SweepNow falls back to sweeping inline for the operator who asks.
		return
	}
	sy.mu.Lock()
	if sy.stopped || sy.ctx == nil {
		sy.mu.Unlock()
		return
	}
	if _, ok := sy.workers[linkID]; ok {
		sy.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(sy.ctx)
	w := &linkWorker{
		id:     linkID,
		nudge:  make(chan struct{}, 1),
		force:  make(chan chan error),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	sy.workers[linkID] = w
	sy.wg.Add(1)
	sy.mu.Unlock()

	go func() {
		defer sy.wg.Done()
		sy.run(ctx, w)
	}()
}

func (sy *Syncer) stopWorker(linkID string) {
	sy.mu.Lock()
	w := sy.workers[linkID]
	delete(sy.workers, linkID)
	sy.mu.Unlock()
	if w != nil {
		w.cancel()
		<-w.done
	}
}

func (sy *Syncer) worker(linkID string) *linkWorker {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	return sy.workers[linkID]
}

// run is one Link's whole life: sweep, decide how long until the next one, and
// wait for whichever of the three reasons arrives first.
//
// The FIRST sweep is immediate. That is what makes a Server that boots with its
// friends reachable say `connected` within a second of coming up, rather than an
// interval later, and it is the same reasoning that puts the first pull inside
// the POST that created the Link.
func (sy *Syncer) run(ctx context.Context, w *linkWorker) {
	defer close(w.done)
	go sy.hold(ctx, w)

	failures := 0
	first := true
	for {
		var reply chan error
		if !first {
			wait := sy.nextWait(failures)
			var timer <-chan time.Time
			if wait > 0 {
				timer = sy.after(wait)
			}
			select {
			case <-ctx.Done():
				return
			case <-timer:
			case <-w.nudge:
				sy.settle(ctx, w)
			case reply = <-w.force:
			}
		}
		first = false

		err := sy.sweep(ctx, w.id, reply != nil)
		if reply != nil {
			reply <- err
		}
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			failures = 0
		case errors.Is(err, ErrCredentialDead):
			// `revoked`: no retries, ever. Only a re-key (which restarts this worker
			// through OnLinked) or an operator's explicit sync tries again, and both
			// arrive on a channel this loop is already waiting on.
			failures = -1
		default:
			if failures < 0 {
				failures = 0
			}
			failures++
		}
	}
}

// nextWait is the whole schedule: park a revoked Link, retry an unreachable one
// with a doubling backoff, and otherwise wait out the interval. 0 means "wait for
// something to happen" — a nudge or an operator.
func (sy *Syncer) nextWait(failures int) time.Duration {
	if failures < 0 {
		return 0
	}
	if failures == 0 {
		return sy.interval
	}
	wait := linkBackoffMin << (failures - 1)
	if wait > linkBackoffMax || wait <= 0 {
		wait = linkBackoffMax
	}
	return wait
}

// settle is the nudge debounce: once one arrives, wait a moment for the rest of
// the burst before sweeping.
func (sy *Syncer) settle(ctx context.Context, w *linkWorker) {
	if sy.debounce <= 0 {
		return
	}
	window := sy.after(sy.debounce)
	for {
		select {
		case <-ctx.Done():
			return
		case <-window:
			return
		case <-w.nudge:
		}
	}
}

func (sy *Syncer) sweep(ctx context.Context, linkID string, forced bool) error {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	err := sy.svc.sweep(ctx, linkID, forced)
	if sy.onSweep != nil {
		sy.onSweep(linkID, err)
	}
	return err
}

// hold keeps the Link's /events subscription up for as long as the worker lives,
// reconnecting with the same backoff a sweep uses.
//
// A dropped subscription is not an error anybody is told about: the timer is the
// contract and this is the optimisation. The one exception is a 401, which is the
// sharer saying the credential is dead — that IS a state transition, and it is
// handed to the same recorder a sweep uses so the Link reads `revoked` even if
// the next sweep is an hour away.
func (sy *Syncer) hold(ctx context.Context, w *linkWorker) {
	failures := 0
	for {
		l, err := sy.svc.store.LinkByID(w.id)
		if err != nil {
			return
		}
		if l.State == store.LinkStateRevoked || l.Token == "" || l.ActiveOrigin == "" {
			return
		}
		started := time.Now()
		err = sy.subscribe(ctx, l, func(string) { nudge(w) })
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrCredentialDead) {
			sy.svc.record(l, err)
			return
		}
		if time.Since(started) > linkBackoffMax {
			// A stream that lived longer than the longest backoff was a HEALTHY one
			// that ended — a peer rebooting, a tailnet blinking. Counting it against
			// the retry schedule would leave a Server that has been up for a month
			// waiting half an hour to reconnect to a friend who was down for five
			// seconds.
			failures = 0
		}
		failures++
		select {
		case <-ctx.Done():
			return
		case <-sy.after(sy.nextWait(failures)):
		}
	}
}

// nudge wakes a worker without ever blocking the caller: the channel holds one,
// which is all "something changed" ever needs to say.
func nudge(w *linkWorker) {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// --- the outbound subscription -------------------------------------------------

// watchEvents holds `GET /events` open under the Link's token and calls onUpdate
// for every `libraryUpdated` the sharer sends (ADR-0056 §4).
//
// The sharer's own audience gating does the filtering for free: the `remote` User
// is granted exactly the Libraries it may mirror, and a library-scoped event
// reaches only a subscriber who can see that Library (events.Audience). So every
// libraryUpdated arriving here is one about a Library this Server mirrors, and the
// id is passed along rather than matched — a Library newly granted must nudge too,
// and this side has not heard of it yet.
//
// It returns when the stream ends, the peer refuses it, or ctx is cancelled.
func (s *Service) watchEvents(ctx context.Context, l store.Link, onUpdate func(remoteLibraryID string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.ActiveOrigin+apiPrefix+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.dialer.StreamClient().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer closeBody(resp)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrCredentialDead
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: %s answered %d for the event stream",
			ErrUnreachable, l.ServerName, resp.StatusCode)
	}
	return readEventStream(resp.Body, onUpdate)
}

// remoteLibraryUpdatedEvent is the sharer's event name (docs/api-contract.md
// Part 2). It is spelled out here rather than taken from internal/events because
// it is a WIRE name on somebody else's stream: it happens to match this Server's
// own event type today, and renaming ours must not silently change what this
// mirror listens for.
const remoteLibraryUpdatedEvent = "libraryUpdated"

// maxEventLine bounds one SSE line. The events this Server cares about are a
// dozen bytes of JSON; the cap is what stops a peer — friendly or not — from
// growing this process's memory with one very long line.
const maxEventLine = 64 * 1024

// readEventStream parses the `event:`/`data:` pairs of an SSE stream (ADR-0016's
// wire format, read from the other side) and reports each libraryUpdated.
//
// It knows only the one event it acts on and skips everything else by name, so a
// sharer that grows a new event type does not confuse a mirror that has never
// heard of it.
func readEventStream(body io.Reader, onUpdate func(remoteLibraryID string)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), maxEventLine)

	var event, data string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if event == remoteLibraryUpdatedEvent {
				var payload struct {
					LibraryID string `json:"libraryId"`
				}
				if err := json.Unmarshal([]byte(data), &payload); err == nil {
					onUpdate(payload.LibraryID)
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, ":"):
			// A comment — the stream's own ": connected" keepalive.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return sc.Err()
}
