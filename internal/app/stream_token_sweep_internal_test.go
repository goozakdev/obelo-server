package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/bundled/bundledtest"
	"github.com/goozakdev/obelo-server/internal/config"
	"github.com/goozakdev/obelo-server/internal/store"
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

// TestNewSweepsExpiredStreamTokens: the sweep loop is only worth testing if New
// starts it. An expired row seeded before boot is gone shortly after, and a live
// one is left alone; drop the goroutine from New and the expired row stays.
func TestNewSweepsExpiredStreamTokens(t *testing.T) {
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

	seed, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO users (id, username, role, password_hash) VALUES ('u1','ada','member','x')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for hash, expires := range map[string]time.Time{
		"expired": now.Add(-time.Hour),
		"live":    now.Add(time.Hour),
	} {
		if err := seed.InsertStreamToken(store.StreamToken{
			TokenHash: hash, SessionID: "s-" + hash, UserID: "u1",
			CreatedAt: now.Format(time.RFC3339), ExpiresAt: expires.Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	reader, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	count := func(hash string) int {
		var n int
		if err := reader.QueryRow(`SELECT COUNT(*) FROM stream_tokens WHERE token_hash = ?`, hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(10 * time.Second)
	for count("expired") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count("expired"); n != 0 {
		t.Fatalf("expired stream token rows after boot = %d, want the sweep to have deleted it", n)
	}
	if n := count("live"); n != 1 {
		t.Fatalf("live stream token rows = %d, want it left alone", n)
	}
}
