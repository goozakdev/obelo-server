package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The narrow reads behind an Event sink's payload (ADR-0057 decision 6,
// .scratch/plugin-system issue 06).
//
// A sink event carries ids, names and kinds and NOTHING else — never a catalog
// row, never a path, never a credential. These three lookups exist so that rule is
// enforced by the query rather than by the caller remembering: the translator
// cannot leak a password hash, a Device's client id or a Title's folder because it
// never receives them.
//
// That is why they do not reuse UserByID / DeviceByID / TitleByID. Those answer
// "tell me everything about this row" — the right shape for an admin screen and
// the wrong shape for an outbound integration. Three columns is the whole of what
// a sink is allowed to know, so three columns is what is selected.

// EventRef is one entity as a sink event names it: its id, the name an operator
// would recognize it by, and its kind in this server's vocabulary.
//
// "Kind" is deliberately loose, because the three things a playback event names
// answer the question differently and none of them needs a richer type: a Title's
// kind is movie/episode/track, a Device's is its platform, and a User's is its
// ROLE — which is the one that carries a rule, because `remote` means this is a
// linked Server and not a person (ADR-0054 §3).
type EventRef struct {
	ID   string
	Name string
	Kind string
}

// EventTitleRef names a Title: id, title, kind. Returns ErrNotFound for an
// unknown id.
func (db *DB) EventTitleRef(id string) (EventRef, error) {
	return db.eventRef(`SELECT id, title, kind FROM titles WHERE id = ?`, "title", id)
}

// EventDeviceRef names a Device (ADR-0015): id, name, platform. Returns
// ErrNotFound for an unknown id.
func (db *DB) EventDeviceRef(id string) (EventRef, error) {
	return db.eventRef(`SELECT id, name, platform FROM devices WHERE id = ?`, "device", id)
}

// EventUserRef names a User: id, username, ROLE as the kind. Returns ErrNotFound
// for an unknown id.
//
// The role is the point of this lookup. A session under a `remote` User is a
// relayed play from another household, and the sink payload must name the linked
// Server rather than the person behind it — so whoever builds that payload has to
// be told the role, and this is the cheapest honest way to learn it. The password
// hash is not selected, which is the other half of why this is not UserByID.
func (db *DB) EventUserRef(id string) (EventRef, error) {
	return db.eventRef(`SELECT id, username, role FROM users WHERE id = ?`, "user", id)
}

// eventRef runs one three-column lookup, mapping "no such row" onto ErrNotFound
// so every caller can treat an absence the same way.
func (db *DB) eventRef(query, what, id string) (EventRef, error) {
	var ref EventRef
	err := db.QueryRow(query, id).Scan(&ref.ID, &ref.Name, &ref.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return EventRef{}, ErrNotFound
	}
	if err != nil {
		return EventRef{}, fmt.Errorf("store: naming %s %q: %w", what, id, err)
	}
	return ref, nil
}
