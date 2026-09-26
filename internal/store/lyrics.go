package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goozakdev/obelo-server/internal/lyrics"
)

// Local lyrics: the words the Scanner read for a Track, one row per Track in the
// lyrics table with source 'local'. The Scanner owns the row — every scan of the
// Track rewrites it, and a scan that finds none deletes it — exactly as it owns
// the sidecar subtitle rows.
//
// Fetched lyrics: what the Lyric providers answered for a Track, one row per
// Track with source 'fetched', written only by internal/lyricfetch. A scan never
// touches it, and it is never written anywhere but this table.

// lyricLineJSON is one stored Synced line.
type lyricLineJSON struct {
	StartMs int64  `json:"startMs"`
	Text    string `json:"text"`
}

// writeLocalLyrics replaces a Track's Local lyrics with l, or clears them when l
// is nil. Runs inside the Track's upsert transaction.
func writeLocalLyrics(tx *sql.Tx, titleID string, l *lyrics.Lyrics) error {
	if _, err := tx.Exec(`DELETE FROM lyrics WHERE title_id = ? AND source = 'local'`, titleID); err != nil {
		return fmt.Errorf("store: clearing local lyrics: %w", err)
	}
	if l == nil {
		return nil
	}
	body, err := lyricsBody(*l)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO lyrics (title_id, source, kind, body) VALUES (?, 'local', ?, ?)`,
		titleID, string(l.Kind), body,
	); err != nil {
		return fmt.Errorf("store: inserting local lyrics: %w", err)
	}
	return nil
}

// LocalLyrics returns a Track's Local lyrics, and false when it has none.
func (db *DB) LocalLyrics(titleID string) (lyrics.Lyrics, bool, error) {
	var kind, body string
	err := db.QueryRow(
		`SELECT kind, body FROM lyrics WHERE title_id = ? AND source = 'local'`, titleID,
	).Scan(&kind, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return lyrics.Lyrics{}, false, nil
	}
	if err != nil {
		return lyrics.Lyrics{}, false, fmt.Errorf("store: reading local lyrics: %w", err)
	}
	l, err := parseLyricsBody(kind, body)
	if err != nil {
		return lyrics.Lyrics{}, false, err
	}
	return l, true, nil
}

// FetchedLyrics is a Track's remembered Lyric provider answer. Lyrics is nil for
// a remembered miss. Provider is the slug that answered ("" for a miss), and
// Question is what the providers were asked.
type FetchedLyrics struct {
	Lyrics   *lyrics.Lyrics
	Provider string
	Question string
}

// FetchedLyrics returns a Track's remembered Lyric provider answer, hit or miss,
// and false when the providers were never asked about it.
func (db *DB) FetchedLyrics(titleID string) (FetchedLyrics, bool, error) {
	var kind, body string
	var f FetchedLyrics
	err := db.QueryRow(
		`SELECT kind, body, provider, question FROM lyrics WHERE title_id = ? AND source = 'fetched'`, titleID,
	).Scan(&kind, &body, &f.Provider, &f.Question)
	if errors.Is(err, sql.ErrNoRows) {
		return FetchedLyrics{}, false, nil
	}
	if err != nil {
		return FetchedLyrics{}, false, fmt.Errorf("store: reading fetched lyrics: %w", err)
	}
	if kind != "none" {
		l, err := parseLyricsBody(kind, body)
		if err != nil {
			return FetchedLyrics{}, false, err
		}
		f.Lyrics = &l
	}
	return f, true, nil
}

// WriteFetchedLyrics replaces a Track's remembered Lyric provider answer with f:
// a hit when f.Lyrics is set, a miss when it is nil.
func (db *DB) WriteFetchedLyrics(titleID string, f FetchedLyrics) error {
	kind, body := "none", ""
	if f.Lyrics != nil {
		b, err := lyricsBody(*f.Lyrics)
		if err != nil {
			return err
		}
		kind, body = string(f.Lyrics.Kind), b
	}
	if _, err := db.Exec(
		`INSERT INTO lyrics (title_id, source, kind, body, provider, question)
		 VALUES (?, 'fetched', ?, ?, ?, ?)
		 ON CONFLICT (title_id, source) DO UPDATE SET
		     kind = excluded.kind, body = excluded.body,
		     provider = excluded.provider, question = excluded.question`,
		titleID, kind, body, f.Provider, f.Question,
	); err != nil {
		return fmt.Errorf("store: writing fetched lyrics: %w", err)
	}
	return nil
}

// RejectedLyrics returns the answers marked wrong for a Track, as the ids
// internal/lyricfetch named them.
func (db *DB) RejectedLyrics(titleID string) ([]string, error) {
	rows, err := db.Query(`SELECT answer FROM lyric_rejections WHERE title_id = ?`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: reading rejected lyrics: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var answer string
		if err := rows.Scan(&answer); err != nil {
			return nil, fmt.Errorf("store: scanning rejected lyrics: %w", err)
		}
		out = append(out, answer)
	}
	return out, rows.Err()
}

// maxLyricRejections is how many rejected answers a Track keeps: the most
// recent ones. Every asking reads them all, so they are bounded.
const maxLyricRejections = 20

// RejectFetchedLyrics records answer as wrong for a Track and forgets the
// Track's remembered Lyric provider answer, together, so the next open asks
// again knowing what to pass over. Past maxLyricRejections the oldest rejection
// is forgotten.
func (db *DB) RejectFetchedLyrics(titleID, answer string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: beginning lyrics rejection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO lyric_rejections (title_id, answer) VALUES (?, ?) ON CONFLICT DO NOTHING`, titleID, answer,
	); err != nil {
		return fmt.Errorf("store: recording rejected lyrics: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM lyric_rejections WHERE title_id = ? AND rowid NOT IN (
		     SELECT rowid FROM lyric_rejections WHERE title_id = ? ORDER BY rowid DESC LIMIT ?)`,
		titleID, titleID, maxLyricRejections,
	); err != nil {
		return fmt.Errorf("store: capping rejected lyrics: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM lyrics WHERE title_id = ? AND source = 'fetched'`, titleID); err != nil {
		return fmt.Errorf("store: forgetting rejected lyrics: %w", err)
	}
	return tx.Commit()
}

