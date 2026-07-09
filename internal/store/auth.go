package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by store lookups when no matching row exists. Callers
// translate it into the appropriate API response (e.g. 401 for an unknown
// token, 404 for an unknown device).
var ErrNotFound = errors.New("store: not found")

// User is a person with credentials on this server (CONTEXT.md). The first
// Admin is bootstrapped via the claim token (CreateAdmin); every later User —
// Admin or Member — is minted through CreateUser by an existing Admin.
type User struct {
	ID           string
	Username     string
	Role         string
	PasswordHash string
	CreatedAt    string
}

// Device is a first-class, named client installation belonging to a User,
// deduplicated per User by its stable ClientID (CONTEXT.md, ADR-0015).
type Device struct {
	ID         string
	UserID     string
	ClientID   string
	Name       string
	Platform   string
	CreatedAt  string
	LastSeenAt string
}

// CreateAdmin inserts the first Admin User with the given pre-hashed password.
// It is the only User-creating path this slice exposes; role is fixed to
// "admin". A duplicate username surfaces as a plain error for the caller to map.
func (db *DB) CreateAdmin(id, username, passwordHash string) (User, error) {
	_, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, 'admin', ?)`,
		id, username, passwordHash,
	)
	if err != nil {
		return User{}, fmt.Errorf("store: creating admin: %w", err)
	}
	return db.UserByID(id)
}

// CreateUser inserts a User with the given role and pre-hashed password — the
// management path an Admin uses to add Members (and further Admins) after the
// first-Admin bootstrap. role must be a known value ('admin' or 'member'); the
// caller validates it. A duplicate username surfaces as a UNIQUE-constraint
// error for the caller to map (the api layer answers 409).
func (db *DB) CreateUser(id, username, role, passwordHash string) (User, error) {
	_, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, ?, ?)`,
		id, username, role, passwordHash,
	)
	if err != nil {
		return User{}, fmt.Errorf("store: creating user: %w", err)
	}
	return db.UserByID(id)
}

