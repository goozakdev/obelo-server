package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// The Library Export (ADR-0056 §4): one flat, cursor-paginated feed of every
// entity beneath a Library, for the mirror on another household's Server.
//
// This file is THE place the sharer decides what a remote peer may learn about a
// Title. That is the whole reason the Export exists rather than a walk of the
// browse API, so the projection is written out longhand here — one builder per
// entity type, naming every field that leaves the house — instead of being
// derived from a struct somebody may later widen. Nothing here selects a `path`,
// a folder name, an mtime or an enrichment origin/attempt column; the only
// external ids that cross are the ones the browse API already publishes
// (tmdbId, imdbId, and the MusicBrainz ids).
//
// Ordering is a keyset seek on (updated_at, type, id) — the same shape as
// ListTitles' (sort_title, id), extended by the type because the feed
// interleaves ten tables. updated_at is maintained by the triggers migration
// 0061 installs, so a row's stamp does not depend on the writer remembering.

// Export entity type names. They are the wire values of `type` and, because the
// keyset tuple is (updated_at, type, id), also the tie-break order — compared as
// plain strings, so this list is sorted the way SQLite would sort it.
const (
	ExportAlbum   = "album"
	ExportArtist  = "artist"
	ExportEdition = "edition"
	ExportEpisode = "episode"
	ExportFile    = "file"
	ExportSeason  = "season"
	ExportShow    = "show"
	ExportStream  = "stream"
	ExportTitle   = "title"
	ExportTrack   = "track"
)

// ExportEntity is one row of the feed. Data is the entity's public field set,
// with zero values omitted (absent means empty/0/false, exactly as the browse
// API's omitempty fields do). DeletedAt is non-empty for a tombstone: a Missing
// File (soft delete, ADR-0008) or a hidden Title/Show/Season/Artist/Album — the
// derived "every File is Missing" state. It carries the instant the row last
// changed, which is when it became deleted.
type ExportEntity struct {
	Type      string
	ID        string
	ParentID  string
	UpdatedAt string
	DeletedAt string
	Data      map[string]any
}

// ExportCursor is a decoded position in the feed: the (updated_at, type, id) of
// the last row already delivered. The zero value means "from the beginning",
// which is the full pull.
type ExportCursor struct {
	UpdatedAt string
	Type      string
	ID        string
}

// IsZero reports whether the cursor names no position (a full pull).
func (c ExportCursor) IsZero() bool {
	return c.UpdatedAt == "" && c.Type == "" && c.ID == ""
}

// ExportPage is one page of the feed plus whether more rows follow.
type ExportPage struct {
	Entities []ExportEntity
	HasMore  bool
}

// exportSource is one entity type's contribution to the feed: the SQL that
// selects its rows for a Library and the scanner that turns a row into an
// ExportEntity. Each query selects its own columns, leading with updated_at and
// id (and the parent id where there is one) so every scanner starts the same way.
type exportSource struct {
	typ string
	// query is everything up to (but not including) WHERE. where is the Library
	// scope, parameterized on the library id and nothing else — the keyset
	// predicate is appended to it. updated and id name the QUALIFIED columns the
	// predicate and the ORDER BY both use, since several sources join.
	query   string
	where   string
	updated string
	id      string
	scan    func(*sql.Rows) (ExportEntity, error)
}

// ExportLibrary returns one page of the Library's export feed strictly after
// `after`, at most limit rows, ordered by (updated_at, type, id). It reads
// limit+1 rows per source to detect HasMore without a count.
//
// It does NOT check that the Library exists or that the caller may see it —
// that is the service/handler's job, exactly as for ListTitles.
func (db *DB) ExportLibrary(libraryID string, after ExportCursor, limit int) (ExportPage, error) {
	if limit <= 0 {
		limit = 100
	}

	sources := exportSources()
	var all []ExportEntity
	for _, src := range sources {
		rows, err := db.queryExportSource(libraryID, src, after, limit+1)
		if err != nil {
			return ExportPage{}, err
		}
		all = append(all, rows...)
	}

	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt < b.UpdatedAt
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})

	page := ExportPage{}
	if len(all) > limit {
		page.HasMore = true
		all = all[:limit]
	}
	page.Entities = all

	if err := db.decorateExport(page.Entities); err != nil {
		return ExportPage{}, err
	}
	return page, nil
}

