package auth_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for the Device authorization grant (ADR-0036).
//
// These live here rather than in the api package's harness tests because every
// interesting rule in this feature is a rule about TIME — codes expire, polls
// are paced, rate-limit windows open — and the harness boots a real app whose
// clock cannot be moved. auth.WithClock exists for exactly this: the alternative
// is a test that sleeps five real minutes to watch a code expire, which is to
// say an expiry that is never tested at all.

// fakeClock is a hand-wound clock. No mutex: each test drives it from one
// goroutine.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newFixture builds a real store on a temp DB plus an auth service on a fake
// clock, seeded with one Admin. The store is real because the flow's correctness
// lives in SQL — the one-shot CAS and the expiry comparison are both WHERE
// clauses, and a fake store would assert the mock rather than the rule.
func newFixture(t *testing.T) (*auth.Service, *fakeClock, store.User) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clock := &fakeClock{t: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, err := auth.NewService(db, auth.WithClock(clock.now))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	user, err := svc.Setup(context.Background(), svc.ClaimToken(), "admin", "correct-horse-battery")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return svc, clock, user
}

func tvDevice() auth.DeviceInput {
	return auth.DeviceInput{Name: "Living Room TV", Platform: "tvos", ClientID: "tv-client-1"}
}

// Source addresses for the per-source start quota. Drawn from the documentation
// ranges (RFC 5737) so nothing here can be mistaken for a real host, and kept
// apart from each other so "a different source is unaffected" is a claim these
// tests can actually make.
const (
	tvSourceIP    = "192.0.2.10"
	otherSourceIP = "198.51.100.7"
)

// TestDeviceAuthHappyPath walks the whole grant: the TV starts, polls once and
// is told to wait, the phone approves, the TV polls again and gets a session.
func TestDeviceAuthHappyPath(t *testing.T) {
	svc, clock, admin := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(start.UserCode) != 4 {
		t.Errorf("user code %q, want 4 characters", start.UserCode)
	}
	if start.DeviceCode == start.UserCode || len(start.DeviceCode) < 32 {
		t.Errorf("device code %q must be the long secret, not the human code", start.DeviceCode)
	}

	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodePending) {
		t.Fatalf("poll before approval = %v, want ErrDeviceCodePending", err)
	}

	approved, err := svc.ApproveDeviceCode(start.UserCode, admin.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The approve response names the TV so the phone can show what it signed in.
	if approved.DeviceName != "Living Room TV" || approved.DevicePlatform != "tvos" {
		t.Errorf("approved device = %q/%q, want Living Room TV/tvos",
			approved.DeviceName, approved.DevicePlatform)
	}

	clock.advance(deviceAuthPollGap)
	res, err := svc.RedeemDeviceCode(start.DeviceCode)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if res.Token == "" {
		t.Error("redeem returned an empty token")
	}
	if res.User.ID != admin.ID {
		t.Errorf("session user = %q, want the approving user %q", res.User.ID, admin.ID)
	}
	// The Device is minted from what the TV declared at START, not from anything
	// the poll said — the poll carries only the device code.
	if res.Device.ClientID != "tv-client-1" || res.Device.Name != "Living Room TV" {
		t.Errorf("device = %q/%q, want tv-client-1/Living Room TV",
			res.Device.ClientID, res.Device.Name)
	}

	// The minted token is a real, usable session.
	id, err := svc.Authenticate(res.Token)
	if err != nil {
		t.Fatalf("authenticate with device-granted token: %v", err)
	}
	if id.User.ID != admin.ID {
		t.Errorf("token resolves to %q, want %q", id.User.ID, admin.ID)
	}
}

// TestDeviceCodeIsOneShot is the anti-replay guarantee. A redeemed code must
// never mint a second session, or a device code recovered from a log or a proxy
// would be a permanent key to the account.
func TestDeviceCodeIsOneShot(t *testing.T) {
	svc, clock, admin := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.ApproveDeviceCode(start.UserCode, admin.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); err != nil {
		t.Fatalf("first redeem: %v", err)
	}

	clock.advance(deviceAuthPollGap)
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodeUnknown) {
		t.Errorf("second redeem = %v, want ErrDeviceCodeUnknown", err)
	}
}

