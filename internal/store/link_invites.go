package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Link invites (ADR-0055 §1): the one-time credential a sharing Admin mints for
// a `remote` User and another Server redeems exactly once. See
// migrations/0060_link_invites.sql for the shape and for why there is only one
// secret here where the Device authorization grant has two.
//
// Every timestamp crossing this file is an RFC3339-UTC string supplied by the
// caller, never SQLite's datetime('now') — expiry is compared in SQL and the two
// formats do not compare (0041_device_auth.sql records the bug).

// LinkInvite is one minted invite. It never carries the raw code — only its
// hash, which is the lookup key and the only form that is ever stored.
type LinkInvite struct {
	CodeHash   string
	UserID     string
	CreatedAt  string
	ExpiresAt  string
	RedeemedAt string // "" until redeemed
}

// InsertLinkInvite records a fresh, unredeemed invite.
func (db *DB) InsertLinkInvite(inv LinkInvite) error {
	if _, err := db.Exec(
		`INSERT INTO link_invites (code_hash, user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		inv.CodeHash, inv.UserID, inv.CreatedAt, inv.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store: inserting link invite: %w", err)
	}
	return nil
}

// RedeemLinkInvite atomically claims an unspent, unexpired invite, stamping
// redeemed_at and reporting the row. This is the compare-and-swap that makes an
// invite single-use: two redemptions arriving together both run this UPDATE,
// exactly one affects a row, and only that one mints a token. Without the
// redeemed_at guard in the WHERE clause a racing pair would mint two sessions
// against one code, which is precisely the thing "single use" promises cannot
// happen.
//
// It returns ErrNotFound when nothing matched, and the caller must NOT try to
// tell the three reasons apart on the wire: unknown, expired and already-spent
// are one answer (INVALID_INVITE), because a caller who could distinguish them
// would hold an oracle over which codes exist.
func (db *DB) RedeemLinkInvite(hash, now string) (LinkInvite, error) {
	res, err := db.Exec(
		`UPDATE link_invites
		    SET redeemed_at = ?
		  WHERE code_hash = ? AND redeemed_at IS NULL AND expires_at > ?`,
		now, hash, now)
	if err != nil {
		return LinkInvite{}, fmt.Errorf("store: redeeming link invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return LinkInvite{}, fmt.Errorf("store: redeeming link invite: %w", err)
	}
	if n == 0 {
		return LinkInvite{}, ErrNotFound
	}
	// Safe to read after the CAS: the row is now spent and no other caller can
	// transition it, so the values cannot change under us.
	return db.LinkInviteByCodeHash(hash)
}

// LinkInviteByCodeHash reads one invite by its stored hash. Expired and spent
// rows are NOT filtered — this is the raw row, and every policy decision about
// it belongs to the caller.
func (db *DB) LinkInviteByCodeHash(hash string) (LinkInvite, error) {
	var inv LinkInvite
	var redeemedAt sql.NullString
	err := db.QueryRow(
		`SELECT code_hash, user_id, created_at, expires_at, redeemed_at
		   FROM link_invites WHERE code_hash = ?`, hash,
	).Scan(&inv.CodeHash, &inv.UserID, &inv.CreatedAt, &inv.ExpiresAt, &redeemedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkInvite{}, ErrNotFound
	}
	if err != nil {
		return LinkInvite{}, fmt.Errorf("store: scanning link invite: %w", err)
	}
	inv.RedeemedAt = redeemedAt.String
	return inv, nil
}

// DeleteUnredeemedLinkInvites drops a User's outstanding invites, so that
// minting a fresh one invalidates whatever the Admin sent before (ADR-0055 §1:
// re-linking means a fresh invite). SPENT rows are left alone — they are the
// record that a Link was established, and reaping them is the sweeper's job.
//
// Deleted rather than marked: a deleted unspent row reads as "never existed",
// which is the same INVALID_INVITE answer a guess gets, so nothing is disclosed
// by the difference.
func (db *DB) DeleteUnredeemedLinkInvites(userID string) error {
	if _, err := db.Exec(
		`DELETE FROM link_invites WHERE user_id = ? AND redeemed_at IS NULL`, userID,
	); err != nil {
		return fmt.Errorf("store: clearing link invites: %w", err)
	}
	return nil
}

// DeleteExpiredLinkInvites reaps aged-out rows, spent or not. Called before each
// mint rather than on a timer — the same cadence and the same reasoning as
// DeleteExpiredDeviceAuthRequests: the table only has to be tidy at the moment a
// code is minted, and a request-time sweep needs no background goroutine to own,
// stop, or leak.
//
// The sweep is not what makes an expired invite unredeemable. RedeemLinkInvite's
// WHERE clause is, so an invite dies on its expiry whether or not anybody has
// minted since.
func (db *DB) DeleteExpiredLinkInvites(now string) error {
	if _, err := db.Exec(
		`DELETE FROM link_invites WHERE expires_at <= ?`, now,
	); err != nil {
		return fmt.Errorf("store: sweeping link invites: %w", err)
	}
	return nil
}
