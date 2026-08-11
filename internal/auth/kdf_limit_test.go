package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/argon2"
)

// Tests for the argon2 concurrency meter (the KDF concurrency section of
// password.go). They are in-package because the thing under test is the cap
// itself, which is deliberately not exported — there is no knob here for an
// operator to turn, and nothing outside this package has any business reading it.

// TestKDFConcurrencyIsBounded pins the sizing rule. The upper clamp is the memory
// arithmetic (kdfMaxConcurrency × 64 MiB); the lower one is the difference between
// a bound and an outage.
func TestKDFConcurrencyIsBounded(t *testing.T) {
	if kdfConcurrency < kdfMinConcurrency || kdfConcurrency > kdfMaxConcurrency {
		t.Fatalf("kdfConcurrency = %d, want it clamped to [%d,%d]",
			kdfConcurrency, kdfMinConcurrency, kdfMaxConcurrency)
	}
	if cap(kdfSem) != kdfConcurrency {
		t.Errorf("semaphore capacity = %d, want kdfConcurrency %d", cap(kdfSem), kdfConcurrency)
	}
}

// TestConcurrentHashingNeverExceedsTheCap is the memory bound stated as a test:
// however many callers hash at once, the number of derivations actually running
// at once is never more than the cap — which is what makes peak KDF memory a
// constant instead of a function of request volume.
//
// It runs REAL argon2 at production parameters rather than a stand-in, because
// "one derivation is 64 MiB for as long as it runs" is the fact being bounded —
// with a sleep in place of the KDF the test would prove nothing about memory and
// would pass with the parameters set to anything.
//
// It drives acquireKDF directly rather than HashPasswordContext, which is not a
// weakening of the assertion but the only way to make it: instrumenting from
// inside a call that acquires its own slot would mean holding two, and eight
// goroutines each holding two of four slots is a deadlock, not a test.
// TestHashWaitsForASlotAndIsCancellable covers the other half — that the exported
// hashing really does go through this gate.
func TestConcurrentHashingNeverExceedsTheCap(t *testing.T) {
	const callersPerSlot = 8

	salt := []byte("0123456789abcdef")
	var inflight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < callersPerSlot*kdfConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireKDF(context.Background())
			if err != nil {
				t.Errorf("acquireKDF with a live context: %v", err)
				return
			}
			defer release()

			now := inflight.Add(1)
			for {
				high := peak.Load()
				if now <= high || peak.CompareAndSwap(high, now) {
					break
				}
			}
			argon2.IDKey([]byte("a password to derive"), salt,
				argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
			inflight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > int64(kdfConcurrency) {
		t.Errorf("peak concurrent derivations = %d, want at most %d "+
			"(that many × 64 MiB is the memory bound)", got, kdfConcurrency)
	}
	// A run where nothing ever overlapped would satisfy the assertion above
	// without testing it. On any multi-slot host, dozens of goroutines contending
	// for the semaphore do overlap; if they somehow did not, say so rather than
	// report a pass that measured nothing.
	if kdfConcurrency > 1 && peak.Load() < 2 {
		t.Logf("peak concurrency was %d: nothing overlapped, so the cap was never "+
			"actually exercised on this host", peak.Load())
	}
}

// TestHashWaitsForASlotAndIsCancellable proves two things at once, because they
// are the same mechanism seen from both sides: HashPasswordContext really does go
// through the meter (a full semaphore blocks it), and a caller waiting on the
// meter can be given up on (a cancelled context releases it).
//
// Without the first, the cap would be decorative. Without the second, a flood of
// clients that hang up mid-request would each keep a place in line forever and
// starve the one caller still listening.
func TestHashWaitsForASlotAndIsCancellable(t *testing.T) {
	// Take every slot, so the next acquisition cannot possibly succeed. No sleeps
	// and no polling: the semaphore is full by construction from here.
	releases := make([]func(), 0, kdfConcurrency)
	for i := 0; i < kdfConcurrency; i++ {
		release, err := acquireKDF(context.Background())
		if err != nil {
			t.Fatalf("filling the semaphore: %v", err)
		}
		releases = append(releases, release)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := HashPasswordContext(ctx, "a password to derive"); !errors.Is(err, context.Canceled) {
		t.Errorf("hash with no free slot and a cancelled context = %v, want context.Canceled", err)
	}
	if err := VerifyPasswordContext(ctx, dummyHash, "a password to verify"); !errors.Is(err, context.Canceled) {
		t.Errorf("verify with no free slot and a cancelled context = %v, want context.Canceled", err)
	}

	// Hand the slots back and the same calls go through, so what was observed
	// above was the meter and not some unrelated refusal.
	for _, release := range releases {
		release()
	}
	if _, err := HashPasswordContext(context.Background(), "a password to derive"); err != nil {
		t.Errorf("hash once the slots were released: %v", err)
	}
}

// TestAFreeSlotIsNotDeniedToACancelledCaller pins the documented fast path: with
// a slot going spare, acquireKDF does not consult the context at all. The
// alternative is a random select between two ready cases, i.e. a request that
// could have been served instantly losing a coin flip — and, worse, a
// non-deterministic one, which is not a behavior anything should have to reason
// about.
func TestAFreeSlotIsNotDeniedToACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	release, err := acquireKDF(ctx)
	if err != nil {
		t.Fatalf("acquireKDF with a free slot and a cancelled context = %v, want a slot", err)
	}
	release()
}
