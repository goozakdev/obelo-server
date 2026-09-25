package auth_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Service-level tests for Group mapping (ADR-0063 decisions 4 and 5): the sync
// a sign-in runs, and the guard that the Server never loses its last Admin with
// a Local password. Against a real store, for the reason sign_in_test.go gives:
// the users CHECK and ApplyMappedAccess's own WHERE clause are half of what is
// under test.

// libraries creates one movie Library per name, with that name as its id.
func libraries(t *testing.T, db *store.DB, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := db.CreateLibrary(n, n, "movie", []store.LibraryRootInput{{ID: n + "-root", Path: "/media/" + n}}); err != nil {
			t.Fatalf("creating library %s: %v", n, err)
		}
	}
}

func mapGroups(t *testing.T, db *store.DB, pluginID string, rules ...store.GroupMappingRule) {
	t.Helper()
	if err := db.SetGroupMapping(pluginID, rules); err != nil {
		t.Fatalf("setting the %s group mapping: %v", pluginID, err)
	}
}

func grantsOf(t *testing.T, db *store.DB, userID string) []string {
	t.Helper()
	got, err := db.LibraryAccessForUser(userID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		got = []string{}
	}
	return got
}

// changingDirectory is a password-flow provider whose one account's groups the
// test changes between sign-ins.
type changingDirectory struct {
	id, login, password, subject string
	groups                       []string
}

func (d *changingDirectory) ID() string { return d.id }

func (d *changingDirectory) CheckPassword(_ context.Context, username, password string) (auth.ExternalAnswer, bool) {
	if username != d.login || password != d.password {
		return auth.ExternalAnswer{}, false
	}
	return auth.ExternalAnswer{Subject: d.subject, Username: d.login, Groups: append([]string(nil), d.groups...)}, true
}

