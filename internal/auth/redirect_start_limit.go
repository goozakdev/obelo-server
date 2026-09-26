package auth

import (
	"errors"
	"net/netip"
	"time"
)

// The per-address limit on starting a redirect sign-in (ADR-0063 decision 2).
//
// POST /auth/redirect/start is unauthenticated, like password login, and every
// call runs the Sign-in provider's AuthorizeURL and takes a slot in the table of
// sign-ins in flight (internal/signin). So it is limited the way login is, by
// the client address — and for the reason login is, behind a reverse proxy the
// operator has not named in OBELO_TRUSTED_PROXIES every browser shares one
// address and one budget.
//
// It counts every start, not failures: a start has no outcome to judge until
// the browser comes back, and the work a start costs is spent either way.
const (
	// redirectStartWindow is login's window, for the same reasons.
	redirectStartWindow = 15 * time.Minute

	// redirectStartLimit is how many starts one address may make per window. It
	// sits above the sixteen sign-ins one client may have in flight on purpose:
	// a start at that cap gives up the client's oldest rather than being refused,
	// and a limit at or below it would turn that into a lockout.
	redirectStartLimit = 30

	// redirectStartSources is how many sources the limit remembers at once. A
	// start costs no password derivation and succeeds without a cap on how many
	// may, so nothing else bounds how many sources one window can charge: past
	// this, a start from a source the limit does not already hold is refused
	// until a window runs out. Sources it holds keep their own budget, so the
	// people already signing in are not locked out by someone rotating addresses.
	redirectStartSources = 4096
)

// ErrTooManyRedirectStarts is ChargeRedirectStart refusing an address over its
// limit. The api layer maps it to 429 TOO_MANY_ATTEMPTS with a Retry-After.
var ErrTooManyRedirectStarts = errors.New("auth: too many redirect sign-ins started from this address")

// RedirectStartThrottledError is what ChargeRedirectStart returns when it
// refuses, carrying what is left of the window for Retry-After. Same shape as
// LoginThrottledError, and its own type for the reason DeviceAuthThrottledError
// is.
type RedirectStartThrottledError struct {
	RetryAfter time.Duration
}

func (e *RedirectStartThrottledError) Error() string { return ErrTooManyRedirectStarts.Error() }

// Unwrap makes errors.Is(err, ErrTooManyRedirectStarts) true.
func (e *RedirectStartThrottledError) Unwrap() error { return ErrTooManyRedirectStarts }

func newRedirectStartLimiter() *fixedWindowLimiter {
	l := newFixedWindowLimiter(redirectStartLimit, redirectStartWindow)
	l.maxKeys = redirectStartSources
	return l
}

// ChargeRedirectStart counts one redirect sign-in start from clientIP, or
// refuses it — before anything is asked of a provider — when that address is
// over its limit, or is new while the limit holds redirectStartSources others.
func (s *Service) ChargeRedirectStart(clientIP string) error {
	now := s.now()
	key := redirectStartKey(clientIP)
	if ok, retryAfter := s.redirectStarts.allow(key, now); !ok {
		return &RedirectStartThrottledError{RetryAfter: retryAfter}
	}
	s.redirectStarts.charge(key, now)
	return nil
}

// redirectStartKey is the source a start is counted against: an IPv4 address
// as it is, and an IPv6 address by its /64, which one client is handed whole and
// may start from any address in — as internal/signin counts sign-ins in flight.
func redirectStartKey(clientIP string) string {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return clientIP
	}
	prefix, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return clientIP
	}
	return prefix.String()
}