// TestDeviceCodeExpires covers the reason WithClock exists.
func TestDeviceCodeExpires(t *testing.T) {
	svc, clock, admin := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	clock.advance(start.ExpiresIn + time.Second)

	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodeExpired) {
		t.Errorf("poll after expiry = %v, want ErrDeviceCodeExpired", err)
	}
	// And an expired code cannot be approved into life again.
	if _, err := svc.ApproveDeviceCode(start.UserCode, admin.ID); !errors.Is(err, auth.ErrUserCodeUnknown) {
		t.Errorf("approve after expiry = %v, want ErrUserCodeUnknown", err)
	}
}

// TestExpiryComparesAcrossTheDatetimeFormatBoundary pins the trap the migration
// comment documents. SQLite's datetime('now') writes "2026-07-15 12:00:00" while
// this table writes RFC3339 "2026-07-15T12:00:00Z", and 'T' sorts after ' ' — so
// a row written in the wrong format compares as unexpired FOREVER, and every
// code in the system would be immortal. A same-day expiry is exactly where the
// two formats disagree, so this is the case that catches it.
func TestExpiryComparesAcrossTheDatetimeFormatBoundary(t *testing.T) {
	svc, clock, _ := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Move to later the SAME day: string-comparing the two formats would put the
	// RFC3339 expires_at above any same-day datetime('now') and read as live.
	clock.advance(6 * time.Hour)
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodeExpired) {
		t.Errorf("poll 6h later = %v, want ErrDeviceCodeExpired "+
			"(a timestamp format mix would report it live)", err)
	}
}

// TestDeviceAuthSlowDown covers the RFC 8628 pacing rule.
func TestDeviceAuthSlowDown(t *testing.T) {
	svc, _, _ := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// First poll: pending, and it records the poll time.
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodePending) {
		t.Fatalf("first poll = %v, want ErrDeviceCodePending", err)
	}
	// Second poll with the clock unmoved is instantaneous — too fast by definition.
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodeSlowDown) {
		t.Errorf("immediate second poll = %v, want ErrDeviceCodeSlowDown", err)
	}
}

// TestExpiryOutranksSlowDown pins the ordering. A client hammering a dead code
// must be told the code is dead; telling it to poll the same dead code more
// slowly is an instruction that never terminates.
func TestExpiryOutranksSlowDown(t *testing.T) {
	svc, clock, _ := newFixture(t)

	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodePending) {
		t.Fatalf("first poll = %v, want ErrDeviceCodePending", err)
	}
	clock.advance(start.ExpiresIn + time.Second)

	// Polling instantly after the last poll AND after expiry: both rules fire.
	if _, err := svc.RedeemDeviceCode(start.DeviceCode); !errors.Is(err, auth.ErrDeviceCodeExpired) {
		t.Errorf("expired + too-fast poll = %v, want ErrDeviceCodeExpired", err)
	}
}

// TestApproveRateLimit is the control that makes a 4-character code defensible.
// Without it, 810k combinations is a weekend of guessing.
func TestApproveRateLimit(t *testing.T) {
	svc, clock, admin := newFixture(t)

	// Burn the allowance on wrong codes. "ZZZZ" is well-formed but (almost
	// certainly) not live — and if it ever were, the test would still be asserting
	// the limiter, which counts failures either way.
	var lastErr error
	for i := 0; i < 32; i++ {
		_, lastErr = svc.ApproveDeviceCode("ZZZZ", admin.ID)
		if errors.Is(lastErr, auth.ErrTooManyAttempts) {
			break
		}
	}
	if !errors.Is(lastErr, auth.ErrTooManyAttempts) {
		t.Fatalf("after 32 wrong codes err = %v, want ErrTooManyAttempts", lastErr)
	}

	// While limited, even the RIGHT code is refused — otherwise the limiter would
	// be a speed bump rather than a limit.
	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.ApproveDeviceCode(start.UserCode, admin.ID); !errors.Is(err, auth.ErrTooManyAttempts) {
		t.Errorf("correct code while limited = %v, want ErrTooManyAttempts", err)
	}

	// Once the window reopens, approving works again. It needs a FRESH code: the
	// rate-limit window (5m) and the code TTL (5m) are the same length, so any
	// wait long enough to clear the limiter also outlives the code the caller was
	// holding. That is not a conflict to fix — a user who has just been rate
	// limited for a minutes-long window has long since lost the 5-minute code on
	// the TV screen, and starts over there anyway.
	clock.advance(approveWindowGap)
	fresh, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start after the window reopened: %v", err)
	}
	if _, err := svc.ApproveDeviceCode(fresh.UserCode, admin.ID); err != nil {
		t.Errorf("correct code after the window reopened: %v", err)
	}
}