// TestAGroupMappingNeverGovernsAUserWithALocalPassword: bea holds a Local
// password and two Libraries an Admin granted her by hand. She attaches a
// directory identity in a group mapped to only one of them, then signs in
// through it and is synced again. She keeps both Libraries and her role: a
// mapping never touches a User with a Local password.
func TestAGroupMappingNeverGovernsAUserWithALocalPassword(t *testing.T) {
	svc, db := newRemoteFixture(t)
	libraries(t, db, "films", "cartoons")
	mapGroups(t, db, "dir", store.GroupMappingRule{Group: "kids", Role: auth.RoleMember, LibraryIDs: []string{"cartoons"}})
	bea, err := svc.CreateUser(context.Background(), "bea", "bea-local-pw", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceLibraryAccess(bea.ID, []string{"cartoons", "films"}); err != nil {
		t.Fatal(err)
	}
	dir := &changingDirectory{id: "dir", login: "bea-dir", password: "dir-pw", subject: "s-bea", groups: []string{"kids"}}
	svc.UseSignInProviders(providerList{dir})

	session, err := svc.Login(context.Background(), "bea", "bea-local-pw", laptop, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AttachWithPassword(context.Background(), bea.ID, session.Token,
		auth.Reauth{LocalPassword: "bea-local-pw"}, "dir", "bea-dir", "dir-pw", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}
	res, err := svc.Login(context.Background(), "bea-dir", "dir-pw", laptop, "")
	if err != nil || res.User.ID != bea.ID {
		t.Fatalf("sign-in through the directory = %+v, %v; want bea", res.User, err)
	}
	if err := svc.SyncGroupMapping(bea.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got, want := grantsOf(t, db, bea.ID), []string{"cartoons", "films"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bea's grants after a sync = %v, want the %v an Admin gave her", got, want)
	}
	if u, _ := db.UserByID(bea.ID); u.Role != auth.RoleMember {
		t.Fatalf("bea's role after a sync = %q, want member", u.Role)
	}
}

// TestASignInReappliesTheMappingWhenGroupsChange: cy has no Local password. Each
// sign-in answers her current groups, and the SAME sign-in leaves her with the
// role and grants the mapping gives them — the session included.
func TestASignInReappliesTheMappingWhenGroupsChange(t *testing.T) {
	svc, db := newRemoteFixture(t)
	libraries(t, db, "films", "cartoons")
	mapGroups(t, db, "dir",
		store.GroupMappingRule{Group: "kids", Role: auth.RoleMember, LibraryIDs: []string{"cartoons"}},
		store.GroupMappingRule{Group: "adults", Role: auth.RoleMember, LibraryIDs: []string{"films"}},
		store.GroupMappingRule{Group: "admins", Role: auth.RoleAdmin},
	)
	dir := &changingDirectory{id: "dir", login: "cy", password: "dir-pw", subject: "s-cy"}
	svc.UseSignInProviders(providerList{dir})

	for _, step := range []struct {
		groups []string
		role   string
		grants []string
	}{
		{[]string{"kids"}, auth.RoleMember, []string{"cartoons"}},
		{[]string{"kids", "adults"}, auth.RoleMember, []string{"cartoons", "films"}},
		{[]string{"admins"}, auth.RoleAdmin, []string{}},
		{[]string{"strangers"}, auth.RoleMember, []string{}},
	} {
		dir.groups = step.groups
		res, err := svc.Login(context.Background(), "cy", "dir-pw", laptop, "")
		if err != nil {
			t.Fatalf("sign-in in %v: %v", step.groups, err)
		}
		if res.User.Role != step.role {
			t.Fatalf("in %v the sign-in answered role %q, want %q", step.groups, res.User.Role, step.role)
		}
		who, err := svc.Authenticate(res.Token)
		if err != nil || who.User.Role != step.role {
			t.Fatalf("in %v the session is %q (%v), want %q", step.groups, who.User.Role, err, step.role)
		}
		if got := grantsOf(t, db, res.User.ID); !reflect.DeepEqual(got, step.grants) {
			t.Fatalf("in %v the grants are %v, want %v", step.groups, got, step.grants)
		}
	}
}

// TestAProviderWithNoMappingLeavesItsUsersAlone: with no mapping for the
// provider, what an Admin granted a password-less Member by hand survives her
// next sign-in.
func TestAProviderWithNoMappingLeavesItsUsersAlone(t *testing.T) {
	svc, db := newRemoteFixture(t)
	libraries(t, db, "films")
	dir := &changingDirectory{id: "dir", login: "cy", password: "dir-pw", subject: "s-cy", groups: []string{"kids"}}
	svc.UseSignInProviders(providerList{dir})
	res, err := svc.Login(context.Background(), "cy", "dir-pw", laptop, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceLibraryAccess(res.User.ID, []string{"films"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(context.Background(), "cy", "dir-pw", laptop, ""); err != nil {
		t.Fatal(err)
	}
	if got := grantsOf(t, db, res.User.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("grants = %v, want the films an Admin granted", got)
	}
}

// mappedAdmin signs dee in through a directory whose "admins" group is mapped
// to the Admin role, and answers her: a password-less Admin.
func mappedAdmin(t *testing.T, svc *auth.Service, db *store.DB) store.User {
	t.Helper()
	mapGroups(t, db, "dir", store.GroupMappingRule{Group: "admins", Role: auth.RoleAdmin})
	svc.UseSignInProviders(providerList{&changingDirectory{
		id: "dir", login: "dee", password: "dir-pw", subject: "s-dee", groups: []string{"admins"}}})
	res, err := svc.Login(context.Background(), "dee", "dir-pw", laptop, "")
	if err != nil {
		t.Fatalf("dee's sign-in: %v", err)
	}
	dee, err := db.UserByID(res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dee.Role != auth.RoleAdmin || dee.PasswordHash != "" {
		t.Fatalf("dee = %+v, want a password-less admin", dee)
	}
	return dee
}

// TestAMappedAdminCannotDeleteTheLastLocalPasswordAdmin: dee is an Admin by
// mapping, with no Local password; "admin" is the only Admin who has one. Two
// Admins exist, so a guard counting Admins of any kind lets the delete through
// and leaves nobody who can sign in with the directory down. It is refused.
func TestAMappedAdminCannotDeleteTheLastLocalPasswordAdmin(t *testing.T) {
	svc, db := newRemoteFixture(t)
	mappedAdmin(t, svc, db)
	admin := userNamed(t, db, "admin")

	err := svc.DeleteUser(admin.ID)
	if !errors.Is(err, auth.ErrLastLocalPasswordAdmin) || !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("deleting the last Admin with a Local password: err = %v, want ErrLastLocalPasswordAdmin", err)
	}
	if _, err := db.UserByID(admin.ID); err != nil {
		t.Fatalf("the admin is gone after a refused delete: %v", err)
	}
}

// TestALocalPasswordAdminMayDeleteAMappedAdmin: the other way round. Deleting
// dee leaves the one Admin with a Local password where it was, so it goes
// through even though that Admin is the only one.
func TestALocalPasswordAdminMayDeleteAMappedAdmin(t *testing.T) {
	svc, db := newRemoteFixture(t)
	dee := mappedAdmin(t, svc, db)

	if err := svc.DeleteUser(dee.ID); err != nil {
		t.Fatalf("deleting the mapped admin: %v", err)
	}
	if _, err := db.UserByID(dee.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dee after the delete: %v, want not found", err)
	}
	if n, _ := db.CountLocalPasswordAdmins(); n != 1 {
		t.Fatalf("local-password admins = %d, want 1", n)
	}
}

// TestSchemaAdmitsAPasswordlessAdminOnlyByMapping: the CHECK under this
// relaxation. A password-less Admin is admitted only as an external-origin User
// whose role a mapping set.
func TestSchemaAdmitsAPasswordlessAdminOnlyByMapping(t *testing.T) {
	_, db := newRemoteFixture(t)
	for _, c := range []struct {
		origin, mapped int
		ok             bool
	}{{1, 1, true}, {1, 0, false}, {0, 1, false}, {0, 0, false}} {
		id := fmt.Sprintf("a-%d-%d", c.origin, c.mapped)
		_, err := db.Exec(`INSERT INTO users (id, username, role, password_hash, external_origin, role_mapped)
			VALUES (?, ?, 'admin', NULL, ?, ?)`, id, id, c.origin, c.mapped)
		if (err == nil) != c.ok {
			t.Errorf("a password-less admin with external_origin=%d role_mapped=%d: err = %v, want accepted=%v",
				c.origin, c.mapped, err, c.ok)
		}
	}
}

// TestASignInPersistsNoPassword: a sign-in through a provider whose mapping
// promotes the person leaves the password nowhere in the database — not on the
// User, not on the identity, not in any table.
func TestASignInPersistsNoPassword(t *testing.T) {
	svc, db := newRemoteFixture(t)
	const secret = "dir-pw-7f3a9c"
	mapGroups(t, db, "dir", store.GroupMappingRule{Group: "admins", Role: auth.RoleAdmin})
	svc.UseSignInProviders(providerList{&changingDirectory{
		id: "dir", login: "dee", password: secret, subject: "s-dee", groups: []string{"admins"}}})
	if _, err := svc.Login(context.Background(), "dee", secret, laptop, ""); err != nil {
		t.Fatal(err)
	}
	if where := findInDatabase(t, db, secret); where != "" {
		t.Fatalf("the password is stored in %s", where)
	}
}

// findInDatabase answers the first table.column holding needle anywhere in a
// value, or "".
func findInDatabase(t *testing.T, db *store.DB, needle string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		r, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if strings.Contains(fmt.Sprint(v), needle) || strings.Contains(fmt.Sprintf("%s", v), needle) {
					r.Close()
					return table + "." + cols[i]
				}
			}
		}
		r.Close()
	}
	return ""
}

// TestARevocationVoidsAnApprovedDeviceCode: the admin approved a TV's code, and
// before the TV polls every session the admin holds is revoked. The approval
// goes with them: the TV's next poll mints nothing.
func TestARevocationVoidsAnApprovedDeviceCode(t *testing.T) {
	svc, clock, admin := newFixture(t)
	start, err := svc.StartDeviceAuth(tvDevice(), tvSourceIP)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.ApproveDeviceCode(start.UserCode, admin.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := svc.RevokeSessions(admin.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	clock.advance(deviceAuthPollGap)
	res, err := svc.RedeemDeviceCode(start.DeviceCode)
	if err == nil || res.Token != "" {
		t.Fatalf("redeeming a code approved before the revocation minted a session (err %v); want none", err)
	}
}

// racingDeletes holds every CountAdmins call until two have been made, so two
// deletes both finish their checks before either deletes anything.
type racingDeletes struct {
	*store.DB
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (r *racingDeletes) CountAdmins() (int, error) {
	n, err := r.DB.CountAdmins()
	r.mu.Lock()
	r.arrived++
	if r.arrived == 2 {
		close(r.both)
	}
	r.mu.Unlock()
	select {
	case <-r.both:
	case <-time.After(5 * time.Second):
	}
	return n, err
}

// TestConcurrentDeletesCannotRemoveBothLastLocalPasswordAdmins: "admin" and
// bea are the only Admins with a Local password, and each is deleted at the
// same moment. Each delete, checked alone, leaves one; both together would leave
// none. Exactly one goes through. dee, an Admin by mapping with no Local
// password, is a third Admin, so only the Local-password count can refuse.
func TestConcurrentDeletesCannotRemoveBothLastLocalPasswordAdmins(t *testing.T) {
	svc, db := newRemoteFixture(t)
	mappedAdmin(t, svc, db)
	admin := userNamed(t, db, "admin")
	bea, err := svc.CreateUser(context.Background(), "bea", "bea-local-pw", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	racing, err := auth.NewService(&racingDeletes{DB: db, both: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{admin.ID, bea.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = racing.DeleteUser(id)
		}()
	}
	wg.Wait()
	if n, _ := db.CountLocalPasswordAdmins(); n != 1 {
		t.Fatalf("after two concurrent deletes: %d local-password admins (errors %v, %v), want 1", n, errs[0], errs[1])
	}
	if (errs[0] == nil) == (errs[1] == nil) || !(errors.Is(errs[0], auth.ErrLastLocalPasswordAdmin) || errors.Is(errs[1], auth.ErrLastLocalPasswordAdmin)) {
		t.Fatalf("errors = %v, %v; want one delete refused with ErrLastLocalPasswordAdmin", errs[0], errs[1])
	}
}
