package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The links table (ADR-0055, ADR-0056 §6). What is asserted here is the SQL and
// the one constraint that carries a design decision: server_id is UNIQUE, which
// is what makes a fresh invite from a friend a RE-KEY of the existing Link rather
// than a second one beside it.

func aLink(id, serverID string, origins ...string) store.Link {
	if len(origins) == 0 {
		origins = []string{"http://obelo.tail1a2b.ts.net", "https://media.example.org"}
	}
	return store.Link{
		ID:                  id,
		ServerID:            serverID,
		ServerName:          "Amy's server",
		Origins:             origins,
		ActiveOrigin:        origins[0],
		Token:               "obelo_" + strings.Repeat("a", 32),
		DeviceID:            "device-" + id,
		LinkProtocolVersion: 1,
		State:               store.LinkStateConnected,
		CreatedAt:           rfc3339(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)),
	}
}

func TestLinkRoundTripsIncludingTheOrderOfItsOrigins(t *testing.T) {
	db := openTemp(t)
	want := aLink("l1", "amy-server-id", "https://second.example.org", "http://first.example.org")
	if err := db.InsertLink(want); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}

	got, err := db.LinkByID("l1")
	if err != nil {
		t.Fatalf("LinkByID: %v", err)
	}
	if got.ServerID != want.ServerID || got.Token != want.Token || got.DeviceID != want.DeviceID {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	// The ORDER is the contract (ADR-0055 §2: tried in order), so the storage has
	// to be an array and not a set. Deliberately seeded out of alphabetical order
	// so a sort somewhere would show up here.
	if len(got.Origins) != 2 || got.Origins[0] != "https://second.example.org" ||
		got.Origins[1] != "http://first.example.org" {
		t.Errorf("origins = %v, want them in the order they went in", got.Origins)
	}
	if got.LastSyncedAt != "" || got.LastError != "" {
		t.Errorf("a fresh Link carries bookkeeping: syncedAt %q, error %q", got.LastSyncedAt, got.LastError)
	}

	byPeer, err := db.LinkByServerID("amy-server-id")
	if err != nil || byPeer.ID != "l1" {
		t.Errorf("LinkByServerID = %+v, %v", byPeer, err)
	}
	if _, err := db.LinkByID("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LinkByID of an unknown id = %v, want ErrNotFound", err)
	}
	if _, err := db.LinkByServerID("nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LinkByServerID of an unknown peer = %v, want ErrNotFound", err)
	}
}

// TestOnePeerCannotHaveTwoLinks is the constraint the whole re-key story rests
// on. Without it a second invite from the same friend would silently fork the
// relationship, and the mirror behind the first Link would be orphaned with
// nothing to say so.
func TestOnePeerCannotHaveTwoLinks(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertLink(aLink("l1", "amy-server-id")); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	if err := db.InsertLink(aLink("l2", "amy-server-id")); err == nil {
		t.Fatal("a second Link to the same server was accepted")
	}
	// A DIFFERENT peer is fine, of course: a household may link to several.
	if err := db.InsertLink(aLink("l2", "bob-server-id")); err != nil {
		t.Fatalf("InsertLink for a second peer: %v", err)
	}
	links, err := db.Links()
	if err != nil || len(links) != 2 {
		t.Fatalf("Links() = %d rows, %v; want 2", len(links), err)
	}
}

