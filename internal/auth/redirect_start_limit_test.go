package auth_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
)

// TestRedirectStartsAreLimitedPerAddress: POST /auth/redirect/start runs the
// plugin's AuthorizeURL on every call and holds a slot in the table of sign-ins
// in flight, so it is limited per source address like login. One address is
// refused, with how long is left of the window, only after more starts than
// the sixteen one client may have in flight — a start at that cap gives up the
// client's oldest rather than being refused, and the limit must not take that
// away — while another address is untouched, and the window reopens.
func TestRedirectStartsAreLimitedPerAddress(t *testing.T) {
	svc, clock, _ := newFixture(t)

	var allowed int
	var refusal error
	for i := 0; i < 1000; i++ {
		if refusal = svc.ChargeRedirectStart("203.0.113.7"); refusal != nil {
			break
		}
		allowed++
	}
	if !errors.Is(refusal, auth.ErrTooManyRedirectStarts) {
		t.Fatalf("start %d from one address = %v, want ErrTooManyRedirectStarts", allowed+1, refusal)
	}
	if allowed <= 16 {
		t.Errorf("one address was allowed %d starts, want more than the 16 one client may have in flight", allowed)
	}
	var throttled *auth.RedirectStartThrottledError
	if !errors.As(refusal, &throttled) || throttled.RetryAfter <= 0 {
		t.Fatalf("refusal %v carries no retry-after; the 429 would have nothing to say", refusal)
	}

	if err := svc.ChargeRedirectStart("198.51.100.4"); err != nil {
		t.Fatalf("a start from another address = %v, want it allowed", err)
	}

	clock.advance(throttled.RetryAfter)
	if err := svc.ChargeRedirectStart("203.0.113.7"); err != nil {
		t.Fatalf("a start once the window reopened = %v, want it allowed", err)
	}
}

// TestRedirectStartsFromOneIPv6SubnetShareOneBudget: an IPv6 client is handed
// a /64 and may start from any address in it, so the limit counts the /64 — a
// client rotating its address within it is refused past the one budget, while
// an address in another /64 is untouched.
func TestRedirectStartsFromOneIPv6SubnetShareOneBudget(t *testing.T) {
	svc, _, _ := newFixture(t)

	var allowed int
	var refusal error
	for i := 1; i <= 1000; i++ {
		if refusal = svc.ChargeRedirectStart(fmt.Sprintf("2001:db8:0:1::%x", i)); refusal != nil {
			break
		}
		allowed++
	}
	if !errors.Is(refusal, auth.ErrTooManyRedirectStarts) {
		t.Fatalf("%d starts from different addresses in one /64 were all allowed; want them refused past one address's limit", allowed)
	}
	if allowed != 30 {
		t.Errorf("one /64 was allowed %d starts, want 30", allowed)
	}
	if err := svc.ChargeRedirectStart("2001:db8:0:2::1"); err != nil {
		t.Fatalf("a start from another /64 = %v, want it allowed", err)
	}
}

// TestRedirectStartsFromTooManySourcesAreRefusedForNewOnes: a client rotating
// across addresses — IPv4 addresses or IPv6 /64s — mints one entry in the
// limiter per source, so the limiter holds at most a fixed number of sources
// per window. Past it, a start from a source it does not hold is refused, with
// a Retry-After, rather than growing the table; a source it holds keeps its own
// budget, and a new source is admitted again once the window has run out.
func TestRedirectStartsFromTooManySourcesAreRefusedForNewOnes(t *testing.T) {
	svc, clock, _ := newFixture(t)

	if err := svc.ChargeRedirectStart("203.0.113.7"); err != nil {
		t.Fatalf("the first start = %v, want it allowed", err)
	}
	var sources int
	var refusal error
	for i := 0; i < 1<<20; i++ {
		source := fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff)
		if refusal = svc.ChargeRedirectStart(source); refusal != nil {
			break
		}
		sources++
	}
	if refusal == nil {
		t.Fatalf("%d starts from different sources were all allowed; the limiter's table has no bound", sources)
	}
	if !errors.Is(refusal, auth.ErrTooManyRedirectStarts) {
		t.Fatalf("the start past the bound = %v, want ErrTooManyRedirectStarts", refusal)
	}
	var throttled *auth.RedirectStartThrottledError
	if !errors.As(refusal, &throttled) || throttled.RetryAfter <= 0 {
		t.Fatalf("refusal %v carries no retry-after; the 429 would have nothing to say", refusal)
	}
	if err := svc.ChargeRedirectStart("198.51.100.4"); !errors.Is(err, auth.ErrTooManyRedirectStarts) {
		t.Fatalf("another new source with the table full = %v, want it refused", err)
	}
	if err := svc.ChargeRedirectStart("203.0.113.7"); err != nil {
		t.Fatalf("a source the limiter already holds = %v, want its own budget to stand", err)
	}

	clock.advance(throttled.RetryAfter)
	if err := svc.ChargeRedirectStart("198.51.100.4"); err != nil {
		t.Fatalf("a new source once the window ran out = %v, want it allowed", err)
	}
}