// ListUsers returns every User, oldest first then by username for a stable
// order. The password hash is loaded (callers project away anything secret).
func (db *DB) ListUsers() ([]User, error) {
	rows, err := db.Query(
		`SELECT id, username, role, COALESCE(password_hash, ''), created_at
		   FROM users ORDER BY created_at, username`)
	if err != nil {
		return nil, fmt.Errorf("store: listing users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountAdmins returns how many Users hold the Admin role — the input to the
// last-Admin guard (the server must never be left with no one who can manage
// it).
func (db *DB) CountAdmins() (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting admins: %w", err)
	}
	return n, nil
}

// SetUserPassword replaces a User's password hash (Admin password reset).
// ErrNotFound if no such User.
func (db *DB) SetUserPassword(id, passwordHash string) error {
	res, err := db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("store: setting user password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: setting user password: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a User by id. Its Devices, tokens, and watch state cascade
// away via their ON DELETE CASCADE foreign keys, so this is a total, immediate
// revocation. ErrNotFound if no such User.
func (db *DB) DeleteUser(id string) error {
	res, err := db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting user: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserByUsername looks up a User by username, returning ErrNotFound if absent.
func (db *DB) UserByUsername(username string) (User, error) {
	return db.scanUser(db.QueryRow(
		`SELECT id, username, role, COALESCE(password_hash, ''), created_at
		   FROM users WHERE username = ?`, username))
}

// UserByID looks up a User by id, returning ErrNotFound if absent.
func (db *DB) UserByID(id string) (User, error) {
	return db.scanUser(db.QueryRow(
		`SELECT id, username, role, COALESCE(password_hash, ''), created_at
		   FROM users WHERE id = ?`, id))
}

func (db *DB) scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: scanning user: %w", err)
	}
	return u, nil
}

// UpsertDevice reuses the existing Device for (userID, clientID) if present —
// refreshing its name, platform, and last-seen — or creates a new one with
// newID otherwise. This is the clientId dedup contract (ADR-0015): re-login
// from a stable clientId never produces a duplicate Device. It returns the
// resolved Device row.
func (db *DB) UpsertDevice(newID, userID, clientID, name, platform string) (Device, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO devices (id, user_id, client_id, name, platform, last_seen_at)
		      VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, client_id)
		 DO UPDATE SET name = excluded.name,
		               platform = excluded.platform,
		               last_seen_at = excluded.last_seen_at`,
		newID, userID, clientID, name, platform, now,
	)
	if err != nil {
		return Device{}, fmt.Errorf("store: upserting device: %w", err)
	}
	return db.deviceBy(`user_id = ? AND client_id = ?`, userID, clientID)
}

// DevicesByUser lists a User's Devices, most-recently-seen first.
func (db *DB) DevicesByUser(userID string) ([]Device, error) {
	rows, err := db.Query(
		`SELECT id, user_id, client_id, name, platform, created_at, last_seen_at
		   FROM devices WHERE user_id = ? ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing devices: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.ClientID, &d.Name, &d.Platform, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, fmt.Errorf("store: scanning device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceByID looks up a single Device by id, returning ErrNotFound if absent.
func (db *DB) DeviceByID(id string) (Device, error) {
	return db.deviceBy(`id = ?`, id)
}

func (db *DB) deviceBy(where string, args ...any) (Device, error) {
	var d Device
	err := db.QueryRow(
		`SELECT id, user_id, client_id, name, platform, created_at, last_seen_at
		   FROM devices WHERE `+where, args...,
	).Scan(&d.ID, &d.UserID, &d.ClientID, &d.Name, &d.Platform, &d.CreatedAt, &d.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("store: scanning device: %w", err)
	}
	return d, nil
}

// DeleteDevice removes a Device and (via ON DELETE CASCADE) all of its tokens,
// revoking access for that Device immediately. It returns ErrNotFound if no
// such Device exists.
func (db *DB) DeleteDevice(id string) error {
	res, err := db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting device: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting device: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertToken records an opaque bearer token by its hash, bound to a Device and
// User. The raw token is never stored; only tokenHash reaches the database
// (ADR-0015).
func (db *DB) InsertToken(tokenHash, deviceID, userID string) error {
	_, err := db.Exec(
		`INSERT INTO auth_tokens (token_hash, device_id, user_id) VALUES (?, ?, ?)`,
		tokenHash, deviceID, userID,
	)
	if err != nil {
		return fmt.Errorf("store: inserting token: %w", err)
	}
	return nil
}

// TokenIdentity is the (User, Device) a valid token resolves to, returned by
// LookupToken for the auth middleware to attach to the request context.
type TokenIdentity struct {
	User   User
	Device Device
}

// LookupToken resolves a token hash to its User and Device, returning
// ErrNotFound if the token is unknown or has been revoked (its row deleted).
// It also refreshes the Device's last-seen timestamp as a side effect.
func (db *DB) LookupToken(tokenHash string) (TokenIdentity, error) {
	var deviceID, userID string
	err := db.QueryRow(
		`SELECT device_id, user_id FROM auth_tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&deviceID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenIdentity{}, ErrNotFound
	}
	if err != nil {
		return TokenIdentity{}, fmt.Errorf("store: looking up token: %w", err)
	}

	user, err := db.UserByID(userID)
	if err != nil {
		return TokenIdentity{}, err
	}
	device, err := db.DeviceByID(deviceID)
	if err != nil {
		return TokenIdentity{}, err
	}

	// Best-effort liveness touch; a failure here must not fail the request.
	_, _ = db.Exec(`UPDATE devices SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), deviceID)

	return TokenIdentity{User: user, Device: device}, nil
}

// DeleteToken revokes a single token by its hash (logout). It is a no-op if the
// token is already gone.
func (db *DB) DeleteToken(tokenHash string) error {
	if _, err := db.Exec(`DELETE FROM auth_tokens WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("store: deleting token: %w", err)
	}
	return nil
}