// queryExportSource runs one source's keyset query.
func (db *DB) queryExportSource(libraryID string, src exportSource, after ExportCursor, limit int) ([]ExportEntity, error) {
	where := src.where
	args := []any{libraryID}

	if !after.IsZero() {
		// Within one source the type is constant, so the three-way tuple
		// comparison collapses to a two-way one whose tail depends on how this
		// source's type sorts against the cursor's.
		switch {
		case src.typ > after.Type:
			// Same instant is still ahead of the cursor for this type.
			where += fmt.Sprintf(" AND %s >= ?", src.updated)
			args = append(args, after.UpdatedAt)
		case src.typ < after.Type:
			where += fmt.Sprintf(" AND %s > ?", src.updated)
			args = append(args, after.UpdatedAt)
		default:
			where += fmt.Sprintf(" AND (%s, %s) > (?, ?)", src.updated, src.id)
			args = append(args, after.UpdatedAt, after.ID)
		}
	}

	q := fmt.Sprintf("%s WHERE %s ORDER BY %s ASC, %s ASC LIMIT ?",
		src.query, where, src.updated, src.id)
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: exporting %s: %w", src.typ, err)
	}
	defer rows.Close()

	var out []ExportEntity
	for rows.Next() {
		e, err := src.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning exported %s: %w", src.typ, err)
		}
		e.Type = src.typ
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: exporting %s: %w", src.typ, err)
	}
	return out, nil
}

// --- projections -------------------------------------------------------------

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func putInt(m map[string]any, k string, v int) {
	if v != 0 {
		m[k] = v
	}
}

func putInt64(m map[string]any, k string, v int64) {
	if v != 0 {
		m[k] = v
	}
}

func putBool(m map[string]any, k string, v bool) {
	if v {
		m[k] = v
	}
}

// sqliteDateTime is the layout SQLite's datetime('now') writes for the added_at
// defaults; exportInstant folds it to RFC3339 so every timestamp that crosses
// the Link is the one shape the contract promises, whichever column it came
// from. An unrecognized value is passed through rather than dropped (api/time.go
// takes the same posture on the browse side).
const sqliteDateTime = "2006-01-02 15:04:05"

func exportInstant(stored string) string {
	if stored == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, stored); err == nil {
		return stored
	}
	if t, err := time.Parse(sqliteDateTime, stored); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return stored
}

// tombstone stamps DeletedAt when the row is soft-deleted. The instant is the
// row's own updated_at: the stamp advanced when the row was hidden/marked
// Missing, which is exactly when it died.
func tombstone(e *ExportEntity, deleted bool) {
	if deleted {
		e.DeletedAt = e.UpdatedAt
	}
}

