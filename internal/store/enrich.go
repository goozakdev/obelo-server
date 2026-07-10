package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Enrichment persistence (external-metadata-enrichment). The Enrichment step
// (ADR-0002) decorates a Title the scanner already filed; everything here writes
// descriptive/artwork/bookkeeping rows and NEVER identity (identity_key / watch
// state are untouched). Reads select candidates for a pass; the write applies a
// resolved result, honoring Locked fields (CONTEXT.md) so a hand-edited field is
// never overwritten.

// EnrichSelect chooses which Titles a pass considers.
type EnrichSelect int

const (
	// EnrichPending selects only Titles never successfully enriched
	// (enrichment_status='pending') — the only-new pass + the auto-after-scan path.
	EnrichPending EnrichSelect = iota
	// EnrichAll selects every visible Title for a full refresh (still unlocked-only).
	EnrichAll
)

// TitlesForEnrichment returns the visible (non-hidden) Titles of a Library that
// a pass should consider, oldest-added first for a stable order. Hidden
// (all-Files-Missing) Titles are skipped — enrichment doesn't spend calls on
// soft-deleted media (ADR-0008); they re-enter as 'pending' when they return.
func (db *DB) TitlesForEnrichment(libraryID string, sel EnrichSelect) ([]Title, error) {
	where := "library_id = ? AND hidden = 0"
	if sel == EnrichPending {
		where += " AND enrichment_status = 'pending'"
	}
	rows, err := db.Query(
		`SELECT `+enrichedTitleColumns+`
		   FROM titles WHERE `+where+` ORDER BY added_at, id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: selecting titles for enrichment: %w", err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		t, err := scanEnrichedTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ExternalMatch is the external id an Admin assigns to re-point a Title's
// Enrichment lookup (PUT /titles/{id}/enrichmentMatch). Only the non-empty ids
// are written; identity_key and watch state are NEVER touched (ADR-0002/0014) —
// this is the metadata match, deliberately distinct from an identity fix-match
// (which re-keys identity).
type ExternalMatch struct {
	TMDBID        string
	IMDBID        string
	MusicbrainzID string
}

// SetTitleExternalMatch writes the supplied external id(s) onto a Title and
// resets its enrichment_status to 'pending' so the next lookup re-resolves by the
// new id. It touches ONLY the external-id columns + status — never identity_key,
// season/episode numbers, or any identity field — so the Title keeps its parsed
// identity and watch state (ADR-0002/0014). An empty id leaves that column
// unchanged. Returns ErrNotFound for an unknown Title.
func (db *DB) SetTitleExternalMatch(titleID string, m ExternalMatch) error {
	res, err := db.Exec(
		`UPDATE titles SET
		     tmdb_id        = CASE WHEN ? <> '' THEN ? ELSE tmdb_id END,
		     imdb_id        = CASE WHEN ? <> '' THEN ? ELSE imdb_id END,
		     musicbrainz_id = CASE WHEN ? <> '' THEN ? ELSE musicbrainz_id END,
		     enrichment_status = 'pending'
		   WHERE id = ?`,
		m.TMDBID, m.TMDBID, m.IMDBID, m.IMDBID, m.MusicbrainzID, m.MusicbrainzID, titleID,
	)
	if err != nil {
		return fmt.Errorf("store: setting external match: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: external match rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TitleForEnrichmentByID returns one Title with its enrichment columns for a
// single-Title re-enrich (PUT /enrichmentMatch). ErrNotFound for an unknown id.
func (db *DB) TitleForEnrichmentByID(titleID string) (Title, error) {
	row := db.QueryRow(`SELECT `+enrichedTitleColumns+` FROM titles WHERE id = ?`, titleID)
	t, err := scanEnrichedTitle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Title{}, ErrNotFound
	}
	if err != nil {
		return Title{}, fmt.Errorf("store: scanning title for enrichment: %w", err)
	}
	return t, nil
}

// TitlesNeedingMatch returns the visible Titles of a Library whose Enrichment
// could not settle on a record — enrichment_status 'unmatched' or 'failed' — the
// Admin attention surface for hand-matching (CONTEXT.md). It is deliberately
// distinct from the identity Unmatched files (recognized media with no Title) and
// from needs-review Titles; here the Title browses fine but its descriptive
// metadata is missing. Hidden (all-Files-Missing) Titles are excluded; ordered by
// sort title for a stable list.
func (db *DB) TitlesNeedingMatch(libraryID string) ([]Title, error) {
	rows, err := db.Query(
		`SELECT `+enrichedTitleColumns+`
		   FROM titles
		  WHERE library_id = ? AND hidden = 0
		    AND enrichment_status IN ('unmatched', 'failed')
		  ORDER BY sort_title, id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: selecting titles needing match: %w", err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		t, err := scanEnrichedTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTitleEnrichmentStatus records a terminal non-matched outcome for a Title
// (unmatched / failed / disabled) without touching its descriptive fields — the
// Title keeps whatever metadata it had and stays browsable (graceful
// degradation, ADR-0001).
func (db *DB) SetTitleEnrichmentStatus(titleID, status string) error {
	if _, err := db.Exec(
		`UPDATE titles SET enrichment_status = ?, enriched_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), titleID,
	); err != nil {
		return fmt.Errorf("store: setting enrichment status: %w", err)
	}
	return nil
}

// LockedFields returns the set of Locked field names for a Title (CONTEXT.md):
// a field present here was hand-edited and must not be overwritten by
// enrichment. Empty for a Title with no manual edits.
func (db *DB) LockedFields(titleID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT field FROM title_field_locks WHERE title_id = ?`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: reading locked fields: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("store: scanning locked field: %w", err)
		}
		out[f] = true
	}
	return out, rows.Err()
}

// TitleEnrichmentForMany bulk-reads the enrichment display fields (overview,
// enriched_title, status) for a page of Titles, keyed by id (absent = un-enriched
// zero value). Lets the Home + search readers — which use the lean scanTitle
// projection — attach enrichment without switching their bespoke queries.
func (db *DB) TitleEnrichmentForMany(ids []string) (map[string]Title, error) {
	out := map[string]Title{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(
		`SELECT id, overview, enriched_title, enrichment_status FROM titles
		   WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading title enrichment: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t Title
		if err := rows.Scan(&t.ID, &t.Overview, &t.EnrichedTitle, &t.EnrichmentStatus); err != nil {
			return nil, fmt.Errorf("store: scanning title enrichment: %w", err)
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}

// GenresForTitles bulk-reads the genres for a page of Titles, keyed by Title id
// (absent = no genres). Lets a browse list attach genres without an N+1 query,
// mirroring WatchStatesForTitles.
func (db *DB) GenresForTitles(titleIDs []string) (map[string][]string, error) {
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
		`SELECT title_id, genre FROM title_genres
		   WHERE title_id IN (`+placeholders+`) ORDER BY title_id, ord, genre`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading genres: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, g string
		if err := rows.Scan(&id, &g); err != nil {
			return nil, fmt.Errorf("store: scanning bulk genre: %w", err)
		}
		out[id] = append(out[id], g)
	}
	return out, rows.Err()
}

// ArtworkVersionsForTitles bulk-reads a per-Title artwork "version" for a page of
// Titles, keyed by id (absent = the Title has no artwork). The version is the
// newest added_at across the Title's artwork rows — and because a (re-)enrich
// replaces a role's fetched row (DELETE + INSERT, so added_at re-defaults to
// now), and a rescan likewise rewrites the local row, that timestamp changes
// exactly when the served bytes could have changed. A browse client uses it as
// the poster cache-bust token so a re-fetched image reloads while text-only
// edits leave the poster untouched. Mirrors GenresForTitles (no N+1).
func (db *DB) ArtworkVersionsForTitles(titleIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(titleIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(titleIDs)), ",")
	args := make([]any, len(titleIDs))
	for i, id := range titleIDs {
		args[i] = id
	}
	rows, err := db.Query(
		`SELECT title_id, MAX(added_at) FROM artwork
		   WHERE title_id IN (`+placeholders+`) GROUP BY title_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading artwork versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("store: scanning bulk artwork version: %w", err)
		}
		out[id] = version
	}
	return out, rows.Err()
}

// MetadataEdit is an Admin's hand-edit of a Title's descriptive fields (the
// write side of PUT /titles/{id}/metadata). Every non-nil scalar / slice field
// is written AND Locked, so a subsequent enrich pass never overwrites it
// (CONTEXT.md "Locked field"); a nil field is left untouched (not locked). The
// lock field names match the ones WriteTitleEnrichment honors, so a hand-edit
// here is exactly what a later pass skips.
type MetadataEdit struct {
	Overview       *string
	Tagline        *string
	ContentRating  *string
	ReleaseDate    *string
	RuntimeMinutes *int
	Studio         *string
	// Name edits the canonical DISPLAY title (enriched_title); its lock field is
	// "title" (never the parsed identity Title, which stays untouched — ADR-0002).
	Name   *string
	Genres *[]string
	Cast   *[]Credit
	// LockArtwork names artwork roles to pin against a refresh ('poster' /
	// 'background' / 'logo'). The currently-chosen image is kept; the role is just
	// locked so the next pass skips fetching it (the hand-chosen poster sticks).
	LockArtwork []string
}

// WriteTitleMetadata applies an Admin hand-edit in one transaction: it writes
// each supplied descriptive field and records a Lock for it (CONTEXT.md), so
// re-enrichment refreshes only the unlocked fields. Genres/Cast are rebuilt
// wholesale (like an enrich write) when supplied. Identity is never touched —
// only descriptive columns, child rows, and lock rows change. The caller has
// already confirmed the Title exists (so the lock FK is satisfied).
func (db *DB) WriteTitleMetadata(titleID string, edit MetadataEdit) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin write metadata: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lock := func(field string) error {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO title_field_locks (title_id, field) VALUES (?, ?)`,
			titleID, field,
		); err != nil {
			return fmt.Errorf("store: locking field %q: %w", field, err)
		}
		return nil
	}
	// setStr writes a scalar TEXT column and locks its field; the column name is a
	// fixed literal (never user input), so the string-built SQL is safe.
	setStr := func(col, field, val string) error {
		if _, err := tx.Exec(`UPDATE titles SET `+col+` = ? WHERE id = ?`, val, titleID); err != nil {
			return fmt.Errorf("store: editing %q: %w", field, err)
		}
		return lock(field)
	}

	if edit.Overview != nil {
		if err := setStr("overview", "overview", *edit.Overview); err != nil {
			return err
		}
	}
	if edit.Tagline != nil {
		if err := setStr("tagline", "tagline", *edit.Tagline); err != nil {
			return err
		}
	}
	if edit.ContentRating != nil {
		if err := setStr("content_rating", "content_rating", *edit.ContentRating); err != nil {
			return err
		}
	}
	if edit.ReleaseDate != nil {
		if err := setStr("release_date", "release_date", *edit.ReleaseDate); err != nil {
			return err
		}
	}
	if edit.Studio != nil {
		if err := setStr("studio", "studio", *edit.Studio); err != nil {
			return err
		}
	}
	if edit.Name != nil {
		// Display title lives in enriched_title; its lock field is "title".
		if _, err := tx.Exec(`UPDATE titles SET enriched_title = ? WHERE id = ?`, *edit.Name, titleID); err != nil {
			return fmt.Errorf("store: editing display title: %w", err)
		}
		if err := lock("title"); err != nil {
			return err
		}
	}
	if edit.RuntimeMinutes != nil {
		if _, err := tx.Exec(`UPDATE titles SET runtime_minutes = ? WHERE id = ?`, *edit.RuntimeMinutes, titleID); err != nil {
			return fmt.Errorf("store: editing runtime: %w", err)
		}
		if err := lock("runtime_minutes"); err != nil {
			return err
		}
	}
	if edit.Genres != nil {
		if _, err := tx.Exec(`DELETE FROM title_genres WHERE title_id = ?`, titleID); err != nil {
			return fmt.Errorf("store: clearing genres: %w", err)
		}
		for i, g := range *edit.Genres {
			if _, err := tx.Exec(
				`INSERT INTO title_genres (title_id, genre, ord) VALUES (?, ?, ?)`,
				titleID, g, i,
			); err != nil {
				return fmt.Errorf("store: inserting genre %q: %w", g, err)
			}
		}
		if err := lock("genres"); err != nil {
			return err
		}
	}
	if edit.Cast != nil {
		if _, err := tx.Exec(`DELETE FROM title_credits WHERE title_id = ?`, titleID); err != nil {
			return fmt.Errorf("store: clearing credits: %w", err)
		}
		for i, c := range *edit.Cast {
			kind := c.Kind
			if kind == "" {
				kind = "cast"
			}
			if _, err := tx.Exec(
				`INSERT INTO title_credits (title_id, person, role, character, kind, ord)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				titleID, c.Person, c.Role, c.Character, kind, i,
			); err != nil {
				return fmt.Errorf("store: inserting credit %q: %w", c.Person, err)
			}
		}
		if err := lock("cast"); err != nil {
			return err
		}
	}
	for _, role := range edit.LockArtwork {
		if err := lock(role); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit write metadata: %w", err)
	}
	return nil
}

