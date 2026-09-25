package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// A sign-in racing the uninstall of its Sign-in provider (ADR-0063 decision 10).
// The provider has answered, and only then does the uninstall commit; the
// sign-in's writes land after it. They must not leave an External identity, a
// User or a session behind for a Plugin that is no longer installed.

// uninstallingProvider is a provider that, having accepted the credential,
// lets the uninstall of its own Plugin commit before it hands the answer back —
// the moment between the provider's answer and the sign-in's writes, with no
// sleeps.
type uninstallingProvider struct {
	*fakeProvider
	db *store.DB
	t  *testing.T
}

func (p uninstallingProvider) CheckPassword(ctx context.Context, username, password string) (auth.ExternalAnswer, bool) {
	answer, ok := p.fakeProvider.CheckPassword(ctx, username, password)
	if err := p.db.DeleteSignInPlugin(p.id, nil, nil); err != nil {
		p.t.Fatalf("uninstalling %s mid sign-in: %v", p.id, err)
	}
	return answer, ok
}

func countOf(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// wantNothingLeftFor fails if anything a sign-in writes exists for the
// uninstalled provider: its identities, a User named username, or any session
// beyond the Admin's none.
func wantNothingLeftFor(t *testing.T, db *store.DB, pluginID, username string) {
	t.Helper()
	if n := countOf(t, db, `SELECT COUNT(*) FROM external_identities WHERE plugin_id = ?`, pluginID); n != 0 {
		t.Fatalf("%s identities after its uninstall = %d, want 0", pluginID, n)
	}
	if n := countOf(t, db, `SELECT COUNT(*) FROM users WHERE username = ?`, username); n != 0 {
		t.Fatalf("users named %s after the uninstall = %d, want 0", username, n)
	}
	if n := countOf(t, db, `SELECT COUNT(*) FROM auth_tokens`); n != 0 {
		t.Fatalf("sessions after the uninstall = %d, want 0", n)
	}
}

// TestAPasswordSignInRacingTheUninstallIsRefused: the directory accepts ada's
// credential, then is uninstalled before her first sign-in writes anything. The
// sign-in is refused as a failed one and nothing of it stays.
func TestAPasswordSignInRacingTheUninstallIsRefused(t *testing.T) {
	svc, db := newRemoteFixture(t)
	dir := directory("directory", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})
	svc.UseSignInProviders(providerList{uninstallingProvider{fakeProvider: dir, db: db, t: t}})

	if _, err := svc.Login(context.Background(), "ada", "pw", laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("sign-in racing the uninstall: err = %v, want ErrInvalidCredentials", err)
	}
	wantNothingLeftFor(t, db, "directory", "ada")
}

// TestARedirectSignInRacingTheUninstallIsRefused: the redirect flow's answer
// arrives for a provider uninstalled since it answered. It is refused, and
// nothing of it stays.
func TestARedirectSignInRacingTheUninstallIsRefused(t *testing.T) {
	svc, db := newRemoteFixture(t)
	if err := db.DeleteSignInPlugin("directory", nil, nil); err != nil {
		t.Fatal(err)
	}

	_, err := svc.SignInExternal("directory", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"}, laptop)
	if !errors.Is(err, auth.ErrSignInProviderGone) {
		t.Fatalf("redirect sign-in racing the uninstall: err = %v, want ErrSignInProviderGone", err)
	}
	wantNothingLeftFor(t, db, "directory", "ada")
}

// TestReinstallingASignInProviderLetsItSignPeopleInAgain: the refusal lasts
// only while the Plugin is gone. Installed again under the same id, its first
// sign-in creates the Member as it always did.
func TestReinstallingASignInProviderLetsItSignPeopleInAgain(t *testing.T) {
	svc, db := newRemoteFixture(t)
	if err := db.DeleteSignInPlugin("directory", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertPlugin(store.PluginInsert{ID: "directory", Origin: "admin"}); err != nil {
		t.Fatal(err)
	}
	svc.UseSignInProviders(providerList{directory("directory", "ada", "pw",
		auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})})

	res, err := svc.Login(context.Background(), "ada", "pw", laptop, "")
	if err != nil || res.User.Username != "ada" {
		t.Fatalf("sign-in after the reinstall = %+v, %v; want ada", res.User, err)
	}
}

// TestAPasswordAttachRacingTheUninstallIsRefused: the directory accepts the
// admin's credential for an attach, then is uninstalled before the attach
// writes anything. The attach is refused and leaves no identity behind that a
// Plugin reinstalled under the same id would honour.
func TestAPasswordAttachRacingTheUninstallIsRefused(t *testing.T) {
	svc, db := newRemoteFixture(t)
	dir := directory("directory", "ada", "pw", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})
	svc.UseSignInProviders(providerList{uninstallingProvider{fakeProvider: dir, db: db, t: t}})
	admin := userNamed(t, db, "admin")

	_, err := svc.AttachWithPassword(context.Background(), admin.ID, adminSession(t, svc), adminProof, "directory", "ada", "pw", "")
	if !errors.Is(err, auth.ErrSignInProviderGone) {
		t.Fatalf("password attach racing the uninstall: err = %v, want ErrSignInProviderGone", err)
	}
	if n := countOf(t, db, `SELECT COUNT(*) FROM external_identities WHERE plugin_id = 'directory'`); n != 0 {
		t.Fatalf("directory identities after its uninstall = %d, want 0", n)
	}
}

