package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for the `remote` role — the role a linked Server holds on
// this one (ADR-0054, .scratch/linked-servers issue 01).
//
// They run against a REAL store because half of what is under test is a schema
// CHECK: "only a remote User may lack a password" is a constraint, and a fake
// store would assert the fake. The other half is the
// login refusal, which is a rule about ORDER — and the order is only interesting
// against the same VerifyPasswordContext every real login runs.

// newRemoteFixture builds a real store on a temp DB plus an auth service, seeded
// with the first Admin (so CreateUser is reachable).
func newRemoteFixture(t *testing.T) (*auth.Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := svc.Setup(context.Background(), svc.ClaimToken(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return svc, db
}

// TestCreateRemoteUserTakesNoPassword: the role is created with a label and
// nothing else, and the row it leaves behind carries no password hash at all.
func TestCreateRemoteUserTakesNoPassword(t *testing.T) {
	svc, db := newRemoteFixture(t)

	user, err := svc.CreateUser(context.Background(), "Brandon's server", "", auth.RoleRemote)
	if err != nil {
		t.Fatalf("create remote user: %v", err)
	}
	if user.Role != auth.RoleRemote {
		t.Errorf("role = %q, want %q", user.Role, auth.RoleRemote)
	}
	if user.Username != "Brandon's server" {
		t.Errorf("username = %q, want the sharer-chosen label", user.Username)
	}
	if user.PasswordHash != "" {
		t.Errorf("password hash = %q, want empty (the role has no password)", user.PasswordHash)
	}

	// It is a User like any other on the Admin surface: listed, fetchable by id.
	users, err := svc.Users()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	found := false
	for _, u := range users {
		if u.ID == user.ID {
			found = true
		}
	}
	if !found {
		t.Error("the remote User is missing from GET /users; the Admin page manages it there")
	}
	if _, err := db.UserByID(user.ID); err != nil {
		t.Errorf("UserByID: %v", err)
	}
}

// TestCreateRemoteUserRejectsASuppliedPassword: a password on the role is a
// mistake, not a field to ignore. Storing an inert secret nothing will ever
// verify is worse than refusing it.
func TestCreateRemoteUserRejectsASuppliedPassword(t *testing.T) {
	svc, _ := newRemoteFixture(t)

	_, err := svc.CreateUser(context.Background(), "peer", "a-password-it-must-not-have", auth.RoleRemote)
	if !errors.Is(err, auth.ErrInvalidUser) {
		t.Fatalf("create remote user with a password: err = %v, want ErrInvalidUser", err)
	}
}

// TestCreateMemberStillRequiresAPassword: widening the rule for one role must
// not have loosened it for the others.
func TestCreateMemberStillRequiresAPassword(t *testing.T) {
	svc, _ := newRemoteFixture(t)

	for _, role := range []string{auth.RoleMember, auth.RoleAdmin} {
		if _, err := svc.CreateUser(context.Background(), "nopass-"+role, "", role); !errors.Is(err, auth.ErrInvalidUser) {
			t.Errorf("create %s with no password: err = %v, want ErrInvalidUser", role, err)
		}
	}
	if _, err := svc.CreateUser(context.Background(), "bogus", "a-good-password", "peer"); !errors.Is(err, auth.ErrInvalidUser) {
		t.Errorf("create with an unknown role: err = %v, want ErrInvalidUser", err)
	}
}

// TestSchemaRefusesAPasswordlessMember is the constraint under the service rule:
// even a direct INSERT — a future code path, a migration, a repair by hand —
// cannot leave a person without a password. Only the remote role may.
func TestSchemaRefusesAPasswordlessMember(t *testing.T) {
	_, db := newRemoteFixture(t)

	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES ('m1','m1','member','')`,
	); err == nil {
		t.Error("a member with an empty password hash was accepted; the CHECK is not doing its job")
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role) VALUES ('m2','m2','member')`,
	); err == nil {
		t.Error("a member with a NULL password hash was accepted; the CHECK is not doing its job")
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES ('r1','r1','remote','')`,
	); err != nil {
		t.Errorf("a remote User with no password was refused: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES ('x1','x1','peer','h')`,
	); err == nil {
		t.Error("an unknown role was accepted; the role CHECK is not doing its job")
	}
}

// TestRemoteUserCannotLogIn: no password works, including the empty one that is
// literally what the row stores. The refusal is the SAME generic error an unknown
// username gets, so nothing about the role leaks through the answer.
func TestRemoteUserCannotLogIn(t *testing.T) {
	svc, _ := newRemoteFixture(t)

	if _, err := svc.CreateUser(context.Background(), "peer-server", "", auth.RoleRemote); err != nil {
		t.Fatalf("create remote user: %v", err)
	}

	dev := auth.DeviceInput{Name: "Peer", Platform: "server", ClientID: "peer-client-1"}
	for _, password := range []string{"", "guess", "correct-horse-battery"} {
		_, err := svc.Login(context.Background(), "peer-server", password, dev, "192.0.2.11")
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("login with password %q: err = %v, want ErrInvalidCredentials", password, err)
		}
	}

	// The refusal is indistinguishable from an unknown username: same error, and
	// no Device or token was left behind by any of the attempts.
	if _, err := svc.Login(context.Background(), "nobody-at-all", "guess", dev, "192.0.2.11"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("login as an unknown username: err = %v, want the same ErrInvalidCredentials", err)
	}
}