// PickTitleArtwork applies an Admin-chosen provider image to a Title's artwork
// role and Locks that role, in one transaction (Fix label image picker, ADR-0019).
// The image is stored as the role's 'fetched' row (replacing any prior fetched row
// for the role) and the role is Locked, so a later enrich pass skips re-fetching it
// — the hand-picked image sticks. A LOCAL image for the role still wins at serve
// time (ArtworkByTitleRole orders local first), exactly as an auto-fetched image
// does. The caller has already downloaded+cached the bytes and confirmed the Title
// exists. path is the on-disk cache path; artworkID a fresh id for the row.
func (db *DB) PickTitleArtwork(titleID, role, path, artworkID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin pick artwork: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM artwork WHERE title_id = ? AND role = ? AND source = 'fetched'`,
		titleID, role,
	); err != nil {
		return fmt.Errorf("store: clearing fetched artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO artwork (id, title_id, role, path, source) VALUES (?, ?, ?, ?, 'fetched')`,
		artworkID, titleID, role, path,
	); err != nil {
		return fmt.Errorf("store: inserting picked artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO title_field_locks (title_id, field) VALUES (?, ?)`,
		titleID, role,
	); err != nil {
		return fmt.Errorf("store: locking artwork role %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit pick artwork: %w", err)
	}
	return nil
}