// exportSources is the ten queries, one per wire type. The three Title kinds are
// three sources over the titles table because the wire distinguishes them
// (`title` = a Movie, `episode`, `track`) and because their parent differs.
func exportSources() []exportSource {
	return []exportSource{
		{
			typ:     ExportShow,
			query:   `SELECT updated_at, id, title, year, sort_title, identity_key, tmdb_id, imdb_id, needs_review, hidden, added_at FROM shows`,
			where:   "library_id = ?",
			updated: "updated_at", id: "id",
			scan: scanExportShow,
		},
		{
			typ: ExportSeason,
			query: `SELECT s.updated_at, s.id, s.show_id, s.season_number, s.identity_key, s.hidden, s.added_at
			          FROM seasons s JOIN shows sh ON sh.id = s.show_id`,
			where:   "sh.library_id = ?",
			updated: "s.updated_at", id: "s.id",
			scan: scanExportSeason,
		},
		{
			typ:     ExportArtist,
			query:   `SELECT updated_at, id, name, sort_name, identity_key, musicbrainz_id, hidden, added_at FROM artists`,
			where:   "library_id = ?",
			updated: "updated_at", id: "id",
			scan: scanExportArtist,
		},
		{
			typ: ExportAlbum,
			query: `SELECT a.updated_at, a.id, a.artist_id, a.title, a.year, a.sort_title, a.identity_key,
			               a.release_type, a.musicbrainz_id, a.musicbrainz_release_id, a.hidden, a.added_at
			          FROM albums a JOIN artists ar ON ar.id = a.artist_id`,
			where:   "ar.library_id = ?",
			updated: "a.updated_at", id: "a.id",
			scan: scanExportAlbum,
		},
		{
			typ:     ExportTitle,
			query:   `SELECT ` + exportTitleColumns + ` FROM titles`,
			where:   "library_id = ? AND kind = 'movie'",
			updated: "updated_at", id: "id",
			scan: scanExportTitleRow(ExportTitle),
		},
		{
			typ:     ExportEpisode,
			query:   `SELECT ` + exportTitleColumns + ` FROM titles`,
			where:   "library_id = ? AND kind = 'episode'",
			updated: "updated_at", id: "id",
			scan: scanExportTitleRow(ExportEpisode),
		},
		{
			typ:     ExportTrack,
			query:   `SELECT ` + exportTitleColumns + ` FROM titles`,
			where:   "library_id = ? AND kind = 'track'",
			updated: "updated_at", id: "id",
			scan: scanExportTitleRow(ExportTrack),
		},
		{
			typ: ExportEdition,
			query: `SELECT e.updated_at, e.id, e.title_id, e.name, e.added_at
			          FROM editions e JOIN titles t ON t.id = e.title_id`,
			where:   "t.library_id = ?",
			updated: "e.updated_at", id: "e.id",
			scan: scanExportEdition,
		},
		{
			typ: ExportFile,
			query: `SELECT f.updated_at, f.id, f.edition_id, f.container, f.video_codec, f.audio_codec,
			               f.width, f.height, f.bitrate, f.duration_ms, f.size_bytes, f.part_ordinal,
			               f.present, f.added_at
			          FROM files f JOIN editions e ON e.id = f.edition_id
			                       JOIN titles t   ON t.id = e.title_id`,
			where:   "t.library_id = ?",
			updated: "f.updated_at", id: "f.id",
			scan: scanExportFile,
		},
		{
			typ: ExportStream,
			query: `SELECT st.updated_at, st.id, st.file_id, st.stream_index, st.kind, st.codec, st.language,
			               st.width, st.height, st.channels, st.is_default, st.forced, st.title,
			               st.commentary, st.hearing_impaired
			          FROM streams st JOIN files f    ON f.id = st.file_id
			                          JOIN editions e ON e.id = f.edition_id
			                          JOIN titles t   ON t.id = e.title_id`,
			where:   "t.library_id = ?",
			updated: "st.updated_at", id: "st.id",
			scan: scanExportStream,
		},
	}
}

// exportTitleColumns is the Title projection: identity, ordering, and the
// descriptive fields Enrichment writes — the same set the browse API surfaces.
// Deliberately absent: enrichment_source / enrichment_id_origin /
// enrichment_attempts / enrichment_retry_at / enrichment_reason /
// enrichment_tmdb_id / enrichment_imdb_id / enrichment_season /
// enrichment_episode (this Server's bookkeeping about how it reached its own
// answer, which is nobody else's business and means nothing on their side), and
// reviewed (an Admin's dismissal of a local attention flag).
const exportTitleColumns = `updated_at, id, kind, title, year, sort_title, identity_key,
	tmdb_id, imdb_id, musicbrainz_id, musicbrainz_recording_id,
	needs_review, ambiguous, hidden,
	season_id, season_number, episode_number, episode_label,
	album_id, disc_number, track_number,
	overview, tagline, content_rating, release_date, runtime_minutes, studio,
	enrichment_status, enriched_title, added_at`

