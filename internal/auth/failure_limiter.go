package auth

import (
	"sync"
	"time"
)

// failureLimiter is the fixed-window failure counter this package uses wherever
// an endpoint has to survive someone guessing at it: the device-code approve
// endpoint (device_auth.go) and password login (login_limit.go). It counts
// FAILURES only, so a household that types the right thing never meets it — the
// control costs nothing until someone is wrong a lot.
//
// It is per-boot and in memory, like the claim token (ADR-0013) and for the same
// reason: this is safety state, not a record of anything. A restart clears it,
// which is acceptable — restarting the server is not a primitive a remote
// attacker has, and persisting it would buy a table and a sweeper for nothing.
//
// Fixed window, not sliding. An attacker who straddles a window boundary gets
// 2×limit guesses in one window's width and then stops, which is not a rate worth
// the per-attempt bookkeeping a sliding window costs.
//
// The type carries no clock: callers pass now. That is what keeps the Service's
// WithClock seam meaningful — window expiry is testable by winding a fake clock
// rather than by sleeping out a real five or fifteen minutes, which is to say
// testable at all.
type failureLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	fails   map[string]*failureWindow
	sweepAt int
}

// failureWindow is one key's failure count and the instant its window opened.
type failureWindow struct {
	count       int
	windowStart time.Time
}

// failureLimiterMinSweep is the map size below which expired-entry sweeping is
// not worth doing. See sweep.
const failureLimiterMinSweep = 1024

func newFailureLimiter(limit int, window time.Duration) *failureLimiter {
	return &failureLimiter{
		limit:   limit,
		window:  window,
		fails:   map[string]*failureWindow{},
		sweepAt: failureLimiterMinSweep,
	}
}

// allow reports whether key is still under its limit, and when it is not, how
// much of the window is left — which is what a caller turns into Retry-After.
//
// An expired window is dropped here rather than merely ignored, so a key that has
// served its time stops costing anything to remember.
func (l *failureLimiter) allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, tracked := l.fails[key]
	if !tracked {
		return true, 0
	}
	elapsed := now.Sub(w.windowStart)
	if elapsed >= l.window {
		delete(l.fails, key)
		return true, 0
	}
	if w.count < l.limit {
		return true, 0
	}
	return false, l.window - elapsed
}

// charge records one failure against key, opening a fresh window if the key is
// new or its previous window has run out.
func (l *failureLimiter) charge(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.fails == nil {
		l.fails = map[string]*failureWindow{}
	}
	w, tracked := l.fails[key]
	if !tracked || now.Sub(w.windowStart) >= l.window {
		l.sweep(now)
		l.fails[key] = &failureWindow{count: 1, windowStart: now}
		return
	}
	w.count++
}

// sweep drops entries whose windows have expired. Callers of allow retire their
// own keys, so this only matters for keys nobody ever asks about again — which is
// precisely the login limiter's per-IP map under a flood from rotating source
// addresses, where every failure can mint a key that is never looked up twice.
//
// Growth is bounded even without this: a new key costs a failure, a failure costs
// an argon2 derivation, and derivations are capped at kdfConcurrency (password.go),
// so a window can accumulate on the order of tens of thousands of entries — a few
// MB, not a leak. The sweep exists so that steady state after an attack returns to
// nothing rather than staying at the high-water mark.
//
// It runs only once the map has doubled since the last sweep. Sweeping is O(n), so
// tying it to growth keeps it amortized O(1) per charge; a flat threshold would
// re-scan the whole map on every new key once the map sat above it, which is the
// pathological case exactly when the server is already under load.
//
// The caller must hold l.mu.
func (l *failureLimiter) sweep(now time.Time) {
	if len(l.fails) < l.sweepAt {
		return
	}
	for k, w := range l.fails {
		if now.Sub(w.windowStart) >= l.window {
			delete(l.fails, k)
		}
	}
	l.sweepAt = max(2*len(l.fails), failureLimiterMinSweep)
}