// TestApproveRateLimitCountsOnlyFailures: a household approving real codes must
// never trip the limiter, or the control would cost more than it buys.
func TestApproveRateLimitCountsOnlyFailures(t *testing.T) {
	svc, clock, admin := newFixture(t)

	for i := 0; i < 20; i++ {
		start, err := svc.StartDeviceAuth(auth.DeviceInput{
			Name: "TV", Platform: "tvos", ClientID: "tv",
		}, tvSourceIP)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if _, err := svc.ApproveDeviceCode(start.UserCode, admin.ID); err != nil {
			t.Fatalf("approve %d of 20 consecutive VALID codes failed: %v", i, err)
		}
		// Keep the live set from filling up; each approved code is consumed by a
		// redeem so the cap is not what this test measures.
		clock.advance(time.Second)
		if _, err := svc.RedeemDeviceCode(start.DeviceCode); err != nil {
			t.Fatalf("redeem %d: %v", i, err)
		}
	}
}

// TestUserCodeAlphabetExcludesConfusables is a spec test, not an implementation
// test: the whole point of the alphabet is that a human reads it off a TV across
// a room and retypes it correctly. 0/O and 1/I/L are the pairs that fail that.
func TestUserCodeAlphabetExcludesConfusables(t *testing.T) {
	svc, clock, _ := newFixture(t)

	const banned = "01OILU"
	// 200 codes is 800 character draws, so a single confusable slipping into the
	// alphabet is caught with overwhelming probability. Each iteration steps past
	// the TTL so the previous request is swept at the next Start — otherwise this
	// loop hits the live-request cap at 32, which is the cap working correctly and
	// has nothing to say about the alphabet.
	for i := 0; i < 200; i++ {
		clock.advance(deviceAuthTTLGap)
		start, err := svc.StartDeviceAuth(auth.DeviceInput{
			Name: "TV", Platform: "tvos", ClientID: "tv",
		}, tvSourceIP)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if strings.ContainsAny(start.UserCode, banned) {
			t.Fatalf("user code %q contains a confusable from %q", start.UserCode, banned)
		}
		if start.UserCode != strings.ToUpper(start.UserCode) {
			t.Fatalf("user code %q is not uppercase", start.UserCode)
		}
	}
}

