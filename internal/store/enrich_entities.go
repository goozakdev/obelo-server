package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Parent-entity Enrichment persistence (external-metadata-enrichment issue 03).
// The TV/Music browse parents — Show, Season, Artist, Album — are not Titles, so
// their enriched descriptive metadata + fetched artwork live in the generic
// entity_enrichment / entity_genres / entity_artwork tables keyed by
// (entity_type, entity_id). Everything here is descriptive/bookkeeping and NEVER
// identity (ADR-0002): a parent's identity_key and the catalog hierarchy are
// untouched. Mirrors the Title enrichment writer (enrich.go) for the leaf kinds.

// Entity types for the generic parent-enrichment tables.
const (
	EntityShow   = "show"
	EntitySeason = "season"
	EntityArtist = "artist"
	EntityAlbum  = "album"
	// EntityPerson keys a cast member's headshot in entity_artwork (cast-photos/01).
	// Unlike the browse parents above a person is NOT a catalog entity with a
	// Library — entity_id is the provider-namespaced person ref ("tmdb:<id>"), and
	// one row (role='profile') is shared by every Title that credits the person.
	EntityPerson = "person"
)

// EntityEnrichment is the read shape of a parent entity's enriched metadata.
// Status is pending|matched|unmatched|failed|disabled; an absent row reads as the
// zero value with Status "pending" (never enriched).
type EntityEnrichment struct {
	Overview      string
	ContentRating string
	Network       string
	Status        string
	Source        string
	ExternalID    string
	// ExternalIDLocked is true when ExternalID is an Admin-pinned durable
	// Enrichment override (Fix-info on a Show/Artist/Album, ADR-0019) rather than a
	// transient auto-resolved id. The enrich pass then looks the parent up BY it
	// every pass and never re-searches, so the correction survives later passes and
	// rescans (item-editing/02). The detail read surfaces it as the active override.
	ExternalIDLocked bool
	Genres           []string
}

// EntityEnrichmentWrite is a resolved parent result ready to persist. Genres and
// Artwork are rebuilt wholesale (idempotent) UNLESS the group / role is Locked.
// A Locked scalar keeps its current value (the hand-edit wins) — the parent
// analogue of the leaf overlay (item-editing/02), so re-enrichment refreshes only
// unlocked fields exactly as WriteTitleEnrichment does for a Title.
type EntityEnrichmentWrite struct {
	Overview      string
	ContentRating string
	Network       string
	Source        string
	ExternalID    string
	Genres        []string
	Artwork       []EntityArtworkRow
	// Cast is the parent's ordered cast (a Show's series main cast), rebuilt
	// wholesale into entity_credits unless 'cast' is Locked — the parent analogue
	// of a Title's title_credits (cast-photos/02). Empty for a parent kind that
	// captures no cast (Season/Artist/Album today).
	Cast []Credit
}

// EntityArtworkRow is one fetched image already written to the artwork cache,
// pending insertion against a parent entity (Source is forced to 'fetched').
type EntityArtworkRow struct {
	Role string
	Path string
}

