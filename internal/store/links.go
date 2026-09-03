package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Links (ADR-0055, ADR-0056 §6): the home half of linking — this Server's
// standing relationship with another household's Server, the credential it
// redeemed, the addresses it may be reached at, and which of them answered. See
// migrations/0062_links.sql for the shape and for why the token is stored the
// way provider keys are.
//
// Every timestamp crossing this file is an RFC3339-UTC string supplied by the
// caller, never SQLite's datetime('now') — the format 0041_device_auth.sql
// records the comparison bug for.

// The three states a Link is in (ADR-0056 §6). They are the wire spellings and
// the column's CHECK values at once, so a typo cannot reach the database.
const (
	// LinkStateConnected: the last export or relay call succeeded.
	LinkStateConnected = "connected"
	// LinkStateUnreachable: a transport failure or 5xx. Transient. The mirror
	// stays and is badged unavailable; nothing is deleted.
	LinkStateUnreachable = "unreachable"
	// LinkStateRevoked: a 401, which the contract defines as "the token is dead" —
	// the sharer deleted the User or the Device. The mirror stays and the admin
	// page offers "paste a new invite", which re-keys this row in place.
	LinkStateRevoked = "revoked"
)

// Link is one row of the links table. Origins is decoded from the stored JSON
// array and its ORDER is meaningful (ADR-0055 §2).
type Link struct {
	ID           string
	ServerID     string
	ServerName   string
	Origins      []string
	ActiveOrigin string
	Token        string
	// DeviceID is the Device this Link is over there (ADR-0055 §4), kept so the
	// unlink can remove it rather than leave a ghost with a last-seen.
	DeviceID            string
	LinkProtocolVersion int
	State               string
	LastSyncedAt        string
	LastError           string
	CreatedAt           string
}

// InsertLink records a newly established Link. A second Link to the same
// server_id is refused by the column's UNIQUE constraint rather than by a check
// here: re-keying is an UPDATE (UpdateLinkCredential), and the two paths must
// not be able to disagree about which one a caller is on.
func (db *DB) InsertLink(l Link) error {
	origins, err := encodeOrigins(l.Origins)
	if err != nil {
		return err
	}
	if _, err := db.Exec(
		`INSERT INTO links (id, server_id, server_name, origins, active_origin, token,
		                    device_id, link_protocol_version, state, last_synced_at,
		                    last_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.ServerID, l.ServerName, origins, l.ActiveOrigin, l.Token, l.DeviceID,
		l.LinkProtocolVersion, l.State, l.LastSyncedAt, l.LastError, l.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: inserting link: %w", err)
	}
	return nil
}

// UpdateLinkCredential replaces everything a fresh invite settles: the peer's
// current name, the origins it now advertises, the one that answered, the new
// token and the agreed protocol version — and returns the Link to `connected`
// with no error on it (ADR-0055 §2, re-key in place).
//
// It deliberately does NOT touch created_at (the relationship is the same one)
// or last_synced_at (the mirror is still as fresh as it was; a re-key is not a
// resync). server_id is not settable at all: a row is bound to one peer for its
// whole life, and the caller checks the match before it gets here.
func (db *DB) UpdateLinkCredential(l Link) error {
	origins, err := encodeOrigins(l.Origins)
	if err != nil {
		return err
	}
	res, err := db.Exec(
		`UPDATE links
		    SET server_name = ?, origins = ?, active_origin = ?, token = ?,
		        device_id = ?, link_protocol_version = ?, state = ?, last_error = ''
		  WHERE id = ?`,
		l.ServerName, origins, l.ActiveOrigin, l.Token, l.DeviceID,
		l.LinkProtocolVersion, LinkStateConnected, l.ID)
	if err != nil {
		return fmt.Errorf("store: updating link credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: updating link credential: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Links lists every Link, oldest first — the order the Linked servers page
// shows them in, which is stable as Links come and go.
func (db *DB) Links() ([]Link, error) {
	rows, err := db.Query(
		`SELECT id, server_id, server_name, origins, active_origin, token, device_id,
		        link_protocol_version, state, last_synced_at, last_error, created_at
		   FROM links ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing links: %w", err)
	}
	defer rows.Close()

	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing links: %w", err)
	}
	return out, nil
}

// LinkByID reads one Link, ErrNotFound when there is none.
func (db *DB) LinkByID(id string) (Link, error) {
	return db.linkBy(`id = ?`, id)
}

// LinkByServerID reads the Link to a given peer, ErrNotFound when there is none.
// This is the re-key lookup: an invite carrying a server id already on file
// updates that Link instead of creating a second one (ADR-0055 §2).
func (db *DB) LinkByServerID(serverID string) (Link, error) {
	return db.linkBy(`server_id = ?`, serverID)
}

func (db *DB) linkBy(where string, arg string) (Link, error) {
	row := db.QueryRow(
		`SELECT id, server_id, server_name, origins, active_origin, token, device_id,
		        link_protocol_version, state, last_synced_at, last_error, created_at
		   FROM links WHERE `+where, arg)
	l, err := scanLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// DeleteLink removes a Link. This is the ONLY thing that deletes what came over
// it (ADR-0056 §6): neither `unreachable` nor `revoked` removes anything, so an
// operator whose friend rebooted keeps their Continue Watching.
func (db *DB) DeleteLink(id string) error {
	res, err := db.Exec(`DELETE FROM links WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting link: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetLinkState records the outcome of a call to the peer — the state machine of
// ADR-0056 §6. lastError is the human-readable reason, empty when there is none;
// it is always written, so recovering from `unreachable` clears the message that
// explained it rather than leaving a stale line on a healthy Link.
func (db *DB) SetLinkState(id, state, lastError string) error {
	res, err := db.Exec(
		`UPDATE links SET state = ?, last_error = ? WHERE id = ?`, state, lastError, id)
	if err != nil {
		return fmt.Errorf("store: setting link state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: setting link state: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// rowScanner is what *sql.Row and *sql.Rows have in common, so one scan serves
// both the single reads and the listing.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanLink(s rowScanner) (Link, error) {
	var l Link
	var origins string
	if err := s.Scan(&l.ID, &l.ServerID, &l.ServerName, &origins, &l.ActiveOrigin,
		&l.Token, &l.DeviceID, &l.LinkProtocolVersion, &l.State, &l.LastSyncedAt,
		&l.LastError, &l.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Link{}, err
		}
		return Link{}, fmt.Errorf("store: scanning link: %w", err)
	}
	if err := json.Unmarshal([]byte(origins), &l.Origins); err != nil {
		return Link{}, fmt.Errorf("store: decoding link origins: %w", err)
	}
	return l, nil
}

// encodeOrigins renders the origin list for storage. A nil list becomes "[]"
// rather than "null", so every row decodes back into a slice and no reader has
// to special-case a Link whose origins somehow went missing.
func encodeOrigins(origins []string) (string, error) {
	if origins == nil {
		origins = []string{}
	}
	raw, err := json.Marshal(origins)
	if err != nil {
		return "", fmt.Errorf("store: encoding link origins: %w", err)
	}
	return string(raw), nil
}
