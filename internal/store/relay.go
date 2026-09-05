package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The id translation a relay needs (ADR-0056 §5).
//
// A mirrored row carries the sharer's id in `remote_id` (ADR-0056 §3, issue 07),
// and a relay is the one operation that has to speak BOTH sides' ids in the same
// breath: the client names a local Title, the sharer only understands its own.
// These three reads are that translation and nothing else — they are deliberately
// not part of the mirror's write half, which never needs to look a row up by the
// id this Server minted for it.
//
// The kinds are the mirror's own (store/mirror.go): the ten wire types collapse
// to eight tables, because a Movie, an Episode and a Track are all `titles`.

// remoteIDTable maps a mirrored entity kind to the table carrying its remote_id.
// An unknown kind resolves to "" and every read below then answers ErrNotFound,
// so a typo is a miss rather than a query against a table name from a caller.
func remoteIDTable(kind string) string {
	switch kind {
	case ExportTitle, ExportEpisode, ExportTrack:
		return "titles"
	case ExportEdition:
		return "editions"
	case ExportFile:
		return "files"
	case ExportStream:
		return "streams"
	case EntityShow:
		return "shows"
	case EntitySeason:
		return "seasons"
	case EntityArtist:
		return "artists"
	case EntityAlbum:
		return "albums"
	default:
		return ""
	}
}

// RemoteIDOf returns the sharer's id for a mirrored row, or ErrNotFound when the
// row is unknown or is not mirrored at all (a local row's remote_id is NULL).
func (db *DB) RemoteIDOf(kind, id string) (string, error) {
	table := remoteIDTable(kind)
	if table == "" || id == "" {
		return "", ErrNotFound
	}
	var remote sql.NullString
	err := db.QueryRow(`SELECT remote_id FROM `+table+` WHERE id = ?`, id).Scan(&remote)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: reading remote id: %w", err)
	}
	if !remote.Valid || remote.String == "" {
		return "", ErrNotFound
	}
	return remote.String, nil
}

// LocalIDForRemote is the other direction: this Server's id for one of the
// sharer's. It is how a relay Decision's `edition.id` — which the sharer wrote,
// and which means nothing here — becomes the local Edition whose duration the
// Session is measured against.
//
// A remote id is the other Server's UUID, so it is looked up without a Library
// scope: `editions`, `files` and `streams` reach their Library only through a
// join, and issue 07's index is per-table for exactly that reason.
func (db *DB) LocalIDForRemote(kind, remoteID string) (string, error) {
	table := remoteIDTable(kind)
	if table == "" || remoteID == "" {
		return "", ErrNotFound
	}
	var id string
	err := db.QueryRow(`SELECT id FROM `+table+` WHERE remote_id = ?`, remoteID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: resolving remote id: %w", err)
	}
	return id, nil
}

// MirrorStamp returns a mirrored row's `updated_at` — the instant this Server
// last applied a change to it from the feed.
//
// It is the relayed artwork cache's freshness signal (issue 09): the bytes behind
// a mirrored poster are cached under a name keyed by the sharer's id, and a
// cached file older than the row it belongs to is re-fetched. The stamp is
// maintained by migration 0061's triggers, so it moves whenever ApplyMirror
// writes the row and not otherwise.
func (db *DB) MirrorStamp(kind, id string) (string, error) {
	table := remoteIDTable(kind)
	if table == "" || id == "" {
		return "", ErrNotFound
	}
	var stamp sql.NullString
	err := db.QueryRow(`SELECT updated_at FROM `+table+` WHERE id = ?`, id).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: reading update stamp: %w", err)
	}
	return stamp.String, nil
}
