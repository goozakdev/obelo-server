package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for the link invite (ADR-0055 §1, §4).
//
// They live here rather than in the api harness for the reason
// device_auth_test.go records: the interesting rules are rules about TIME —
// invites expire after 24 hours, and the rate-limit window opens — and the
// harness boots a real app whose clock cannot be moved. auth.WithClock exists
// for exactly this; the alternative is a test that sleeps a day, which is to say
// an expiry that is never tested at all.
//
// newFixture, fakeClock and the source-address constants come from
// device_auth_test.go, in this same package.

const (
	// The redeeming Server's identity (ADR-0034), presented as the Device
	// clientId and name (ADR-0055 §4).
	homeServerID   = "3f6a1c2e-0000-4000-8000-abcdefabcdef"
	homeServerName = "Brandon's server"
	// A second household, for the "a different server gets its own Device" case.
	otherServerID   = "9b7c5d4a-1111-4111-9111-fedcbafedcba"
	otherServerName = "Sam's server"
	// The redeeming Server's source address. RFC 5737 documentation range, like
	// the device-grant constants, so nothing here can be mistaken for a real host.
	homeSourceIP = "203.0.113.24"
)

// newRemoteUser mints a `remote` User through the real service — no password,
// which is the role's whole shape (ADR-0054).
func newRemoteUser(t *testing.T, svc *auth.Service, label string) store.User {
	t.Helper()
	u, err := svc.CreateUser(context.Background(), label, "", auth.RoleRemote)
	if err != nil {
		t.Fatalf("create remote user: %v", err)
	}
	return u
}

// TestLinkInviteRedeemsOnce is the happy path and the single-use rule in one:
// the first redemption yields a session, the second yields nothing.
func TestLinkInviteRedeemsOnce(t *testing.T) {
	svc, _, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")

	invite, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if invite.Code == "" {
		t.Fatal("mint returned an empty code")
	}

	res, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if res.Token == "" {
		t.Error("redeem returned no token")
	}
	if res.User.ID != peer.ID {
		t.Errorf("redeemed as user %q, want %q", res.User.ID, peer.ID)
	}
	// An ordinary Device-bound bearer: the token authenticates like any other.
	id, err := svc.Authenticate(res.Token)
	if err != nil {
		t.Fatalf("the redeemed token does not authenticate: %v", err)
	}
	if id.User.ID != peer.ID {
		t.Errorf("token resolves to user %q, want %q", id.User.ID, peer.ID)
	}

	// Second attempt with the same code. Spent is spent.
	if _, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("second redeem: err = %v, want ErrInvalidInvite", err)
	}
}

// TestLinkInviteExpires: 24 hours is the TTL (ADR-0055 §1), and one second past
// it the code is dead — with the SAME error a spent one gives, which is the
// property the api layer's single INVALID_INVITE rests on.
func TestLinkInviteExpires(t *testing.T) {
	svc, clock, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")

	invite, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Just inside the window: still good. (Redeeming here would spend it, so this
	// half asserts the boundary from a SECOND invite below instead.)
	clock.advance(24*time.Hour - time.Second)
	if _, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP); err != nil {
		t.Fatalf("redeem one second before expiry: %v", err)
	}

	// A fresh invite, aged past the TTL.
	second, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}
	clock.advance(24*time.Hour + time.Second)
	_, err = svc.RedeemLinkInvite(second.Code, homeServerID, homeServerName, homeSourceIP)
	if !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("redeem after expiry: err = %v, want ErrInvalidInvite", err)
	}
}

// TestMintingInvalidatesTheUnredeemedOne: re-minting is how a lapsed or
// mis-sent invite is replaced (ADR-0055 §1), so the string the Admin sent first
// must stop working the moment they mint again. Two live codes for one User
// would mean the Admin could not take back what they had already sent.
func TestMintingInvalidatesTheUnredeemedOne(t *testing.T) {
	svc, _, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")

	first, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint first: %v", err)
	}
	second, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}
	if first.Code == second.Code {
		t.Fatal("the two mints returned the same code")
	}

	if _, err := svc.RedeemLinkInvite(first.Code, homeServerID, homeServerName, homeSourceIP); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("redeem the superseded invite: err = %v, want ErrInvalidInvite", err)
	}
	if _, err := svc.RedeemLinkInvite(second.Code, homeServerID, homeServerName, homeSourceIP); err != nil {
		t.Errorf("redeem the current invite: %v", err)
	}
}

// TestInviteOnlyForRemoteUsers: an invite is the `remote` role's only
// credential, and no other role has any use for one. Minting for a Member would
// be a second, passwordless way into a person's account.
func TestInviteOnlyForRemoteUsers(t *testing.T) {
	svc, _, admin := newFixture(t)
	member, err := svc.CreateUser(context.Background(), "sam", "correct-horse-battery", auth.RoleMember)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{name: "admin", id: admin.ID},
		{name: "member", id: member.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.MintLinkInvite(tc.id); !errors.Is(err, auth.ErrNotRemoteUser) {
				t.Errorf("mint for a %s: err = %v, want ErrNotRemoteUser", tc.name, err)
			}
		})
	}

	if _, err := svc.MintLinkInvite("no-such-user"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("mint for an unknown user: err = %v, want ErrUserNotFound", err)
	}
}

