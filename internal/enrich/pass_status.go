package enrich

import (
	"errors"
	"time"
)

// The vocabulary of a BACKGROUND Enrichment pass — the one an operator starts and
// then walks away from (ADR-0051, "a pass is STARTED, never awaited").
//
// The pass itself is Service.EnrichLibraryProgress, which knows nothing about
// queues or workers: it is handed a context and it runs. What lives here is the
// language the layers ABOVE it need — how a start can fail, and what "is a pass
// running for this Library?" answers with — so that the app (which owns the
// worker and the in-memory status) and the API (which reports both) can name the
// same three states without either importing the other.
//
// The three errors exist because all three were SILENT before this file. A start
// with no worker running was a no-op that returned nothing; a full queue logged a
// line the operator never saw and dropped the request; a duplicate start quietly
// queued a second pass behind the first. An operator pressing a button and being
// told nothing at all is precisely the failure ADR-0051's amendment was written
// about.
var (
	// ErrPassWorkerUnavailable: no background enrich worker is running, so there is
	// nothing to start a pass ON. Nothing would happen, forever, and the caller must
	// be told that rather than shown a hopeful spinner.
	ErrPassWorkerUnavailable = errors.New("enrich: no background pass worker is running")
	// ErrPassQueueFull: the worker's queue is at capacity. The request is refused
	// rather than dropped on the floor, because "we did not run your pass" is
	// information the caller can act on (wait, and press it again).
	ErrPassQueueFull = errors.New("enrich: the background pass queue is full")
	// ErrPassInProgress: a pass is already in flight for this Library. Queueing a
	// second one would only make the operator wait twice as long for the same
	// answer — the per-Library lock serializes them anyway — so the honest reply is
	// "it is already running", with the running pass's status attached.
	ErrPassInProgress = errors.New("enrich: a pass is already running for this library")
)

// String names a Mode on the wire and in a log line: "new", "full", "recheck".
// The API's request parser (api.enrichMode) reads exactly these spellings, so the
// mode a client asks for and the mode a status report names are the same word.
func (m Mode) String() string {
	switch m {
	case ModeFull:
		return "full"
	case ModeRecheck:
		return "recheck"
	default:
		return "new"
	}
}

// PassStatus is the answer to "is a pass running over this Library, and what came
// of the last one?" — the shape behind GET /libraries/{id}/enrich.
//
// It is deliberately a VALUE, snapshotted under the holder's lock, so a reader
// never observes a half-updated pass.
//
// This status is held IN MEMORY by whoever runs the worker, never in a table. A
// persisted one would have to claim a pass was running across a restart that
// killed it — the scan status persists for a different reason, namely that a
// scan's result stays meaningful long after it finishes. The requirement here is
// that a page RELOAD rejoins a running pass, not that a server restart does.
type PassStatus struct {
	// Running is true while a pass is queued or executing for this Library.
	Running bool
	// Mode is the running pass's mode; meaningless when Running is false.
	Mode Mode
	// StartedAt is when the running pass was queued (zero when idle).
	StartedAt time.Time
	// Progress is the running pass's latest snapshot — done/total plus the running
	// counts the enrichProgress SSE already carries. Zero until the pass's first
	// callback, which for a Music Library is after the whole parent phase
	// (collectMusicLeaves re-asks unmatched parents before any leaf settles; noted
	// as out of scope in ADR-0051's amendment, and visible rather than mysterious
	// now that progress is reported at all).
	Progress Progress
	// Last is the summary of the most recent FINISHED pass over this Library, or
	// nil if none has finished since this process started. LastMode / LastFinishedAt
	// describe it.
	Last           *Result
	LastMode       Mode
	LastFinishedAt time.Time
}
