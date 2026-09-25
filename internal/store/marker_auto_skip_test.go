package store_test

import (
	"reflect"
	"testing"
)

// Auto-skip (ADR-0065 §6) is a per-User setting per Marker kind: a User with no
// rows skips nothing automatically, a set replaces the whole choice, and one
// User's choice is never another's.

func TestMarkerAutoSkipIsPerUserAndReplaced(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','x'), ('u2','u2','member','x')`)

	if got, err := db.MarkerAutoSkipKinds("u1"); err != nil || len(got) != 0 {
		t.Fatalf("fresh User's auto-skip = %v, %v; want none", got, err)
	}
	if err := db.SetMarkerAutoSkipKinds("u1", []string{"intro", "recap"}); err != nil {
		t.Fatalf("SetMarkerAutoSkipKinds: %v", err)
	}
	if got, _ := db.MarkerAutoSkipKinds("u1"); !reflect.DeepEqual(got, []string{"intro", "recap"}) {
		t.Errorf("u1 auto-skip = %v, want [intro recap]", got)
	}
	if got, _ := db.MarkerAutoSkipKinds("u2"); len(got) != 0 {
		t.Errorf("u2 auto-skip = %v, want none: u1's choice is not u2's", got)
	}
	if err := db.SetMarkerAutoSkipKinds("u1", []string{"credits"}); err != nil {
		t.Fatalf("SetMarkerAutoSkipKinds: %v", err)
	}
	if got, _ := db.MarkerAutoSkipKinds("u1"); !reflect.DeepEqual(got, []string{"credits"}) {
		t.Errorf("u1 auto-skip after replacing = %v, want [credits]", got)
	}
	if err := db.SetMarkerAutoSkipKinds("u1", []string{"commercial"}); err == nil {
		t.Error("an unknown kind was stored")
	}
}
