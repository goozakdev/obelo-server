package auth_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for attaching an External identity to the caller's own
// User (ADR-0063 decision 3), against a real store for the reason sign_in_test.go
// gives.

func userNamed(t *testing.T, db *store.DB, name string) store.User {
	t.Helper()
	u, err := db.UserByUsername(name)
	if err != nil {
		t.Fatalf("user %q: %v", name, err)
	}
	return u
}

// TestAnAttachedIdentitySignsInAsTheUserWhoAttachedIt: the directory's "admin"
// is a new identity whose name is taken here — the collision refusal — until the
// admin attaches it; afterwards it signs in as the admin, and nobody new exists.
func TestAnAttachedIdentitySignsInAsTheUserWhoAttachedIt(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "admin", "dir-pw",
		auth.ExternalAnswer{Subject: "s-admin", Username: "admin"})})
	admin := userNamed(t, db, "admin")

	if _, err := svc.Login(context.Background(), "admin", "dir-pw", laptop, ""); !errors.Is(err, auth.ErrUsernameCollision) {
		t.Fatalf("before the attach: err = %v, want ErrUsernameCollision", err)
	}
	answer, err := svc.AttachWithPassword(context.Background(), admin.ID, adminSession(t, svc), adminProof, "dir", "admin", "dir-pw", "")
	if err != nil || answer.Subject != "s-admin" {
		t.Fatalf("attach = %+v, %v; want the directory's s-admin", answer, err)
	}
	res, err := svc.Login(context.Background(), "admin", "dir-pw", laptop, "")
	if err != nil || res.User.ID != admin.ID {
		t.Fatalf("after the attach: login = %+v, %v; want the admin %q", res.User, err, admin.ID)
	}
	if n, _ := db.CountUsers(); n != 1 {
		t.Fatalf("users = %d, want still 1", n)
	}
}

// TestAttachingAHeldIdentityChangesNeitherUser: ada holds s-ada from her first
// sign-in; the admin attaching it is ErrExternalIdentityHeld, and both Users'
// identities are exactly what they were.
func TestAttachingAHeldIdentityChangesNeitherUser(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "ada", "pw",
		auth.ExternalAnswer{Subject: "s-ada", Username: "ada", Groups: []string{"family"}})})
	res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
	if err != nil {
		t.Fatal(err)
	}
	adaBefore, _ := db.ExternalIdentitiesByUser(res.User.ID)
	admin := userNamed(t, db, "admin")

	_, err = svc.AttachWithPassword(context.Background(), admin.ID, adminSession(t, svc), adminProof, "dir", "ada", "pw", "")
	if !errors.Is(err, auth.ErrExternalIdentityHeld) {
		t.Fatalf("password attach: err = %v, want ErrExternalIdentityHeld", err)
	}
	err = svc.AttachExternal(admin.ID, "dir", auth.ExternalAnswer{Subject: "s-ada", Username: "ada-renamed"})
	if !errors.Is(err, auth.ErrExternalIdentityHeld) {
		t.Fatalf("redirect attach: err = %v, want ErrExternalIdentityHeld", err)
	}
	if got, _ := db.ExternalIdentitiesByUser(admin.ID); len(got) != 0 {
		t.Fatalf("admin's identities = %+v, want none", got)
	}
	if got, _ := db.ExternalIdentitiesByUser(res.User.ID); !reflect.DeepEqual(got, adaBefore) {
		t.Fatalf("ada's identities = %+v, want unchanged %+v", got, adaBefore)
	}
}

// TestARemoteUserCannotAttachAnExternalIdentity: a `remote` User is a linked
// Server, not a person, and may never sign in interactively (ADR-0054). An
// identity attached to one would be exactly that, so the attach is refused.
func TestARemoteUserCannotAttachAnExternalIdentity(t *testing.T) {
	svc, db := newRemoteFixture(t)
	remote, err := svc.CreateUser(context.Background(), "Brandon's server", "", auth.RoleRemote)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.AttachExternal(remote.ID, "dir", auth.ExternalAnswer{Subject: "s-server", Username: "server"})
	if !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if got, _ := db.ExternalIdentitiesByUser(remote.ID); len(got) != 0 {
		t.Fatalf("the remote User holds %+v, want nothing", got)
	}
}

// TestAttachRejectionsAreChargedLikeAWrongPassword: attaching is one more way
// to put a password to a directory, so a rejection counts against the same
// limits a login does — it is not a way round them.
func TestAttachRejectionsAreChargedLikeAWrongPassword(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{&fakeProvider{id: "dir"}})
	admin := userNamed(t, db, "admin")
	session := adminSession(t, svc)

	var err error
	for i := 0; i < 64; i++ {
		_, err = svc.AttachWithPassword(context.Background(), admin.ID, session, adminProof, "dir", "ada", "wrong", "")
		if errors.Is(err, auth.ErrTooManyLoginAttempts) {
			if _, err := svc.Login(context.Background(), "ada", "wrong", laptop, ""); !errors.Is(err, auth.ErrTooManyLoginAttempts) {
				t.Fatalf("a login after throttled attaches: err = %v, want throttled too", err)
			}
			return
		}
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidCredentials", i, err)
		}
	}
	t.Fatalf("64 rejected attaches were never throttled; last err = %v", err)
}

// TestAnAttachNamesAPasswordProviderThisServerHas: a provider id that is not an
// enabled password-flow Sign-in provider is ErrUnknownSignInProvider.
func TestAnAttachNamesAPasswordProviderThisServerHas(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})})
	admin := userNamed(t, db, "admin")
	if _, err := svc.AttachWithPassword(context.Background(), admin.ID, adminSession(t, svc), adminProof, "other", "ada", "pw", ""); !errors.Is(err, auth.ErrUnknownSignInProvider) {
		t.Fatalf("err = %v, want ErrUnknownSignInProvider", err)
	}
}
