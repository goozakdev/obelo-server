package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// needsreview.go is the Admin IDENTITY attention surface (the resolvable successor
// to the per-page client walk). It collects the two flags the scanner raises about
// how a Title was FILED — as opposed to how it was decorated, which is enrich.go's
// list:
//
//   - `needs_review` — the parse was a guess: a folder with no year, a TV Episode
//     numbered by date or absolute number, a Track with no usable tags. It browses
//     fine, but the identity is uncertain. An Admin can dismiss it (`reviewed = 1`,
//     migration 0012), which sticks across rescans (writeTitleRow / upsertShow).
//   - `ambiguous` — the collision rule: two Files parsed to the same Edition
//     identity and are not parts, so the convention refuses to guess which is the
//     real one ("flagged ambiguous in the web app, never silently guessed",
//     docs/naming-convention.md). It is NOT dismissible: the files really do
//     collide, one of them is not being played (store.Edition.Parts), and only
//     changing what is on disk — or, for an Episode, arranging the files in the
//     matcher — settles it.
//
// The two ride one list because they are one question for the Admin ("is this
// filed right?"), they are raised by the same scanner pass, and a Title can carry
// both at once. Each item says which flags it carries, so a reader never has to
// infer one from the other.
//
// These reads collect every still-flagged item of a Library in one query each.

// NeedsReviewItem is one entry on the identity attention list: a Title
// (Movie / Episode / Track) or a Show carrying a `needs_review` or `ambiguous`
// flag (see the file comment for what each means and how each is settled). Path
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
	// NeedsReview and Ambiguous say WHICH flags put this item on the list; at least
	// one is always set, and both can be. They are separate because they mean
	// different things and are settled differently — needs-review is a guess an
	// Admin can confirm ("looks right"), ambiguous is a real conflict that only a
	// change on disk or an arrangement can resolve.
	NeedsReview bool
	Ambiguous   bool
	// CollidingPaths are the Files that claim one Edition between them — the
	// evidence behind Ambiguous, and the only thing that makes the flag actionable:
	// "this title is ambiguous" without the two paths tells the Admin nothing they
	// can go and look at. Empty unless Ambiguous (and for a Show, which carries no
	// Files of its own).
	CollidingPaths []string
	// Context is the identifying breadcrumb the Admin needs to tell one flagged
	// item from another on sight — the Show/season/episode behind a bare episode
	// name, the Artist/Album behind a bare track name, and a representative file
	// path for every kind. Filled by the catalog service from TitleFixContexts /
	// ShowFixContexts, not by the queries above (see fixcontext.go).
	Context FixContext
}

