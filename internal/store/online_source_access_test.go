package store_test

import "testing"

// TestOnlineSourceGrantCountCountsUsersHoldingThatSource: one per User granted the
// source, none for a source nobody holds, and a grant to another source is not counted.
func TestOnlineSourceGrantCountCountsUsersHoldingThatSource(t *testing.T) {
	db := openTemp(t)
	for _, id := range []string{"ada", "bea", "cy"} {
		mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, 'member', 'h')`, id, id)
	}
	for _, u := range []string{"ada", "bea"} {
		if err := db.ReplaceOnlineSourceAccess(u, []string{"tube", "other"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ReplaceOnlineSourceAccess("cy", []string{"other"}); err != nil {
		t.Fatal(err)
	}

	for source, want := range map[string]int{"tube": 2, "other": 3, "nobody": 0} {
		got, err := db.OnlineSourceGrantCount(source)
		if err != nil || got != want {
			t.Fatalf("OnlineSourceGrantCount(%q) = %d, %v; want %d", source, got, err, want)
		}
	}
}
