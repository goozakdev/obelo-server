package auth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
)

// Service-level tests for the re-authentication an attach needs: a session
// alone never attaches an External identity. A User with a Local password puts
// it in the attach itself; a User without one presents a re-auth grant — a
// fresh sign-in through an identity they already hold, bound to their User and
// session, good once and briefly.

// outsideOnly signs ada in through the directory, so she is a Member with no
// Local password who holds dir/s-ada, and answers her User id and session token.
// "other" is a second directory where she is s-ada-2, not yet attached.
func outsideOnly(t *testing.T, svc *auth.Service) (userID, session string) {
	t.Helper()
	svc.UseSignInProviders(providerList{
		directory("dir", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"}),
		directory("other", "ada2", "pw2", auth.ExternalAnswer{Subject: "s-ada-2", Username: "ada2"}),
	})
	res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
	if err != nil {
		t.Fatalf("ada's first sign-in: %v", err)
	}
	return res.User.ID, res.Token
}

// adminProof is the fixture admin's Local password, as an attach carries it.
var adminProof = auth.Reauth{LocalPassword: "correct-horse-battery"}

// adminSession signs the fixture's admin in with the Local password.
func adminSession(t *testing.T, svc *auth.Service) string {
	t.Helper()
	res, err := svc.Login(context.Background(), "admin", "correct-horse-battery", laptop, "")
	if err != nil {
		t.Fatalf("admin sign-in: %v", err)
	}
	return res.Token
}

// TestAnAttachWithoutTheLocalPasswordIsRefused: the admin has a Local password
// and a good session. An attach without the password, or with a wrong one, is
// ErrReauthRequired and never reaches the provider; with it, the attach goes
// through. A re-auth grant is no substitute for a Local password.
func TestAnAttachWithoutTheLocalPasswordIsRefused(t *testing.T) {
	svc, _, admin := newFixture(t)
	dir := directory("dir", "bj", "dir-pw", auth.ExternalAnswer{Subject: "s-bj", Username: "bj"})
	svc.UseSignInProviders(providerList{dir})
	session := adminSession(t, svc)

	for _, proof := range []auth.Reauth{{}, {LocalPassword: "wrong"}, {Grant: "anything"}} {
		_, err := svc.AttachWithPassword(context.Background(), admin.ID, session, proof, "dir", "bj", "dir-pw", "")
		if !errors.Is(err, auth.ErrReauthRequired) {
			t.Fatalf("attach with %+v: err = %v, want ErrReauthRequired", proof, err)
		}
		if err := svc.CheckReauth(context.Background(), admin.ID, session, proof, ""); !errors.Is(err, auth.ErrReauthRequired) {
			t.Fatalf("redirect attach with %+v: err = %v, want ErrReauthRequired", proof, err)
		}
	}
	if dir.asked != 0 {
		t.Fatalf("a refused attach asked the provider %d times, want never", dir.asked)
	}
	if got, _ := svc.ExternalIdentities(admin.ID); len(got) != 0 {
		t.Fatalf("identities after refused attaches = %+v, want none", got)
	}

	right := auth.Reauth{LocalPassword: "correct-horse-battery"}
	if err := svc.CheckReauth(context.Background(), admin.ID, session, right, ""); err != nil {
		t.Fatalf("redirect attach with the Local password: %v", err)
	}
	if _, err := svc.AttachWithPassword(context.Background(), admin.ID, session, right, "dir", "bj", "dir-pw", ""); err != nil {
		t.Fatalf("attach with the Local password: %v", err)
	}
}

// TestAWrongLocalPasswordOnAnAttachIsChargedLikeALogin: an attach is one more
// place to guess the Local password, so a wrong one counts against the same
// limits a login does, keyed by the User's own username.
func TestAWrongLocalPasswordOnAnAttachIsChargedLikeALogin(t *testing.T) {
	svc, _, admin := newFixture(t)
	svc.UseSignInProviders(providerList{&fakeProvider{id: "dir"}})
	session := adminSession(t, svc)

	var err error
	for i := 0; i < 64; i++ {
		err = svc.CheckReauth(context.Background(), admin.ID, session, auth.Reauth{LocalPassword: "wrong"}, "")
		if errors.Is(err, auth.ErrTooManyLoginAttempts) {
			if _, err := svc.Login(context.Background(), "admin", "correct-horse-battery", laptop, ""); !errors.Is(err, auth.ErrTooManyLoginAttempts) {
				t.Fatalf("a login after throttled attaches: err = %v, want throttled too", err)
			}
			return
		}
		if !errors.Is(err, auth.ErrReauthRequired) {
			t.Fatalf("attempt %d: err = %v, want ErrReauthRequired", i, err)
		}
	}
	t.Fatalf("64 wrong Local passwords were never throttled; last err = %v", err)
}

// TestWrongLocalPasswordsOnAnAttachTripTheUsernameCounter: each wrong Local
// password comes from a different address, so no per-IP counter ever fills —
// the per-username one, keyed by the User's own username, is what refuses, and
// it refuses a login for that username from a fresh address too.
func TestWrongLocalPasswordsOnAnAttachTripTheUsernameCounter(t *testing.T) {
	svc, _, admin := newFixture(t)
	svc.UseSignInProviders(providerList{&fakeProvider{id: "dir"}})
	session := adminSession(t, svc)

	var err error
	for i := 0; i < 64; i++ {
		err = svc.CheckReauth(context.Background(), admin.ID, session, auth.Reauth{LocalPassword: "wrong"}, fmt.Sprintf("198.51.100.%d", i))
		if errors.Is(err, auth.ErrTooManyLoginAttempts) {
			if _, err := svc.Login(context.Background(), "admin", "correct-horse-battery", laptop, "203.0.113.200"); !errors.Is(err, auth.ErrTooManyLoginAttempts) {
				t.Fatalf("admin's login from a fresh address after throttled attaches: err = %v, want throttled too", err)
			}
			return
		}
		if !errors.Is(err, auth.ErrReauthRequired) {
			t.Fatalf("attempt %d: err = %v, want ErrReauthRequired", i, err)
		}
	}
	t.Fatalf("64 wrong Local passwords from 64 addresses were never throttled; last err = %v", err)
}

// TestAGrantIsNoSubstituteForALocalPassword: the admin has a Local password AND
// holds a directory identity, so a re-auth through it mints a real, live grant.
// An attach that presents that grant instead of the password is refused; the
// same attach with the password goes through.
func TestAGrantIsNoSubstituteForALocalPassword(t *testing.T) {
	svc, _, admin := newFixture(t)
	svc.UseSignInProviders(providerList{
		directory("dir", "bj", "dir-pw", auth.ExternalAnswer{Subject: "s-bj", Username: "bj"}),
		directory("other", "bj2", "pw2", auth.ExternalAnswer{Subject: "s-bj-2", Username: "bj2"}),
	})
	session := adminSession(t, svc)
	if _, err := svc.AttachWithPassword(context.Background(), admin.ID, session, adminProof, "dir", "bj", "dir-pw", ""); err != nil {
		t.Fatalf("attach dir/s-bj with the Local password: %v", err)
	}

	grant, err := svc.ReauthWithPassword(context.Background(), admin.ID, session, "dir", "bj", "dir-pw", "")
	if err != nil || grant.Grant == "" {
		t.Fatalf("re-auth through dir/s-bj = %+v, %v; want a grant", grant, err)
	}
	if _, err := svc.AttachWithPassword(context.Background(), admin.ID, session, auth.Reauth{Grant: grant.Grant}, "other", "bj2", "pw2", ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("attach with a live grant instead of the Local password: err = %v, want ErrReauthRequired", err)
	}
	if got, _ := svc.ExternalIdentities(admin.ID); len(got) != 1 {
		t.Fatalf("identities after the refused attach = %+v, want only dir/s-bj", got)
	}
	if _, err := svc.AttachWithPassword(context.Background(), admin.ID, session, adminProof, "other", "bj2", "pw2", ""); err != nil {
		t.Fatalf("the same attach with the Local password: %v", err)
	}
}

// TestAnOutsideOnlyUserNeedsAFreshReauthToAttach: ada has no Local password, so
// no password — hers or anybody's — gets her an attach, nor does her session;
// a re-auth through the directory identity she holds does.
func TestAnOutsideOnlyUserNeedsAFreshReauthToAttach(t *testing.T) {
	svc, _, _ := newFixture(t)
	ada, session := outsideOnly(t, svc)

	for _, proof := range []auth.Reauth{{}, {LocalPassword: "pw"}, {LocalPassword: ""}, {Grant: "made-up"}} {
		if _, err := svc.AttachWithPassword(context.Background(), ada, session, proof, "other", "ada2", "pw2", ""); !errors.Is(err, auth.ErrReauthRequired) {
			t.Fatalf("attach with %+v: err = %v, want ErrReauthRequired", proof, err)
		}
	}

	grant, err := svc.ReauthWithPassword(context.Background(), ada, session, "dir", "ada", "pw", "")
	if err != nil || grant.Grant == "" || grant.ExpiresIn != auth.ReauthGrantTTL {
		t.Fatalf("re-auth = %+v, %v; want a grant good for %s", grant, err, auth.ReauthGrantTTL)
	}
	if _, err := svc.AttachWithPassword(context.Background(), ada, session, auth.Reauth{Grant: grant.Grant}, "other", "ada2", "pw2", ""); err != nil {
		t.Fatalf("attach with a fresh re-auth: %v", err)
	}
	if got, _ := svc.ExternalIdentities(ada); len(got) != 2 {
		t.Fatalf("ada's identities = %+v, want two", got)
	}
}

// TestAReauthGrantIsGoodOnceForItsOwnSession: a grant spent is spent, and one
// minted on ada's laptop is no good from her phone, nor to anybody else — here
// bob, who has no Local password either, so the grant is what decides.
func TestAReauthGrantIsGoodOnceForItsOwnSession(t *testing.T) {
	svc, _, _ := newFixture(t)
	ada, session := outsideOnly(t, svc)
	svc.UseSignInProviders(providerList{
		directory("dir", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"}),
		directory("bobdir", "bob", "bob-pw", auth.ExternalAnswer{Subject: "s-bob", Username: "bob"}),
	})
	bob, err := svc.Login(context.Background(), "bob", "bob-pw", laptop, "")
	if err != nil {
		t.Fatalf("bob's first sign-in: %v", err)
	}
	phone, err := svc.Login(context.Background(), "ada", "pw", auth.DeviceInput{Name: "Phone", Platform: "test", ClientID: "phone"}, "")
	if err != nil {
		t.Fatal(err)
	}
	mint := func() string {
		t.Helper()
		g, err := svc.ReauthWithPassword(context.Background(), ada, session, "dir", "ada", "pw", "")
		if err != nil {
			t.Fatalf("re-auth: %v", err)
		}
		return g.Grant
	}

	g := mint()
	if err := svc.CheckReauth(context.Background(), ada, session, auth.Reauth{Grant: g}, ""); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := svc.CheckReauth(context.Background(), ada, session, auth.Reauth{Grant: g}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("second use: err = %v, want ErrReauthRequired", err)
	}

	g = mint()
	if err := svc.CheckReauth(context.Background(), ada, phone.Token, auth.Reauth{Grant: g}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("from another session: err = %v, want ErrReauthRequired", err)
	}
	g = mint()
	if err := svc.CheckReauth(context.Background(), bob.User.ID, session, auth.Reauth{Grant: g}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("for another User: err = %v, want ErrReauthRequired", err)
	}
}

// TestAReauthGrantExpires: a grant is good for ReauthGrantTTL and not after.
func TestAReauthGrantExpires(t *testing.T) {
	svc, clock, _ := newFixture(t)
	ada, session := outsideOnly(t, svc)

	g, err := svc.ReauthWithPassword(context.Background(), ada, session, "dir", "ada", "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(auth.ReauthGrantTTL - time.Second)
	if err := svc.CheckReauth(context.Background(), ada, session, auth.Reauth{Grant: g.Grant}, ""); err != nil {
		t.Fatalf("just inside the window: %v", err)
	}

	g, err = svc.ReauthWithPassword(context.Background(), ada, session, "dir", "ada", "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(auth.ReauthGrantTTL)
	if err := svc.CheckReauth(context.Background(), ada, session, auth.Reauth{Grant: g.Grant}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("after the window: err = %v, want ErrReauthRequired", err)
	}
}

// TestAReauthIsOnlyThroughAnIdentityTheUserHolds: a credential the provider
// rejects is a charged ErrInvalidCredentials; one it accepts for an identity
// ada does not hold — by either flow — mints nothing.
func TestAReauthIsOnlyThroughAnIdentityTheUserHolds(t *testing.T) {
	svc, _, _ := newFixture(t)
	ada, session := outsideOnly(t, svc)

	if _, err := svc.ReauthWithPassword(context.Background(), ada, session, "dir", "ada", "wrong", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("a rejected re-auth: err = %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.ReauthWithPassword(context.Background(), ada, session, "other", "ada2", "pw2", ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("a re-auth through an identity ada does not hold: err = %v, want ErrReauthRequired", err)
	}
	if _, err := svc.ReauthExternal(ada, session, "oidc", auth.ExternalAnswer{Subject: "s-ada"}); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("a redirect re-auth through another provider's s-ada: err = %v, want ErrReauthRequired", err)
	}
	g, err := svc.ReauthExternal(ada, session, "dir", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})
	if err != nil {
		t.Fatalf("a redirect re-auth through dir/s-ada: %v", err)
	}
	if err := svc.CheckReauth(context.Background(), ada, session, auth.Reauth{Grant: g.Grant}, ""); err != nil {
		t.Fatalf("its grant: %v", err)
	}
}

// TestARefundedReauthGrantIsGoodAgainUntilItsOwnExpiry: the attach start spends
// the grant before it calls the provider, so a provider failure hands it back.
// The refund restores the grant as it was: it does not extend its life, and one
// that has since expired stays expired.
func TestARefundedReauthGrantIsGoodAgainUntilItsOwnExpiry(t *testing.T) {
	svc, clock, _ := newFixture(t)
	ada, session := outsideOnly(t, svc)
	ctx := context.Background()

	g, err := svc.ReauthWithPassword(ctx, ada, session, "dir", "ada", "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	refund, err := svc.CheckReauthRefundable(ctx, ada, session, auth.Reauth{Grant: g.Grant}, "")
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := svc.CheckReauth(ctx, ada, session, auth.Reauth{Grant: g.Grant}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("a spent grant err = %v, want ErrReauthRequired", err)
	}
	refund()
	clock.advance(auth.ReauthGrantTTL - time.Second)
	refund, err = svc.CheckReauthRefundable(ctx, ada, session, auth.Reauth{Grant: g.Grant}, "")
	if err != nil {
		t.Fatalf("a refunded grant was refused: %v", err)
	}
	refund()
	clock.advance(time.Second)
	if err := svc.CheckReauth(ctx, ada, session, auth.Reauth{Grant: g.Grant}, ""); !errors.Is(err, auth.ErrReauthRequired) {
		t.Errorf("a refunded grant past its original expiry err = %v, want ErrReauthRequired", err)
	}
}
