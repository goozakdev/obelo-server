package auth_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for the password flow of a Sign-in provider (ADR-0063).
// Against a REAL store for the reason remote_role_test.go gives: half of what is
// under test is the users CHECK and a transaction, and a fake store would assert
// the fake.

// fakeProvider is one password-flow Sign-in provider: it accepts exactly the
// logins in accounts, answering with the identity recorded there, or fails every
// call when broken is set.
type fakeProvider struct {
	id       string
	accounts map[[2]string]auth.ExternalAnswer
	broken   bool
	asked    int
}

func (f *fakeProvider) ID() string { return f.id }

func (f *fakeProvider) CheckPassword(_ context.Context, username, password string) (auth.ExternalAnswer, bool) {
	f.asked++
	if f.broken {
		return auth.ExternalAnswer{}, false
	}
	a, ok := f.accounts[[2]string{username, password}]
	return a, ok
}

type providerList []auth.PasswordProvider

func (l providerList) PasswordProviders() []auth.PasswordProvider { return l }

var laptop = auth.DeviceInput{Name: "Laptop", Platform: "test", ClientID: "laptop"}

func directory(id string, login, password string, answer auth.ExternalAnswer) *fakeProvider {
	return &fakeProvider{id: id, accounts: map[[2]string]auth.ExternalAnswer{{login, password}: answer}}
}

// TestAFirstTimeExternalIdentityCreatesAPasswordlessMember: the new User is a
// Member, holds NO Local password, and holds exactly the one identity — with the
// groups the provider named stored on it.
func TestAFirstTimeExternalIdentityCreatesAPasswordlessMember(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "ada", "pw",
		auth.ExternalAnswer{Subject: "s-ada", Username: "ada", Groups: []string{"family", "media"}})})

	res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	u, err := db.UserByID(res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != auth.RoleMember || u.PasswordHash != "" || u.Username != "ada" {
		t.Fatalf("new user = %+v, want a member named ada with no password hash", u)
	}
	ids, err := db.ExternalIdentitiesByUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ExternalIdentity{{PluginID: "dir", Subject: "s-ada", UserID: u.ID, Username: "ada",
		Groups: []string{"family", "media"}}}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("identities = %+v, want %+v", ids, want)
	}
}

// TestAPasswordlessMemberHasNoLocalPasswordToGuess: once minted, the Member's
// absent Local password matches nothing — not the empty string, not the
// provider's password — so with the provider rejecting it is refused like
// anyone else.
func TestAPasswordlessMemberHasNoLocalPasswordToGuess(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	dir := directory("dir", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})
	svc.UseSignInProviders(providerList{dir})
	if _, err := svc.Login(context.Background(), "ada", "pw", laptop, ""); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}

	dir.broken = true
	for _, pw := range []string{"", "pw", "anything"} {
		if _, err := svc.Login(context.Background(), "ada", pw, laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login ada/%q with the provider rejecting: err = %v, want ErrInvalidCredentials", pw, err)
		}
	}
}

// TestAFailingProviderIsARejectionAndTheNextIsAsked: the first provider fails
// every call; the second accepts. Sign-in succeeds on the second's answer.
func TestAFailingProviderIsARejectionAndTheNextIsAsked(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	broken := &fakeProvider{id: "broken", broken: true}
	second := directory("second", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada-2"})
	svc.UseSignInProviders(providerList{broken, second})

	res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
	if err != nil || res.User.Username != "ada-2" {
		t.Fatalf("login = %+v, %v; want the second provider's member ada-2", res.User, err)
	}
	if broken.asked != 1 {
		t.Fatalf("the failing provider was asked %d times, want 1", broken.asked)
	}
}

// TestTheLocalPasswordIsTriedFirst: a User whose Local password is right is
// signed in without any provider being asked — even one that would accept the
// same credential as somebody else.
func TestTheLocalPasswordIsTriedFirst(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	dir := directory("dir", "admin", "correct-horse-battery", auth.ExternalAnswer{Subject: "s", Username: "other"})
	svc.UseSignInProviders(providerList{dir})

	res, err := svc.Login(context.Background(), "admin", "correct-horse-battery", laptop, "")
	if err != nil || res.User.Role != auth.RoleAdmin {
		t.Fatalf("login = %+v, %v; want the local admin", res.User, err)
	}
	if dir.asked != 0 {
		t.Fatalf("the provider was asked %d times, want 0: the Local password signed in first", dir.asked)
	}
}

// TestAUsernameCollisionCreatesAndLinksNothing: the provider accepts an identity
// whose username is the local Admin's. The answer is ErrUsernameCollision, no User
// is created, no identity is linked to anyone, and the login-failure counter is
// not charged (it was not a refusal).
func TestAUsernameCollisionCreatesAndLinksNothing(t *testing.T) {
	svc, db := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "admin", "dir-pw",
		auth.ExternalAnswer{Subject: "s-admin", Username: "admin"})})

	if _, err := svc.Login(context.Background(), "admin", "dir-pw", laptop, ""); !errors.Is(err, auth.ErrUsernameCollision) {
		t.Fatalf("login err = %v, want ErrUsernameCollision", err)
	}
	users, err := db.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %+v, want only the admin", users)
	}
	if _, err := db.ExternalIdentityUser("dir", "s-admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the colliding identity resolves (err %v); it must be linked to nobody", err)
	}
	ids, err := db.ExternalIdentitiesByUser(users[0].ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("the admin holds identities %+v (err %v), want none", ids, err)
	}
}