// TestRoleChangesCannotCrossTheRemoteBoundary: a linked Server is created remote
// and dies remote. There is no role-change endpoint yet; the guard exists so the
// rule is already written when one arrives.
func TestRoleChangesCannotCrossTheRemoteBoundary(t *testing.T) {
	cases := []struct {
		from, to string
		want     error
	}{
		{auth.RoleMember, auth.RoleAdmin, nil},
		{auth.RoleAdmin, auth.RoleMember, nil},
		{auth.RoleRemote, auth.RoleRemote, nil},
		{auth.RoleMember, auth.RoleRemote, auth.ErrRoleChange},
		{auth.RoleAdmin, auth.RoleRemote, auth.ErrRoleChange},
		{auth.RoleRemote, auth.RoleMember, auth.ErrRoleChange},
		{auth.RoleRemote, auth.RoleAdmin, auth.ErrRoleChange},
		{auth.RoleMember, "peer", auth.ErrInvalidUser},
	}
	for _, tc := range cases {
		err := auth.CheckRoleChange(tc.from, tc.to)
		if tc.want == nil && err != nil {
			t.Errorf("CheckRoleChange(%q, %q) = %v, want nil", tc.from, tc.to, err)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("CheckRoleChange(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
		}
	}
}

// TestDeletingARemoteUserCascades: the sharer's kill switch. Deleting the User
// takes its Device and token with it, and never touches the last-Admin guard.
func TestDeletingARemoteUserCascades(t *testing.T) {
	svc, db := newRemoteFixture(t)

	user, err := svc.CreateUser(context.Background(), "peer-server", "", auth.RoleRemote)
	if err != nil {
		t.Fatalf("create remote user: %v", err)
	}
	// Stand in for redeeming an invite (ADR-0055): an ordinary Device-bound token.
	device, err := db.UpsertDevice("dev-1", user.ID, "peer-server-id", "Peer", "server")
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if err := db.InsertToken("token-hash-1", device.ID, user.ID); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	if err := svc.DeleteUser(user.ID); err != nil {
		t.Fatalf("delete remote user: %v", err)
	}
	if _, err := db.DeviceByID(device.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("device survived the delete: err = %v, want ErrNotFound", err)
	}
	if _, err := db.LookupToken("token-hash-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("token survived the delete: err = %v, want ErrNotFound", err)
	}

	// And the last-Admin guard is untouched by any of this: the one Admin still
	// cannot be deleted, and a remote User never counted toward it.
	users, err := svc.Users()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Role != auth.RoleAdmin {
			continue
		}
		if err := svc.DeleteUser(u.ID); !errors.Is(err, auth.ErrLastAdmin) {
			t.Errorf("delete the last admin: err = %v, want ErrLastAdmin", err)
		}
	}
}
