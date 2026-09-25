package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// seedUninstallUsers is one User per kind of sign-in path, each holding an
// identity at "directory":
//
//	ada   — only the directory
//	bea   — a Local password too
//	cy    — an identity at "backup", another installed provider
//	dee   — an identity at "gone", a provider that is not installed
//	link  — a 'remote' User, who signs in over a Link
//	kate  — an Admin with a Local password, so none of the above is the last one
func seedUninstallUsers(t *testing.T, db *store.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('kate', 'kate', 'admin', 'h')`)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('bea', 'bea', 'member', 'h')`)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('link', 'link', 'remote', NULL)`)
	for _, id := range []string{"ada", "cy", "dee"} {
		mustExec(t, db, `INSERT INTO users (id, username, role, password_hash, external_origin)
		                 VALUES (?, ?, 'member', NULL, 1)`, id, id)
	}
	for _, x := range [][3]string{
		{"directory", "s-ada", "ada"}, {"directory", "s-bea", "bea"}, {"directory", "s-cy", "cy"},
		{"directory", "s-dee", "dee"}, {"directory", "s-link", "link"}, {"backup", "s-cy", "cy"},
		{"gone", "s-dee", "dee"},
	} {
		mustExec(t, db, `INSERT INTO external_identities (plugin_id, subject, user_id) VALUES (?, ?, ?)`,
			x[0], x[1], x[2])
	}
	mustExec(t, db, `INSERT INTO devices (id, user_id, client_id, name, platform) VALUES ('d-ada', 'ada', 'c', 'n', 'p')`)
	mustExec(t, db, `INSERT INTO auth_tokens (token_hash, device_id, user_id) VALUES ('t-ada', 'd-ada', 'ada')`)
	mustExec(t, db, `INSERT INTO plugins (id, origin) VALUES ('directory', 'admin')`)
}

func casualtyIDs(cs []store.SignInCasualty) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestSignInCasualtiesAreTheUsersWithNoOtherPath: a Local password, an identity
// at another installed provider and a Link are each a way in; an identity at a
// provider that is not installed is not.
func TestSignInCasualtiesAreTheUsersWithNoOtherPath(t *testing.T) {
	db := openTemp(t)
	seedUninstallUsers(t, db)

	got, err := db.SignInCasualties("directory", []string{"backup"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := casualtyIDs(got); !reflect.DeepEqual(ids, []string{"ada", "dee"}) {
		t.Fatalf("casualties = %v, want ada and dee", ids)
	}
}

// TestDeleteSignInPluginRefusesAnythingButTheExactListAndChangesNothing: a
// confirm missing a User, naming an extra one, or naming nobody is refused with
// who would be deleted, and every row is where it was.
func TestDeleteSignInPluginRefusesAnythingButTheExactListAndChangesNothing(t *testing.T) {
	db := openTemp(t)
	seedUninstallUsers(t, db)

	for _, confirmed := range [][]string{nil, {"ada"}, {"ada", "dee", "bea"}} {
		err := db.DeleteSignInPlugin("directory", []string{"backup"}, confirmed)
		var unconfirmed *store.UnconfirmedUninstallError
		if !errors.As(err, &unconfirmed) || !reflect.DeepEqual(casualtyIDs(unconfirmed.Users), []string{"ada", "dee"}) {
			t.Fatalf("confirming %v: err = %v, want an unconfirmed uninstall naming ada and dee", confirmed, err)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM users`); n != 6 {
		t.Fatalf("users after refusals = %d, want 6", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM external_identities`); n != 7 {
		t.Fatalf("identities after refusals = %d, want 7", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM plugins`); n != 1 {
		t.Fatalf("plugin rows after refusals = %d, want 1", n)
	}
}