func scanExportShow(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                                             ExportEntity
		title, sortTitle, identityKey, tmdbID, imdbID string
		addedAt                                       string
		year                                          sql.NullInt64
		needsReview, hidden                           int
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &title, &year, &sortTitle, &identityKey,
		&tmdbID, &imdbID, &needsReview, &hidden, &addedAt); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{"title": title}
	putInt(d, "year", int(year.Int64))
	putStr(d, "sortTitle", sortTitle)
	putStr(d, "identityKey", identityKey)
	putStr(d, "tmdbId", tmdbID)
	putStr(d, "imdbId", imdbID)
	putBool(d, "needsReview", needsReview != 0)
	putStr(d, "addedAt", exportInstant(addedAt))
	e.Data = d
	tombstone(&e, hidden != 0)
	return e, nil
}

func scanExportSeason(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                    ExportEntity
		identityKey, addedAt string
		seasonNumber, hidden int
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &e.ParentID, &seasonNumber, &identityKey,
		&hidden, &addedAt); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{"seasonNumber": seasonNumber}
	putStr(d, "identityKey", identityKey)
	putStr(d, "addedAt", exportInstant(addedAt))
	e.Data = d
	tombstone(&e, hidden != 0)
	return e, nil
}

func scanExportArtist(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                                        ExportEntity
		name, sortName, identityKey, mbID, added string
		hidden                                   int
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &name, &sortName, &identityKey, &mbID,
		&hidden, &added); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{"name": name}
	putStr(d, "sortName", sortName)
	putStr(d, "identityKey", identityKey)
	putStr(d, "musicbrainzId", mbID)
	putStr(d, "addedAt", exportInstant(added))
	e.Data = d
	tombstone(&e, hidden != 0)
	return e, nil
}

func scanExportAlbum(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                                                                ExportEntity
		title, sortTitle, identityKey, releaseType, mbID, mbRelID, added string
		year                                                             sql.NullInt64
		hidden                                                           int
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &e.ParentID, &title, &year, &sortTitle,
		&identityKey, &releaseType, &mbID, &mbRelID, &hidden, &added); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{"title": title}
	putInt(d, "year", int(year.Int64))
	putStr(d, "sortTitle", sortTitle)
	putStr(d, "identityKey", identityKey)
	putStr(d, "releaseType", releaseType)
	putStr(d, "musicbrainzId", mbID)
	putStr(d, "musicbrainzReleaseId", mbRelID)
	putStr(d, "addedAt", exportInstant(added))
	e.Data = d
	tombstone(&e, hidden != 0)
	return e, nil
}

// scanExportTitleRow builds the scanner for one of the three Title wire types.
// The parent is the Season for an Episode and the Album for a Track; a Movie has
// none (its parent is the Library, which the body already names).
func scanExportTitleRow(typ string) func(*sql.Rows) (ExportEntity, error) {
	return func(rows *sql.Rows) (ExportEntity, error) {
		var (
			e                                                             ExportEntity
			kind, title, sortTitle, identityKey                           string
			tmdbID, imdbID, mbID, mbRecID                                 string
			episodeLabel                                                  string
			overview, tagline, contentRating, releaseDate, studio         string
			enrichmentStatus, enrichedTitle, addedAt                      string
			year                                                          sql.NullInt64
			seasonID, albumID                                             sql.NullString
			needsReview, ambiguous, hidden                                int
			seasonNumber, episodeNumber, discNumber, trackNumber, runtime int
		)
		if err := rows.Scan(&e.UpdatedAt, &e.ID, &kind, &title, &year, &sortTitle, &identityKey,
			&tmdbID, &imdbID, &mbID, &mbRecID,
			&needsReview, &ambiguous, &hidden,
			&seasonID, &seasonNumber, &episodeNumber, &episodeLabel,
			&albumID, &discNumber, &trackNumber,
			&overview, &tagline, &contentRating, &releaseDate, &runtime, &studio,
			&enrichmentStatus, &enrichedTitle, &addedAt); err != nil {
			return ExportEntity{}, err
		}
		switch typ {
		case ExportEpisode:
			e.ParentID = seasonID.String
		case ExportTrack:
			e.ParentID = albumID.String
		}
		d := map[string]any{"title": title}
		putInt(d, "year", int(year.Int64))
		putStr(d, "sortTitle", sortTitle)
		putStr(d, "identityKey", identityKey)
		putStr(d, "tmdbId", tmdbID)
		putStr(d, "imdbId", imdbID)
		putStr(d, "musicbrainzId", mbID)
		putStr(d, "musicbrainzRecordingId", mbRecID)
		putBool(d, "needsReview", needsReview != 0)
		putBool(d, "ambiguous", ambiguous != 0)
		if typ == ExportEpisode {
			putInt(d, "seasonNumber", seasonNumber)
			putInt(d, "episodeNumber", episodeNumber)
			putStr(d, "episodeLabel", episodeLabel)
		}
		if typ == ExportTrack {
			putInt(d, "discNumber", discNumber)
			putInt(d, "trackNumber", trackNumber)
		}
		putStr(d, "overview", overview)
		putStr(d, "tagline", tagline)
		putStr(d, "contentRating", contentRating)
		putStr(d, "releaseDate", releaseDate)
		putInt(d, "runtimeMinutes", runtime)
		putStr(d, "studio", studio)
		putStr(d, "enrichmentStatus", enrichmentStatus)
		putStr(d, "displayTitle", enrichedTitle)
		putStr(d, "addedAt", exportInstant(addedAt))
		e.Data = d
		tombstone(&e, hidden != 0)
		return e, nil
	}
}

