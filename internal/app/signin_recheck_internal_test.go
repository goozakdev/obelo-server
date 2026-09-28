package app

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/bundled/bundledtest"
	"github.com/goozakdev/obelo-server/internal/config"
)

// TestCloseStopsTheSignInRecheckLoop: the sign-in re-check runs in a goroutine
// of its own from New. Close ends it and waits for it, so no re-check can ask a
// provider, or revoke a session, against a server that has shut down.
func TestCloseStopsTheSignInRecheckLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: boots a full App; skipped under -short")
	}
	bundledtest.Ensure(t)
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = ""
	cfg.ScanInterval = 0
	cfg.SessionIdleTimeout = 0
	cfg.AutoEnrichAfterScan = false
	cfg.EnrichInterval = 0
	cfg.LinkSyncInterval = 0
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := a.recheckDone
	if done == nil {
		t.Fatal("New started no sign-in re-check loop")
	}
	select {
	case <-done:
		t.Fatal("the sign-in re-check loop ended before Close")
	default:
	}

	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in re-check loop is still running after Close")
	}
}