// LyricProviderOrder returns the Admin's Lyric provider order, first first.
func (db *DB) LyricProviderOrder() ([]string, error) {
	rows, err := db.Query(`SELECT slug FROM lyric_provider_order ORDER BY position, slug`)
	if err != nil {
		return nil, fmt.Errorf("store: reading lyric provider order: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("store: scanning lyric provider order: %w", err)
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// SetLyricProviderOrder replaces the Admin's Lyric provider order with slugs,
// first first.
func (db *DB) SetLyricProviderOrder(slugs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: beginning lyric provider order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM lyric_provider_order`); err != nil {
		return fmt.Errorf("store: clearing lyric provider order: %w", err)
	}
	for i, slug := range slugs {
		if _, err := tx.Exec(`INSERT INTO lyric_provider_order (slug, position) VALUES (?, ?)`, slug, i); err != nil {
			return fmt.Errorf("store: writing lyric provider order: %w", err)
		}
	}
	return tx.Commit()
}

// lyricsBody encodes l for the body column: the text of Plain lyrics, or the
// JSON array of lines of Synced ones.
func lyricsBody(l lyrics.Lyrics) (string, error) {
	if l.Kind != lyrics.Synced {
		return l.Text, nil
	}
	lines := make([]lyricLineJSON, len(l.Lines))
	for i, ln := range l.Lines {
		lines[i] = lyricLineJSON{StartMs: ln.StartMs, Text: ln.Text}
	}
	b, err := json.Marshal(lines)
	if err != nil {
		return "", fmt.Errorf("store: encoding synced lyrics: %w", err)
	}
	return string(b), nil
}

// parseLyricsBody decodes a stored row of the given kind.
func parseLyricsBody(kind, body string) (lyrics.Lyrics, error) {
	if lyrics.Kind(kind) != lyrics.Synced {
		return lyrics.Lyrics{Kind: lyrics.Plain, Text: body}, nil
	}
	var lines []lyricLineJSON
	if err := json.Unmarshal([]byte(body), &lines); err != nil {
		return lyrics.Lyrics{}, fmt.Errorf("store: decoding synced lyrics: %w", err)
	}
	out := lyrics.Lyrics{Kind: lyrics.Synced, Lines: make([]lyrics.Line, len(lines))}
	for i, ln := range lines {
		out.Lines[i] = lyrics.Line{StartMs: ln.StartMs, Text: ln.Text}
	}
	return out, nil
}
