package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Remembered audio persistence (audio-streams/05, ADR-0023). An explicit audio
// Stream pick is stored per (User, Title) and, for an Episode, bubbles up as the
// (User, Show) default. What is stored is the pick's MEANING — normalized language
// plus distinguishing traits — never a stream index, so it re-resolves against the
// current File's Streams and survives a re-rip or Edition switch. This layer is a
// thin upsert/read over two tables; the trait re-resolution POLICY lives in the
// playback domain, not here.

// RememberedAudio is the meaning of a remembered audio pick: the normalized
// ISO-639-1 language ("" = Unknown) plus the distinguishing traits the negotiation
// re-resolves by (the embedded title-tag label, the channel count, and the
// commentary disposition). The zero value is a valid "unknown-language, no-label,
// unknown-layout" pick; presence is carried by the found bool the readers return
// (row-existence), never by a sentinel field.
type RememberedAudio struct {
	Language   string
	Label      string
	Channels   int
	Commentary bool
}

// RememberedAudioForTitle returns the User's Remembered audio for a Title, with
// found=false when there is no pick (the natural "no memory" default, so callers
// never special-case a missing row).
func (db *DB) RememberedAudioForTitle(userID, titleID string) (RememberedAudio, bool, error) {
	return db.rememberedAudio(
		`SELECT language, label, channels, commentary
		   FROM title_audio_memory WHERE user_id = ? AND title_id = ?`,
		userID, titleID)
}

// RememberedAudioForShow returns the User's Show-level Remembered audio (the
// bubble-up default for Episodes without their own pick), found=false when none.
func (db *DB) RememberedAudioForShow(userID, showID string) (RememberedAudio, bool, error) {
	return db.rememberedAudio(
		`SELECT language, label, channels, commentary
		   FROM show_audio_memory WHERE user_id = ? AND show_id = ?`,
		userID, showID)
}

// rememberedAudio runs one of the two single-row memory reads. A missing row is
// (zero, false, nil) — "not remembered".
func (db *DB) rememberedAudio(query, userID, scopeID string) (RememberedAudio, bool, error) {
	var m RememberedAudio
	var commentary int
	err := db.QueryRow(query, userID, scopeID).Scan(&m.Language, &m.Label, &m.Channels, &commentary)
	if errors.Is(err, sql.ErrNoRows) {
		return RememberedAudio{}, false, nil
	}
	if err != nil {
		return RememberedAudio{}, false, fmt.Errorf("store: reading remembered audio: %w", err)
	}
	m.Commentary = commentary != 0
	return m, true, nil
}

// SaveRememberedAudioForTitle upserts the User's Remembered audio for a Title to
// exactly the given meaning (last-write-wins on the UNIQUE(user_id, title_id)),
// refreshing updated_at. It never touches watch_state, so a pick and a progress
// report do not clobber each other.
func (db *DB) SaveRememberedAudioForTitle(userID, titleID string, m RememberedAudio) error {
	return db.saveRememberedAudio("title_audio_memory", "title_id", userID, titleID, m)
}

// SaveRememberedAudioForShow upserts the Show-level bubble-up default, last-write-
// wins on UNIQUE(user_id, show_id). The negotiation writes it only for a
// non-commentary Episode pick (a commentary pick stays quarantined on its Title).
func (db *DB) SaveRememberedAudioForShow(userID, showID string, m RememberedAudio) error {
	return db.saveRememberedAudio("show_audio_memory", "show_id", userID, showID, m)
}

// saveRememberedAudio is the shared upsert for both memory tables. The table/scope
// column are trusted constants supplied by the two exported wrappers (never user
// input), so interpolating them is safe; the values are bound parameters.
func (db *DB) saveRememberedAudio(table, scopeCol, userID, scopeID string, m RememberedAudio) error {
	_, err := db.Exec(fmt.Sprintf(
		`INSERT INTO %s (id, user_id, %s, language, label, channels, commentary, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))
		 ON CONFLICT (user_id, %s) DO UPDATE SET
		   language   = excluded.language,
		   label      = excluded.label,
		   channels   = excluded.channels,
		   commentary = excluded.commentary,
		   updated_at = excluded.updated_at`, table, scopeCol, scopeCol),
		uuid.NewString(), userID, scopeID, m.Language, m.Label, m.Channels, boolToInt(m.Commentary))
	if err != nil {
		return fmt.Errorf("store: saving remembered audio: %w", err)
	}
	return nil
}

// ShowIDForTitle returns the Show id an Episode Title belongs to, found=false when
// the Title is not an Episode (a Movie / Track has no Show). It is the linkage the
// Remembered-audio bubble-up reads to store/resolve a Show-level pick, reusing the
// titles -> seasons -> shows join. A genuine store failure is returned as an error;
// a Movie is simply (,"", false, nil).
func (db *DB) ShowIDForTitle(titleID string) (string, bool, error) {
	var showID string
	err := db.QueryRow(
		`SELECT sh.id
		   FROM titles t
		   JOIN seasons s  ON s.id = t.season_id
		   JOIN shows   sh ON sh.id = s.show_id
		  WHERE t.id = ?`, titleID,
	).Scan(&showID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: reading show id for title: %w", err)
	}
	return showID, true, nil
}