func scanExportEdition(rows *sql.Rows) (ExportEntity, error) {
	var e ExportEntity
	var name, added string
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &e.ParentID, &name, &added); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{}
	putStr(d, "name", name)
	putStr(d, "addedAt", exportInstant(added))
	e.Data = d
	return e, nil
}

// scanExportFile carries what a Capability profile needs to badge a File and
// nothing that names disk: no path, no mtime. The `present` flag becomes a
// tombstone rather than a field, so the mirror sees a Missing File exactly as it
// sees a deleted one (ADR-0008).
func scanExportFile(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                                   ExportEntity
		container, videoCodec, audioCodec   string
		added                               string
		width, height, partOrdinal, present int
		bitrate, durationMs, sizeBytes      int64
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &e.ParentID, &container, &videoCodec, &audioCodec,
		&width, &height, &bitrate, &durationMs, &sizeBytes, &partOrdinal, &present, &added); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{}
	putStr(d, "container", container)
	putStr(d, "videoCodec", videoCodec)
	putStr(d, "audioCodec", audioCodec)
	putInt(d, "width", width)
	putInt(d, "height", height)
	putInt64(d, "bitrate", bitrate)
	putInt64(d, "durationMs", durationMs)
	putInt64(d, "sizeBytes", sizeBytes)
	putInt(d, "partOrdinal", partOrdinal)
	putStr(d, "addedAt", exportInstant(added))
	e.Data = d
	tombstone(&e, present == 0)
	return e, nil
}

func scanExportStream(rows *sql.Rows) (ExportEntity, error) {
	var (
		e                                              ExportEntity
		kind, codec, language, title                   string
		index, width, height, channels                 int
		isDefault, forced, commentary, hearingImpaired int
	)
	if err := rows.Scan(&e.UpdatedAt, &e.ID, &e.ParentID, &index, &kind, &codec, &language,
		&width, &height, &channels, &isDefault, &forced, &title,
		&commentary, &hearingImpaired); err != nil {
		return ExportEntity{}, err
	}
	d := map[string]any{"index": index, "kind": kind}
	putStr(d, "codec", codec)
	putStr(d, "language", language)
	putInt(d, "width", width)
	putInt(d, "height", height)
	putInt(d, "channels", channels)
	putBool(d, "isDefault", isDefault != 0)
	putBool(d, "forced", forced != 0)
	putStr(d, "title", title)
	putBool(d, "commentary", commentary != 0)
	putBool(d, "hearingImpaired", hearingImpaired != 0)
	e.Data = d
	return e, nil
}

// --- side tables -------------------------------------------------------------