// TitlesNeedingReview returns the visible Titles (Movies, Episodes, Tracks) of a
// Library flagged for identity attention: still needs_review and not yet dismissed
// (reviewed = 0), or ambiguous. Hidden (all-Files-Missing) Titles are excluded;
// ordered by sort title for stability. A Movie carries a representative present
// file path so the caller can offer a folder-keyed fix-match; other kinds leave
// Path empty.
//
// The `reviewed` dismissal deliberately gates only the needs_review half. An Admin
// saying "looks right" about an uncertain parse has said nothing about two files
// colliding, so an ambiguous Title stays listed until the collision itself is gone.
func (db *DB) TitlesNeedingReview(libraryID string) ([]NeedsReviewItem, error) {
	rows, err := db.Query(
		`SELECT t.id, t.kind, t.title, t.year,
		        t.needs_review AND NOT t.reviewed AS flagged, t.ambiguous,
		        (SELECT f.path FROM editions e JOIN files f ON f.edition_id = e.id
		          WHERE e.title_id = t.id AND f.present = 1
		          ORDER BY f.path LIMIT 1) AS path
		   FROM titles t
		  WHERE t.library_id = ? AND t.hidden = 0
		    AND ((t.needs_review = 1 AND t.reviewed = 0) OR t.ambiguous = 1)
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
		if err := rows.Scan(&it.ID, &it.Kind, &it.Title, &year,
			&it.NeedsReview, &it.Ambiguous, &path); err != nil {
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
		// A Show has no Files and no Edition, so it can never be ambiguous; it is on
		// this list for its needs_review flag alone.
		it := NeedsReviewItem{Kind: "show", NeedsReview: true}
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

// CollidingFilePaths returns, per Title id, the on-disk Files that CLAIM ONE
// EDITION between them — the evidence behind the `ambiguous` flag. Titles with no
// collision are simply absent from the map.
//
// It is a read rather than a stored list because the collision is a property of
// the Files, and store.Edition.Parts is already the one definition of it: an
// Edition with more than one present File that is not a numbered part set. Reading
// it here rather than recording it at scan time means the queue and the playback
// tiers can never disagree about which files are the problem — the row names
// exactly the Files that Parts is refusing to join.
//
// Ordered by (edition, part_ordinal, path), matching filesForEdition, so the first
// path listed is the one that actually plays.
func (db *DB) CollidingFilePaths(titleIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(titleIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(titleIDs)), ",")
	args := make([]any, len(titleIDs))
	for i, id := range titleIDs {
		args[i] = id
	}
	rows, err := db.Query(
		`SELECT e.title_id, e.id, f.path, f.part_ordinal
		   FROM editions e JOIN files f ON f.edition_id = e.id
		  WHERE e.title_id IN (`+placeholders+`) AND f.present = 1
		  ORDER BY e.title_id, e.id, f.part_ordinal, f.path`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading colliding files: %w", err)
	}
	defer rows.Close()

	// Accumulate one Edition at a time, then ask Parts whether it is a genuine
	// multi-part set. Files is all Parts needs (path + ordinal + present).
	var curTitle, curEdition string
	var files []File
	flush := func() {
		if len(files) > 1 && len(Edition{Files: files}.Parts()) < len(files) {
			paths := make([]string, 0, len(files))
			for _, f := range files {
				paths = append(paths, f.Path)
			}
			out[curTitle] = append(out[curTitle], paths...)
		}
		files = nil
	}
	for rows.Next() {
		var titleID, editionID, path string
		var ordinal int
		if err := rows.Scan(&titleID, &editionID, &path, &ordinal); err != nil {
			return nil, fmt.Errorf("store: scanning colliding file: %w", err)
		}
		if editionID != curEdition {
			flush()
			curTitle, curEdition = titleID, editionID
		}
		files = append(files, File{Path: path, PartOrdinal: ordinal, Present: true})
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading colliding files: %w", err)
	}
	return out, nil
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

// MarkShowEpisodesReviewed dismisses the needs-review flag on every still-flagged
// Episode of one Show, returning how many it cleared.
//
// It exists because the Needs-Fixing queue no longer has a row per flagged
// Episode: a Show's episode-level problems collapse into ONE row
// (file-matcher/07), so its "Looks right" has to settle the same set the row
// counted. Doing that as N calls from the browser would make a five-episode Show
// five requests that can half-succeed, leaving a row that says "3 episodes" for
// reasons the Admin cannot see.
//
// The semantics are exactly MarkTitleReviewed's, applied to a set: reviewed = 1
// (sticky across rescans) and needs_review cleared now. A Show with nothing
// flagged is not an error — it reports 0, which is the honest answer to "dismiss
// what is flagged" when nothing is.
func (db *DB) MarkShowEpisodesReviewed(showID string) (int, error) {
	res, err := db.Exec(
		`UPDATE titles SET reviewed = 1, needs_review = 0
		  WHERE needs_review = 1 AND reviewed = 0
		    AND season_id IN (SELECT id FROM seasons WHERE show_id = ?)`, showID)
	if err != nil {
		return 0, fmt.Errorf("store: marking show episodes reviewed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: marking show episodes reviewed rows affected: %w", err)
	}
	return int(n), nil
}
