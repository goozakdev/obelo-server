package auth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/auth"
)

// Service-level tests for the login brute-force limiter (login_limit.go).
//
// They live here for the same reason the device-auth ones do: every rule worth
// testing is a rule about TIME or about a counter, and auth.WithClock is what
// makes "the window reopened" a line of code rather than a fifteen-minute sleep.
// They reuse newFixture from device_auth_test.go, which seeds one Admin —
// username "admin", password "correct-horse-battery".
//
// These tests are slower than the rest of the package by construction: every
// failed attempt is a real argon2 derivation, because that is exactly the work
// the limiter exists to stop an attacker from commissioning at will. Burning an
// allowance costs what an attacker would pay for it.

const (
	// loginPassword is the fixture Admin's real password.
	loginPassword = "correct-horse-battery"
	// wrongPassword is never anyone's password.
	wrongPassword = "not-the-password"
	// attackerIP is one source address; victimIP is another. Both are TEST-NET-3
	// (RFC 5737) so they can never be mistaken for something real.
	attackerIP = "203.0.113.9"
	victimIP   = "203.0.113.42"

	// loginWindowGap outlives the login failure window, so a wound-forward clock
	// reopens it. Named for what it is past, not for its value: the service's own
	// window is unexported and this must stay comfortably beyond it.
	loginWindowGap = 16 * time.Minute

	// tripLimit is more failures than either counter permits — a loop bound, not
	// an assertion about the thresholds, which are unexported and free to move.
	// The tests assert that the limiter tripped, never at which count.
	tripLimit = 64
)

func testDevice() auth.DeviceInput {
	return auth.DeviceInput{Name: "Laptop", Platform: "macos", ClientID: "test-client"}
}

// failLogin makes one wrong-password attempt and returns the error.
func failLogin(t *testing.T, svc *auth.Service, username, ip string) error {
	t.Helper()
	_, err := svc.Login(context.Background(), username, wrongPassword, testDevice(), ip)
	return err
}

// burnUntilThrottled makes wrong-password attempts until the limiter refuses,
// varying the IP or the username per attempt via next. It fails the test if the
// limiter never trips, and returns the refusal.
func burnUntilThrottled(t *testing.T, svc *auth.Service, next func(i int) (username, ip string)) error {
	t.Helper()
	for i := 0; i < tripLimit; i++ {
		username, ip := next(i)
		err := failLogin(t, svc, username, ip)
		if errors.Is(err, auth.ErrTooManyLoginAttempts) {
			return err
		}
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i, err)
		}
	}
	t.Fatalf("after %d wrong passwords the limiter never tripped", tripLimit)
	return nil
}

// TestLoginFailuresBelowThresholdStillAllowLogin is the property that keeps this
// control from costing more than it buys: a household member who fumbles their
// password a few times and then gets it right must simply be logged in.
func TestLoginFailuresBelowThresholdStillAllowLogin(t *testing.T) {
	svc, _, _ := newFixture(t)

	// Ten wrong guesses from one address, comfortably under both limits.
	for i := 0; i < 10; i++ {
		if err := failLogin(t, svc, "admin", victimIP); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i, err)
		}
	}
	res, err := svc.Login(context.Background(), "admin", loginPassword, testDevice(), victimIP)
	if err != nil {
		t.Fatalf("correct password after 10 failures: %v", err)
	}
	if res.Token == "" {
		t.Error("login returned an empty token")
	}
}

// TestLoginRateLimitPerIP walks the whole per-IP lifecycle: the counter trips,
// the RIGHT password is refused while it is tripped, and the window reopening
// clears it.
//
// Every attempt uses a DIFFERENT username, so the username counter cannot be what
// trips — this is the per-IP counter on its own, which is the half that stops one
// host from walking the whole user list a guess per name.
func TestLoginRateLimitPerIP(t *testing.T) {
	svc, clock, _ := newFixture(t)

	err := burnUntilThrottled(t, svc, func(i int) (string, string) {
		return fmt.Sprintf("nobody-%d", i), attackerIP
	})

	// The refusal carries a usable Retry-After.
	var throttled *auth.LoginThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("refusal %v is not a *LoginThrottledError, so the api layer has no Retry-After", err)
	}
	if throttled.RetryAfter <= 0 || throttled.RetryAfter > loginWindowGap {
		t.Errorf("RetryAfter = %v, want a positive duration inside the window", throttled.RetryAfter)
	}

	// While limited, even the CORRECT password is refused. Otherwise the limiter
	// would be a speed bump rather than a limit — and note this is a real user's
	// real password being turned away, which is the cost being accepted.
	_, err = svc.Login(context.Background(), "admin", loginPassword, testDevice(), attackerIP)
	if !errors.Is(err, auth.ErrTooManyLoginAttempts) {
		t.Errorf("correct password while limited = %v, want ErrTooManyLoginAttempts", err)
	}

	// A DIFFERENT address is unaffected: the counter is per source, not global.
	if _, err := svc.Login(context.Background(), "admin", loginPassword, testDevice(), victimIP); err != nil {
		t.Errorf("correct password from an unrelated address: %v", err)
	}

	// Once the window reopens, the throttled address works again.
	clock.advance(loginWindowGap)
	if _, err := svc.Login(context.Background(), "admin", loginPassword, testDevice(), attackerIP); err != nil {
		t.Errorf("correct password after the window reopened: %v", err)
	}
}