// TestARedirectAttachRacingTheUninstallIsRefused: the redirect flow's answer
// for an attach arrives for a provider uninstalled since it answered. Refused,
// and nothing is attached.
func TestARedirectAttachRacingTheUninstallIsRefused(t *testing.T) {
	svc, db := newRemoteFixture(t)
	admin := userNamed(t, db, "admin")
	if err := db.DeleteSignInPlugin("directory", nil, nil); err != nil {
		t.Fatal(err)
	}

	err := svc.AttachExternal(admin.ID, "directory", auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})
	if !errors.Is(err, auth.ErrSignInProviderGone) {
		t.Fatalf("redirect attach racing the uninstall: err = %v, want ErrSignInProviderGone", err)
	}
	if n := countOf(t, db, `SELECT COUNT(*) FROM external_identities WHERE plugin_id = 'directory'`); n != 0 {
		t.Fatalf("directory identities after its uninstall = %d, want 0", n)
	}
}

// uninstallingOnLookup is the store with one seam: once armed, finding the
// holder of an identity lets the uninstall of that identity's Plugin commit
// before the returning sign-in records anything — deleting the holder too when
// the Plugin was their only way in.
type uninstallingOnLookup struct {
	*store.DB
	t     *testing.T
	armed bool
}

func (s *uninstallingOnLookup) ExternalIdentityUser(pluginID, subject string) (store.User, error) {
	u, err := s.DB.ExternalIdentityUser(pluginID, subject)
	if err != nil || !s.armed {
		return u, err
	}
	casualties, cerr := s.DB.SignInCasualties(pluginID, nil)
	if cerr != nil {
		s.t.Fatalf("listing who uninstalling %s deletes: %v", pluginID, cerr)
	}
	var ids []string
	for _, c := range casualties {
		ids = append(ids, c.ID)
	}
	if err := s.DB.DeleteSignInPlugin(pluginID, nil, ids); err != nil {
		s.t.Fatalf("uninstalling %s mid sign-in: %v", pluginID, err)
	}
	return u, err
}

// newUninstallingOnLookupFixture is newRemoteFixture over that seam.
func newUninstallingOnLookupFixture(t *testing.T) (*auth.Service, *store.DB, *uninstallingOnLookup) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seam := &uninstallingOnLookup{DB: db, t: t}
	svc, err := auth.NewService(seam)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := svc.Setup(context.Background(), svc.ClaimToken(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return svc, db, seam
}

// TestAReturningPasswordSignInRacingTheUninstallIsRefused: ada has signed in
// through the directory before. This time the uninstall commits after her
// identity is found and before her sign-in records anything. It is refused as
// a failed sign-in, not an error, and nothing of it stays.
func TestAReturningPasswordSignInRacingTheUninstallIsRefused(t *testing.T) {
	svc, db, seam := newUninstallingOnLookupFixture(t)
	svc.UseSignInProviders(providerList{directory("directory", "ada", "pw",
		auth.ExternalAnswer{Subject: "s-ada", Username: "ada"})})
	if _, err := svc.Login(context.Background(), "ada", "pw", laptop, ""); err != nil {
		t.Fatalf("ada's first sign-in: %v", err)
	}

	seam.armed = true
	if _, err := svc.Login(context.Background(), "ada", "pw", laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("returning sign-in racing the uninstall: err = %v, want ErrInvalidCredentials", err)
	}
	wantNothingLeftFor(t, db, "directory", "ada")
}

// TestAReturningRedirectSignInRacingTheUninstallIsRefused: the same race in
// the redirect flow is ErrSignInProviderGone — what the callback answers as a
// refused sign-in — and nothing of it stays.
func TestAReturningRedirectSignInRacingTheUninstallIsRefused(t *testing.T) {
	svc, db, seam := newUninstallingOnLookupFixture(t)
	answer := auth.ExternalAnswer{Subject: "s-ada", Username: "ada"}
	if _, err := svc.SignInExternal("directory", answer, laptop); err != nil {
		t.Fatalf("ada's first sign-in: %v", err)
	}

	seam.armed = true
	if _, err := svc.SignInExternal("directory", answer, laptop); !errors.Is(err, auth.ErrSignInProviderGone) {
		t.Fatalf("returning redirect sign-in racing the uninstall: err = %v, want ErrSignInProviderGone", err)
	}
	wantNothingLeftFor(t, db, "directory", "ada")
}