// UploadTitleArtwork records an Admin-uploaded image for a leaf Title's artwork
// role and Locks that role, in one transaction (ADR-0026). The bytes are already
// written to the artwork cache under a source-qualified name; this inserts the
// 'uploaded' row and Locks the role so a re-enrich keeps it. Uploading IS
// selecting: a new upload REPLACES any prior upload for the role (Option B, no
// pool). Local/fetched rows are left untouched — they are the "auto" image the
// role reverts to when the Lock is released, and an 'uploaded' row outranks both
// at serve time (ArtworkByTitleRole orders uploaded first). Returns the on-disk
// path of the REPLACED prior upload (empty if none) so the caller can delete a
// now-orphaned file when a re-upload changed the extension. path is the cache
// path; artworkID a fresh id for the row.
func (db *DB) UploadTitleArtwork(titleID, role, path, artworkID string) (replacedPath string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin upload artwork: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRow(
		`SELECT path FROM artwork WHERE title_id = ? AND role = ? AND source = 'uploaded'`,
		titleID, role,
	).Scan(&replacedPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: reading prior uploaded artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM artwork WHERE title_id = ? AND role = ? AND source = 'uploaded'`,
		titleID, role,
	); err != nil {
		return "", fmt.Errorf("store: clearing uploaded artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO artwork (id, title_id, role, path, source) VALUES (?, ?, ?, ?, 'uploaded')`,
		artworkID, titleID, role, path,
	); err != nil {
		return "", fmt.Errorf("store: inserting uploaded artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO title_field_locks (title_id, field) VALUES (?, ?)`,
		titleID, role,
	); err != nil {
		return "", fmt.Errorf("store: locking artwork role %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit upload artwork: %w", err)
	}
	return replacedPath, nil
}

// ReleaseTitleArtworkLock releases a leaf Title's Lock on an artwork role and,
// when that role is backed by an Admin upload, deletes the 'uploaded' row so the
// role reverts to its auto image (Fetched/Local) — the lock-coupled "undo my
// upload" (ADR-0026). It returns the on-disk path of the deleted upload (empty
// when the role had no upload) so the caller removes the cached bytes. A role
// with no upload behaves exactly like ReleaseFieldLock: only the lock is dropped
// and the fetched/local rows are left for the next enrich pass.
func (db *DB) ReleaseTitleArtworkLock(titleID, role string) (removedPath string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin release artwork lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRow(
		`SELECT path FROM artwork WHERE title_id = ? AND role = ? AND source = 'uploaded'`,
		titleID, role,
	).Scan(&removedPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: reading uploaded artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM artwork WHERE title_id = ? AND role = ? AND source = 'uploaded'`,
		titleID, role,
	); err != nil {
		return "", fmt.Errorf("store: deleting uploaded artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM title_field_locks WHERE title_id = ? AND field = ?`, titleID, role,
	); err != nil {
		return "", fmt.Errorf("store: releasing artwork lock %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit release artwork lock: %w", err)
	}
	return removedPath, nil
}

// ReleaseFieldLock removes a Title's Lock for one field so the next enrich pass
// refreshes it again (CONTEXT.md "a lock is releasable back to auto"). Releasing
// an absent lock is a no-op (idempotent). The field's current value is left as
// is — only the lock is dropped; enrichment will overwrite it on the next pass.
func (db *DB) ReleaseFieldLock(titleID, field string) error {
	if _, err := db.Exec(
		`DELETE FROM title_field_locks WHERE title_id = ? AND field = ?`, titleID, field,
	); err != nil {
		return fmt.Errorf("store: releasing lock %q: %w", field, err)
	}
	return nil
}

// LockedFieldsSorted returns a Title's Locked field names in a stable order, for
// the lockedFields[] the Title detail reports (a client shows which fields are
// pinned and which it may release).
func (db *DB) LockedFieldsSorted(titleID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT field FROM title_field_locks WHERE title_id = ? ORDER BY field`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: reading locked fields: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("store: scanning locked field: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// TitleEnrichment is a resolved external-metadata result ready to persist. The
// store writes only the UNLOCKED fields; Genres/Cast/Artwork are rebuilt
// wholesale (idempotent) unless their group is locked.
type TitleEnrichment struct {
	Overview       string
	Tagline        string
	ContentRating  string
	ReleaseDate    string
	RuntimeMinutes int
	Studio         string
	Source         string // provider id, e.g. "tmdb"
	// Name is the canonical DISPLAY title for an Episode/Track (written to
	// enriched_title) — never identity. Empty leaves enriched_title unchanged, so
	// a Movie (no canonical name) keeps its parsed title as the display title.
	Name   string
	Genres []string
	Cast   []Credit
	// Artwork is the set of fetched images already written to the artwork cache;
	// each carries Role + Path. Source is forced to 'fetched' on write.
	Artwork []Artwork
	// ExternalIDs carries the id(s) of the provider record this result was
	// resolved FROM, persisted onto the Title's external-id columns FILL-ONLY: a
	// column that already has an id keeps it — a {tmdb-…} token is identity
	// authority (ADR-0002) and a Fix-info pin is an Admin's durable override, and
	// neither may be rewritten by whatever a provider response reports. The leaf
	// analogue of EntityEnrichmentWrite.ExternalID: without it a search-resolved
	// Title has no stored id for the LIVE artwork-candidate lookup to key on, and
	// every Edit-item image tab comes back empty.
	ExternalIDs ExternalMatch
}

// WriteTitleEnrichment persists a matched enrichment result for a Title in one
// transaction: it overlays the UNLOCKED scalar fields (a locked field keeps its
// current value — the hand-edit wins), rebuilds genres/cast wholesale unless
// locked, replaces the fetched artwork rows (local artwork is untouched, so it
// still wins), and marks the Title matched. Identity is never touched.
func (db *DB) WriteTitleEnrichment(titleID string, e TitleEnrichment, locks map[string]bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin write enrichment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read current scalar values so a locked field is preserved verbatim (the
	// overlay: locked ? current : new). For an un-enriched Title these are empty.
	var cur TitleEnrichment
	var curName string
	if err := tx.QueryRow(
		`SELECT overview, tagline, content_rating, release_date, runtime_minutes, studio, enriched_title
		   FROM titles WHERE id = ?`, titleID,
	).Scan(&cur.Overview, &cur.Tagline, &cur.ContentRating, &cur.ReleaseDate,
		&cur.RuntimeMinutes, &cur.Studio, &curName); err != nil {
		return fmt.Errorf("store: reading current enrichment: %w", err)
	}

	pick := func(field, newVal, curVal string) string {
		if locks[field] {
			return curVal
		}
		return newVal
	}
	overview := pick("overview", e.Overview, cur.Overview)
	tagline := pick("tagline", e.Tagline, cur.Tagline)
	contentRating := pick("content_rating", e.ContentRating, cur.ContentRating)
	releaseDate := pick("release_date", e.ReleaseDate, cur.ReleaseDate)
	studio := pick("studio", e.Studio, cur.Studio)
	runtime := e.RuntimeMinutes
	if locks["runtime_minutes"] {
		runtime = cur.RuntimeMinutes
	}
	// enriched_title (DISPLAY only, never identity). An empty provider Name keeps
	// the current value, so a Movie/untitled-result never blanks a prior display
	// title. The "title" lock pins a hand-edited display title (issue 04).
	enrichedTitle := curName
	if !locks["title"] && e.Name != "" {
		enrichedTitle = e.Name
	}

	if _, err := tx.Exec(
		`UPDATE titles SET overview = ?, tagline = ?, content_rating = ?, release_date = ?,
		     runtime_minutes = ?, studio = ?, enriched_title = ?, enrichment_status = 'matched',
		     enriched_at = ?, enrichment_source = ?,
		     tmdb_id        = CASE WHEN ? <> '' AND IFNULL(tmdb_id, '') = '' THEN ? ELSE tmdb_id END,
		     imdb_id        = CASE WHEN ? <> '' AND IFNULL(imdb_id, '') = '' THEN ? ELSE imdb_id END,
		     musicbrainz_id = CASE WHEN ? <> '' AND IFNULL(musicbrainz_id, '') = '' THEN ? ELSE musicbrainz_id END
		   WHERE id = ?`,
		overview, tagline, contentRating, releaseDate, runtime, studio, enrichedTitle,
		time.Now().UTC().Format(time.RFC3339), e.Source,
		e.ExternalIDs.TMDBID, e.ExternalIDs.TMDBID,
		e.ExternalIDs.IMDBID, e.ExternalIDs.IMDBID,
		e.ExternalIDs.MusicbrainzID, e.ExternalIDs.MusicbrainzID,
		titleID,
	); err != nil {
		return fmt.Errorf("store: updating enriched title: %w", err)
	}

	// Genres + cast: rebuilt wholesale (idempotent) unless the group is locked.
	if !locks["genres"] {
		if _, err := tx.Exec(`DELETE FROM title_genres WHERE title_id = ?`, titleID); err != nil {
			return fmt.Errorf("store: clearing genres: %w", err)
		}
		for i, g := range e.Genres {
			if _, err := tx.Exec(
				`INSERT INTO title_genres (title_id, genre, ord) VALUES (?, ?, ?)`,
				titleID, g, i,
			); err != nil {
				return fmt.Errorf("store: inserting genre %q: %w", g, err)
			}
		}
	}
	if !locks["cast"] {
		if _, err := tx.Exec(`DELETE FROM title_credits WHERE title_id = ?`, titleID); err != nil {
			return fmt.Errorf("store: clearing credits: %w", err)
		}
		for i, c := range e.Cast {
			kind := c.Kind
			if kind == "" {
				kind = "cast"
			}
			if _, err := tx.Exec(
				`INSERT INTO title_credits (title_id, person, role, character, kind, ord, person_ref)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				titleID, c.Person, c.Role, c.Character, kind, i, c.PersonRef,
			); err != nil {
				return fmt.Errorf("store: inserting credit %q: %w", c.Person, err)
			}
		}
	}

	// Fetched artwork: replace the fetched rows per role (unless that role is
	// locked). Local artwork (source='local') is never touched, so it still wins.
	for _, a := range e.Artwork {
		if locks[a.Role] {
			continue
		}
		if _, err := tx.Exec(
			`DELETE FROM artwork WHERE title_id = ? AND role = ? AND source = 'fetched'`,
			titleID, a.Role,
		); err != nil {
			return fmt.Errorf("store: clearing fetched artwork %q: %w", a.Role, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO artwork (id, title_id, role, path, source) VALUES (?, ?, ?, ?, 'fetched')`,
			a.ID, titleID, a.Role, a.Path,
		); err != nil {
			return fmt.Errorf("store: inserting fetched artwork %q: %w", a.Role, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit write enrichment: %w", err)
	}
	return nil
}
