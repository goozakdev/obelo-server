package store_test

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The three narrow lookups an Event sink's payload is built from
// (.scratch/plugin-system issue 06). What is asserted is the kind each one reports,
// because that is the field with meaning behind it — a User's kind is its ROLE, and
// the whole attribution rule (ADR-0054 §3) turns on reading it correctly.

// TestEventRefsNameThingsAndReportTheirKind: each lookup answers with the id, the
// operator-facing name, and the kind — the Title's kind, the Device's platform, the
// User's role.
func TestEventRefsNameThingsAndReportTheirKind(t *testing.T) {
	db := openTemp(t)

	// A person and a linked Server, so the role the sink rule reads is a real value
	// from a real row rather than a constant in a test.
	person, err := db.CreateAdmin("u1", "brandon", "hash")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	peer, err := db.CreateUser("u9", "Brandon's server", "remote", "")
	if err != nil {
		t.Fatalf("CreateUser(remote): %v", err)
	}

	got, err := db.EventUserRef(person.ID)
	if err != nil {
		t.Fatalf("EventUserRef(person): %v", err)
	}
	if got != (store.EventRef{ID: "u1", Name: "brandon", Kind: "admin"}) {
		t.Fatalf("EventUserRef(person) = %+v, want the username and the role", got)
	}
	got, err = db.EventUserRef(peer.ID)
	if err != nil {
		t.Fatalf("EventUserRef(peer): %v", err)
	}
	if got.Kind != "remote" {
		t.Fatalf("EventUserRef(peer) = %+v, want kind \"remote\" — the rule reads this", got)
	}
	if got.Name != "Brandon's server" {
		t.Fatalf("EventUserRef(peer) name = %q, want the label the Admin typed", got.Name)
	}

	device, err := db.UpsertDevice("d1", person.ID, "admin-client", "Laptop", "macos")
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	got, err = db.EventDeviceRef(device.ID)
	if err != nil {
		t.Fatalf("EventDeviceRef: %v", err)
	}
	if got != (store.EventRef{ID: "d1", Name: "Laptop", Kind: "macos"}) {
		t.Fatalf("EventDeviceRef = %+v, want the name and the platform as the kind", got)
	}
}

// TestEventRefsReportNotFound: an unknown id is ErrNotFound, not a zero value that
// would read as "a Title with no name". The translator relies on telling those
// apart — it names an entity by its id alone when the lookup fails, and omits the
// ACTOR entirely, which it could not decide to do without an error.
func TestEventRefsReportNotFound(t *testing.T) {
	db := openTemp(t)

	if _, err := db.EventUserRef("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("EventUserRef(unknown) err = %v, want ErrNotFound", err)
	}
	if _, err := db.EventDeviceRef("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("EventDeviceRef(unknown) err = %v, want ErrNotFound", err)
	}
	if _, err := db.EventTitleRef("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("EventTitleRef(unknown) err = %v, want ErrNotFound", err)
	}
}