// TestUpdateLinkCredentialReplacesTheCredentialAndNotTheRelationship: a re-key
// is the same Link with a new key, so created_at and the sync position survive
// it while the token, the addresses and the state do not.
func TestUpdateLinkCredentialReplacesTheCredentialAndNotTheRelationship(t *testing.T) {
	db := openTemp(t)
	first := aLink("l1", "amy-server-id")
	if err := db.InsertLink(first); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	// The mirror has run and then the Link went bad, which is the state an
	// operator re-keys from (ADR-0056 §6).
	if err := db.SetLinkState("l1", store.LinkStateRevoked, "the sharer deleted the user"); err != nil {
		t.Fatalf("SetLinkState: %v", err)
	}
	mustExec(t, db, `UPDATE links SET last_synced_at = ? WHERE id = 'l1'`, "2026-09-03T13:00:00Z")

	next := aLink("l1", "amy-server-id", "https://media.example.org")
	next.ServerName = "Amy's new server"
	next.Token = "obelo_" + strings.Repeat("b", 32)
	next.DeviceID = "device-2"
	if err := db.UpdateLinkCredential(next); err != nil {
		t.Fatalf("UpdateLinkCredential: %v", err)
	}

	got, err := db.LinkByID("l1")
	if err != nil {
		t.Fatalf("LinkByID: %v", err)
	}
	if got.Token != next.Token || got.DeviceID != "device-2" || got.ServerName != "Amy's new server" {
		t.Errorf("the credential did not move: %+v", got)
	}
	if len(got.Origins) != 1 || got.ActiveOrigin != "https://media.example.org" {
		t.Errorf("the addresses did not move: %v / %q", got.Origins, got.ActiveOrigin)
	}
	if got.State != store.LinkStateConnected || got.LastError != "" {
		t.Errorf("state = %q / %q, want connected with no error after a re-key", got.State, got.LastError)
	}
	// The two things a re-key must NOT touch: it is the same relationship, and the
	// mirror is exactly as fresh as it was a moment ago.
	if got.CreatedAt != first.CreatedAt {
		t.Errorf("createdAt moved: %q → %q", first.CreatedAt, got.CreatedAt)
	}
	if got.LastSyncedAt != "2026-09-03T13:00:00Z" {
		t.Errorf("lastSyncedAt = %q; a re-key is not a resync", got.LastSyncedAt)
	}

	orphan := aLink("nope", "amy-server-id")
	if err := db.UpdateLinkCredential(orphan); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("re-keying a Link that is not there = %v, want ErrNotFound", err)
	}
}

// TestOnlyTheThreeStatesAreStorable: the admin page branches on this value, so a
// typo has to be an error rather than a dead branch nobody notices.
func TestOnlyTheThreeStatesAreStorable(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertLink(aLink("l1", "amy-server-id")); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	for _, state := range []string{store.LinkStateConnected, store.LinkStateUnreachable, store.LinkStateRevoked} {
		if err := db.SetLinkState("l1", state, ""); err != nil {
			t.Errorf("SetLinkState(%q): %v", state, err)
		}
	}
	if err := db.SetLinkState("l1", "disconnected", ""); err == nil {
		t.Error("a state outside the closed set was stored")
	}
	if err := db.SetLinkState("nope", store.LinkStateConnected, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLinkState on an unknown Link = %v, want ErrNotFound", err)
	}
}

// TestTheSweepsTwoWriters covers what a sweep records (issue 08): a success
// stamps the sync, returns the Link to connected and CLEARS the reason that
// explained the last failure; a moved address is remembered on its own, without
// disturbing anything else.
func TestTheSweepsTwoWriters(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertLink(aLink("l1", "amy-server-id")); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	if err := db.SetLinkState("l1", store.LinkStateUnreachable, "nobody answered"); err != nil {
		t.Fatalf("SetLinkState: %v", err)
	}

	synced := rfc3339(time.Date(2026, 9, 4, 9, 30, 0, 0, time.UTC))
	if err := db.SetLinkSynced("l1", synced); err != nil {
		t.Fatalf("SetLinkSynced: %v", err)
	}
	l, err := db.LinkByID("l1")
	if err != nil {
		t.Fatalf("LinkByID: %v", err)
	}
	if l.State != store.LinkStateConnected || l.LastSyncedAt != synced || l.LastError != "" {
		t.Fatalf("after a good sweep = %+v, want connected, stamped, with no error", l)
	}

	if err := db.SetLinkActiveOrigin("l1", "https://media.example.org"); err != nil {
		t.Fatalf("SetLinkActiveOrigin: %v", err)
	}
	l, _ = db.LinkByID("l1")
	if l.ActiveOrigin != "https://media.example.org" {
		t.Errorf("activeOrigin = %q, want the address that answered", l.ActiveOrigin)
	}
	if l.LastSyncedAt != synced || l.Token == "" {
		t.Errorf("moving the origin disturbed the rest of the row: %+v", l)
	}

	if err := db.SetLinkSynced("nope", synced); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLinkSynced on an unknown Link = %v, want ErrNotFound", err)
	}
	if err := db.SetLinkActiveOrigin("nope", "https://x.example"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLinkActiveOrigin on an unknown Link = %v, want ErrNotFound", err)
	}
}

func TestDeleteLink(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertLink(aLink("l1", "amy-server-id")); err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	if err := db.DeleteLink("l1"); err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
	if links, _ := db.Links(); len(links) != 0 {
		t.Errorf("%d links remain", len(links))
	}
	if err := db.DeleteLink("l1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}
	// The peer is free again: unlinking really does undo the relationship, so a
	// later invite from the same friend links rather than colliding.
	if err := db.InsertLink(aLink("l2", "amy-server-id")); err != nil {
		t.Fatalf("re-linking to the same peer after an unlink: %v", err)
	}
}
