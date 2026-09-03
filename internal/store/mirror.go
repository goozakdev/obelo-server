package store

import (
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// The mirror's write half (ADR-0056 §3): one pull of another Server's Export,
// written into THIS Server's own catalog tables.
//
// It is the exact inverse of export.go, and the two belong read together: that
// file names every field a sharer discloses, this one names where each of those
// fields lands. Nothing here derives, corrects or enriches anything — the sharer
// is the identity authority for its own files (ADR-0002, ADR-0019), so a value
// that did not cross the Link does not exist on this side either.
//
// Rows are matched by remote_id and NEVER by title. That is the whole reason
// migration 0063 adds the column: a rename on the sharer's side updates the row
// in place, so this household's Watch state — keyed to the local Title id —
// survives it.

// MirrorEntity is one row of an Export as the mirror consumes it: the wire
// entity with its `data` still a decoded JSON object. The field names are the
// Export's (camelCase), because the feed is the contract.
type MirrorEntity struct {
	// Type is one of the ten Export wire types (ExportTitle … ExportStream).
	Type string
	// RemoteID and ParentID are the sharer's ids, never this Server's.
	RemoteID string
	ParentID string
	// Deleted is the feed's `deletedAt` reduced to the only question the mirror
	// asks of it: a Missing File, or a hidden Title/Show/Season/Artist/Album.
	Deleted bool
	Data    map[string]any
}

// mirrorTables maps a wire type to the table it lands in. The three Title kinds
// share one table, which is why the parent lookups below are keyed by TABLE and
// not by wire type.
var mirrorTables = map[string]string{
	ExportShow:    "shows",
	ExportSeason:  "seasons",
	ExportArtist:  "artists",
	ExportAlbum:   "albums",
	ExportTitle:   "titles",
	ExportEpisode: "titles",
	ExportTrack:   "titles",
	ExportEdition: "editions",
	ExportFile:    "files",
	ExportStream:  "streams",
}

// mirrorParentTables says which table an entity's ParentID names. A Movie has no
// parent inside the Library, so `title` is absent.
var mirrorParentTables = map[string]string{
	ExportSeason:  "shows",
	ExportAlbum:   "artists",
	ExportEpisode: "seasons",
	ExportTrack:   "albums",
	ExportEdition: "titles",
	ExportFile:    "editions",
	ExportStream:  "files",
}

// mirrorApplyOrder is parent-before-child, so a child never looks for a parent
// that has not been written yet. The feed itself is ordered by CHANGE TIME, not
// by hierarchy, so this re-ordering is the mirror's job and not the sharer's.
var mirrorApplyOrder = []string{
	ExportShow, ExportSeason, ExportArtist, ExportAlbum,
	ExportTitle, ExportEpisode, ExportTrack,
	ExportEdition, ExportFile, ExportStream,
}

// ApplyMirror writes one pull into a linked Library, in a single transaction.
//
// `full` says the entities are the WHOLE truth for this Library — a first pull,
// or a restart after 410 RESYNC. Only then does it prune: every mirrored row
// whose remote id did not appear is deleted. That is not a nicety, it is the only
// answer this side has to the one gap the Export cannot close — Editions and
// Streams have no soft-delete of their own and the sharer's scanner rebuilds them
// with fresh ids, so a removed Edition or Stream can carry no tombstone and is
// visible only as an absence. On an incremental pull nothing is pruned, because
// an absence there means "did not change" (issue 08 owns that reconciliation).
//
// It is idempotent by construction: every write is keyed on remote_id, so
// applying the same pull twice leaves the same rows with the same local ids.
func (db *DB) ApplyMirror(libraryID string, entities []MirrorEntity, full bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin mirror apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	m := &mirrorTx{tx: tx, libraryID: libraryID, local: map[string]string{}}
	if err := m.loadExisting(); err != nil {
		return err
	}

	byType := map[string][]MirrorEntity{}
	for _, e := range entities {
		byType[e.Type] = append(byType[e.Type], e)
	}
	for _, typ := range mirrorApplyOrder {
		for _, e := range byType[typ] {
			if err := m.apply(e); err != nil {
				return err
			}
		}
	}
	if full {
		if err := m.prune(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit mirror apply: %w", err)
	}
	return nil
}

// mirrorTx carries the one transaction and the remote→local id map for it.
type mirrorTx struct {
	tx        *sql.Tx
	libraryID string
	// local maps "<table>\x00<remote id>" to the local row id. It is seeded from
	// what is already mirrored and grown as new rows are written, so a child
	// written in this same pull can find the parent written moments ago.
	local map[string]string
	// seen records every "<table>\x00<remote id>" this pull carried, for the prune.
	seen map[string]bool
}

func mirrorKey(table, remoteID string) string { return table + "\x00" + remoteID }

// mirrorSources is the (table, query) pair used both to seed `local` and, on a
// full pull, to find what to prune. Each query yields (remote_id, id) for the
// Library, reaching it through whatever join the table needs.
var mirrorSources = []struct{ table, query string }{
	{"shows", `SELECT remote_id, id FROM shows WHERE library_id = ? AND remote_id IS NOT NULL`},
	{"seasons", `SELECT s.remote_id, s.id FROM seasons s JOIN shows sh ON sh.id = s.show_id
	              WHERE sh.library_id = ? AND s.remote_id IS NOT NULL`},
	{"artists", `SELECT remote_id, id FROM artists WHERE library_id = ? AND remote_id IS NOT NULL`},
	{"albums", `SELECT a.remote_id, a.id FROM albums a JOIN artists ar ON ar.id = a.artist_id
	             WHERE ar.library_id = ? AND a.remote_id IS NOT NULL`},
	{"titles", `SELECT remote_id, id FROM titles WHERE library_id = ? AND remote_id IS NOT NULL`},
	{"editions", `SELECT e.remote_id, e.id FROM editions e JOIN titles t ON t.id = e.title_id
	               WHERE t.library_id = ? AND e.remote_id IS NOT NULL`},
	{"files", `SELECT f.remote_id, f.id FROM files f JOIN editions e ON e.id = f.edition_id
	            JOIN titles t ON t.id = e.title_id
	           WHERE t.library_id = ? AND f.remote_id IS NOT NULL`},
	{"streams", `SELECT st.remote_id, st.id FROM streams st JOIN files f ON f.id = st.file_id
	              JOIN editions e ON e.id = f.edition_id JOIN titles t ON t.id = e.title_id
	             WHERE t.library_id = ? AND st.remote_id IS NOT NULL`},
}

func (m *mirrorTx) loadExisting() error {
	for _, src := range mirrorSources {
		rows, err := m.tx.Query(src.query, m.libraryID)
		if err != nil {
			return fmt.Errorf("store: reading mirrored %s: %w", src.table, err)
		}
		for rows.Next() {
			var remoteID, id string
			if err := rows.Scan(&remoteID, &id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scanning mirrored %s: %w", src.table, err)
			}
			m.local[mirrorKey(src.table, remoteID)] = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: reading mirrored %s: %w", src.table, err)
		}
		_ = rows.Close()
	}
	return nil
}

// idFor returns the local id for a remote one, minting a fresh uuid the first
// time this Server sees it. The local id is this household's forever after: it is
// what Watch state, Playlists and Collections point at.
func (m *mirrorTx) idFor(table, remoteID string) (id string, isNew bool) {
	k := mirrorKey(table, remoteID)
	if id, ok := m.local[k]; ok {
		return id, false
	}
	id = uuid.NewString()
	m.local[k] = id
	return id, true
}

func (m *mirrorTx) note(table, remoteID string) {
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	m.seen[mirrorKey(table, remoteID)] = true
}

// parentOf resolves an entity's parent to a local id. A miss is not an error: on
// an incremental pull a child can legitimately arrive without its (unchanged)
// parent, and this side simply skips it until the pull that carries both.
func (m *mirrorTx) parentOf(e MirrorEntity) (string, bool) {
	table, ok := mirrorParentTables[e.Type]
	if !ok {
		return "", true // a Movie: its parent is the Library
	}
	if e.ParentID == "" {
		return "", false
	}
	id, ok := m.local[mirrorKey(table, e.ParentID)]
	return id, ok
}

func (m *mirrorTx) apply(e MirrorEntity) error {
	table, ok := mirrorTables[e.Type]
	if !ok {
		// A type this build has never heard of. A newer sharer may export one; the
		// version handshake is what decides whether the two can talk at all, so an
		// unknown row is ignored rather than fatal.
		return nil
	}
	parent, ok := m.parentOf(e)
	if !ok {
		return nil
	}
	m.note(table, e.RemoteID)
	id, isNew := m.idFor(table, e.RemoteID)

	switch e.Type {
	case ExportShow:
		return m.writeShow(id, isNew, e)
	case ExportSeason:
		return m.writeSeason(id, isNew, parent, e)
	case ExportArtist:
		return m.writeArtist(id, isNew, e)
	case ExportAlbum:
		return m.writeAlbum(id, isNew, parent, e)
	case ExportTitle, ExportEpisode, ExportTrack:
		return m.writeTitle(id, isNew, parent, e)
	case ExportEdition:
		return m.writeEdition(id, isNew, parent, e)
	case ExportFile:
		return m.writeFile(id, isNew, parent, e)
	case ExportStream:
		return m.writeStream(id, isNew, parent, e)
	}
	return nil
}

// --- reading the feed's `data` ------------------------------------------------

func mirrorStr(d map[string]any, k string) string {
	s, _ := d[k].(string)
	return s
}

// mirrorInt reads a number that came through JSON, which decodes every number as
// a float64 — and tolerates an int, so a caller can build a MirrorEntity by hand.
func mirrorInt(d map[string]any, k string) int {
	switch v := d[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func mirrorInt64(d map[string]any, k string) int64 {
	switch v := d[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func mirrorBool(d map[string]any, k string) int {
	if b, _ := d[k].(bool); b {
		return 1
	}
	return 0
}

// mirrorYear keeps an absent year absent (NULL) rather than turning it into 0:
// the browse API distinguishes them and the comparison with the sharer would
// otherwise fail on every year-less Title.
func mirrorYear(d map[string]any) any {
	if _, ok := d["year"]; !ok {
		return nil
	}
	return mirrorInt(d, "year")
}

func mirrorStrings(d map[string]any, k string) []string {
	raw, _ := d[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mirrorCredit is one cast/crew row as it arrived — export.go's exportCredit,
// read back off the wire.
type mirrorCredit struct{ person, role, character, kind, personRef string }

func mirrorCredits(d map[string]any) []mirrorCredit {
	raw, _ := d["cast"].([]any)
	out := make([]mirrorCredit, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		out = append(out, mirrorCredit{
			person: mirrorStr(m, "person"), role: mirrorStr(m, "role"),
			character: mirrorStr(m, "character"), kind: mirrorStr(m, "kind"),
			personRef: mirrorStr(m, "personId"),
		})
	}
	return out
}

// mirrorStatus defaults an absent enrichment status to the column's own default.
// The Export omits zero values, and 'pending' is what a row that never met a
// provider carries here too.
func mirrorStatus(d map[string]any) string {
	if s := mirrorStr(d, "enrichmentStatus"); s != "" {
		return s
	}
	return "pending"
}

// mirrorHidden is the feed's Title/Show/Season/Artist/Album tombstone.
//
// It sets `hidden`, which is EXACTLY the column the sharer's tombstone came from
// (export.go stamps deletedAt from it), so the round trip is lossless and this
// Server's browse hides precisely what theirs hides. It deliberately does not
// DELETE the row: a delete would take this household's Watch state with it, which
// is the "dropping mirrored rows" ADR-0056 rejects by name.
func mirrorHidden(e MirrorEntity) int {
	if e.Deleted {
		return 1
	}
	return 0
}

// --- the eight writers ---------------------------------------------------------

func (m *mirrorTx) writeShow(id string, isNew bool, e MirrorEntity) error {
	d := e.Data
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO shows (id, library_id, title, year, identity_key, sort_title,
			   tmdb_id, imdb_id, needs_review, hidden, added_at, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, m.libraryID, mirrorStr(d, "title"), mirrorYear(d), mirrorStr(d, "identityKey"),
			mirrorStr(d, "sortTitle"), mirrorStr(d, "tmdbId"), mirrorStr(d, "imdbId"),
			mirrorBool(d, "needsReview"), mirrorHidden(e), mirrorStr(d, "addedAt"), e.RemoteID)
	} else {
		_, err = m.tx.Exec(
			`UPDATE shows SET title = ?, year = ?, identity_key = ?, sort_title = ?,
			   tmdb_id = ?, imdb_id = ?, needs_review = ?, hidden = ?, added_at = ?
			 WHERE id = ?`,
			mirrorStr(d, "title"), mirrorYear(d), mirrorStr(d, "identityKey"),
			mirrorStr(d, "sortTitle"), mirrorStr(d, "tmdbId"), mirrorStr(d, "imdbId"),
			mirrorBool(d, "needsReview"), mirrorHidden(e), mirrorStr(d, "addedAt"), id)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring show: %w", err)
	}
	return m.writeEntityExtras(EntityShow, id, d)
}

func (m *mirrorTx) writeSeason(id string, isNew bool, showID string, e MirrorEntity) error {
	d := e.Data
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO seasons (id, show_id, season_number, identity_key, hidden, added_at, remote_id)
			 VALUES (?,?,?,?,?,?,?)`,
			id, showID, mirrorInt(d, "seasonNumber"), mirrorStr(d, "identityKey"),
			mirrorHidden(e), mirrorStr(d, "addedAt"), e.RemoteID)
	} else {
		_, err = m.tx.Exec(
			`UPDATE seasons SET show_id = ?, season_number = ?, identity_key = ?, hidden = ?,
			   added_at = ? WHERE id = ?`,
			showID, mirrorInt(d, "seasonNumber"), mirrorStr(d, "identityKey"),
			mirrorHidden(e), mirrorStr(d, "addedAt"), id)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring season: %w", err)
	}
	return m.writeEntityExtras(EntitySeason, id, d)
}

func (m *mirrorTx) writeArtist(id string, isNew bool, e MirrorEntity) error {
	d := e.Data
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO artists (id, library_id, name, sort_name, identity_key,
			   musicbrainz_id, hidden, added_at, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			id, m.libraryID, mirrorStr(d, "name"), mirrorStr(d, "sortName"),
			mirrorStr(d, "identityKey"), mirrorStr(d, "musicbrainzId"),
			mirrorHidden(e), mirrorStr(d, "addedAt"), e.RemoteID)
	} else {
		_, err = m.tx.Exec(
			`UPDATE artists SET name = ?, sort_name = ?, identity_key = ?, musicbrainz_id = ?,
			   hidden = ?, added_at = ? WHERE id = ?`,
			mirrorStr(d, "name"), mirrorStr(d, "sortName"), mirrorStr(d, "identityKey"),
			mirrorStr(d, "musicbrainzId"), mirrorHidden(e), mirrorStr(d, "addedAt"), id)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring artist: %w", err)
	}
	return m.writeEntityExtras(EntityArtist, id, d)
}

func (m *mirrorTx) writeAlbum(id string, isNew bool, artistID string, e MirrorEntity) error {
	d := e.Data
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO albums (id, artist_id, title, year, sort_title, identity_key,
			   release_type, musicbrainz_id, musicbrainz_release_id, hidden, added_at, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, artistID, mirrorStr(d, "title"), mirrorYear(d), mirrorStr(d, "sortTitle"),
			mirrorStr(d, "identityKey"), mirrorStr(d, "releaseType"), mirrorStr(d, "musicbrainzId"),
			mirrorStr(d, "musicbrainzReleaseId"), mirrorHidden(e), mirrorStr(d, "addedAt"), e.RemoteID)
	} else {
		_, err = m.tx.Exec(
			`UPDATE albums SET artist_id = ?, title = ?, year = ?, sort_title = ?, identity_key = ?,
			   release_type = ?, musicbrainz_id = ?, musicbrainz_release_id = ?, hidden = ?,
			   added_at = ? WHERE id = ?`,
			artistID, mirrorStr(d, "title"), mirrorYear(d), mirrorStr(d, "sortTitle"),
			mirrorStr(d, "identityKey"), mirrorStr(d, "releaseType"), mirrorStr(d, "musicbrainzId"),
			mirrorStr(d, "musicbrainzReleaseId"), mirrorHidden(e), mirrorStr(d, "addedAt"), id)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring album: %w", err)
	}
	return m.writeEntityExtras(EntityAlbum, id, d)
}

// mirrorTitleKind maps the three Title wire types onto the one column that tells
// them apart here.
var mirrorTitleKind = map[string]string{
	ExportTitle: "movie", ExportEpisode: "episode", ExportTrack: "track",
}

func (m *mirrorTx) writeTitle(id string, isNew bool, parent string, e MirrorEntity) error {
	d := e.Data
	var seasonID, albumID any
	switch e.Type {
	case ExportEpisode:
		if parent != "" {
			seasonID = parent
		}
	case ExportTrack:
		if parent != "" {
			albumID = parent
		}
	}

	args := []any{
		mirrorTitleKind[e.Type], mirrorStr(d, "title"), mirrorYear(d), mirrorStr(d, "identityKey"),
		mirrorStr(d, "sortTitle"), mirrorStr(d, "addedAt"),
		mirrorStr(d, "tmdbId"), mirrorStr(d, "imdbId"),
		mirrorStr(d, "musicbrainzId"), mirrorStr(d, "musicbrainzRecordingId"),
		mirrorBool(d, "needsReview"), mirrorBool(d, "ambiguous"), mirrorHidden(e),
		seasonID, mirrorInt(d, "seasonNumber"), mirrorInt(d, "episodeNumber"),
		mirrorStr(d, "episodeLabel"),
		albumID, mirrorInt(d, "discNumber"), mirrorInt(d, "trackNumber"),
		mirrorStr(d, "overview"), mirrorStr(d, "tagline"), mirrorStr(d, "contentRating"),
		mirrorStr(d, "releaseDate"), mirrorInt(d, "runtimeMinutes"), mirrorStr(d, "studio"),
		mirrorStatus(d), mirrorStr(d, "displayTitle"),
	}
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO titles (kind, title, year, identity_key, sort_title, added_at,
			   tmdb_id, imdb_id, musicbrainz_id, musicbrainz_recording_id,
			   needs_review, ambiguous, hidden,
			   season_id, season_number, episode_number, episode_label,
			   album_id, disc_number, track_number,
			   overview, tagline, content_rating, release_date, runtime_minutes, studio,
			   enrichment_status, enriched_title,
			   id, library_id, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			append(args, id, m.libraryID, e.RemoteID)...)
	} else {
		_, err = m.tx.Exec(
			`UPDATE titles SET kind = ?, title = ?, year = ?, identity_key = ?, sort_title = ?,
			   added_at = ?, tmdb_id = ?, imdb_id = ?, musicbrainz_id = ?, musicbrainz_recording_id = ?,
			   needs_review = ?, ambiguous = ?, hidden = ?,
			   season_id = ?, season_number = ?, episode_number = ?, episode_label = ?,
			   album_id = ?, disc_number = ?, track_number = ?,
			   overview = ?, tagline = ?, content_rating = ?, release_date = ?,
			   runtime_minutes = ?, studio = ?, enrichment_status = ?, enriched_title = ?
			 WHERE id = ?`,
			append(args, id)...)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring title: %w", err)
	}

	if _, err := m.tx.Exec(`DELETE FROM title_genres WHERE title_id = ?`, id); err != nil {
		return fmt.Errorf("store: clearing mirrored title genres: %w", err)
	}
	for i, g := range mirrorStrings(d, "genres") {
		if _, err := m.tx.Exec(
			`INSERT INTO title_genres (title_id, genre, ord) VALUES (?,?,?)`, id, g, i,
		); err != nil {
			return fmt.Errorf("store: mirroring title genre: %w", err)
		}
	}
	if _, err := m.tx.Exec(`DELETE FROM title_credits WHERE title_id = ?`, id); err != nil {
		return fmt.Errorf("store: clearing mirrored title credits: %w", err)
	}
	for i, c := range mirrorCredits(d) {
		if _, err := m.tx.Exec(
			`INSERT INTO title_credits (title_id, person, role, character, kind, ord, person_ref)
			 VALUES (?,?,?,?,?,?,?)`,
			id, c.person, c.role, c.character, c.kind, i, c.personRef,
		); err != nil {
			return fmt.Errorf("store: mirroring title credit: %w", err)
		}
	}
	return nil
}

func (m *mirrorTx) writeEdition(id string, isNew bool, titleID string, e MirrorEntity) error {
	d := e.Data
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO editions (id, title_id, name, added_at, remote_id) VALUES (?,?,?,?,?)`,
			id, titleID, mirrorStr(d, "name"), mirrorStr(d, "addedAt"), e.RemoteID)
	} else {
		_, err = m.tx.Exec(
			`UPDATE editions SET title_id = ?, name = ?, added_at = ? WHERE id = ?`,
			titleID, mirrorStr(d, "name"), mirrorStr(d, "addedAt"), id)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring edition: %w", err)
	}
	return nil
}

// writeFile stores a File with an EMPTY path, and that is the point: the bytes
// are on the other household's disk and arrive through the relay (ADR-0056 §5),
// so there is no path here to invent. Migration 0063 relaxed the UNIQUE
// (edition_id, path) constraint to a partial index for exactly this row.
//
// The feed's `deletedAt` becomes present = 0, which is the same soft-delete a
// Missing File carries locally (ADR-0008), so every read path already knows what
// to do with it.
func (m *mirrorTx) writeFile(id string, isNew bool, editionID string, e MirrorEntity) error {
	d := e.Data
	present := 1
	if e.Deleted {
		present = 0
	}
	args := []any{
		editionID, mirrorStr(d, "container"), mirrorStr(d, "videoCodec"), mirrorStr(d, "audioCodec"),
		mirrorInt(d, "width"), mirrorInt(d, "height"), mirrorInt64(d, "bitrate"),
		mirrorInt64(d, "durationMs"), mirrorInt64(d, "sizeBytes"), mirrorInt(d, "partOrdinal"),
		present, mirrorStr(d, "addedAt"),
	}
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO files (edition_id, container, video_codec, audio_codec, width, height,
			   bitrate, duration_ms, size_bytes, part_ordinal, present, added_at,
			   id, path, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'',?)`,
			append(args, id, e.RemoteID)...)
	} else {
		_, err = m.tx.Exec(
			`UPDATE files SET edition_id = ?, container = ?, video_codec = ?, audio_codec = ?,
			   width = ?, height = ?, bitrate = ?, duration_ms = ?, size_bytes = ?,
			   part_ordinal = ?, present = ?, added_at = ? WHERE id = ?`,
			append(args, id)...)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring file: %w", err)
	}
	return nil
}

func (m *mirrorTx) writeStream(id string, isNew bool, fileID string, e MirrorEntity) error {
	d := e.Data
	args := []any{
		fileID, mirrorInt(d, "index"), mirrorStr(d, "kind"), mirrorStr(d, "codec"),
		mirrorStr(d, "language"), mirrorInt(d, "width"), mirrorInt(d, "height"),
		mirrorInt(d, "channels"), mirrorBool(d, "isDefault"), mirrorBool(d, "forced"),
		mirrorStr(d, "title"), mirrorBool(d, "commentary"), mirrorBool(d, "hearingImpaired"),
	}
	var err error
	if isNew {
		_, err = m.tx.Exec(
			`INSERT INTO streams (file_id, stream_index, kind, codec, language, width, height,
			   channels, is_default, forced, title, commentary, hearing_impaired, id, remote_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			append(args, id, e.RemoteID)...)
	} else {
		_, err = m.tx.Exec(
			`UPDATE streams SET file_id = ?, stream_index = ?, kind = ?, codec = ?, language = ?,
			   width = ?, height = ?, channels = ?, is_default = ?, forced = ?, title = ?,
			   commentary = ?, hearing_impaired = ? WHERE id = ?`,
			append(args, id)...)
	}
	if err != nil {
		return fmt.Errorf("store: mirroring stream: %w", err)
	}
	return nil
}

// writeEntityExtras rewrites the Show's / Season's / Artist's / Album's
// enrichment block, genres and cast — the fields the Export carries in `data`
// but that live beside the row here (export.go's decorateExportEntities, read
// backwards). Rewritten wholesale, which is how the enrichment pass writes them
// locally too, so the mirror produces exactly the rows a local enrich would.
func (m *mirrorTx) writeEntityExtras(entityType, id string, d map[string]any) error {
	overview := mirrorStr(d, "overview")
	rating := mirrorStr(d, "contentRating")
	network := mirrorStr(d, "network")
	status := mirrorStr(d, "enrichmentStatus")

	if overview+rating+network+status == "" {
		if _, err := m.tx.Exec(
			`DELETE FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?`,
			entityType, id,
		); err != nil {
			return fmt.Errorf("store: clearing mirrored entity enrichment: %w", err)
		}
	} else {
		if status == "" {
			status = "pending"
		}
		if _, err := m.tx.Exec(
			`INSERT INTO entity_enrichment (entity_type, entity_id, overview, content_rating,
			   network, enrichment_status)
			 VALUES (?,?,?,?,?,?)
			 ON CONFLICT (entity_type, entity_id) DO UPDATE SET
			   overview = excluded.overview, content_rating = excluded.content_rating,
			   network = excluded.network, enrichment_status = excluded.enrichment_status`,
			entityType, id, overview, rating, network, status,
		); err != nil {
			return fmt.Errorf("store: mirroring entity enrichment: %w", err)
		}
	}

	if _, err := m.tx.Exec(
		`DELETE FROM entity_genres WHERE entity_type = ? AND entity_id = ?`, entityType, id,
	); err != nil {
		return fmt.Errorf("store: clearing mirrored entity genres: %w", err)
	}
	for i, g := range mirrorStrings(d, "genres") {
		if _, err := m.tx.Exec(
			`INSERT INTO entity_genres (entity_type, entity_id, genre, ord) VALUES (?,?,?,?)`,
			entityType, id, g, i,
		); err != nil {
			return fmt.Errorf("store: mirroring entity genre: %w", err)
		}
	}

	if _, err := m.tx.Exec(
		`DELETE FROM entity_credits WHERE entity_type = ? AND entity_id = ?`, entityType, id,
	); err != nil {
		return fmt.Errorf("store: clearing mirrored entity credits: %w", err)
	}
	for i, c := range mirrorCredits(d) {
		if _, err := m.tx.Exec(
			`INSERT INTO entity_credits (id, entity_type, entity_id, person, character, kind,
			   ord, person_ref) VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("%s-%s-%d", entityType, id, i), entityType, id,
			c.person, c.character, c.kind, i, c.personRef,
		); err != nil {
			return fmt.Errorf("store: mirroring entity credit: %w", err)
		}
	}
	return nil
}

// prune deletes what a FULL pull did not mention. See ApplyMirror for why this
// runs only on a full pull, and for the Edition/Stream gap it exists to close.
//
// It walks child-before-parent so a delete never depends on a cascade it cannot
// see, and it deletes rather than hides: these rows are not tombstones the sharer
// sent, they are rows the sharer no longer has any record of at all.
func (m *mirrorTx) prune() error {
	for i := len(mirrorSources) - 1; i >= 0; i-- {
		src := mirrorSources[i]
		rows, err := m.tx.Query(src.query, m.libraryID)
		if err != nil {
			return fmt.Errorf("store: pruning mirrored %s: %w", src.table, err)
		}
		var doomed [][2]string // (remote id, local id)
		for rows.Next() {
			var remoteID, id string
			if err := rows.Scan(&remoteID, &id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scanning mirrored %s: %w", src.table, err)
			}
			if !m.seen[mirrorKey(src.table, remoteID)] {
				doomed = append(doomed, [2]string{remoteID, id})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: pruning mirrored %s: %w", src.table, err)
		}
		_ = rows.Close()

		for _, row := range doomed {
			if _, err := m.tx.Exec(
				`DELETE FROM `+src.table+` WHERE id = ?`, row[1], //nolint:gosec // table from a fixed list
			); err != nil {
				return fmt.Errorf("store: pruning mirrored %s: %w", src.table, err)
			}
			delete(m.local, mirrorKey(src.table, row[0]))
		}
	}
	return nil
}
