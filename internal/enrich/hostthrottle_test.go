package enrich

import (
	"context"
	"testing"
	"time"
)

// A rate limit belongs to the host, not to the caller (ADR-0049). Manager.
// resolveLibrary builds a provider PER LIBRARY, so a per-instance throttle let a
// three-Library server send 3 req/sec while each Library believed it was pacing
// itself correctly.
func TestOneThrottlePerHostAcrossProviderInstances(t *testing.T) {
	const interval = 60 * time.Millisecond
	// Two providers built independently, exactly as two Libraries' snapshots are.
	a := NewMusicBrainzProvider("https://mb.test.invalid/ws/2", "", "en")
	b := NewMusicBrainzProvider("https://mb.test.invalid/ws/2", "", "en")
	a.MinInterval, b.MinInterval = interval, interval

	ctx := context.Background()
	start := time.Now()
	// Four requests alternating between the instances. Sharing one limiter, they
	// queue: three intervals must elapse. With a limiter each, the pairs run
	// concurrently and this finishes in roughly one.
	for _, p := range []*MusicBrainzProvider{a, b, a, b} {
		if err := p.throttle(ctx); err != nil {
			t.Fatalf("throttle: %v", err)
		}
	}
	elapsed := time.Since(start)
	if min := 3 * interval; elapsed < min {
		t.Fatalf("four requests across two providers for one host took %s, want at least %s — "+
			"each instance is pacing itself separately, so N Libraries send N times the "+
			"agreed rate at one host that counts them together", elapsed, min)
	}
}

// Different hosts must not queue behind each other: a slow MusicBrainz should not
// hold up a fanart.tv call.
func TestDifferentHostsThrottleIndependently(t *testing.T) {
	const interval = 80 * time.Millisecond
	a := NewMusicBrainzProvider("https://one.test.invalid/ws/2", "", "en")
	b := NewMusicBrainzProvider("https://two.test.invalid/ws/2", "", "en")
	a.MinInterval, b.MinInterval = interval, interval

	ctx := context.Background()
	_ = a.throttle(ctx) // claim each host's first slot
	_ = b.throttle(ctx)

	start := time.Now()
	if err := b.throttle(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > interval*2 {
		t.Fatalf("the second host waited %s, want about one interval — hosts are sharing a queue",
			elapsed)
	}
}

// Zero disables throttling (the documented setting for a self-hosted mirror with
// no rate policy).
func TestZeroIntervalDoesNotThrottle(t *testing.T) {
	p := NewMusicBrainzProvider("https://free.test.invalid/ws/2", "", "en")
	p.MinInterval = 0
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := p.throttle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an unthrottled provider waited %s", elapsed)
	}
}

// A cancelled context must not be swallowed by a pending wait — that is how a
// shutdown mid-pass stalls.
func TestThrottleHonorsCancellation(t *testing.T) {
	p := NewMusicBrainzProvider("https://cancel.test.invalid/ws/2", "", "en")
	p.MinInterval = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.throttle(ctx); err != nil { // claims the slot, returns immediately
		t.Fatal(err)
	}
	cancel()
	if err := p.throttle(ctx); err == nil {
		t.Fatal("a cancelled context waited out a 10s throttle instead of returning")
	}
}
