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
	body := l.Text
	if l.Kind == lyrics.Synced {
		lines := make([]lyricLineJSON, len(l.Lines))
		for i, ln := range l.Lines {
			lines[i] = lyricLineJSON{StartMs: ln.StartMs, Text: ln.Text}
		}
		b, err := json.Marshal(lines)
		if err != nil {
			return fmt.Errorf("store: encoding synced lyrics: %w", err)
		}
		body = string(b)
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
	if lyrics.Kind(kind) != lyrics.Synced {
		return lyrics.Lyrics{Kind: lyrics.Plain, Text: body}, true, nil
	}
	var lines []lyricLineJSON
	if err := json.Unmarshal([]byte(body), &lines); err != nil {
		return lyrics.Lyrics{}, false, fmt.Errorf("store: decoding synced lyrics: %w", err)
	}
	out := lyrics.Lyrics{Kind: lyrics.Synced, Lines: make([]lyrics.Line, len(lines))}
	for i, ln := range lines {
		out.Lines[i] = lyrics.Line{StartMs: ln.StartMs, Text: ln.Text}
	}
	return out, true, nil
}