// barrierProvider accepts one login, and holds every call until `callers` of them
// have arrived, so the logins it answers reach the first-sign-in insert together.
type barrierProvider struct {
	id      string
	answer  auth.ExternalAnswer
	arrived sync.WaitGroup
}

func (b *barrierProvider) ID() string { return b.id }

func (b *barrierProvider) CheckPassword(_ context.Context, _, _ string) (auth.ExternalAnswer, bool) {
	b.arrived.Done()
	b.arrived.Wait()
	return b.answer, true
}

// TestConcurrentFirstSignInsOfOneIdentityAreOneUser: the same new identity signing
// in several times at once is one person. Every login succeeds as the SAME User —
// none is told its own username collides — and exactly one users row and one
// identity row exist afterwards.
func TestConcurrentFirstSignInsOfOneIdentityAreOneUser(t *testing.T) {
	svc, db := newRemoteFixture(t)
	const callers = 8
	dir := &barrierProvider{id: "dir", answer: auth.ExternalAnswer{Subject: "s-ada", Username: "ada"}}
	dir.arrived.Add(callers)
	svc.UseSignInProviders(providerList{dir})

	ids := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
			ids[i], errs[i] = res.User.ID, err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("login %d: %v, want every concurrent first sign-in to succeed", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("login %d signed in as %q, login 0 as %q; want one User", i, ids[i], ids[0])
		}
	}
	var users, identities int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'ada'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM external_identities WHERE plugin_id = 'dir' AND subject = 's-ada'`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if users != 1 || identities != 1 {
		t.Fatalf("rows: %d users named ada, %d identities; want exactly one of each", users, identities)
	}
}

// TestEveryProviderRejectingIsChargedLikeAWrongPassword: the refusal the
// providers produce goes down the same path as a wrong Local password, limiter
// included — enough of them and the username is throttled.
func TestEveryProviderRejectingIsChargedLikeAWrongPassword(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{&fakeProvider{id: "dir"}})

	var err error
	for i := 0; i < 64; i++ {
		_, err = svc.Login(context.Background(), "ada", "wrong", laptop, "")
		if errors.Is(err, auth.ErrTooManyLoginAttempts) {
			return
		}
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidCredentials", i, err)
		}
	}
	t.Fatalf("64 provider rejections were never throttled; last err = %v", err)
}

// TestSchemaAdmitsAPasswordlessPersonOnlyAsAnExternalMember: the CHECK under
// the relaxed rule. A Member marked external_origin may lack a password; an Admin
// may not, marked or not; and an unmarked Member still may not (see
// TestSchemaRefusesAPasswordlessMember).
func TestSchemaAdmitsAPasswordlessPersonOnlyAsAnExternalMember(t *testing.T) {
	_, db := newRemoteFixture(t)

	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash, external_origin) VALUES ('m1','m1','member',NULL,1)`,
	); err != nil {
		t.Errorf("an external-origin member with no password was refused: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash, external_origin) VALUES ('a1','a1','admin',NULL,1)`,
	); err == nil {
		t.Error("an external-origin admin with no password was accepted; only a Member may be minted password-less")
	}
}