// TestNormalizeUserCode covers what a human actually types.
func TestNormalizeUserCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"K7R9", "K7R9"},
		{"k7r9", "K7R9"},   // phones love to lowercase
		{" K7R9 ", "K7R9"}, // trailing space from a paste
		{"K7-R9", "K7R9"},  // people group what they retype
		{"K7 R9", "K7R9"},
		{"K7R", ""},   // too short
		{"K7R9X", ""}, // too long
		{"K0R9", ""},  // 0 is not in the alphabet: misread, unrepairable
		{"K1R9", ""},  // likewise 1
		{"K@R9", ""},  // junk
		{"", ""},
	}
	for _, c := range cases {
		if got := auth.NormalizeUserCode(c.in); got != c.want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- the caps on starting a flow -------------------------------------------
//
// POST /auth/device/code takes no credential, by design (ADR-0036), so these two
// caps are the only things standing between an anonymous caller and every TV in
// the house. They are tested here rather than over HTTP because both are sized in
// the hundreds and one of them is a time window; the api tests cover the wire.

// startUntilRefused starts flows from ip until one is refused, and returns how
// many succeeded first along with the refusal. It gives up at probe attempts so a
// cap that stopped capping fails the test instead of looping forever.
func startUntilRefused(t *testing.T, svc *auth.Service, ip string, probe int) (succeeded int, refusal error) {
	t.Helper()
	for i := 0; i < probe; i++ {
		if _, err := svc.StartDeviceAuth(tvDevice(), ip); err != nil {
			return succeeded, err
		}
		succeeded++
	}
	t.Fatalf("%d starts from %s and none was refused; a cap has stopped capping", probe, ip)
	return 0, nil
}

// TestDeviceStartQuotaRefusesOneNoisySource is the whole point of the quota.
// Before it existed, one unauthenticated caller could hold every slot in the code
// space and lock the household's TVs out indefinitely for the price of 32
// requests every five minutes.
//
// The assertions are about SHAPE, not about the constant (this package exports
// neither), and each one is a separate property:
//
//   - the refusal arrives at all, so one address cannot own the pool;
//   - it is a throttle rather than "the server is busy", so a client learns that
//     backing off is what helps and retrying is what does not;
//   - it carries a usable Retry-After, so the client need not guess;
//   - and it arrives only after MORE than the old global cap of 32. That last one
//     is the reverse-proxy guard: behind a proxy every request in the world keys
//     to the same address, so this quota becomes the global cap, and a quota below
//     32 would ship a regression as a security fix.
func TestDeviceStartQuotaRefusesOneNoisySource(t *testing.T) {
	svc, _, _ := newFixture(t)

	succeeded, refusal := startUntilRefused(t, svc, tvSourceIP, startQuotaProbe)

	if !errors.Is(refusal, auth.ErrDeviceAuthThrottled) {
		t.Fatalf("start %d from one address = %v, want ErrDeviceAuthThrottled "+
			"(ErrDeviceAuthBusy would mean one source can still exhaust the whole pool)",
			succeeded+1, refusal)
	}
	if succeeded <= oldGlobalLiveCap {
		t.Errorf("one address got %d codes before being refused, want more than the %d "+
			"the entire server used to allow — behind a reverse proxy this quota IS the "+
			"global cap, so anything at or below that makes proxied deployments worse",
			succeeded, oldGlobalLiveCap)
	}

	var throttled *auth.DeviceAuthThrottledError
	if !errors.As(refusal, &throttled) {
		t.Fatalf("refusal %v carries no retry-after; the 429 would have nothing to say", refusal)
	}
	if throttled.RetryAfter <= 0 || throttled.RetryAfter > deviceAuthTTLGap {
		t.Errorf("retry-after = %v, want a positive duration inside the window", throttled.RetryAfter)
	}
}

// TestDeviceStartQuotaSpendsOnlyTheOffendersBudget: the quota has to be a
// per-source bound and not a global one in disguise, or it would be the busy cap
// again with a different error code. A household TV on its own LAN address must
// be untouched by whatever the noisy address did.
func TestDeviceStartQuotaSpendsOnlyTheOffendersBudget(t *testing.T) {
	svc, _, _ := newFixture(t)

	if _, refusal := startUntilRefused(t, svc, tvSourceIP, startQuotaProbe); !errors.Is(refusal, auth.ErrDeviceAuthThrottled) {
		t.Fatalf("exhausting the noisy source = %v, want ErrDeviceAuthThrottled", refusal)
	}
	if _, err := svc.StartDeviceAuth(tvDevice(), otherSourceIP); err != nil {
		t.Fatalf("start from a second address after the first was throttled: %v — "+
			"one caller must not be able to spend everyone else's budget", err)
	}
}

// TestDeviceStartQuotaWindowReopens: the refusal is a delay, not a ban. Nothing
// clears this state but time — there is no admin unlock and no restart to wait
// on — so a window that never reopened would be the outage it exists to prevent.
func TestDeviceStartQuotaWindowReopens(t *testing.T) {
	svc, clock, _ := newFixture(t)

	if _, refusal := startUntilRefused(t, svc, tvSourceIP, startQuotaProbe); !errors.Is(refusal, auth.ErrDeviceAuthThrottled) {
		t.Fatalf("exhausting the source = %v, want ErrDeviceAuthThrottled", refusal)
	}

	// One step past the window. It also outlives every code just minted, which is
	// not a coincidence to fix: the quota window is the code TTL, so a caller who
	// waits out the quota has necessarily lost the codes it was holding anyway.
	clock.advance(deviceAuthTTLGap)
	if _, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP); err != nil {
		t.Fatalf("start after the window reopened: %v", err)
	}
}