// TestRedeemUpsertsOneDevicePerServer is ADR-0055 §4: the redeeming Server's own
// id is the Device clientId, so re-linking from the same household updates one
// row rather than leaving a trail of dead ones — and a different household gets
// its own row.
func TestRedeemUpsertsOneDevicePerServer(t *testing.T) {
	svc, _, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")

	redeem := func(serverID, serverName string) store.Device {
		t.Helper()
		invite, err := svc.MintLinkInvite(peer.ID)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		res, err := svc.RedeemLinkInvite(invite.Code, serverID, serverName, homeSourceIP)
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		return res.Device
	}

	first := redeem(homeServerID, homeServerName)
	if first.ClientID != homeServerID {
		t.Errorf("device clientId = %q, want the redeeming server's id %q", first.ClientID, homeServerID)
	}
	if first.Name != homeServerName {
		t.Errorf("device name = %q, want %q", first.Name, homeServerName)
	}
	if first.Platform != auth.LinkDevicePlatform {
		t.Errorf("device platform = %q, want %q", first.Platform, auth.LinkDevicePlatform)
	}

	// A re-key from the same home, with a renamed Server: same row, new name.
	again := redeem(homeServerID, "Brandon's new server")
	if again.ID != first.ID {
		t.Errorf("re-link created device %q, want the existing %q", again.ID, first.ID)
	}
	if again.Name != "Brandon's new server" {
		t.Errorf("re-link left the device named %q", again.Name)
	}

	// A different household is a different Device.
	other := redeem(otherServerID, otherServerName)
	if other.ID == first.ID {
		t.Error("a second server reused the first server's Device row")
	}

	devices, err := svc.Devices(peer.ID)
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("the remote User holds %d Devices, want 2 (one per linked server)", len(devices))
	}
}

// TestRedeemRefusesGarbage: a made-up code, an empty one, and a redemption with
// no server id all answer identically. The last is not a code failure at all,
// but answering it differently would tell a caller its code was good.
func TestRedeemRefusesGarbage(t *testing.T) {
	svc, _, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")
	invite, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for _, tc := range []struct {
		name     string
		code     string
		serverID string
	}{
		{name: "unknown code", code: "not-a-real-invite-code", serverID: homeServerID},
		{name: "empty code", code: "", serverID: homeServerID},
		{name: "no server id", code: invite.Code, serverID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RedeemLinkInvite(tc.code, tc.serverID, homeServerName, homeSourceIP)
			if !errors.Is(err, auth.ErrInvalidInvite) {
				t.Errorf("err = %v, want ErrInvalidInvite", err)
			}
		})
	}

	// None of that spent the real invite.
	if _, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP); err != nil {
		t.Errorf("the invite was consumed by a failed attempt: %v", err)
	}
}

// TestRedeemThrottlesOneAddress: the endpoint is unauthenticated by necessity,
// so failures are counted per source address. A different address is unaffected,
// and the window reopens.
func TestRedeemThrottlesOneAddress(t *testing.T) {
	svc, clock, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")
	invite, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Fifteen failures is the allowance; the sixteenth is refused.
	var throttled error
	for i := 0; i < 32; i++ {
		_, err := svc.RedeemLinkInvite("wrong-code", homeServerID, homeServerName, homeSourceIP)
		if errors.Is(err, auth.ErrLinkRedeemThrottled) {
			throttled = err
			break
		}
		if !errors.Is(err, auth.ErrInvalidInvite) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidInvite", i, err)
		}
	}
	if throttled == nil {
		t.Fatal("32 wrong codes from one address were never throttled")
	}
	var typed *auth.LinkRedeemThrottledError
	if !errors.As(throttled, &typed) || typed.RetryAfter <= 0 {
		t.Errorf("throttled error carries no usable RetryAfter: %#v", throttled)
	}

	// A GOOD code from the throttled address is refused too — the limiter runs
	// before the code is looked at, which is what stops it being an oracle.
	if _, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP); !errors.Is(err, auth.ErrLinkRedeemThrottled) {
		t.Errorf("a good code from a throttled address: err = %v, want throttled", err)
	}

	// Another household is unaffected...
	if _, err := svc.RedeemLinkInvite(invite.Code, otherServerID, otherServerName, otherSourceIP); err != nil {
		t.Errorf("a different address was caught by the first one's limit: %v", err)
	}

	// ...and the window reopens.
	clock.advance(16 * time.Minute)
	second, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}
	if _, err := svc.RedeemLinkInvite(second.Code, homeServerID, homeServerName, homeSourceIP); err != nil {
		t.Errorf("after the window reopened: %v", err)
	}
}

// TestDeletingTheRemoteUserKillsItsInvites: deleting the linked Server is the
// sharer's kill switch, and it must take the unspent invite with it — otherwise
// a code in a chat log would outlive the User it was minted for.
func TestDeletingTheRemoteUserKillsItsInvites(t *testing.T) {
	svc, _, _ := newFixture(t)
	peer := newRemoteUser(t, svc, "Brandon's server")
	invite, err := svc.MintLinkInvite(peer.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := svc.DeleteUser(peer.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.RedeemLinkInvite(invite.Code, homeServerID, homeServerName, homeSourceIP); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("redeem after the User was deleted: err = %v, want ErrInvalidInvite", err)
	}
}
