package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestCollectionVisibleSummariesCountsPerViewer: one read answers every
// Collection's visible-member count and first member, agreeing with what
// Members would resolve per Collection — Missing members and members the filter
// excludes are not counted, and a Collection with none visible is absent.
func TestCollectionVisibleSummariesCountsPerViewer(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('la','A','movie')`)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lb','B','movie')`)
	for _, row := range [][3]string{{"t1", "la", "b"}, {"t2", "la", "a"}, {"t3", "lb", "c"}, {"t4", "la", "d"}} {
		mustExec(t, db,
			`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
			 VALUES (?, ?, 'movie', ?, ?, ?)`, row[0], row[1], row[2], row[0], row[2])
	}
	mustExec(t, db, `UPDATE titles SET hidden = 1 WHERE id = 't4'`)
	for _, id := range []string{"c1", "c2", "c3"} {
		mustExec(t, db, `INSERT INTO collections (id, name) VALUES (?, ?)`, id, id)
	}
	for _, m := range [][2]string{{"c1", "t1"}, {"c1", "t2"}, {"c1", "t3"}, {"c1", "t4"}, {"c2", "t3"}, {"c3", "t4"}} {
		mustExec(t, db, `INSERT INTO collection_items (collection_id, title_id) VALUES (?, ?)`, m[0], m[1])
	}

	all, err := db.CollectionVisibleSummaries(store.AllAccess())
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	if got := all["c1"]; got.Count != 3 || got.FirstTitleID != "t2" {
		t.Errorf("c1 for an admin = %+v, want 3 members first t2 (t4 is Missing)", got)
	}
	if got := all["c2"]; got.Count != 1 || got.FirstTitleID != "t3" {
		t.Errorf("c2 for an admin = %+v, want 1 member t3", got)
	}
	if _, ok := all["c3"]; ok {
		t.Errorf("c3 (only a Missing member) present for an admin: %+v", all["c3"])
	}

	got, err := db.CollectionVisibleSummaries(store.AccessFilter{LibraryIDs: []string{"la"}})
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if s := got["c1"]; s.Count != 2 || s.FirstTitleID != "t2" {
		t.Errorf("c1 for a member of la = %+v, want 2 members first t2", s)
	}
	if _, ok := got["c2"]; ok {
		t.Errorf("c2 (all members in lb) leaked to a member of la: %+v", got["c2"])
	}
}

// TestLibrariesForLinksGroupsByLink: one read returns each Link's linked
// Libraries, oldest first, and nothing for a Link with none.
func TestLibrariesForLinksGroupsByLink(t *testing.T) {
	db := openTemp(t)
	for _, id := range []string{"l1", "l2", "l3"} {
		if err := db.InsertLink(store.Link{
			ID: id, ServerID: "peer-" + id, ServerName: id,
			Origins: []string{"http://x.example"}, ActiveOrigin: "http://x.example",
			Token: "t", State: store.LinkStateConnected, CreatedAt: "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("inserting link %s: %v", id, err)
		}
	}
	if _, err := db.UpsertLinkedLibrary("lib-a", "A", "movie", "l1", "ra"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertLinkedLibrary("lib-b", "B", "movie", "l1", "rb"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertLinkedLibrary("lib-c", "C", "movie", "l2", "rc"); err != nil {
		t.Fatal(err)
	}

	got, err := db.LibrariesForLinks([]string{"l1", "l2", "l3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["l1"]) != 2 || got["l1"][0].ID != "lib-a" || got["l1"][1].ID != "lib-b" {
		t.Errorf("l1 = %+v, want lib-a, lib-b", got["l1"])
	}
	if len(got["l2"]) != 1 || got["l2"][0].ID != "lib-c" {
		t.Errorf("l2 = %+v, want lib-c", got["l2"])
	}
	if len(got["l3"]) != 0 {
		t.Errorf("l3 = %+v, want none", got["l3"])
	}
	if empty, err := db.LibrariesForLinks(nil); err != nil || len(empty) != 0 {
		t.Errorf("no ids = %v, %v; want empty, nil", empty, err)
	}
}

// TestSetGroupMappingAndIntervalIsOneTransaction: the rules and the re-check
// interval land together, and a rule naming an absent Library leaves BOTH as
// they were — the interval is not written when the mapping is refused.
func TestSetGroupMappingAndIntervalIsOneTransaction(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('la','A','movie')`)

	rules := []store.GroupMappingRule{{Group: "kids", Role: "member", LibraryIDs: []string{"la"}}}
	if err := db.SetGroupMappingAndInterval("oidc", rules, 6*time.Hour); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := db.GroupMapping("oidc")
	if err != nil || len(got) != 1 || got[0].Group != "kids" {
		t.Fatalf("mapping = %+v, %v", got, err)
	}
	if d, err := db.RecheckInterval("oidc"); err != nil || d != 6*time.Hour {
		t.Fatalf("interval = %v, %v; want 6h", d, err)
	}

	bad := []store.GroupMappingRule{{Group: "x", Role: "member", LibraryIDs: []string{"nope"}}}
	if err := db.SetGroupMappingAndInterval("oidc", bad, 12*time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent library: err = %v, want ErrNotFound", err)
	}
	if d, _ := db.RecheckInterval("oidc"); d != 6*time.Hour {
		t.Errorf("interval after a refused write = %v, want it unchanged at 6h", d)
	}
	if got, _ := db.GroupMapping("oidc"); len(got) != 1 || got[0].Group != "kids" {
		t.Errorf("mapping after a refused write = %+v, want unchanged", got)
	}

	if err := db.SetGroupMappingAndInterval("oidc", rules, 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.RecheckInterval("oidc"); d != 0 {
		t.Errorf("interval 0 should clear the override, got %v", d)
	}
}
