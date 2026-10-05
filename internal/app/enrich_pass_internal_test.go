package app

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
)

func newPassApp(depth int) *App {
	a := &App{enrichQueue: make(chan enrichRequest, depth)}
	a.enrichWorkerUp.Store(true)
	return a
}

// TestEnrichStatusDescribesTheRunningPassNotTheQueuedOne: a pass queued behind a
// running one must not relabel it, and the status takes the queued pass's mode only
// once the worker actually starts it.
func TestEnrichStatusDescribesTheRunningPassNotTheQueuedOne(t *testing.T) {
	a := newPassApp(4)
	if err := a.dispatchEnrichPass(enrichRequest{libraryID: "lib", mode: enrich.ModeNew}, false); err != nil {
		t.Fatal(err)
	}
	a.noteEnrichStart("lib", enrich.ModeNew)
	started := a.EnrichPassStatus("lib").StartedAt

	// A policy change queues a Full pass behind the running New one.
	if err := a.dispatchEnrichPass(enrichRequest{libraryID: "lib", mode: enrich.ModeFull}, false); err != nil {
		t.Fatal(err)
	}
	st := a.EnrichPassStatus("lib")
	if st.Mode != enrich.ModeNew || !st.StartedAt.Equal(started) {
		t.Fatalf("status = %s @ %v, want the running New pass @ %v", st.Mode, st.StartedAt, started)
	}

	// New finishes: LastMode must say New, not the queued Full.
	a.settleEnrichPass("lib", enrich.Result{}, nil, true)
	if got := a.EnrichPassStatus("lib").LastMode; got != enrich.ModeNew {
		t.Fatalf("LastMode = %s, want %s", got, enrich.ModeNew)
	}

	// The worker dequeues Full: now the status describes it.
	a.noteEnrichStart("lib", enrich.ModeFull)
	if got := a.EnrichPassStatus("lib").Mode; got != enrich.ModeFull {
		t.Fatalf("Mode = %s, want %s once Full starts", got, enrich.ModeFull)
	}
}

// TestRefusedEnrichPassLeavesNoModeBehind: a queue-full refusal never ran, so it
// must not leave its mode or start time on the status.
func TestRefusedEnrichPassLeavesNoModeBehind(t *testing.T) {
	a := newPassApp(0) // unbuffered, nobody reading: the send is refused
	err := a.dispatchEnrichPass(enrichRequest{libraryID: "lib", mode: enrich.ModeFull}, false)
	if !errors.Is(err, enrich.ErrPassQueueFull) {
		t.Fatalf("err = %v, want ErrPassQueueFull", err)
	}
	st := a.EnrichPassStatus("lib")
	if st.Running || st.Mode == enrich.ModeFull || !st.StartedAt.IsZero() {
		t.Fatalf("refused pass left state behind: %+v", st)
	}
}