// WriteEntityEnrichment persists a matched parent result in one transaction:
// overlays the UNLOCKED scalar fields (a Locked scalar keeps its current value —
// the hand-edit wins), rebuilds genres wholesale unless 'genres' is Locked,
// replaces the fetched artwork rows per role unless that role is Locked (a local
// Album cover lives in albums.artwork_path and is untouched, so it still wins),
// and marks the entity matched. external_id_locked is never touched here, so a
// durable Fix-info override survives the re-enrich it triggers. Identity untouched.
func (db *DB) WriteEntityEnrichment(entityType, entityID string, e EntityEnrichmentWrite, locks map[string]bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin write entity enrichment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read current scalars so a Locked field is preserved verbatim (locked ?
	// current : new). An absent row reads as empty, so a first enrich overlays all.
	var cur EntityEnrichmentWrite
	_ = tx.QueryRow(
		`SELECT overview, content_rating, network FROM entity_enrichment
		   WHERE entity_type = ? AND entity_id = ?`, entityType, entityID,
	).Scan(&cur.Overview, &cur.ContentRating, &cur.Network)
	pick := func(field, newVal, curVal string) string {
		if locks[field] {
			return curVal
		}
		return newVal
	}
	overview := pick("overview", e.Overview, cur.Overview)
	contentRating := pick("content_rating", e.ContentRating, cur.ContentRating)
	network := pick("network", e.Network, cur.Network)

	if _, err := tx.Exec(
		`INSERT INTO entity_enrichment
		   (entity_type, entity_id, overview, content_rating, network, external_id,
		    enrichment_status, enriched_at, enrichment_source)
		 VALUES (?, ?, ?, ?, ?, ?, 'matched', ?, ?)
		 ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		    overview = excluded.overview, content_rating = excluded.content_rating,
		    network = excluded.network, external_id = excluded.external_id,
		    enrichment_status = 'matched',
		    enriched_at = excluded.enriched_at, enrichment_source = excluded.enrichment_source`,
		entityType, entityID, overview, contentRating, network, e.ExternalID,
		time.Now().UTC().Format(time.RFC3339), e.Source,
	); err != nil {
		return fmt.Errorf("store: upserting entity enrichment: %w", err)
	}

	if !locks["genres"] {
		if _, err := tx.Exec(
			`DELETE FROM entity_genres WHERE entity_type = ? AND entity_id = ?`, entityType, entityID,
		); err != nil {
			return fmt.Errorf("store: clearing entity genres: %w", err)
		}
		for i, g := range e.Genres {
			if _, err := tx.Exec(
				`INSERT INTO entity_genres (entity_type, entity_id, genre, ord) VALUES (?, ?, ?, ?)`,
				entityType, entityID, g, i,
			); err != nil {
				return fmt.Errorf("store: inserting entity genre %q: %w", g, err)
			}
		}
	}

	if !locks["cast"] {
		if _, err := tx.Exec(
			`DELETE FROM entity_credits WHERE entity_type = ? AND entity_id = ?`, entityType, entityID,
		); err != nil {
			return fmt.Errorf("store: clearing entity credits: %w", err)
		}
		for i, c := range e.Cast {
			kind := c.Kind
			if kind == "" {
				kind = "cast"
			}
			if _, err := tx.Exec(
				`INSERT INTO entity_credits (id, entity_type, entity_id, person_ref, person, character, kind, ord)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				uuid.NewString(), entityType, entityID, c.PersonRef, c.Person, c.Character, kind, i,
			); err != nil {
				return fmt.Errorf("store: inserting entity credit %q: %w", c.Person, err)
			}
		}
	}

	for _, a := range e.Artwork {
		if locks[a.Role] {
			continue // a hand-chosen image for this role is Locked
		}
		if _, err := tx.Exec(
			`DELETE FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'fetched'`,
			entityType, entityID, a.Role,
		); err != nil {
			return fmt.Errorf("store: clearing entity artwork %q: %w", a.Role, err)
		}
		if _, err := tx.Exec(
			// added_at is set explicitly (its column default is '' — SQLite forbids a
			// datetime('now') default on the ADD COLUMN that introduced it, 0013), so
			// each re-enrich stamps a fresh time and the per-entity MAX(added_at)
			// version advances — busting the client's poster cache.
			`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source, added_at)
			 VALUES (?, ?, ?, ?, ?, 'fetched', datetime('now'))`,
			uuid.NewString(), entityType, entityID, a.Role, a.Path,
		); err != nil {
			return fmt.Errorf("store: inserting entity artwork %q: %w", a.Role, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit write entity enrichment: %w", err)
	}
	return nil
}

// SetEntityEnrichmentStatus records a terminal non-matched outcome (unmatched /
// failed / disabled) for a parent entity without touching its descriptive fields.
func (db *DB) SetEntityEnrichmentStatus(entityType, entityID, status string) error {
	if _, err := db.Exec(
		`INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status, enriched_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		    enrichment_status = excluded.enrichment_status, enriched_at = excluded.enriched_at`,
		entityType, entityID, status, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("store: setting entity enrichment status: %w", err)
	}
	return nil
}

// EntityEnrichmentStatus returns a parent entity's enrichment_status, or
// "pending" when it has never been enriched (no row). Drives the only-new pass.
func (db *DB) EntityEnrichmentStatus(entityType, entityID string) (string, error) {
	var status string
	err := db.QueryRow(
		`SELECT enrichment_status FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?`,
		entityType, entityID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: reading entity enrichment status: %w", err)
	}
	return status, nil
}

// EntityEnrichmentByID returns a parent entity's enriched metadata (with genres),
// or the zero value (Status "pending") when never enriched. Used by the browse
// detail reads to decorate a Show/Artist/Album.
func (db *DB) EntityEnrichmentByID(entityType, entityID string) (EntityEnrichment, error) {
	e := EntityEnrichment{Status: "pending"}
	var externalIDLocked int
	err := db.QueryRow(
		`SELECT overview, content_rating, network, enrichment_status, enrichment_source, external_id, external_id_locked
		   FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?`,
		entityType, entityID,
	).Scan(&e.Overview, &e.ContentRating, &e.Network, &e.Status, &e.Source, &e.ExternalID, &externalIDLocked)
	e.ExternalIDLocked = externalIDLocked != 0
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EntityEnrichment{}, fmt.Errorf("store: reading entity enrichment: %w", err)
	}
	genres, err := db.entityGenres(entityType, entityID)
	if err != nil {
		return EntityEnrichment{}, err
	}
	e.Genres = genres
	return e, nil
}

func (db *DB) entityGenres(entityType, entityID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT genre FROM entity_genres WHERE entity_type = ? AND entity_id = ?
		   ORDER BY ord, genre`, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("store: listing entity genres: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("store: scanning entity genre: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// EntityCredits returns a browse-parent's enriched cast in billing order (a Show's
// series main cast), the parent analogue of creditsForTitle (cast-photos/02). Each
// credit's person_ref is joined to the person's fetched headshot in entity_artwork
// (entity_type='person', role='profile'); the join's added_at surfaces as
// PhotoVersion (a cache-bust token, empty when the person has no cached photo), so
// the Show detail JSON can point a client at a headshot that busts on a re-enrich.
func (db *DB) EntityCredits(entityType, entityID string) ([]Credit, error) {
	rows, err := db.Query(
		`SELECT ec.person, ec.character, ec.kind, ec.person_ref,
		        COALESCE(pa.added_at, '')
		   FROM entity_credits ec
		   LEFT JOIN entity_artwork pa
		     ON pa.entity_type = 'person' AND pa.entity_id = ec.person_ref
		        AND pa.role = 'profile' AND ec.person_ref <> ''
		   WHERE ec.entity_type = ? AND ec.entity_id = ?
		   ORDER BY ec.kind, ec.ord, ec.person`, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("store: listing entity credits: %w", err)
	}
	defer rows.Close()
	var out []Credit
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.Person, &c.Character, &c.Kind, &c.PersonRef, &c.PhotoVersion); err != nil {
			return nil, fmt.Errorf("store: scanning entity credit: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EntityEnrichmentForMany bulk-reads parent enrichment (scalar + genres) for a
// page of entity ids of one type, keyed by id (absent = never enriched). Lets the
// Show grid attach overview/genres/status without an N+1 query.
func (db *DB) EntityEnrichmentForMany(entityType string, ids []string) (map[string]EntityEnrichment, error) {
	out := map[string]EntityEnrichment{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, entityType)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT entity_id, overview, content_rating, network, enrichment_status, enrichment_source
		   FROM entity_enrichment WHERE entity_type = ? AND entity_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading entity enrichment: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var e EntityEnrichment
		if err := rows.Scan(&id, &e.Overview, &e.ContentRating, &e.Network, &e.Status, &e.Source); err != nil {
			return nil, fmt.Errorf("store: scanning bulk entity enrichment: %w", err)
		}
		out[id] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Genres in one more query, appended onto the rows gathered above.
	grows, err := db.Query(
		`SELECT entity_id, genre FROM entity_genres
		   WHERE entity_type = ? AND entity_id IN (`+placeholders+`) ORDER BY entity_id, ord, genre`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading entity genres: %w", err)
	}
	defer grows.Close()
	for grows.Next() {
		var id, g string
		if err := grows.Scan(&id, &g); err != nil {
			return nil, fmt.Errorf("store: scanning bulk entity genre: %w", err)
		}
		e := out[id]
		if e.Status == "" {
			e.Status = "pending"
		}
		e.Genres = append(e.Genres, g)
		out[id] = e
	}
	return out, grows.Err()
}

// EntityArtworkByRole returns the on-disk path of a parent entity's fetched
// artwork for a role, or ErrNotFound. The API serves the bytes through the
// parent's artwork endpoint (local sources, where they exist, are served ahead of
// this by the caller — local wins).
func (db *DB) EntityArtworkByRole(entityType, entityID, role string) (Artwork, error) {
	var a Artwork
	// Serve precedence uploaded > local > fetched (ADR-0026): an Admin-uploaded
	// parent image outranks a local cover (albums.artwork_path, served ahead of
	// this by the caller) and a fetched/picked one. Take the single highest row.
	err := db.QueryRow(
		`SELECT id, role, path, source FROM entity_artwork
		   WHERE entity_type = ? AND entity_id = ? AND role = ?
		   ORDER BY CASE source WHEN 'uploaded' THEN 0 WHEN 'local' THEN 1 ELSE 2 END LIMIT 1`,
		entityType, entityID, role,
	).Scan(&a.ID, &a.Role, &a.Path, &a.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return Artwork{}, ErrNotFound
	}
	if err != nil {
		return Artwork{}, fmt.Errorf("store: reading entity artwork: %w", err)
	}
	return a, nil
}

// UpsertPersonArtwork records a cast member's fetched headshot as a person row
// in entity_artwork (entity_type='person', entity_id=<person ref>, role,
// source='fetched'), keyed + de-duplicated by the person ref (cast-photos/01).
// The UNIQUE(entity_type, entity_id, role, source) constraint (0011) makes the
// upsert idempotent: a re-fetched headshot replaces the path in place and stamps
// a fresh added_at (busting the client's cached photo). Because the row is keyed
// by ref, one actor's headshot is stored ONCE across every Title they appear in.
func (db *DB) UpsertPersonArtwork(personRef, role, path string) error {
	if _, err := db.Exec(
		`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source, added_at)
		 VALUES (?, 'person', ?, ?, ?, 'fetched', datetime('now'))
		 ON CONFLICT(entity_type, entity_id, role, source) DO UPDATE SET
		    path = excluded.path, added_at = excluded.added_at`,
		uuid.NewString(), personRef, role, path,
	); err != nil {
		return fmt.Errorf("store: upserting person artwork: %w", err)
	}
	return nil
}

// PersonArtworkByRef returns the on-disk path (+ its cache-bust added_at) of a
// person's fetched headshot for a role, or ErrNotFound when none is cached. It
// backs both the enrich pass's cross-title dedupe check (skip the download when
// the person already has a cached photo) and the /people/{ref}/artwork/{role}
// serve handler (the API streams the bytes). Read shape mirrors EntityArtworkByRole.
func (db *DB) PersonArtworkByRef(personRef, role string) (Artwork, error) {
	var a Artwork
	err := db.QueryRow(
		`SELECT id, role, path, source FROM entity_artwork
		   WHERE entity_type = 'person' AND entity_id = ? AND role = ? LIMIT 1`,
		personRef, role,
	).Scan(&a.ID, &a.Role, &a.Path, &a.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return Artwork{}, ErrNotFound
	}
	if err != nil {
		return Artwork{}, fmt.Errorf("store: reading person artwork: %w", err)
	}
	return a, nil
}

// LibrariesCreditingPerson returns the distinct set of Library ids that credit the
// given person ref (cast-photos/01, /02). A person is not a catalog entity with its
// own Library, so its headshot's access follows whatever credits it: a leaf Title's
// cast in title_credits (Movie/Episode) OR a browse-parent's cast in entity_credits
// (a Show — the entity_id is the Show id, whose row carries the library_id). The
// serve handler shows the photo only to a viewer who can access at least one of
// these Libraries (an unreferenced ref → empty set → 404). Empty personRef yields no
// libraries (never served). A Show-only actor is thus visible to viewers who can see
// the Show even though no Title credits them.
func (db *DB) LibrariesCreditingPerson(personRef string) ([]string, error) {
	if personRef == "" {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT DISTINCT t.library_id
		   FROM title_credits tc JOIN titles t ON t.id = tc.title_id
		   WHERE tc.person_ref = ?
		 UNION
		 SELECT DISTINCT s.library_id
		   FROM entity_credits ec JOIN shows s ON s.id = ec.entity_id
		   WHERE ec.entity_type = 'show' AND ec.person_ref = ?`, personRef, personRef)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries crediting person: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var lib string
		if err := rows.Scan(&lib); err != nil {
			return nil, fmt.Errorf("store: scanning library crediting person: %w", err)
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

// EntityArtworkRolesForMany returns, for a page of entity ids of one type, the
// set of roles each has fetched artwork for (keyed by id). Lets the Show grid
// decide whether to advertise a poster URL without an N+1 existence check.
func (db *DB) EntityArtworkRolesForMany(entityType string, ids []string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, entityType)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT entity_id, role FROM entity_artwork
		   WHERE entity_type = ? AND entity_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading entity artwork roles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, fmt.Errorf("store: scanning entity artwork role: %w", err)
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][role] = true
	}
	return out, rows.Err()
}

// EntityArtworkVersionsForMany bulk-reads a per-entity artwork cache-bust version
// for a page of parent entities (Show/Season/Artist/Album), keyed by entity id
// (absent = no fetched artwork). The version is the newest entity_artwork
// added_at for that entity — which advances whenever a (re-)enrich replaces a
// role's row — so a browse client reloads a parent poster only when its fetched
// image actually changed. The parent analogue of ArtworkVersionsForTitles; no
// N+1 (one indexed query for the whole page).
func (db *DB) EntityArtworkVersionsForMany(entityType string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, entityType)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT entity_id, MAX(added_at) FROM entity_artwork
		   WHERE entity_type = ? AND entity_id IN (`+placeholders+`) GROUP BY entity_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bulk reading entity artwork versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("store: scanning entity artwork version: %w", err)
		}
		out[id] = version
	}
	return out, rows.Err()
}

// SetEntityExternalMatch pins an Admin-chosen authoritative external id on a
// browse-parent entity as a durable Enrichment override (Fix-info on a Show/
// Artist/Album, ADR-0019). It sets external_id + external_id_locked=1 and resets
// enrichment_status to 'pending' so the next lookup re-resolves BY the pinned id —
// touching ONLY the override/bookkeeping columns, never identity (the parent's
// identity_key and the catalog hierarchy are untouched, ADR-0002/0014). A row is
// created if the parent was never enriched. The follow-on re-enrich (enrichParent)
// preserves external_id_locked, so the pin is durable across later passes.
func (db *DB) SetEntityExternalMatch(entityType, entityID, externalID string) error {
	if _, err := db.Exec(
		`INSERT INTO entity_enrichment
		   (entity_type, entity_id, external_id, external_id_locked, enrichment_status, enriched_at)
		 VALUES (?, ?, ?, 1, 'pending', ?)
		 ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		    external_id = excluded.external_id, external_id_locked = 1,
		    enrichment_status = 'pending', enriched_at = excluded.enriched_at`,
		entityType, entityID, externalID, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("store: setting entity external match: %w", err)
	}
	return nil
}

// EntityLockedFields returns the set of Locked field names for a browse-parent
// entity (the entity_field_locks analogue of LockedFields): a field present here
// was hand-edited and must not be overwritten by the parent enrich pass. Empty for
// a parent with no manual edits.
func (db *DB) EntityLockedFields(entityType, entityID string) (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT field FROM entity_field_locks WHERE entity_type = ? AND entity_id = ?`,
		entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("store: reading entity locked fields: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("store: scanning entity locked field: %w", err)
		}
		out[f] = true
	}
	return out, rows.Err()
}

// EntityLockedFieldsSorted returns a browse-parent's Locked field names in a
// stable order, for the lockedFields[] the parent detail reports (a client shows
// which fields are pinned and which it may release).
func (db *DB) EntityLockedFieldsSorted(entityType, entityID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT field FROM entity_field_locks WHERE entity_type = ? AND entity_id = ?
		   ORDER BY field`, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("store: reading entity locked fields: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("store: scanning entity locked field: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EntityMetadataEdit is an Admin's hand-edit of a browse-parent's descriptive
// fields (the write side of PUT /shows|artists|albums/{id}/metadata, ADR-0019
// Fix-label for parents). Every non-nil field is written AND Locked, so a
// subsequent parent enrich pass never overwrites it (CONTEXT.md "Locked field");
// a nil field is left untouched. The lock field names match the ones
// WriteEntityEnrichment honors, so a hand-edit here is exactly what a later pass
// skips. Identity is never touched.
type EntityMetadataEdit struct {
	Overview      *string
	ContentRating *string
	Network       *string
	Genres        *[]string
	// Name edits the parent's DISPLAY label (shows.title / artists.name /
	// albums.title) — never its identity_key or the catalog hierarchy (ADR-0002), so
	// a rename changes only the label, not identity or watch state. Its lock field is
	// "title". A parent enrich pass never writes this column, so the lock is a display
	// marker; the edit itself is what changes the shown name.
	Name *string
	// LockArtwork names artwork roles to pin against a refresh ('poster' /
	// 'background' / 'cover'); the currently-chosen image is kept and the role Locked.
	LockArtwork []string
}

// entityDisplayColumns maps a browse-parent entity type onto its table and the
// display + sort columns a hand-edited display name updates. A Season has none
// (it is never edited in v1), so it returns ok=false.
func entityDisplayColumns(entityType string) (table, nameCol, sortCol string, ok bool) {
	switch entityType {
	case EntityShow:
		return "shows", "title", "sort_title", true
	case EntityArtist:
		return "artists", "name", "sort_name", true
	case EntityAlbum:
		return "albums", "title", "sort_title", true
	default:
		return "", "", "", false
	}
}

// WriteEntityMetadata applies an Admin hand-edit to a browse-parent in one
// transaction: it writes each supplied descriptive field and records a Lock for it
// (the parent analogue of WriteTitleMetadata), so re-enrichment refreshes only the
// unlocked fields. Genres are rebuilt wholesale when supplied. A row is created if
// the parent was never enriched (so the lock's fields have a home). Identity is
// never touched — only descriptive columns, genre rows, and lock rows change.
func (db *DB) WriteEntityMetadata(entityType, entityID string, edit EntityMetadataEdit) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin write entity metadata: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Ensure a row exists so the scalar UPDATE lands (an un-enriched parent has none).
	if _, err := tx.Exec(
		`INSERT INTO entity_enrichment (entity_type, entity_id) VALUES (?, ?)
		 ON CONFLICT(entity_type, entity_id) DO NOTHING`, entityType, entityID,
	); err != nil {
		return fmt.Errorf("store: ensuring entity enrichment row: %w", err)
	}

	lock := func(field string) error {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO entity_field_locks (entity_type, entity_id, field) VALUES (?, ?, ?)`,
			entityType, entityID, field,
		); err != nil {
			return fmt.Errorf("store: locking entity field %q: %w", field, err)
		}
		return nil
	}
	// setStr writes a scalar TEXT column and locks its field; the column name is a
	// fixed literal (never user input), so the string-built SQL is safe.
	setStr := func(col, field, val string) error {
		if _, err := tx.Exec(
			`UPDATE entity_enrichment SET `+col+` = ? WHERE entity_type = ? AND entity_id = ?`,
			val, entityType, entityID,
		); err != nil {
			return fmt.Errorf("store: editing entity %q: %w", field, err)
		}
		return lock(field)
	}

	if edit.Overview != nil {
		if err := setStr("overview", "overview", *edit.Overview); err != nil {
			return err
		}
	}
	if edit.ContentRating != nil {
		if err := setStr("content_rating", "content_rating", *edit.ContentRating); err != nil {
			return err
		}
	}
	if edit.Network != nil {
		if err := setStr("network", "network", *edit.Network); err != nil {
			return err
		}
	}
	if edit.Genres != nil {
		if _, err := tx.Exec(
			`DELETE FROM entity_genres WHERE entity_type = ? AND entity_id = ?`, entityType, entityID,
		); err != nil {
			return fmt.Errorf("store: clearing entity genres: %w", err)
		}
		for i, g := range *edit.Genres {
			if _, err := tx.Exec(
				`INSERT INTO entity_genres (entity_type, entity_id, genre, ord) VALUES (?, ?, ?, ?)`,
				entityType, entityID, g, i,
			); err != nil {
				return fmt.Errorf("store: inserting entity genre %q: %w", g, err)
			}
		}
		if err := lock("genres"); err != nil {
			return err
		}
	}
	if edit.Name != nil {
		// The display label lives on the parent's own row (shows/artists/albums), not
		// entity_enrichment — update it there and Lock "title". identity_key and
		// sort-order key move together so the grid stays consistent; identity is never
		// touched (ADR-0002), so a rename leaves identity_key and watch state intact.
		table, nameCol, sortCol, ok := entityDisplayColumns(entityType)
		if !ok {
			return fmt.Errorf("store: display name not editable for entity type %q", entityType)
		}
		// Column names are fixed literals chosen from a closed set (never user input),
		// so the string-built SQL is safe.
		if _, err := tx.Exec(
			`UPDATE `+table+` SET `+nameCol+` = ?, `+sortCol+` = ? WHERE id = ?`,
			*edit.Name, strings.ToLower(strings.TrimSpace(*edit.Name)), entityID,
		); err != nil {
			return fmt.Errorf("store: editing entity display name: %w", err)
		}
		if err := lock("title"); err != nil {
			return err
		}
	}
	for _, role := range edit.LockArtwork {
		if err := lock(role); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit write entity metadata: %w", err)
	}
	return nil
}

// PickEntityArtwork applies an Admin-chosen provider image to a browse-parent's
// artwork role and Locks that role, in one transaction — the parent analogue of
// PickTitleArtwork (Fix label image picker, ADR-0019). The image replaces the
// role's 'fetched' entity_artwork row and the role is Locked, so a later parent
// enrich pass skips re-fetching it. A LOCAL image for the role still wins at serve
// time (a local Album cover in albums.artwork_path is served ahead of this), so
// local-wins precedence is preserved. path is the on-disk cache path; artworkID a
// fresh id for the row.
func (db *DB) PickEntityArtwork(entityType, entityID, role, path, artworkID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin pick entity artwork: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'fetched'`,
		entityType, entityID, role,
	); err != nil {
		return fmt.Errorf("store: clearing fetched entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source, added_at)
		 VALUES (?, ?, ?, ?, ?, 'fetched', datetime('now'))`,
		artworkID, entityType, entityID, role, path,
	); err != nil {
		return fmt.Errorf("store: inserting picked entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO entity_field_locks (entity_type, entity_id, field) VALUES (?, ?, ?)`,
		entityType, entityID, role,
	); err != nil {
		return fmt.Errorf("store: locking entity artwork role %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit pick entity artwork: %w", err)
	}
	return nil
}

// UploadEntityArtwork records an Admin-uploaded image for a browse-parent's
// artwork role and Locks that role, in one transaction — the parent analogue of
// UploadTitleArtwork (ADR-0026). The bytes are already in the artwork cache; this
// inserts the 'uploaded' entity_artwork row (replacing any prior upload for the
// role — Option B) and Locks the role. Local/fetched rows are left in place as the
// role's auto image; an 'uploaded' row outranks them at serve time. Returns the
// on-disk path of the REPLACED prior upload (empty if none) for orphan cleanup.
func (db *DB) UploadEntityArtwork(entityType, entityID, role, path, artworkID string) (replacedPath string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin upload entity artwork: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRow(
		`SELECT path FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'uploaded'`,
		entityType, entityID, role,
	).Scan(&replacedPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: reading prior uploaded entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'uploaded'`,
		entityType, entityID, role,
	); err != nil {
		return "", fmt.Errorf("store: clearing uploaded entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source, added_at)
		 VALUES (?, ?, ?, ?, ?, 'uploaded', datetime('now'))`,
		artworkID, entityType, entityID, role, path,
	); err != nil {
		return "", fmt.Errorf("store: inserting uploaded entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO entity_field_locks (entity_type, entity_id, field) VALUES (?, ?, ?)`,
		entityType, entityID, role,
	); err != nil {
		return "", fmt.Errorf("store: locking entity artwork role %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit upload entity artwork: %w", err)
	}
	return replacedPath, nil
}

// ReleaseEntityArtworkLock releases a browse-parent's Lock on an artwork role and,
// when that role is backed by an Admin upload, deletes the 'uploaded' entity_artwork
// row so the role reverts to its auto image — the parent "undo my upload"
// (ADR-0026). Returns the on-disk path of the deleted upload (empty if none) so the
// caller removes the cached bytes. A role with no upload behaves exactly like
// ReleaseEntityFieldLock (only the lock is dropped).
func (db *DB) ReleaseEntityArtworkLock(entityType, entityID, role string) (removedPath string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin release entity artwork lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRow(
		`SELECT path FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'uploaded'`,
		entityType, entityID, role,
	).Scan(&removedPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: reading uploaded entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM entity_artwork WHERE entity_type = ? AND entity_id = ? AND role = ? AND source = 'uploaded'`,
		entityType, entityID, role,
	); err != nil {
		return "", fmt.Errorf("store: deleting uploaded entity artwork %q: %w", role, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM entity_field_locks WHERE entity_type = ? AND entity_id = ? AND field = ?`,
		entityType, entityID, role,
	); err != nil {
		return "", fmt.Errorf("store: releasing entity artwork lock %q: %w", role, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit release entity artwork lock: %w", err)
	}
	return removedPath, nil
}

// ReleaseEntityFieldLock removes a browse-parent's Lock on one field so the next
// enrich pass refreshes it again (CONTEXT.md "a lock is releasable back to auto").
// Releasing an absent lock is a no-op (idempotent); the field's current value is
// left as is (enrichment overwrites it on the next pass).
func (db *DB) ReleaseEntityFieldLock(entityType, entityID, field string) error {
	if _, err := db.Exec(
		`DELETE FROM entity_field_locks WHERE entity_type = ? AND entity_id = ? AND field = ?`,
		entityType, entityID, field,
	); err != nil {
		return fmt.Errorf("store: releasing entity lock %q: %w", field, err)
	}
	return nil
}

// ListAllShows returns every visible Show of a TV Library (no pagination), for an
// Enrichment pass to walk the whole library. Hidden Shows are skipped (ADR-0008).
func (db *DB) ListAllShows(libraryID string) ([]Show, error) {
	rows, err := db.Query(
		`SELECT id, library_id, title, year, identity_key, sort_title,
		        tmdb_id, imdb_id, needs_review, hidden, added_at
		   FROM shows WHERE library_id = ? AND hidden = 0 ORDER BY sort_title, id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing all shows: %w", err)
	}
	defer rows.Close()
	var out []Show
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAllArtists returns every visible Artist of a Music Library (no pagination),
// for an Enrichment pass to walk the whole library.
func (db *DB) ListAllArtists(libraryID string) ([]Artist, error) {
	rows, err := db.Query(
		`SELECT id, library_id, name, identity_key, sort_name, hidden, added_at
		   FROM artists WHERE library_id = ? AND hidden = 0 ORDER BY sort_name, id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing all artists: %w", err)
	}
	defer rows.Close()
	var out []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
