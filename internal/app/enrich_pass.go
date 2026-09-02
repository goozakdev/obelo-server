package app

import (
	"log"
	"time"

	"github.com/goozakdev/obelo-server/internal/enrich"
)

// Background Enrichment passes: starting one, and knowing one is running.
//
// ADR-0051's amendment, written from a real operator failure: a pass is STARTED,
// never awaited. `POST /libraries/{id}/enrich` used to run the pass inside the
// HTTP request, so a recheck measured in minutes hung the fetch; the operator
// reloaded the page, the reload cancelled the request, and the reload cancelled
// the PASS. Every one of the 724 flagged rows still had a blank
// enrichment_reason afterwards, which is how we know not one leaf had been
// processed.
//
// The fix is the pattern handleScan has always used, applied to enrichment: hand
// the work to the background worker this App already runs — on the APPLICATION's
// context, not the request's — and return at once. What this file adds around
// that worker is the part enrichment did not have: an honest answer to "did my
// press do anything?" and "is it still going?".
//
// The status is in memory, per Library, and dies with the process. See
// enrich.PassStatus for why it is not a table.

// enrichQueueDepth is how many pass requests may wait for the worker. Deep enough
// that a sweep over every Library never overflows; finite so a runaway producer is
// refused rather than allowed to grow without bound.
const enrichQueueDepth = 64

// enrichPassState is the in-memory record of Enrichment passes for ONE Library.
// Guarded by App.enrichMu.
type enrichPassState struct {
	// inFlight counts passes queued-or-executing for this Library. It is a COUNT,
	// not a bool, because the auto-after-scan and policy-change triggers may still
	// enqueue while a manual pass runs (their behaviour is deliberately unchanged);
	// the Library is "running" until the last of them settles.
	inFlight int
	// mode / startedAt describe the most recently STARTED in-flight pass.
	mode      enrich.Mode
	startedAt time.Time
	// progress is the latest snapshot the running pass reported.
	progress enrich.Progress
	// settled is closed when inFlight falls back to 0 — the deterministic "the pass
	// is over" signal a test awaits instead of sleeping, and the counterpart of the
	// `done` channel StartScan takes. Nil while idle.
	settled chan struct{}
	// done are the callbacks to fire when inFlight falls back to 0, each with the
	// last pass's summary and error. Registered by StartEnrichPass's caller.
	done []func(enrich.Result, error)
	// last / lastMode / lastFinishedAt summarize the most recent FINISHED pass.
	last           *enrich.Result
	lastMode       enrich.Mode
	lastFinishedAt time.Time
}

// StartEnrichPass starts a background Enrichment pass over a Library and returns
// IMMEDIATELY. The pass runs on the worker's context — this App's lifetime — so
// cancelling whatever asked for it (an HTTP request whose client navigated away,
// which is the exact bug this exists to prevent) cannot cancel the pass.
//
// done, when non-nil, is invoked once no pass is in flight for the Library any
// more, with the last pass's summary and error. It is the affordance
// scanner.StartScan already has: it lets a caller await a background job
// deterministically rather than polling or sleeping. EnrichPassSettled is built
// on it.
//
// It answers, rather than hides, the three ways a start can fail to be a pass:
// enrich.ErrPassWorkerUnavailable (no worker — nothing would ever run),
// enrich.ErrPassQueueFull (the queue is at capacity), and
// enrich.ErrPassInProgress (this Library already has one; a duplicate would only
// queue behind the same per-Library lock). The caller — the API handler — turns
// each into something the operator can read.
//
// It does NOT validate the Library: the caller checks existence first so an
// unknown id is a 404 before anything is queued, exactly as handleScan does.
func (a *App) StartEnrichPass(libraryID string, mode enrich.Mode, done func(enrich.Result, error)) error {
	return a.dispatchEnrichPass(enrichRequest{libraryID: libraryID, mode: mode, done: done}, true)
}

// EnrichPassStatus reports whether a pass is running over a Library, in which
// mode and how far along, plus the summary of the most recent finished one. The
// zero value (Running false, Last nil) is the honest answer for a Library this
// process has never enriched — including one that WAS enriched before a restart,
// because a status that survived the process would be claiming a pass survived it
// too.
func (a *App) EnrichPassStatus(libraryID string) enrich.PassStatus {
	a.enrichMu.Lock()
	defer a.enrichMu.Unlock()
	st := a.enrichPasses[libraryID]
	if st == nil {
		return enrich.PassStatus{}
	}
	return enrich.PassStatus{
		Running:        st.inFlight > 0,
		Mode:           st.mode,
		StartedAt:      st.startedAt,
		Progress:       st.progress,
		Last:           st.last,
		LastMode:       st.lastMode,
		LastFinishedAt: st.lastFinishedAt,
	}
}