// decorateExport attaches the fields that live beside the row rather than on it:
// a Title's genres and cast (title_genres / title_credits) and a Show's /
// Season's / Artist's / Album's enrichment, genres and cast (the polymorphic
// entity_* tables). Both are bulk reads over the page, never per row, and both
// are covered by migration 0061's triggers so a write to either advances the
// owner's updated_at and the change actually reaches the mirror.
func (db *DB) decorateExport(entities []ExportEntity) error {
	var titleIDs []string
	byTitle := map[string]*ExportEntity{}
	byEntity := map[string]*ExportEntity{}
	for i := range entities {
		e := &entities[i]
		switch e.Type {
		case ExportTitle, ExportEpisode, ExportTrack:
			titleIDs = append(titleIDs, e.ID)
			byTitle[e.ID] = e
		case ExportShow, ExportSeason, ExportArtist, ExportAlbum:
			byEntity[e.Type+"\x00"+e.ID] = e
		}
	}

	if len(titleIDs) > 0 {
		genres, err := db.GenresForTitles(titleIDs)
		if err != nil {
			return err
		}
		for id, g := range genres {
			if e, ok := byTitle[id]; ok && len(g) > 0 {
				e.Data["genres"] = g
			}
		}
		credits, err := db.creditsForExportTitles(titleIDs)
		if err != nil {
			return err
		}
		for id, c := range credits {
			if e, ok := byTitle[id]; ok && len(c) > 0 {
				e.Data["cast"] = c
			}
		}
		if err := db.decorateExportTitleArtwork(byTitle); err != nil {
			return err
		}
	}

	if len(byEntity) == 0 {
		return nil
	}
	keys := make([]string, 0, len(byEntity))
	for k := range byEntity {
		keys = append(keys, k)
	}
	if err := db.decorateExportEntities(keys, byEntity); err != nil {
		return err
	}
	return db.decorateExportArtwork(byEntity)
}