// TestDeviceAuthBusyStillRefusesWhenTheSpaceIsFull pins the OTHER cap, which the
// quota is layered on top of rather than replacing. Filling it now takes a spread
// of source addresses — that is the quota working — and what must still happen at
// the end is a 503-shaped refusal, not a throttle: the caller is within its own
// budget and the server genuinely has nothing to give it.
func TestDeviceAuthBusyStillRefusesWhenTheSpaceIsFull(t *testing.T) {
	svc, _, _ := newFixture(t)

	// The clock never moves, so nothing expires and every successful start stays
	// live. Sources rotate every startsPerFloodSource so no single one is throttled
	// before the pool fills.
	var succeeded int
	var refusal error
	for i := 0; i < globalCapProbe; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i/startsPerFloodSource)
		if _, err := svc.StartDeviceAuth(tvDevice(), ip); err != nil {
			refusal = err
			break
		}
		succeeded++
	}
	if !errors.Is(refusal, auth.ErrDeviceAuthBusy) {
		t.Fatalf("after %d starts across many addresses err = %v, want ErrDeviceAuthBusy", succeeded, refusal)
	}
	if succeeded <= oldGlobalLiveCap {
		t.Errorf("the code space filled after %d live codes, want more than the old %d — "+
			"the cap may be raised but lowering it makes the lockout cheaper", succeeded, oldGlobalLiveCap)
	}

	// A source that has never been seen is refused too, which is what makes this
	// the GLOBAL cap rather than the per-source quota answering under another name.
	if _, err := svc.StartDeviceAuth(tvDevice(), otherSourceIP); !errors.Is(err, auth.ErrDeviceAuthBusy) {
		t.Errorf("start from an unseen address with the space full = %v, want ErrDeviceAuthBusy", err)
	}
}

// Test-local time steps, named for what they are past rather than for their
// value — these must stay comfortably beyond the service's own constants, which
// are unexported, so the tests state the intent instead of restating the number.
const (
	// deviceAuthPollGap clears the poll pacing rule, so a poll is answered rather
	// than slowed down.
	deviceAuthPollGap = 5 * time.Second
	// deviceAuthTTLGap outlives a code, so the next Start sweeps it.
	deviceAuthTTLGap = 6 * time.Minute
	// approveWindowGap outlives the approve rate-limit window.
	approveWindowGap = 6 * time.Minute
)

// Bounds for the cap tests. Same rule as the time steps above: comfortably past
// the service's own numbers without restating them, so raising a cap does not
// silently turn one of these loops into an infinite one or a false pass.
const (
	// oldGlobalLiveCap is what maxLiveDeviceAuthRequests was before the per-source
	// quota existed, and the floor both caps are held above. It is a historical
	// fact rather than a current constant, which is why it is safe to write down:
	// it is the cost of the lockout this work was fixing, and neither cap may ever
	// make that cheaper again.
	oldGlobalLiveCap = 32
	// startQuotaProbe bounds the per-source loops.
	startQuotaProbe = 512
	// globalCapProbe bounds the fill-the-space loop.
	globalCapProbe = 4096
	// startsPerFloodSource is how many starts that loop takes from one address
	// before rotating. Well under the per-source quota, so the global cap is what
	// it measures.
	startsPerFloodSource = 32
)
