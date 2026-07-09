package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Remembered video persistence (selectable-video/04, ADR-0025, ADR-0023 mirrored). An
// explicit video Stream pick is stored per (User, Title) and, for an Episode, bubbles
// up as the (User, Show) default. What is stored is the pick's MEANING — the embedded
// title tag, falling back to the resolution/codec traits — never a stream index, so it
// re-resolves against the current File's Streams and survives a re-rip or remux. This
// layer is a thin upsert/read over two tables, the direct mirror of audio_memory.go;
// the trait re-resolution POLICY lives in the playback domain, not here.

// RememberedVideo is the meaning of a remembered video pick: the embedded title-tag
// label ("" = untagged, then the resolution/codec traits distinguish the pick) plus
// the traits the negotiation re-resolves by (the normalized video codec and the
// resolution). The zero value is a valid "untagged, unknown-codec, unknown-resolution"
// pick; presence is carried by the found bool the readers return (row-existence),
// never by a sentinel field.
type RememberedVideo struct {
	Label  string
	Codec  string
	Width  int
	Height int
}

// RememberedVideoForTitle returns the User's Remembered video for a Title, with
// found=false when there is no pick (the natural "no memory" default, so callers
// never special-case a missing row).
func (db *DB) RememberedVideoForTitle(userID, titleID string) (RememberedVideo, bool, error) {
	return db.rememberedVideo(
		`SELECT label, codec, width, height
		   FROM title_video_memory WHERE user_id = ? AND title_id = ?`,
		userID, titleID)
}

// RememberedVideoForShow returns the User's Show-level Remembered video (the
// bubble-up default for Episodes without their own pick), found=false when none.
func (db *DB) RememberedVideoForShow(userID, showID string) (RememberedVideo, bool, error) {
	return db.rememberedVideo(
		`SELECT label, codec, width, height
		   FROM show_video_memory WHERE user_id = ? AND show_id = ?`,
		userID, showID)
}

// rememberedVideo runs one of the two single-row memory reads. A missing row is
// (zero, false, nil) — "not remembered".
func (db *DB) rememberedVideo(query, userID, scopeID string) (RememberedVideo, bool, error) {
	var m RememberedVideo
	err := db.QueryRow(query, userID, scopeID).Scan(&m.Label, &m.Codec, &m.Width, &m.Height)
	if errors.Is(err, sql.ErrNoRows) {
		return RememberedVideo{}, false, nil
	}
	if err != nil {
		return RememberedVideo{}, false, fmt.Errorf("store: reading remembered video: %w", err)
	}
	return m, true, nil
}

// SaveRememberedVideoForTitle upserts the User's Remembered video for a Title to
// exactly the given meaning (last-write-wins on the UNIQUE(user_id, title_id)),
// refreshing updated_at. It never touches watch_state, so a pick and a progress
// report do not clobber each other.
func (db *DB) SaveRememberedVideoForTitle(userID, titleID string, m RememberedVideo) error {
	return db.saveRememberedVideo("title_video_memory", "title_id", userID, titleID, m)
}

// SaveRememberedVideoForShow upserts the Show-level bubble-up default, last-write-
// wins on UNIQUE(user_id, show_id). Unlike audio (which quarantines a commentary
// pick), every Episode video pick bubbles up — a video Stream has no commentary
// analogue to hold back.
func (db *DB) SaveRememberedVideoForShow(userID, showID string, m RememberedVideo) error {
	return db.saveRememberedVideo("show_video_memory", "show_id", userID, showID, m)
}

// saveRememberedVideo is the shared upsert for both memory tables. The table/scope
// column are trusted constants supplied by the two exported wrappers (never user
// input), so interpolating them is safe; the values are bound parameters.
func (db *DB) saveRememberedVideo(table, scopeCol, userID, scopeID string, m RememberedVideo) error {
	_, err := db.Exec(fmt.Sprintf(
		`INSERT INTO %s (id, user_id, %s, label, codec, width, height, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))
		 ON CONFLICT (user_id, %s) DO UPDATE SET
		   label      = excluded.label,
		   codec      = excluded.codec,
		   width      = excluded.width,
		   height     = excluded.height,
		   updated_at = excluded.updated_at`, table, scopeCol, scopeCol),
		uuid.NewString(), userID, scopeID, m.Label, m.Codec, m.Width, m.Height)
	if err != nil {
		return fmt.Errorf("store: saving remembered video: %w", err)
	}
	return nil
}