// decorateExportArtwork attaches `artworkRoles` + `artworkVersion` to a page's
// Show/Season/Artist/Album entities (.scratch/linked-servers issues 19, 20).
// These are the one thing about a mirrored entity's artwork the receiver cannot
// recover on its own: which roles the sharer advertises and a token that changes
// when the image does. The bytes themselves never cross here — they are relayed
// on demand and cached (ADR-0056 §5) — so this carries the SIGNAL, not the file.
//
// Roles come from entity_artwork for all four types (every source — a local,
// fetched or uploaded image is servable and therefore advertisable), PLUS an
// Album's LOCAL cover, which lives in albums.artwork_path rather than in
// entity_artwork. The version is the newest entity_artwork added_at for the
// entity — the exact token EntityArtworkVersionsForMany computes locally — and is
// absent for an Album whose only cover is the local file (which carries no such
// token, exactly as decorateAlbum leaves it on the sharer). Emitted only when the
// entity actually has artwork, so a mirror advertises nothing the sharer lacks
// (no 404 storms). A Season folds in here exactly like a Show (issue 20); a Movie
// poster lives in the `artwork` table instead and is carried by
// decorateExportTitleArtwork.
func (db *DB) decorateExportArtwork(byEntity map[string]*ExportEntity) error {
	var ids []string
	for _, e := range byEntity {
		switch e.Type {
		case ExportShow, ExportSeason, ExportArtist, ExportAlbum:
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ph, args := inPlaceholders(ids)

	roles := map[string]map[string]bool{} // "<type>\x00<id>" -> set of roles
	version := map[string]string{}        // "<type>\x00<id>" -> newest added_at

	art, err := db.Query(
		`SELECT entity_type, entity_id, role, added_at FROM entity_artwork
		   WHERE entity_type IN ('show','season','artist','album') AND entity_id IN (`+ph+`)`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting entity artwork: %w", err)
	}
	for art.Next() {
		var typ, id, role, added string
		if err := art.Scan(&typ, &id, &role, &added); err != nil {
			_ = art.Close()
			return fmt.Errorf("store: scanning exported entity artwork: %w", err)
		}
		k := typ + "\x00" + id
		if roles[k] == nil {
			roles[k] = map[string]bool{}
		}
		roles[k][role] = true
		if added > version[k] {
			version[k] = added
		}
	}
	if err := art.Err(); err != nil {
		_ = art.Close()
		return fmt.Errorf("store: exporting entity artwork: %w", err)
	}
	_ = art.Close()

	// An Album's LOCAL cover (cover.jpg/folder.jpg) is albums.artwork_path, not an
	// entity_artwork row — decorateAlbum flips HasArtwork on it locally without a
	// role. It advertises the same single "cover" over the Link; its version stays
	// absent, like a local-only cover's does on the sharer.
	alb, err := db.Query(
		`SELECT id FROM albums WHERE artwork_path <> '' AND id IN (`+ph+`)`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting album cover: %w", err)
	}
	for alb.Next() {
		var id string
		if err := alb.Scan(&id); err != nil {
			_ = alb.Close()
			return fmt.Errorf("store: scanning exported album cover: %w", err)
		}
		k := ExportAlbum + "\x00" + id
		if roles[k] == nil {
			roles[k] = map[string]bool{}
		}
		roles[k]["cover"] = true
	}
	if err := alb.Err(); err != nil {
		_ = alb.Close()
		return fmt.Errorf("store: exporting album cover: %w", err)
	}
	_ = alb.Close()

	for k, e := range byEntity {
		rs := roles[k]
		if len(rs) == 0 {
			continue
		}
		list := make([]string, 0, len(rs))
		for r := range rs {
			list = append(list, r)
		}
		sort.Strings(list)
		e.Data["artworkRoles"] = list
		putStr(e.Data, "artworkVersion", exportInstant(version[k]))
	}
	return nil
}

// decorateExportTitleArtwork attaches `artworkRoles` + `artworkVersion` to a
// page's MOVIE entities (.scratch/linked-servers issue 20), the Title analogue of
// decorateExportArtwork. A Title's artwork lives in the `artwork` table keyed by
// title_id (roles poster/background/logo, every source servable), not in
// entity_artwork — so it needs its own read, but lands under the same two `data`
// keys and rides the same mirror/read machinery as a Show's.
//
// Only ExportTitle (movies) are decorated: an Episode still already advertises off
// its enrichment status and a Track uses the album cover, so neither carries a
// redundant signal here (issue 20). Emitted only when the movie actually has
// artwork, so a mirror advertises nothing the sharer lacks.
func (db *DB) decorateExportTitleArtwork(byTitle map[string]*ExportEntity) error {
	var ids []string
	for _, e := range byTitle {
		if e.Type == ExportTitle {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ph, args := inPlaceholders(ids)

	roles := map[string]map[string]bool{} // title id -> set of roles
	version := map[string]string{}        // title id -> newest added_at

	rows, err := db.Query(
		`SELECT title_id, role, added_at FROM artwork WHERE title_id IN (`+ph+`)`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting title artwork: %w", err)
	}
	for rows.Next() {
		var id, role, added string
		if err := rows.Scan(&id, &role, &added); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scanning exported title artwork: %w", err)
		}
		if roles[id] == nil {
			roles[id] = map[string]bool{}
		}
		roles[id][role] = true
		if added > version[id] {
			version[id] = added
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: exporting title artwork: %w", err)
	}
	_ = rows.Close()

	for id, e := range byTitle {
		rs := roles[id]
		if len(rs) == 0 {
			continue
		}
		list := make([]string, 0, len(rs))
		for r := range rs {
			list = append(list, r)
		}
		sort.Strings(list)
		e.Data["artworkRoles"] = list
		putStr(e.Data, "artworkVersion", exportInstant(version[id]))
	}
	return nil
}

// exportCredit is one cast/crew member as it crosses the Link: the same fields
// the browse API's creditJSON carries minus PhotoVersion, which is a local
// cache-bust token for a local file and means nothing on the other side.
type exportCredit struct {
	Person    string `json:"person"`
	Role      string `json:"role,omitempty"`
	Character string `json:"character,omitempty"`
	Kind      string `json:"kind,omitempty"`
	PersonRef string `json:"personId,omitempty"`
}

func (db *DB) creditsForExportTitles(ids []string) (map[string][]exportCredit, error) {
	ph, args := inPlaceholders(ids)
	rows, err := db.Query(
		`SELECT title_id, person, role, character, kind, person_ref
		   FROM title_credits WHERE title_id IN (`+ph+`)
		  ORDER BY title_id, kind, ord, person`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: exporting title credits: %w", err)
	}
	defer rows.Close()
	out := map[string][]exportCredit{}
	for rows.Next() {
		var id string
		var c exportCredit
		if err := rows.Scan(&id, &c.Person, &c.Role, &c.Character, &c.Kind, &c.PersonRef); err != nil {
			return nil, fmt.Errorf("store: scanning exported credit: %w", err)
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// decorateExportEntities fills a Show's / Season's / Artist's / Album's
// enrichment block, genres and cast. The three tables are keyed by
// (entity_type, entity_id) and the page mixes types, so each read pulls every
// row for the page's ids and the caller-side map discards a same-id hit of the
// wrong type.
func (db *DB) decorateExportEntities(keys []string, byEntity map[string]*ExportEntity) error {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k[indexAfterNul(k):])
	}
	ph, args := inPlaceholders(ids)

	enr, err := db.Query(
		`SELECT entity_type, entity_id, overview, content_rating, network, enrichment_status
		   FROM entity_enrichment WHERE entity_id IN (`+ph+`)`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting entity enrichment: %w", err)
	}
	for enr.Next() {
		var typ, id, overview, rating, network, status string
		if err := enr.Scan(&typ, &id, &overview, &rating, &network, &status); err != nil {
			_ = enr.Close()
			return fmt.Errorf("store: scanning exported entity enrichment: %w", err)
		}
		if e, ok := byEntity[typ+"\x00"+id]; ok {
			putStr(e.Data, "overview", overview)
			putStr(e.Data, "contentRating", rating)
			putStr(e.Data, "network", network)
			putStr(e.Data, "enrichmentStatus", status)
		}
	}
	if err := enr.Err(); err != nil {
		_ = enr.Close()
		return fmt.Errorf("store: exporting entity enrichment: %w", err)
	}
	_ = enr.Close()

	gen, err := db.Query(
		`SELECT entity_type, entity_id, genre FROM entity_genres
		  WHERE entity_id IN (`+ph+`) ORDER BY entity_id, ord, genre`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting entity genres: %w", err)
	}
	for gen.Next() {
		var typ, id, genre string
		if err := gen.Scan(&typ, &id, &genre); err != nil {
			_ = gen.Close()
			return fmt.Errorf("store: scanning exported entity genre: %w", err)
		}
		if e, ok := byEntity[typ+"\x00"+id]; ok {
			existing, _ := e.Data["genres"].([]string)
			e.Data["genres"] = append(existing, genre)
		}
	}
	if err := gen.Err(); err != nil {
		_ = gen.Close()
		return fmt.Errorf("store: exporting entity genres: %w", err)
	}
	_ = gen.Close()

	cre, err := db.Query(
		`SELECT entity_type, entity_id, person, '' AS role, character, kind, person_ref
		   FROM entity_credits WHERE entity_id IN (`+ph+`)
		  ORDER BY entity_id, kind, ord, person`, args...)
	if err != nil {
		return fmt.Errorf("store: exporting entity credits: %w", err)
	}
	defer cre.Close()
	for cre.Next() {
		var typ, id string
		var c exportCredit
		if err := cre.Scan(&typ, &id, &c.Person, &c.Role, &c.Character, &c.Kind, &c.PersonRef); err != nil {
			return fmt.Errorf("store: scanning exported entity credit: %w", err)
		}
		if e, ok := byEntity[typ+"\x00"+id]; ok {
			existing, _ := e.Data["cast"].([]exportCredit)
			e.Data["cast"] = append(existing, c)
		}
	}
	return cre.Err()
}

// indexAfterNul returns the offset just past the NUL separator in a
// "<type>\x00<id>" map key.
func indexAfterNul(k string) int {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return i + 1
		}
	}
	return 0
}

// inPlaceholders builds "?, ?, …" and the matching arg slice for an IN clause.
// (store/access.go's placeholders takes a count; this one also carries the args.)
func inPlaceholders(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "''", nil
	}
	b := make([]byte, 0, len(ids)*3)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, '?')
		args = append(args, id)
	}
	return string(b), args
}
