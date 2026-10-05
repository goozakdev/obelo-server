package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestSweepEveryRunsAtStartAndOnEachTickUntilCancelled: mint no longer deletes
// expired stream tokens, so this loop is the only thing that does.
func TestSweepEveryRunsAtStartAndOnEachTickUntilCancelled(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweepEvery(ctx, 5*time.Millisecond, func() error {
			calls.Add(1)
			return nil
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := calls.Load(); n < 3 {
		t.Fatalf("sweeps = %d, want at least 3 (start + ticks)", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweepEvery did not stop on cancel")
	}
}
