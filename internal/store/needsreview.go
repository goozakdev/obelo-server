package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// needsreview.go is the Admin needs-review attention surface (the resolvable
// successor to the per-page client walk). `needs_review` is set by the scanner
// when a folder parsed without a year, or a TV Episode used non-SxxExx numbering
// (it browses fine, but the parse is uncertain). These reads collect every still-
// flagged item of a Library in one query each; the writes let an Admin dismiss a
// flag the parse got right (`reviewed = 1`, migration 0012), which sticks across
// rescans (see writeTitleRow / upsertShow).

// NeedsReviewItem is one entry on the needs-review attention list: a Title
// (Movie / Episode / Track) or a Show whose `needs_review` flag is still set. Path
// is a representative present on-disk file path, populated only for a Movie (whose
// files live directly in its folder); it is "" for Episodes/Tracks (whose file
// folder is a Season/Album subfolder, not the override key) and Shows. Anchor is
// the path a fix-match override must be keyed to for that Movie — derived from
// Path + the Library roots by the catalog service (see OverrideAnchor), "" when
// fix-match does not apply.
type NeedsReviewItem struct {
	ID     string
	Kind   string // "movie" | "episode" | "track" | "show"
	Title  string
	Year   int
	Path   string
	Anchor string
}

// TitlesNeedingReview returns the visible Titles (Movies, Episodes, Tracks) of a
// Library still flagged needs_review and not yet dismissed (reviewed = 0). Hidden
// (all-Files-Missing) Titles are excluded; ordered by sort title for stability. A
// Movie carries a representative present file path so the caller can offer a
// folder-keyed fix-match; other kinds leave Path empty.
func (db *DB) TitlesNeedingReview(libraryID string) ([]NeedsReviewItem, error) {
	rows, err := db.Query(
		`SELECT t.id, t.kind, t.title, t.year,
		        (SELECT f.path FROM editions e JOIN files f ON f.edition_id = e.id
		          WHERE e.title_id = t.id AND f.present = 1
		          ORDER BY f.path LIMIT 1) AS path
		   FROM titles t
		  WHERE t.library_id = ? AND t.needs_review = 1 AND t.reviewed = 0 AND t.hidden = 0
		  ORDER BY t.sort_title, t.id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: selecting titles needing review: %w", err)
	}
	defer rows.Close()
	var out []NeedsReviewItem
	for rows.Next() {
		var it NeedsReviewItem
		var year sql.NullInt64
		var path sql.NullString
		if err := rows.Scan(&it.ID, &it.Kind, &it.Title, &year, &path); err != nil {
			return nil, fmt.Errorf("store: scanning title needing review: %w", err)
		}
		it.Year = int(year.Int64)
		// Carry a file path for the kinds a folder-keyed fix-match can fix — a Movie
		// (keyed to its folder / the file itself) and a Track (keyed to its album
		// folder). An Episode's needs-review is a numbering problem that only
		// Enrichment maps, not a folder override, so it leaves Path empty (no fix).
		if path.Valid && (it.Kind == "movie" || it.Kind == "track") {
			it.Path = path.String
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ShowsNeedingReview returns the visible Shows of a Library still flagged
// needs_review and not yet dismissed. Each carries one present Episode file path so
// the caller can derive the Show folder (the override key) from it — a Show's
// override anchors to its top-level folder, which a non-hidden Show always has at
// least one Episode file under.
func (db *DB) ShowsNeedingReview(libraryID string) ([]NeedsReviewItem, error) {
	rows, err := db.Query(
		`SELECT sh.id, sh.title, sh.year,
		        (SELECT f.path FROM titles t
		           JOIN seasons se ON t.season_id = se.id
		           JOIN editions e ON e.title_id = t.id
		           JOIN files f ON f.edition_id = e.id
		          WHERE se.show_id = sh.id AND f.present = 1
		          ORDER BY f.path LIMIT 1) AS path
		   FROM shows sh
		  WHERE sh.library_id = ? AND sh.needs_review = 1 AND sh.reviewed = 0 AND sh.hidden = 0
		  ORDER BY sh.sort_title, sh.id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: selecting shows needing review: %w", err)
	}
	defer rows.Close()
	var out []NeedsReviewItem
	for rows.Next() {
		it := NeedsReviewItem{Kind: "show"}
		var year sql.NullInt64
		var path sql.NullString
		if err := rows.Scan(&it.ID, &it.Title, &year, &path); err != nil {
			return nil, fmt.Errorf("store: scanning show needing review: %w", err)
		}
		it.Year = int(year.Int64)
		if path.Valid {
			it.Path = path.String
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkTitleReviewed dismisses a Title's needs_review flag: an Admin confirmed the
// uncertain parse is fine. It sets reviewed = 1 (sticky across rescans) and clears
// needs_review immediately so every reader reflects it at once. ErrNotFound when
// no such Title.
func (db *DB) MarkTitleReviewed(id string) error {
	return markReviewed(db, "titles", id)
}

// MarkShowReviewed is MarkTitleReviewed for a Show.
func (db *DB) MarkShowReviewed(id string) error {
	return markReviewed(db, "shows", id)
}

// markReviewed is the shared dismiss UPDATE for the titles / shows tables (table
// is a fixed internal literal, never user input). ErrNotFound when the row is
// absent so the api layer answers 404.
func markReviewed(db *DB, table, id string) error {
	res, err := db.Exec(
		"UPDATE "+table+" SET reviewed = 1, needs_review = 0 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: marking %s reviewed: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: marking reviewed rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// NeedsReviewAnchor returns the path a fix-match override must be keyed to for a
// needs-review item of the given kind, derived from a representative file `path`
// and the Library `roots` — each kind matches how the scanner keys its override:
//
//   - movie: its folder, or the file itself when dropped loose at a root
//     (OverrideAnchor / resolveFolder + resolveBareFile)
//   - show:  its top-level folder under a root, since the Episode file is nested
//     in a Season subfolder (showFolder / resolveShowFolder)
//   - track: its containing (album) folder (filepath.Dir / music_resolve)
//   - episode: "" — a numbering problem Enrichment maps, not a folder override
//
// Returns "" for an empty path or an unfixable kind.
func NeedsReviewAnchor(kind, path string, roots []string) string {
	if path == "" {
		return ""
	}
	switch kind {
	case "movie":
		return OverrideAnchor(path, roots)
	case "show":
		return showFolder(path, roots)
	case "track":
		return filepath.Dir(path)
	default:
		return ""
	}
}

// OverrideAnchor returns the path a Movie's fix-match override must be keyed to. A
// file sitting directly in a Library root is a "bare file" and anchors to the FILE
// PATH (scanner's resolveBareFile keys overrides by the file path); a file inside a
// movie folder anchors to that FOLDER (resolveFolder keys by the folder). roots are
// the Library's (cleaned, absolute) root folder paths. This is why a yearless movie
// dropped loose at a root is still fixable: its override targets the one file, not
// the shared root.
func OverrideAnchor(path string, roots []string) string {
	if path == "" {
		return ""
	}
	parent := filepath.Dir(path) // already cleaned by filepath.Dir
	for _, r := range roots {
		if filepath.Clean(r) == parent {
			return filepath.Clean(path)
		}
	}
	return parent
}

// showFolder returns the top-level folder under a Library root that contains
// `path` — a Show folder, since an Episode lives nested under it (in a Season
// subfolder or directly). Falls back to the file's parent when `path` is not under
// any known root.
func showFolder(path string, roots []string) string {
	clean := filepath.Clean(path)
	for _, r := range roots {
		rc := filepath.Clean(r)
		rel, err := filepath.Rel(rc, clean)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // not under this root
		}
		if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
			return filepath.Join(rc, rel[:i]) // nested → the top-level Show folder
		}
		return clean // directly under the root (loose) → the file itself
	}
	return filepath.Dir(clean)
}
