package auth

import (
	"sync"
	"time"
)

// fixedWindowLimiter is the fixed-window event counter this package uses
// wherever an endpoint has to survive somebody pointing a script at it. Four
// counters share it today: the device-code approve endpoint and password login
// (twice) count FAILURES, and device-code start (device_auth.go) counts
// successful STARTS.
//
// It was called failureLimiter while every user counted failures. The device-code
// start limiter is the one that broke that: what has to be bounded there is how
// many codes one source may hold, and a code it successfully obtained is not a
// failure by any reading. The type is named for its mechanism now — a count per
// key per fixed window — and each CALLER names what it is counting. A limiter
// called "failure" that counted successes would be a lie that costs the next
// reader an hour.
//
// What a caller charges is therefore the caller's decision, and the two shapes
// answer different questions. Charging failures makes the control free for people
// who type the right thing: a household never meets it. Charging successes makes
// it a quota, which is the right shape when the resource itself is scarce and a
// SUCCESSFUL request is what consumes it — see maxDeviceAuthStartsPerSource.
//
// It is per-boot and in memory, like the claim token (ADR-0013) and for the same
// reason: this is safety state, not a record of anything. A restart clears it,
// which is acceptable — restarting the server is not a primitive a remote
// attacker has, and persisting it would buy a table and a sweeper for nothing.
//
// Fixed window, not sliding. An attacker who straddles a window boundary gets
// 2×limit events in one window's width and then stops, which is not a rate worth
// the per-attempt bookkeeping a sliding window costs. Callers that care about the
// straddle — the start quota does, because 2×limit is how many codes one source
// can actually hold at once — say so where they pick their number.
//
// The type carries no clock: callers pass now. That is what keeps the Service's
// WithClock seam meaningful — window expiry is testable by winding a fake clock
// rather than by sleeping out a real five or fifteen minutes, which is to say
// testable at all.
type fixedWindowLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	counts  map[string]*keyWindow
	sweepAt int
}

// keyWindow is one key's event count and the instant its window opened.
type keyWindow struct {
	count       int
	windowStart time.Time
}

// fixedWindowMinSweep is the map size below which expired-entry sweeping is not
// worth doing. See sweep.
const fixedWindowMinSweep = 1024

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		limit:   limit,
		window:  window,
		counts:  map[string]*keyWindow{},
		sweepAt: fixedWindowMinSweep,
	}
}

// allow reports whether key is still under its limit, and when it is not, how
// much of the window is left — which is what a caller turns into Retry-After.
//
// An expired window is dropped here rather than merely ignored, so a key that has
// served its time stops costing anything to remember.
func (l *fixedWindowLimiter) allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, tracked := l.counts[key]
	if !tracked {
		return true, 0
	}
	elapsed := now.Sub(w.windowStart)
	if elapsed >= l.window {
		delete(l.counts, key)
		return true, 0
	}
	if w.count < l.limit {
		return true, 0
	}
	return false, l.window - elapsed
}

// charge records one event against key, opening a fresh window if the key is new
// or its previous window has run out.
func (l *fixedWindowLimiter) charge(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.counts == nil {
		l.counts = map[string]*keyWindow{}
	}
	w, tracked := l.counts[key]
	if !tracked || now.Sub(w.windowStart) >= l.window {
		l.sweep(now)
		l.counts[key] = &keyWindow{count: 1, windowStart: now}
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
// MB, not a leak. The device-code start counter has no KDF behind it, but it does
// have the global live cap (maxLiveDeviceAuthRequests), which bounds how many
// starts can ever succeed in a window and therefore how many keys can ever be
// charged. The sweep exists so that steady state after an attack returns to
// nothing rather than staying at the high-water mark.
//
// It runs only once the map has doubled since the last sweep. Sweeping is O(n), so
// tying it to growth keeps it amortized O(1) per charge; a flat threshold would
// re-scan the whole map on every new key once the map sat above it, which is the
// pathological case exactly when the server is already under load.
//
// The caller must hold l.mu.
func (l *fixedWindowLimiter) sweep(now time.Time) {
	if len(l.counts) < l.sweepAt {
		return
	}
	for k, w := range l.counts {
		if now.Sub(w.windowStart) >= l.window {
			delete(l.counts, k)
		}
	}
	l.sweepAt = max(2*len(l.counts), fixedWindowMinSweep)
}
