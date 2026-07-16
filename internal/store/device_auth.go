package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Device authorization requests (ADR-0036): the short-lived rows backing the
// "sign the TV in from your phone" flow. See migrations/0041_device_auth.sql for
// why the two codes are stored so differently.
//
// Every timestamp crossing this file is an RFC3339-UTC string supplied by the
// caller, never SQLite's datetime('now'). The migration comment explains why:
// this is the first table that compares timestamps in SQL, and the two formats
// do not compare.

// Device authorization request states. There is no 'denied': approval is
// immediate on code entry, so no screen exists to refuse from. See
// auth/device_auth.go for the reasoning and the recourse that replaces it.
const (
	DeviceAuthPending  = "pending"
	DeviceAuthApproved = "approved"
	DeviceAuthRedeemed = "redeemed"
)

// DeviceAuthRequest is one in-flight attempt to sign a Device in from another
// Device. It never carries the raw device code — only its hash, which is the
// lookup key.
type DeviceAuthRequest struct {
	DeviceCodeHash string
	UserCode       string
	ClientID       string
	DeviceName     string
	DevicePlatform string
	State          string
	ApprovedUserID string // "" until approved
	CreatedAt      string
	ExpiresAt      string
	LastPolledAt   string // "" until the first poll
}

// ErrUserCodeTaken reports that a user_code collided with a live request. The
// caller (which generates the codes) retries with a fresh one; with a 4-char
// code and a household-sized set of live requests this is vanishingly rare, but
// it is not impossible, and a collision must never hand two TVs one code.
var ErrUserCodeTaken = errors.New("store: user code already in use")