// EnrichPassSettled returns a channel closed once no Enrichment pass is in flight
// for the Library. An idle Library gets an already-closed channel, so a caller
// can always just receive.
//
// This is the deterministic await a black-box test needs: the pass it started
// over HTTP is somebody else's goroutine, and the alternative — polling the
// status route on a timer — is a sleep wearing a hat.
func (a *App) EnrichPassSettled(libraryID string) <-chan struct{} {
	a.enrichMu.Lock()
	defer a.enrichMu.Unlock()
	if st := a.enrichPasses[libraryID]; st != nil && st.inFlight > 0 {
		return st.settled
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

// enrichWorkerRunning reports whether the background enrich worker is currently
// draining the queue. It is the difference between "your pass is queued" and "no
// one will ever pick it up", which `enqueue` used to answer with silence.
func (a *App) enrichWorkerRunning() bool {
	return a.enrichQueue != nil && a.enrichWorkerUp.Load()
}

// dispatchEnrichPass is the shared start path. exclusive is what separates a
// MANUAL start from the automatic triggers: an Admin pressing the button gets
// enrich.ErrPassInProgress rather than a duplicate pass, while auto-after-scan
// and the policy-change re-enrich keep queueing unconditionally — their
// behaviour is deliberately untouched here, and the per-Library lock has always
// serialized them.
func (a *App) dispatchEnrichPass(req enrichRequest, exclusive bool) error {
	if !a.enrichWorkerRunning() {
		return enrich.ErrPassWorkerUnavailable
	}
	if !a.markEnrichInFlight(req, exclusive) {
		return enrich.ErrPassInProgress
	}
	select {
	case a.enrichQueue <- req:
		return nil
	default:
		// Capacity, not correctness: nothing will run this one, so undo the
		// bookkeeping and SAY so rather than logging into the void.
		log.Printf("obelo: enrich queue full, refusing a %s pass of %q", req.mode, req.libraryID)
		a.settleEnrichPass(req.libraryID, enrich.Result{}, enrich.ErrPassQueueFull, false)
		return enrich.ErrPassQueueFull
	}
}

// markEnrichInFlight records a pass as queued for a Library, returning false when
// exclusive was asked for and one is already in flight.
func (a *App) markEnrichInFlight(req enrichRequest, exclusive bool) bool {
	a.enrichMu.Lock()
	defer a.enrichMu.Unlock()
	if a.enrichPasses == nil {
		a.enrichPasses = make(map[string]*enrichPassState)
	}
	st := a.enrichPasses[req.libraryID]
	if st == nil {
		st = &enrichPassState{}
		a.enrichPasses[req.libraryID] = st
	}
	if exclusive && st.inFlight > 0 {
		return false
	}
	if st.inFlight == 0 {
		st.settled = make(chan struct{})
		st.progress = enrich.Progress{LibraryID: req.libraryID}
	}
	st.inFlight++
	st.mode = req.mode
	st.startedAt = time.Now().UTC()
	if req.done != nil {
		st.done = append(st.done, req.done)
	}
	return true
}

// noteEnrichProgress records a running pass's latest snapshot, so a page that
// joins mid-pass can show how far along it is.
func (a *App) noteEnrichProgress(libraryID string, p enrich.Progress) {
	a.enrichMu.Lock()
	defer a.enrichMu.Unlock()
	if st := a.enrichPasses[libraryID]; st != nil {
		st.progress = p
	}
}

// settleEnrichPass releases one in-flight pass. When it was the last one the
// Library goes idle: the settled channel closes and every registered done
// callback fires (outside the lock, so a callback may call back in). recordLast
// is false for a pass that never ran at all — a queue-full refusal must not
// overwrite the summary of the pass that DID run.
func (a *App) settleEnrichPass(libraryID string, res enrich.Result, err error, recordLast bool) {
	a.enrichMu.Lock()
	st := a.enrichPasses[libraryID]
	if st == nil {
		a.enrichMu.Unlock()
		return
	}
	if st.inFlight > 0 {
		st.inFlight--
	}
	if recordLast && err == nil {
		summary := res
		st.last = &summary
		st.lastMode = st.mode
		st.lastFinishedAt = time.Now().UTC()
	}
	var (
		callbacks []func(enrich.Result, error)
		settled   chan struct{}
	)
	if st.inFlight == 0 {
		callbacks, st.done = st.done, nil
		settled, st.settled = st.settled, nil
	}
	a.enrichMu.Unlock()

	if settled != nil {
		close(settled)
	}
	for _, cb := range callbacks {
		cb(res, err)
	}
}
