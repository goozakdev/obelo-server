package auth_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
)

// The per-source limiters (login, device start, link redeem) count an IPv6 client by
// its /64, as the redirect-start limiter does: one client is handed a whole /64 and
// can rotate through it, so counting the full address gives it a fresh budget per
// address. Each test varies the address inside one /64 and expects ONE shared budget.

// TestLoginIPLimitCountsAnIPv6SubnetOnce: every attempt is a different username (so
// the per-username counter cannot trip) from a different address in one /64.
func TestLoginIPLimitCountsAnIPv6SubnetOnce(t *testing.T) {
	svc, _, _ := newFixture(t)

	err := burnUntilThrottled(t, svc, func(i int) (string, string) {
		return fmt.Sprintf("nobody-%d", i), fmt.Sprintf("2001:db8:0:1::%x", i+1)
	})
	if !errors.Is(err, auth.ErrTooManyLoginAttempts) {
		t.Fatalf("err = %v, want ErrTooManyLoginAttempts", err)
	}
}

// TestDeviceStartQuotaCountsAnIPv6SubnetOnce: starts from different addresses in one
// /64 spend one budget.
func TestDeviceStartQuotaCountsAnIPv6SubnetOnce(t *testing.T) {
	svc, _, _ := newFixture(t)
	for i := 1; i <= startQuotaProbe; i++ {
		_, err := svc.StartDeviceAuth(tvDevice(), fmt.Sprintf("2001:db8:0:1::%x", i))
		if errors.Is(err, auth.ErrDeviceAuthThrottled) {
			return
		}
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	t.Fatalf("%d starts from one /64 were never throttled", startQuotaProbe)
}

// TestRedeemThrottleCountsAnIPv6SubnetOnce: wrong codes from different addresses in
// one /64 spend one budget.
func TestRedeemThrottleCountsAnIPv6SubnetOnce(t *testing.T) {
	svc, _, _ := newFixture(t)
	for i := 1; i <= 64; i++ {
		_, err := svc.RedeemLinkInvite("wrong-code", homeServerID, homeServerName, fmt.Sprintf("2001:db8:0:1::%x", i))
		if errors.Is(err, auth.ErrLinkRedeemThrottled) {
			return
		}
		if !errors.Is(err, auth.ErrInvalidInvite) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidInvite", i, err)
		}
	}
	t.Fatal("64 wrong codes from one /64 were never throttled")
}