// TestDeleteSignInPluginDeletesTheConfirmedUsersAndEveryIdentityItIssued: the
// confirmed Users go with their sessions; every other User stays, losing only
// the directory's identity; the plugin row goes.
func TestDeleteSignInPluginDeletesTheConfirmedUsersAndEveryIdentityItIssued(t *testing.T) {
	db := openTemp(t)
	seedUninstallUsers(t, db)

	if err := db.DeleteSignInPlugin("directory", []string{"backup"}, []string{"dee", "ada"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ada", "dee"} {
		if _, err := db.UserByID(id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s after the uninstall: %v, want not found", id, err)
		}
	}
	for _, id := range []string{"kate", "bea", "cy", "link"} {
		if _, err := db.UserByID(id); err != nil {
			t.Fatalf("%s after the uninstall: %v", id, err)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM auth_tokens WHERE user_id = 'ada'`); n != 0 {
		t.Fatalf("ada's tokens = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM external_identities WHERE plugin_id = 'directory'`); n != 0 {
		t.Fatalf("directory identities = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM external_identities`); n != 1 {
		t.Fatalf("identities left = %d, want only cy's at backup", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM plugins`); n != 0 {
		t.Fatalf("plugin rows = %d, want 0", n)
	}
}

// TestDeleteSignInPluginIsOneTransaction: the plugin row refuses to go, after
// the Users and identities already have. Nothing stays done.
func TestDeleteSignInPluginIsOneTransaction(t *testing.T) {
	db := openTemp(t)
	seedUninstallUsers(t, db)
	mustExec(t, db, `CREATE TRIGGER refuse BEFORE DELETE ON plugins BEGIN SELECT RAISE(ABORT, 'no'); END`)

	if err := db.DeleteSignInPlugin("directory", []string{"backup"}, []string{"ada", "dee"}); err == nil {
		t.Fatal("the uninstall succeeded; want the trigger to fail it")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM users`); n != 6 {
		t.Fatalf("users after a failed uninstall = %d, want 6", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM external_identities`); n != 7 {
		t.Fatalf("identities after a failed uninstall = %d, want 7", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM auth_tokens WHERE user_id = 'ada'`); n != 1 {
		t.Fatalf("ada's tokens after a failed uninstall = %d, want 1", n)
	}
}

// TestDeleteSignInPluginKeepsAnAdmin: dee is an Admin only by mapping and the
// only Admin. Deleting her would leave none, so the uninstall is refused whole.
func TestDeleteSignInPluginKeepsAnAdmin(t *testing.T) {
	db := openTemp(t)
	seedMappedAdmin(t, db, "dee")
	mustExec(t, db, `INSERT INTO external_identities (plugin_id, subject, user_id) VALUES ('directory', 's', 'dee')`)

	if err := db.DeleteSignInPlugin("directory", nil, []string{"dee"}); !errors.Is(err, store.ErrUninstallWouldLeaveNoAdmin) {
		t.Fatalf("err = %v, want ErrUninstallWouldLeaveNoAdmin", err)
	}
	if _, err := db.UserByID("dee"); err != nil {
		t.Fatalf("dee after a refused uninstall: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM external_identities`); n != 1 {
		t.Fatalf("identities after a refused uninstall = %d, want 1", n)
	}
}

// TestDeleteSignInPluginDeletesWhatThePluginLeftBehind: the directory's Group
// mapping, the Libraries it grants, its re-check interval and its place in the
// sign-in order all go with it, so a Plugin installed later under the same id
// inherits none of them. Another provider's rows stay.
func TestDeleteSignInPluginDeletesWhatThePluginLeftBehind(t *testing.T) {
	db := openTemp(t)
	seedUninstallUsers(t, db)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Films', 'movie')`)
	for _, id := range []string{"directory", "backup"} {
		mustExec(t, db, `INSERT INTO group_mappings (plugin_id, group_name, role) VALUES (?, 'admins', 'admin')`, id)
		mustExec(t, db, `INSERT INTO group_mapping_libraries (plugin_id, group_name, library_id) VALUES (?, 'admins', 'lib')`, id)
		mustExec(t, db, `INSERT INTO sign_in_recheck_intervals (plugin_id, seconds) VALUES (?, 60)`, id)
	}
	if err := db.SetSignInProviderOrder([]string{"backup", "directory"}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteSignInPlugin("directory", []string{"backup"}, []string{"ada", "dee"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"group_mappings", "group_mapping_libraries", "sign_in_recheck_intervals",
		"sign_in_provider_order"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM `+table+` WHERE plugin_id = 'directory'`); n != 0 {
			t.Fatalf("%s rows for the directory = %d, want 0", table, n)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM `+table+` WHERE plugin_id = 'backup'`); n != 1 {
			t.Fatalf("%s rows for backup = %d, want 1", table, n)
		}
	}
}

// TestDuplicateConfirmedIDsAreASet: naming ada twice names ada once. It
// confirms exactly {ada}, and it is still refused where the uninstall would
// delete two Users.
func TestDuplicateConfirmedIDsAreASet(t *testing.T) {
	ada := []store.SignInCasualty{{ID: "ada"}}
	adaAndDee := []store.SignInCasualty{{ID: "ada"}, {ID: "dee"}}
	for _, c := range []struct {
		casualties []store.SignInCasualty
		confirmed  []string
		want       bool
	}{
		{ada, []string{"ada", "ada"}, true},
		{adaAndDee, []string{"ada", "ada"}, false},
		{adaAndDee, []string{"ada", "dee", "dee"}, true},
		{ada, []string{"ada", "ada", "dee"}, false},
	} {
		if got := store.CasualtiesConfirmed(c.casualties, c.confirmed); got != c.want {
			t.Fatalf("casualties %v confirmed by %v = %v, want %v", casualtyIDs(c.casualties), c.confirmed, got, c.want)
		}
	}

	db := openTemp(t)
	seedUninstallUsers(t, db)
	if err := db.DeleteSignInPlugin("directory", []string{"backup"}, []string{"ada", "dee", "ada"}); err != nil {
		t.Fatalf("confirming ada twice and dee: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM users WHERE id IN ('ada', 'dee')`); n != 0 {
		t.Fatalf("ada and dee after the uninstall = %d rows, want 0", n)
	}
}