// TestAnIPv4MappedAddressSharesItsIPv4AddressesBudget: "::ffff:a.b.c.d" is the
// IPv4 address a.b.c.d as a dual-stack listener reports it, so it is counted as
// that address — one client does not get a second budget by the form its
// address arrives in.
func TestAnIPv4MappedAddressSharesItsIPv4AddressesBudget(t *testing.T) {
	svc, _, _ := newFixture(t)

	for i := 0; i < 15; i++ {
		if err := svc.ChargeRedirectStart("203.0.113.7"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err := svc.ChargeRedirectStart("::ffff:203.0.113.7"); err != nil {
			t.Fatalf("mapped start %d: %v", i, err)
		}
	}
	for _, addr := range []string{"203.0.113.7", "::ffff:203.0.113.7"} {
		if err := svc.ChargeRedirectStart(addr); !errors.Is(err, auth.ErrTooManyRedirectStarts) {
			t.Fatalf("a start from %s after 30 from the address in both forms = %v, want ErrTooManyRedirectStarts", addr, err)
		}
	}
	if err := svc.ChargeRedirectStart("::ffff:203.0.113.8"); err != nil {
		t.Fatalf("a start from another mapped address = %v, want it allowed", err)
	}
}

// TestTheRedirectStartLimitHoldsExactly4096Sources: the bound on how many
// sources the limit remembers is 4096 — the 4096th new source is admitted and
// the 4097th is not.
func TestTheRedirectStartLimitHoldsExactly4096Sources(t *testing.T) {
	svc, _, _ := newFixture(t)

	var sources int
	for i := 0; i < 1<<16; i++ {
		if err := svc.ChargeRedirectStart(fmt.Sprintf("2001:db8:%x::1", i)); err != nil {
			break
		}
		sources++
	}
	if sources != 4096 {
		t.Fatalf("the limit admitted %d sources, want exactly 4096", sources)
	}
}

// TestAFullRedirectStartLimitRetriesAfterTheOldestWindowEnds: with the table
// full, a new source is told to come back when the OLDEST window it holds runs
// out — the first moment a source can be let go — not a whole window from now.
func TestAFullRedirectStartLimitRetriesAfterTheOldestWindowEnds(t *testing.T) {
	svc, clock, _ := newFixture(t)

	if err := svc.ChargeRedirectStart("198.51.100.1"); err != nil {
		t.Fatalf("the oldest source's start = %v", err)
	}
	clock.advance(5 * time.Minute)
	for i := 1; i < 4096; i++ {
		if err := svc.ChargeRedirectStart(fmt.Sprintf("2001:db8:%x::1", i)); err != nil {
			t.Fatalf("source %d = %v, want the table to fill to 4096", i, err)
		}
	}
	err := svc.ChargeRedirectStart("198.51.100.2")
	var throttled *auth.RedirectStartThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("a new source with the table full = %v, want it refused", err)
	}
	if throttled.RetryAfter != 10*time.Minute {
		t.Fatalf("Retry-After = %v, want 10m0s: the oldest window, opened 5m before the rest, ends then", throttled.RetryAfter)
	}

	clock.advance(throttled.RetryAfter)
	if err := svc.ChargeRedirectStart("198.51.100.2"); err != nil {
		t.Fatalf("a new source when the oldest window ran out = %v, want it allowed", err)
	}
	if err := svc.ChargeRedirectStart("198.51.100.3"); !errors.Is(err, auth.ErrTooManyRedirectStarts) {
		t.Fatalf("a second new source with only the oldest window run out = %v, want it refused", err)
	}
}
