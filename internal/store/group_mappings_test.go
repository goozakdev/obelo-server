package store_test

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// seedMappedAdmin inserts an Admin a Group mapping made: external-origin, no
// Local password — the one state the schema's CHECK admits for a password-less
// Admin.
func seedMappedAdmin(t *testing.T, db *store.DB, id string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO users (id, username, role, password_hash, external_origin, role_mapped)
		 VALUES (?, ?, 'admin', NULL, 1, 1)`, id, id)
}

// TestDeleteKeepingAnAdminRefusesTheLastAdminOfAnyKind: dee, an Admin by
// mapping, is the only Admin. No Local-password Admin is at stake, so only the
// statement's count of Admins of any kind can refuse — and it does, alone,
// with no service check in front of it. With a second such Admin the same
// delete goes through.
func TestDeleteKeepingAnAdminRefusesTheLastAdminOfAnyKind(t *testing.T) {
	db := openTemp(t)
	seedMappedAdmin(t, db, "dee")

	if err := db.DeleteUserKeepingAnAdmin("dee"); !errors.Is(err, store.ErrWouldLeaveNoAdmin) {
		t.Fatalf("deleting the only Admin: err = %v, want ErrWouldLeaveNoAdmin", err)
	}
	if _, err := db.UserByID("dee"); err != nil {
		t.Fatalf("dee after a refused delete: %v", err)
	}

	seedMappedAdmin(t, db, "eve")
	if err := db.DeleteUserKeepingAnAdmin("dee"); err != nil {
		t.Fatalf("deleting one of two Admins: %v", err)
	}
	if _, err := db.UserByID("dee"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dee after the delete: %v, want not found", err)
	}
}
