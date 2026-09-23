package plugins

import (
	"context"
	"testing"
	"time"
)

// TestCallRemainingMillis is ADR-0059 decision 6: absent means only "no deadline"; a callCtx
// that HAS a deadline always yields at least 1, never 0, even one already
// passed by the time this runs. It is an internal test (package plugins, not
// plugins_test) because callRemainingMillis is unexported.
func TestCallRemainingMillis(t *testing.T) {
	t.Run("no deadline is nil", func(t *testing.T) {
		got := callRemainingMillis(context.Background())
		if got != nil {
			t.Fatalf("callRemainingMillis(no deadline) = %v, want nil", *got)
		}
	})

	t.Run("600us left rounds up to 1", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Microsecond)
		defer cancel()
		got := callRemainingMillis(ctx)
		if got == nil || *got != 1 {
			t.Fatalf("callRemainingMillis(600us) = %v, want 1", got)
		}
	})

	t.Run("already passed is 1, never 0 or nil", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		time.Sleep(time.Millisecond)
		got := callRemainingMillis(ctx)
		if got == nil || *got != 1 {
			t.Fatalf("callRemainingMillis(already passed) = %v, want 1", got)
		}
	})

	t.Run("2.5s left is about 2500", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		got := callRemainingMillis(ctx)
		if got == nil {
			t.Fatal("callRemainingMillis(2.5s) = nil, want ~2500")
		}
		if *got < 2499 || *got > 2501 {
			t.Fatalf("callRemainingMillis(2.5s) = %d, want 2500 +/- 1", *got)
		}
	})
}
