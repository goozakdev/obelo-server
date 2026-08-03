package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Stream tokens (.scratch/session-stream-tokens): the short-lived, session-scoped
// media credential a player can carry in a URL path, for receivers that can send
// neither a bearer header nor the ms_media cookie. See
// migrations/0045_stream_tokens.sql for why this is a second table rather than an
// auth_tokens row.
//
// Every timestamp crossing this file is an RFC3339-UTC string supplied by the
// caller, never SQLite's datetime('now') — the same rule device_auth.go states,
// and for the same reason: expiry is compared in SQL, and the two formats do not
// compare.

// StreamToken is one minted media credential. It never carries the raw secret —
// only its hash, which is the lookup key. The raw value exists exactly once, in
// the response that mints it.
type StreamToken struct {
	TokenHash string
	SessionID string
	UserID    string
	CreatedAt string
	ExpiresAt string
}

// InsertStreamToken records a freshly minted token. The caller has already
// hashed the secret (auth owns the generator and the hash, ADR-0015); this
// layer never sees a raw one.
func (db *DB) InsertStreamToken(t StreamToken) error {
	if _, err := db.Exec(
		`INSERT INTO stream_tokens (token_hash, session_id, user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		t.TokenHash, t.SessionID, t.UserID, t.CreatedAt, t.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store: inserting stream token: %w", err)
	}
	return nil
}

// LiveStreamToken resolves an UNEXPIRED token by hash, reporting which session
// and User it authorises. The expiry predicate is IN the query, not compared in
// Go afterwards: an expired row is simply not found, so there is no window in
// which a caller holds a row it must remember to re-check.
//
// It is the token-first lookup the path-carried media routes need — the token
// names its session, so the URL does not have to. Returns ErrNotFound for an
// unknown, revoked, or expired token; the caller must not distinguish those on
// the wire (all three are one 404).
func (db *DB) LiveStreamToken(hash, now string) (StreamToken, error) {
	return db.streamTokenBy(`token_hash = ? AND expires_at > ?`, hash, now)
}

// LiveStreamTokenForSession resolves an unexpired token that ALSO belongs to
// sessionID. Both conditions live in the one WHERE clause, so "live" and
// "belongs to this session" are answered atomically — a token that authenticates
// but names a different session is not found, exactly like one that never
// existed.
func (db *DB) LiveStreamTokenForSession(hash, sessionID, now string) (StreamToken, error) {
	return db.streamTokenBy(`token_hash = ? AND session_id = ? AND expires_at > ?`, hash, sessionID, now)
}

func (db *DB) streamTokenBy(where string, args ...any) (StreamToken, error) {
	var t StreamToken
	err := db.QueryRow(
		`SELECT token_hash, session_id, user_id, created_at, expires_at
		   FROM stream_tokens WHERE `+where, args...,
	).Scan(&t.TokenHash, &t.SessionID, &t.UserID, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StreamToken{}, ErrNotFound
	}
	if err != nil {
		return StreamToken{}, fmt.Errorf("store: scanning stream token: %w", err)
	}
	return t, nil
}

// DeleteStreamTokensForSession revokes every token minted for one Playback
// session. This is the cascade the feature promises: session end IS revocation,
// whether the session ended cleanly (DELETE /sessions/{id}) or was swept by the
// idle reaper. Idempotent — a session with no tokens deletes nothing and
// succeeds, so the session-ended hook can run unconditionally.
func (db *DB) DeleteStreamTokensForSession(sessionID string) error {
	if _, err := db.Exec(
		`DELETE FROM stream_tokens WHERE session_id = ?`, sessionID,
	); err != nil {
		return fmt.Errorf("store: revoking stream tokens for session: %w", err)
	}
	return nil
}

// DeleteExpiredStreamTokens reaps aged-out rows. Called before each mint rather
// than on a timer, exactly like DeleteExpiredDeviceAuthRequests: a request-time
// sweep needs no background goroutine to own, stop, or leak.
//
// The session cascade above already removes tokens the moment their session
// ends, so in ordinary operation this sweeps nothing. It earns its keep across a
// RESTART: sessions are in-memory and vanish with the process, while these rows
// are on disk, so a crash leaves tokens whose session-ended hook will never fire.
// They authorise nothing (their session is gone) but they should not accumulate.
func (db *DB) DeleteExpiredStreamTokens(now string) error {
	if _, err := db.Exec(
		`DELETE FROM stream_tokens WHERE expires_at <= ?`, now,
	); err != nil {
		return fmt.Errorf("store: sweeping stream tokens: %w", err)
	}
	return nil
}
