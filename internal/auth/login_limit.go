package auth

import (
	"context"
	"errors"
	"time"
)

// The brute-force limit on password login.
//
// POST /auth/login is the one unauthenticated endpoint that will verify a
// password for anybody who asks, which makes it two things at once: a guessing
// oracle, and — because every verify allocates 64 MiB of argon2 (password.go) — a
// memory amplifier. The KDF semaphore bounds the memory. This bounds the
// guessing, and by bounding the guessing it also bounds how deep the KDF queue
// can ever get, which is what makes waiting for a slot an acceptable thing to do.
//
// Two counters, checked independently, and an attempt is refused if EITHER is
// over its limit:
//
//   - by username, because an IP-only counter lets a botnet spread one password
//     list across a thousand hosts and never trip anything;
//   - by client IP, because a username-only counter lets one host walk the entire
//     user list, a guess per name, forever.
//
// Neither alone is a control. Together they are.

const (
	// loginFailureWindow is how long a tripped counter stays tripped. Long enough
	// that a refused attacker's effective guess rate collapses (15 guesses per
	// quarter hour is not a password search), short enough that a human who
	// genuinely fumbled their password waits out an annoyance rather than filing a
	// support ticket with themselves.
	loginFailureWindow = 15 * time.Minute

	// loginIPFailureLimit is the per-source-address allowance, and the tighter of
	// the two on purpose — see loginUserFailureLimit for why the ordering matters.
	// Fifteen wrong passwords in a quarter hour from ONE device is already well
	// past "the remote's keyboard is awful"; a household's devices each have their
	// own LAN address, so one member fumbling does not spend anybody else's budget.
	//
	// Behind a reverse proxy this degrades: see clientIP in the api package. Every
	// request appears to come from the proxy, so this becomes a single global
	// counter — stricter than intended rather than looser, which is the safe
	// direction to fail.
	loginIPFailureLimit = 15

	// loginUserFailureLimit is the per-username allowance, and it is deliberately
	// the LOOSER of the two, because this is the counter an attacker can turn
	// around and point at a real person: nothing stops them from spending 30 wrong
	// guesses on "brandon" purely to lock brandon out for the next quarter hour.
	//
	// That tradeoff is accepted. This is a household server with a handful of
	// Users, and the alternative — no username counter at all — hands a distributed
	// attacker unlimited guesses against one account, which is worse than a
	// nuisance. Two things keep it a nuisance rather than an outage: the per-IP
	// limit is LOWER, so a single-host attacker locks themselves out at 15 and
	// never reaches 30 (deliberately locking someone out costs two source
	// addresses, spending 15 failures each), and the window is minutes — there is
	// no admin unlock to wait on and no state anyone has to clear. If this number
	// ever needs changing, raise it. Lowering it makes the lockout cheaper to mount.
	loginUserFailureLimit = 30
)

// ErrTooManyLoginAttempts is returned by Login when either failure counter is
// over its limit. The api layer maps it to 429 with the TOO_MANY_ATTEMPTS
// envelope code and a Retry-After header.
//
// It is returned BEFORE any credential check and is identical for a known and an
// unknown username. That is load-bearing: an attacker who could tell the two
// refusals apart would hold a username oracle that costs no password guesses at
// all — the cheapest possible version of what they came for. It is also why the
// refusal cannot be made friendlier ("no such user, so nothing to lock") without
// giving the whole thing away.
var ErrTooManyLoginAttempts = errors.New("auth: too many failed login attempts")

// LoginThrottledError is what Login actually returns when it refuses. It carries
// what is left of the window so the api layer can answer with Retry-After; a
// caller that does not care matches errors.Is(err, ErrTooManyLoginAttempts) and
// never has to know this type exists.
type LoginThrottledError struct {
	// RetryAfter is the time remaining on the tripped window. When both counters
	// are over, it is the longer of the two, so a client that honors it comes back
	// once rather than being refused a second time for the other counter.
	RetryAfter time.Duration
}

func (e *LoginThrottledError) Error() string { return ErrTooManyLoginAttempts.Error() }

// Unwrap makes errors.Is(err, ErrTooManyLoginAttempts) true, so handlers switch
// on the sentinel like every other auth error and the carried duration is purely
// additive.
func (e *LoginThrottledError) Unwrap() error { return ErrTooManyLoginAttempts }

// refuseLogin reports whether this attempt must be turned away before any
// credential work happens, returning the error to hand back if so.
//
// Both counters are consulted unconditionally, even when the first already says
// no. Short-circuiting would make the refusal's cost depend on which counter
// tripped, and a timing difference is exactly the kind of side channel this
// refusal is otherwise careful not to have.
func (s *Service) refuseLogin(username, clientIP string) error {
	now := s.now()
	userOK, userRetry := s.loginUserFails.allow(username, now)
	ipOK, ipRetry := s.loginIPFails.allow(clientIP, now)
	if userOK && ipOK {
		return nil
	}
	return &LoginThrottledError{RetryAfter: max(userRetry, ipRetry)}
}

// kdfAbandoned reports whether err is the KDF meter (password.go) saying the
// caller went away before their derivation ever started — acquireKDF is the only
// thing on these paths that returns a bare context error, since the verifiers
// themselves answer with ErrPasswordMismatch or a parse complaint.
//
// Such an attempt is NOT charged as a failure: no password was ever checked, so
// there is nothing to count. Nor is that a way to guess for free — a caller who
// hangs up before the derivation runs never learns whether the password was
// right, which is the entire thing the counter is protecting.
func kdfAbandoned(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// chargeLoginFailure records one failed attempt against both counters.
//
// The username key is the string the caller submitted, verbatim, because that is
// what the user lookup matches on (the users.username column is compared with a
// case-sensitive =). Varying the case to get a fresh bucket therefore also means
// guessing at an account that does not exist, which is not a bypass — it is the
// attacker paying the per-IP counter for nothing.
//
// An empty clientIP means the api layer could not parse a host out of
// RemoteAddr; every such request shares one bucket, which is stricter than
// keying them apart and never looser.
func (s *Service) chargeLoginFailure(username, clientIP string) {
	now := s.now()
	s.loginUserFails.charge(username, now)
	s.loginIPFails.charge(clientIP, now)
}
