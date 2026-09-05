package store_test

import (
	"testing"
)

// LastDeviceSeenByUser (.scratch/linked-servers issue 04): the one grouped read
// behind the Admin Users list's "Linked, seen 2h ago" / "Never linked" badge.
// A User with no Device must be ABSENT from the map — that absence is what the
// badge reads as "never linked", and a "" would be indistinguishable from a
// Device whose timestamp failed to write.

func TestLastDeviceSeenSkipsAUserWithNoDevice(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','x')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('peer','Brandon''s server','remote')`)

	got, err := db.LastDeviceSeenByUser()
	if err != nil {
		t.Fatalf("LastDeviceSeenByUser: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("with no Devices anywhere, got %v, want an empty map", got)
	}

	// A Device for one User must not put the other in the map.
	if _, err := db.UpsertDevice("d1", "u1", "client-1", "Ada's iPhone", "ios"); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	got, err = db.LastDeviceSeenByUser()
	if err != nil {
		t.Fatalf("LastDeviceSeenByUser: %v", err)
	}
	if _, ok := got["peer"]; ok {
		t.Errorf("the Device-less remote User appears as %q, want absent", got["peer"])
	}
	if got["u1"] == "" {
		t.Errorf("the User with a Device has no last-seen; got %v", got)
	}
}

func TestLastDeviceSeenTakesTheMostRecentOfSeveral(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','x')`)

	// Written directly so the two Devices carry known, differing timestamps —
	// UpsertDevice always stamps "now", which cannot separate two rows.
	mustExec(t, db, `INSERT INTO devices (id, user_id, client_id, name, platform, created_at, last_seen_at)
	                 VALUES ('d1','u1','c1','Old TV','tvos','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	mustExec(t, db, `INSERT INTO devices (id, user_id, client_id, name, platform, created_at, last_seen_at)
	                 VALUES ('d2','u1','c2','New iPad','ipados','2026-01-01T00:00:00Z','2026-08-09T12:30:00Z')`)

	got, err := db.LastDeviceSeenByUser()
	if err != nil {
		t.Fatalf("LastDeviceSeenByUser: %v", err)
	}
	if got["u1"] != "2026-08-09T12:30:00Z" {
		t.Errorf("last-seen = %q, want the newer Device's 2026-08-09T12:30:00Z", got["u1"])
	}
}

// Deleting the User cascades its Devices away, so the badge goes back to
// "never linked" rather than pointing at a ghost.
func TestLastDeviceSeenFollowsTheUserCascade(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('peer','Brandon''s server','remote')`)
	if _, err := db.UpsertDevice("d1", "peer", "home-server-id", "Brandon's server", "server"); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if got, _ := db.LastDeviceSeenByUser(); got["peer"] == "" {
		t.Fatalf("precondition: the redeemed Link has no last-seen; got %v", got)
	}

	if err := db.DeleteUser("peer"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	got, err := db.LastDeviceSeenByUser()
	if err != nil {
		t.Fatalf("LastDeviceSeenByUser: %v", err)
	}
	if _, ok := got["peer"]; ok {
		t.Errorf("a deleted User still reports a last-seen: %v", got)
	}
}