// TestLoginRateLimitPerUsername is the other half. Every attempt comes from a
// DIFFERENT address — a botnet, which the per-IP counter cannot see — so the only
// thing that can trip is the username counter.
//
// It also pins the ordering the lockout tradeoff rests on: the username limit is
// the looser of the two, so this takes MORE attempts than the per-IP test above,
// which is why a single-host attacker locks themselves out long before they can
// lock anybody else out.
func TestLoginRateLimitPerUsername(t *testing.T) {
	svc, clock, _ := newFixture(t)

	burnUntilThrottled(t, svc, func(i int) (string, string) {
		return "admin", fmt.Sprintf("198.51.100.%d", i+1)
	})

	// The victim is locked out even from an address that has failed nothing.
	_, err := svc.Login(context.Background(), "admin", loginPassword, testDevice(), victimIP)
	if !errors.Is(err, auth.ErrTooManyLoginAttempts) {
		t.Errorf("correct password from a clean address = %v, want ErrTooManyLoginAttempts", err)
	}

	// Another username is untouched — the lockout is one account's, not the
	// server's, which is what keeps it a nuisance rather than an outage.
	if err := failLogin(t, svc, "someone-else", victimIP); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("a different username = %v, want ErrInvalidCredentials", err)
	}

	// And it expires on its own. There is no admin unlock, because there is no
	// state for an admin to clear.
	clock.advance(loginWindowGap)
	if _, err := svc.Login(context.Background(), "admin", loginPassword, testDevice(), victimIP); err != nil {
		t.Errorf("correct password after the window reopened: %v", err)
	}
}

// TestSuccessfulLoginsNeverTrip: only FAILURES are counted. A household of TVs,
// phones and laptops re-logging in all evening from one address must never talk
// its way into a lockout.
func TestSuccessfulLoginsNeverTrip(t *testing.T) {
	svc, _, _ := newFixture(t)

	// Comfortably more successes than either limit allows failures.
	for i := 0; i < 40; i++ {
		dev := auth.DeviceInput{
			Name:     "Laptop",
			Platform: "macos",
			ClientID: fmt.Sprintf("client-%d", i),
		}
		if _, err := svc.Login(context.Background(), "admin", loginPassword, dev, attackerIP); err != nil {
			t.Fatalf("successful login %d of 40: %v", i, err)
		}
	}
}

// TestLoginRefusalIsIdenticalForKnownAndUnknownUsers is the no-leak property.
//
// A refusal that differed between a real username and an invented one would be a
// username oracle costing zero password guesses — cheaper than the brute force the
// limiter is there to stop, and handed over by the mitigation itself. Both must
// come back byte-for-byte the same, including the retry the api layer echoes.
func TestLoginRefusalIsIdenticalForKnownAndUnknownUsers(t *testing.T) {
	svc, _, _ := newFixture(t)

	// Trip the per-IP counter with usernames that are neither of the two probed
	// below, so nothing in the burn distinguishes them either.
	burnUntilThrottled(t, svc, func(i int) (string, string) {
		return fmt.Sprintf("filler-%d", i), attackerIP
	})

	// "admin" exists and this is even its REAL password; "ghost" has never
	// existed. The two answers must be indistinguishable.
	_, knownErr := svc.Login(context.Background(), "admin", loginPassword, testDevice(), attackerIP)
	_, unknownErr := svc.Login(context.Background(), "ghost", wrongPassword, testDevice(), attackerIP)

	if !errors.Is(knownErr, auth.ErrTooManyLoginAttempts) {
		t.Fatalf("known username = %v, want ErrTooManyLoginAttempts", knownErr)
	}
	if !errors.Is(unknownErr, auth.ErrTooManyLoginAttempts) {
		t.Fatalf("unknown username = %v, want ErrTooManyLoginAttempts", unknownErr)
	}
	if knownErr.Error() != unknownErr.Error() {
		t.Errorf("refusal messages differ: known %q vs unknown %q", knownErr, unknownErr)
	}

	var knownThrottled, unknownThrottled *auth.LoginThrottledError
	if !errors.As(knownErr, &knownThrottled) || !errors.As(unknownErr, &unknownThrottled) {
		t.Fatal("one refusal carried no retry and the other did")
	}
	if knownThrottled.RetryAfter != unknownThrottled.RetryAfter {
		t.Errorf("Retry-After differs by username existence: known %v vs unknown %v",
			knownThrottled.RetryAfter, unknownThrottled.RetryAfter)
	}
}