// InsertDeviceAuthRequest records a fresh pending request. It returns
// ErrUserCodeTaken if userCode is already held by a row that has not yet been
// swept, so the caller can re-roll rather than fail the sign-in.
func (db *DB) InsertDeviceAuthRequest(req DeviceAuthRequest) error {
	_, err := db.Exec(
		`INSERT INTO device_auth_requests
		        (device_code_hash, user_code, client_id, device_name, device_platform,
		         state, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		req.DeviceCodeHash, req.UserCode, req.ClientID, req.DeviceName,
		req.DevicePlatform, req.CreatedAt, req.ExpiresAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrUserCodeTaken
		}
		return fmt.Errorf("store: inserting device auth request: %w", err)
	}
	return nil
}

// DeviceAuthByUserCode looks up a request by the human-typed code. Expired rows
// are NOT filtered here — the caller distinguishes "no such code" from "your
// code expired", which are different words on screen.
func (db *DB) DeviceAuthByUserCode(userCode string) (DeviceAuthRequest, error) {
	return db.deviceAuthBy(`user_code = ?`, userCode)
}

// DeviceAuthByCodeHash looks up a request by the TV's poll secret.
func (db *DB) DeviceAuthByCodeHash(hash string) (DeviceAuthRequest, error) {
	return db.deviceAuthBy(`device_code_hash = ?`, hash)
}

func (db *DB) deviceAuthBy(where string, args ...any) (DeviceAuthRequest, error) {
	var r DeviceAuthRequest
	var approvedUserID, lastPolledAt sql.NullString
	err := db.QueryRow(
		`SELECT device_code_hash, user_code, client_id, device_name, device_platform,
		        state, approved_user_id, created_at, expires_at, last_polled_at
		   FROM device_auth_requests WHERE `+where, args...,
	).Scan(&r.DeviceCodeHash, &r.UserCode, &r.ClientID, &r.DeviceName, &r.DevicePlatform,
		&r.State, &approvedUserID, &r.CreatedAt, &r.ExpiresAt, &lastPolledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceAuthRequest{}, ErrNotFound
	}
	if err != nil {
		return DeviceAuthRequest{}, fmt.Errorf("store: scanning device auth request: %w", err)
	}
	r.ApprovedUserID = approvedUserID.String
	r.LastPolledAt = lastPolledAt.String
	return r, nil
}

// ApproveDeviceAuth marks a pending, unexpired request approved by userID. The
// state guard is in the WHERE clause, not a read-then-write, so two phones
// racing on one code produce exactly one approval. It returns ErrNotFound if no
// pending unexpired row matched — the caller has already read the row and can
// say which of "unknown", "expired", or "already used" it was.
func (db *DB) ApproveDeviceAuth(userCode, userID, now string) error {
	return db.setDeviceAuthState(
		`UPDATE device_auth_requests
		    SET state = 'approved', approved_user_id = ?
		  WHERE user_code = ? AND state = 'pending' AND expires_at > ?`,
		userID, userCode, now)
}

// RedeemDeviceAuth atomically claims an approved, unexpired request, moving it
// to 'redeemed' and reporting the approving User. This is the compare-and-swap
// that makes a device code one-shot: two polls arriving together both run this
// UPDATE, exactly one affects a row, and only that one mints a token. Without
// the state guard in the WHERE clause, a racing pair would mint two.
//
// It returns ErrNotFound if the request was not approved-and-unexpired.
func (db *DB) RedeemDeviceAuth(hash, now string) (DeviceAuthRequest, error) {
	res, err := db.Exec(
		`UPDATE device_auth_requests
		    SET state = 'redeemed'
		  WHERE device_code_hash = ? AND state = 'approved' AND expires_at > ?`,
		hash, now)
	if err != nil {
		return DeviceAuthRequest{}, fmt.Errorf("store: redeeming device auth request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return DeviceAuthRequest{}, fmt.Errorf("store: redeeming device auth request: %w", err)
	}
	if n == 0 {
		return DeviceAuthRequest{}, ErrNotFound
	}
	// Safe to read after the CAS: this row is now 'redeemed' and no other caller
	// can transition it, so the values cannot change under us.
	return db.DeviceAuthByCodeHash(hash)
}

// TouchDeviceAuthPoll records that the TV polled, returning the PREVIOUS poll
// time ("" on the first poll) so the caller can enforce the RFC 8628 slow-down
// rule against it.
//
// Read-then-write, deliberately not one atomic statement: two polls racing could
// both read the same previous time and neither would be told to slow down. That
// is the correct trade — the loser of the race is a client polling twice at
// once, which the interval exists to discourage rather than to punish, and the
// alternative (RETURNING the pre-update value) is not something SQLite offers
// without a subquery whose evaluation order is worth nobody's guess.
func (db *DB) TouchDeviceAuthPoll(hash, now string) (string, error) {
	var prev sql.NullString
	err := db.QueryRow(
		`SELECT last_polled_at FROM device_auth_requests WHERE device_code_hash = ?`, hash,
	).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: reading device auth poll time: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE device_auth_requests SET last_polled_at = ? WHERE device_code_hash = ?`,
		now, hash,
	); err != nil {
		return "", fmt.Errorf("store: touching device auth poll: %w", err)
	}
	return prev.String, nil
}

// CountLiveDeviceAuthRequests counts unexpired requests in any state. The
// generator caps on this: a 4-char code space is small enough that an unbounded
// flood of requests could crowd it, turning code generation into a retry loop
// and, at the limit, denying a real TV a code.
func (db *DB) CountLiveDeviceAuthRequests(now string) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM device_auth_requests WHERE expires_at > ?`, now,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting live device auth requests: %w", err)
	}
	return n, nil
}

// DeleteExpiredDeviceAuthRequests reaps expired rows, freeing their user_codes.
// Called before each generation rather than on a timer: the code space only has
// to be clear at the moment a code is minted, and a request-time sweep needs no
// background goroutine to own, stop, or leak.
func (db *DB) DeleteExpiredDeviceAuthRequests(now string) error {
	if _, err := db.Exec(
		`DELETE FROM device_auth_requests WHERE expires_at <= ?`, now,
	); err != nil {
		return fmt.Errorf("store: sweeping device auth requests: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure.
// The driver surfaces it only as a message, so we match on it — the same
// pragmatic check auth.isUniqueViolation makes. Duplicated rather than exported
// because the packages do not otherwise depend on each other for error shapes,
// and a store-level helper that only auth used would be the wrong home.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

func (db *DB) setDeviceAuthState(query string, args ...any) error {
	res, err := db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: updating device auth state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: updating device auth state: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
