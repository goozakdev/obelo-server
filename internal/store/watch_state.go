package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Watch state persistence (issue 08). Watch state is per-(User, Title): resume
// position + watched/unwatched, keyed to the Title's stable identity row so it
// survives an Edition swap or a file rename (ADR-0014). The store is a thin
// upsert over the watch_state table; the Watched-threshold POLICY (the ~90%/~2%
// constants) lives in the playback domain, not here — this layer only persists
// whatever resume/watched it is told to.

// WatchState is one User's playback state for one Title. The zero value (no row)
// means "not started": resume 0, unwatched.
type WatchState struct {
	UserID           string
	TitleID          string
	ResumePositionMs int64
	Watched          bool
	UpdatedAt        string
}

// WatchStateFor returns the (User, Title) watch state, or a zero-value
// WatchState (resume 0, unwatched) when no row exists — "not started" is the
// natural default, so callers never special-case ErrNotFound here.
func (db *DB) WatchStateFor(userID, titleID string) (WatchState, error) {
	var ws WatchState
	var resume int64
	var watched int
	err := db.QueryRow(
		`SELECT resume_position_ms, watched, updated_at
		   FROM watch_state WHERE user_id = ? AND title_id = ?`,
		userID, titleID,
	).Scan(&resume, &watched, &ws.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchState{UserID: userID, TitleID: titleID}, nil
	}
	if err != nil {
		return WatchState{}, fmt.Errorf("store: reading watch state: %w", err)
	}
	ws.UserID = userID
	ws.TitleID = titleID
	ws.ResumePositionMs = resume
	ws.Watched = watched != 0
	return ws, nil
}

// SaveWatchState upserts the (User, Title) watch state to exactly the given
// resume + watched, refreshing updated_at. The UNIQUE(user_id, title_id)
// constraint makes the ON CONFLICT a clean last-write-wins on position — two
// Devices reporting progress simply overwrite each other (no locking/merge),
// the concurrency model the issue specifies. updated_at doubles as the
// "most-recently-played" sort key for Continue Watching.
//
// played distinguishes the two callers behind this one upsert (ADR-0028): the
// PLAYBACK progress path passes played=true, so the write STAMPS played_at (the
// recency signal the Up Next resume point anchors on); the MANUAL mark-watched
// toggle passes played=false, leaving any existing played_at untouched so a
// bookkeeping mark never moves the anchor. played_at is only ever set forward, so
// a not-played write COALESCEs it back to the stored value (never NULLs it).
func (db *DB) SaveWatchState(userID, titleID string, resumeMs int64, watched, played bool) error {
	if resumeMs < 0 {
		resumeMs = 0
	}
	// Millisecond-precision timestamp (strftime %f) so Continue Watching's
	// most-recently-played ordering is stable even for two writes in the same
	// second — datetime('now') is only second-granular, which would tie.
	_, err := db.Exec(
		`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, played_at, updated_at)
		 VALUES (?, ?, ?, ?, ?,
		         CASE WHEN ? = 1 THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') END,
		         strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		 ON CONFLICT (user_id, title_id) DO UPDATE SET
		   resume_position_ms = excluded.resume_position_ms,
		   watched            = excluded.watched,
		   played_at          = COALESCE(excluded.played_at, watch_state.played_at),
		   updated_at         = excluded.updated_at`,
		uuid.NewString(), userID, titleID, resumeMs, boolToInt(watched), boolToInt(played),
	)
	if err != nil {
		return fmt.Errorf("store: saving watch state: %w", err)
	}
	// Multi-episode file (S01E05-E06): one File maps to TWO Episode Titles
	// (naming-convention.md). It plays once, so a progress/watched write against
	// one Episode propagates the SAME resume + watched + played_at to its co-File
	// sibling(s), so "watching it marks both" and both share the anchor's recency.
	// A Movie / single Episode has no co-File sibling, so this is a no-op for them.
	if err := db.propagateToSiblingEpisodes(userID, titleID, resumeMs, watched, played); err != nil {
		return err
	}
	return nil
}

// propagateToSiblingEpisodes copies (resume, watched, played_at) to every OTHER
// Episode Title that shares a File path with titleID — the two Titles of a
// multi-episode file. The shared-File join is the authority: the scanner gives
// both Episodes the same physical File (different edition rows, same path).
// Movies and single-file Episodes have no such sibling, so nothing is written.
// played carries through unchanged so a playback write stamps the siblings'
// played_at too (a manual mark leaves it), mirroring SaveWatchState.
func (db *DB) propagateToSiblingEpisodes(userID, titleID string, resumeMs int64, watched, played bool) error {
	rows, err := db.Query(
		`SELECT DISTINCT t2.id
		   FROM titles t1
		   JOIN editions e1 ON e1.title_id = t1.id
		   JOIN files    f1 ON f1.edition_id = e1.id
		   JOIN files    f2 ON f2.path = f1.path
		   JOIN editions e2 ON e2.id = f2.edition_id
		   JOIN titles   t2 ON t2.id = e2.title_id
		  WHERE t1.id = ? AND t2.id <> ? AND t2.kind = 'episode'`,
		titleID, titleID)
	if err != nil {
		return fmt.Errorf("store: finding sibling episodes: %w", err)
	}
	defer rows.Close()
	var siblings []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store: scanning sibling episode: %w", err)
		}
		siblings = append(siblings, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, sib := range siblings {
		if _, err := db.Exec(
			`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, played_at, updated_at)
			 VALUES (?, ?, ?, ?, ?,
			         CASE WHEN ? = 1 THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') END,
			         strftime('%Y-%m-%dT%H:%M:%fZ','now'))
			 ON CONFLICT (user_id, title_id) DO UPDATE SET
			   resume_position_ms = excluded.resume_position_ms,
			   watched            = excluded.watched,
			   played_at          = COALESCE(excluded.played_at, watch_state.played_at),
			   updated_at         = excluded.updated_at`,
			uuid.NewString(), userID, sib, resumeMs, boolToInt(watched), boolToInt(played),
		); err != nil {
			return fmt.Errorf("store: propagating watch state to sibling: %w", err)
		}
	}
	return nil
}

// WatchStatesForTitles returns the calling User's watch state for the given set
// of Title ids, as a map keyed by title id. Titles with no row are simply
// absent from the map (the caller treats absence as "not started"). Used to
// decorate a browse list without an N+1 query per Title.
func (db *DB) WatchStatesForTitles(userID string, titleIDs []string) (map[string]WatchState, error) {
	out := make(map[string]WatchState, len(titleIDs))
	if len(titleIDs) == 0 {
		return out, nil
	}
	// Build a (?, ?, ...) IN-list; the set is bounded by a single browse page.
	args := make([]any, 0, len(titleIDs)+1)
	args = append(args, userID)
	placeholders := make([]byte, 0, len(titleIDs)*2)
	for i, id := range titleIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT title_id, resume_position_ms, watched, updated_at
		   FROM watch_state WHERE user_id = ? AND title_id IN (`+string(placeholders)+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading watch states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ws WatchState
		var resume int64
		var watched int
		if err := rows.Scan(&ws.TitleID, &resume, &watched, &ws.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning watch state: %w", err)
		}
		ws.UserID = userID
		ws.ResumePositionMs = resume
		ws.Watched = watched != 0
		out[ws.TitleID] = ws
	}
	return out, rows.Err()
}

// ContinueWatching returns the User's in-progress Titles, most-recently-played
// first (CONTEXT.md "Continue Watching"): rows with a recorded resume position
// (> 0), joined to their Title so hidden/Missing Titles are excluded (a Title
// whose every File is Missing is soft-deleted from browse, ADR-0008). watched
// rows are already cleared of their resume on crossing ~90%, so the resume > 0
// filter alone keeps finished Titles out. limit caps the row count.
//
// The 2%/90% band is enforced upstream when the resume is WRITTEN (the playback
// domain stores no resume below the floor and clears it above the ceiling), so
// this query only needs "has a resume" — it never re-derives a percentage and
// has no access to each File's duration, keeping the threshold in exactly one
// place.
func (db *DB) ContinueWatching(userID string, limit int, filter AccessFilter) ([]ContinueWatchingRow, error) {
	if limit <= 0 {
		limit = 20
	}
	clause, clauseArgs := filter.titleClauses("t.library_id", "t.content_rating")
	args := []any{userID}
	args = append(args, clauseArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT t.id, t.library_id, t.kind, t.title, t.year, t.identity_key, t.sort_title,
		        t.added_at, t.tmdb_id, t.imdb_id, t.needs_review, t.ambiguous, t.hidden,
		        w.resume_position_ms, w.updated_at
		   FROM watch_state w
		   JOIN titles t ON t.id = w.title_id
		  WHERE w.user_id = ? AND w.resume_position_ms > 0 AND w.watched = 0 AND t.hidden = 0`+clause+`
		  ORDER BY w.updated_at DESC, t.id DESC
		  LIMIT ?`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing continue watching: %w", err)
	}
	defer rows.Close()

	var out []ContinueWatchingRow
	for rows.Next() {
		var t Title
		var year sql.NullInt64
		var needsReview, ambiguous, hidden int
		var resume int64
		var updatedAt string
		if err := rows.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
			&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden,
			&resume, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning continue watching: %w", err)
		}
		if year.Valid {
			t.Year = int(year.Int64)
		}
		t.NeedsReview = needsReview != 0
		t.Ambiguous = ambiguous != 0
		t.Hidden = hidden != 0
		out = append(out, ContinueWatchingRow{Title: t, ResumePositionMs: resume, UpdatedAt: updatedAt})
	}
	return out, rows.Err()
}

// ContinueWatchingRow is one Continue Watching entry: the Title plus the resume
// position to seek to, ordered by recency by the query.
type ContinueWatchingRow struct {
	Title
	ResumePositionMs int64
	UpdatedAt        string
}

// UpNextRow is one Up Next entry: the next-to-watch Episode Title plus the Show
// context needed to label it ("The Bear · S01E03") and the recency timestamp the
// row is ordered by (the Show's most-recent watch activity).
type UpNextRow struct {
	Title
	ShowID       string
	ShowTitle    string
	SeasonNumber int
	// UpdatedAt is the most-recent watch_state.updated_at across the Show's
	// Episodes for this User — the row's recency sort key (most-recent first),
	// mirroring Continue Watching's ordering.
	UpdatedAt string
}

// UpNext returns the per-User Up Next Home row: each started Show's resume point,
// anchored on the most-recently-PLAYED Episode (CONTEXT.md "Up Next (resume
// point)", ADR-0028). It is a COMPUTED view (no stored "next" pointer) — a pure
// query off watch_state, so correcting watch state is reflected on the next read.
//
// "Started a Show" trigger: a Show is started when the User has any watch_state
// row against one of its Episodes (watched OR resume > 0) — the union of the
// Continue-Watching condition (a mid-band resume) and the Watched-threshold
// condition (an Episode marked watched, by the auto ~90% path or the manual
// toggle). started_shows also carries the Show's most-recent Episode updated_at as
// the row recency key.
//
// The resume-point algorithm, per started Show (ADR-0028):
//   - the ANCHOR is the Episode with the greatest played_at (the most-recently
//     PLAYED — a manual mark stamps no played_at, so it never moves the anchor); a
//     marks-only Show has no anchor;
//   - the resume point is the first UNWATCHED Episode strictly AFTER the anchor in
//     Show order, WRAPPING to the first unwatched from the start once the end is
//     reached (a skipped Episode is therefore never nagged — it resurfaces once, at
//     the wrap). With no anchor the show degenerates cleanly to first-unwatched.
//   - a fully-watched Show has no unwatched Episode and drops out.
//
// Home EXCLUDES a Show whose anchor is still IN PROGRESS (resume > 0, unwatched):
// that Episode is the resume point but belongs to Continue Watching, so the two
// Home rows stay disjoint and never list the same Episode. The Show detail page
// (issue 02) renders the same computation including the in-progress case.
//
// Episode ORDERING for the walk (and the wrap): regular Seasons (number ≥ 1) in
// season-then-episode order FIRST, with Specials (Season 0) ordered LAST — a viewer
// working through Season 1 gets S01E02 next, not a Special. Within that,
// episode_number then sort_title then id break ties deterministically.
//
// Shows are ordered most-recently-active first (the Show's latest Episode
// updated_at), the same recency feel as Continue Watching, then by Show id for a
// stable tiebreak. Hidden Episodes/Seasons/Shows are excluded (a Show whose every
// File went Missing drops out of the row, ADR-0008). limit caps the row count.
func (db *DB) UpNext(userID string, limit int, filter AccessFilter) ([]UpNextRow, error) {
	if limit <= 0 {
		limit = 20
	}
	// The picks CTE projects each candidate Episode's library_id, so the Library
	// filter is applied on the outer SELECT — a started Show whose resume-point
	// Episode is in an inaccessible Library drops out. The Rating filter is applied
	// inside the CTE on the candidate Episode's content_rating (so an above-ceiling
	// next Episode is skipped over to the next accessible unwatched one). Both empty
	// under all-access. The picks clause precedes the outer clause, so its args come
	// first. The two leading userID args feed started_shows and the episodes
	// LEFT JOIN, in that order.
	libClause, libArgs := filter.libraryClause("library_id")
	rateClause, rateArgs := filter.titleRatingClause("e.content_rating")
	args := []any{userID, userID}
	args = append(args, rateArgs...)
	args = append(args, libArgs...)
	args = append(args, limit)
	// The shared resume-point CTE (resumePointCTE) computes pick_rn=1 per started
	// Show over ALL of the User's Shows (empty showFilter). Home then keeps
	// pick_rn=1, EXCLUDES an in-progress anchor (the COALESCE guard — those belong
	// to Continue Watching, kept disjoint), applies the Library filter, and orders
	// most-recently-active first. The per-Show detail resolver (ResumePoint) reads
	// the SAME CTE but KEEPS the in-progress case (issue 02, ADR-0028).
	rows, err := db.Query(
		`WITH `+resumePointCTE("", rateClause)+`
		 SELECT show_id, show_title, id, library_id, kind, title, year, identity_key,
		        sort_title, added_at, tmdb_id, imdb_id, needs_review, ambiguous, hidden,
		        season_number, episode_number, episode_label, last_at
		   FROM picks
		  WHERE pick_rn = 1
		    AND NOT (COALESCE(anchor_resume, 0) > 0 AND COALESCE(anchor_watched, 0) = 0)`+libClause+`
		  ORDER BY last_at DESC, show_id ASC
		  LIMIT ?`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing up next: %w", err)
	}
	defer rows.Close()

	var out []UpNextRow
	for rows.Next() {
		var r UpNextRow
		var t Title
		var year sql.NullInt64
		var needsReview, ambiguous, hidden int
		if err := rows.Scan(&r.ShowID, &r.ShowTitle, &t.ID, &t.LibraryID, &t.Kind, &t.Title,
			&year, &t.IdentityKey, &t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID,
			&needsReview, &ambiguous, &hidden,
			&t.SeasonNumber, &t.EpisodeNumber, &t.EpisodeLabel, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning up next: %w", err)
		}
		if year.Valid {
			t.Year = int(year.Int64)
		}
		t.NeedsReview = needsReview != 0
		t.Ambiguous = ambiguous != 0
		t.Hidden = hidden != 0
		r.Title = t
		r.SeasonNumber = t.SeasonNumber
		out = append(out, r)
	}
	return out, rows.Err()
}

// resumePointCTE builds the shared started_shows → episodes → anchor → picks CTE
// that BOTH the Up Next Home row and the per-Show detail resume point read
// (ADR-0028), so the two surfaces compute the SAME resume point and can never
// drift. showFilter is spliced into started_shows' WHERE: empty for Home (every
// Show the User has touched), " AND sh.id = ?" for the per-Show resolver.
// rateClause is the access Rating filter spliced into picks (so an above-ceiling
// candidate is skipped over). pick_rn=1 is the resume point.
//
//   - started_shows: each started Show (any Episode with a watch_state row —
//     watched OR resume > 0), carrying its most-recent Episode updated_at (last_at)
//     as Home's recency key.
//   - episodes: every visible Episode of a started Show, LEFT-joined to the User's
//     watch state and stamped with its Show-order ordinal (ord): regular Seasons in
//     S/E order FIRST, Specials (Season 0) LAST.
//   - anchor: per Show, the ord + watch state of the Episode with the greatest
//     played_at (ties broken by later ord, so a played multi-episode file anchors on
//     its later half). A marks-only Show has no played_at → no anchor row.
//   - picks: per Show, unwatched candidates ranked so pick_rn=1 is the resume point:
//     the IN-PROGRESS anchor itself first (resume > 0, unwatched — the detail page's
//     Continue/Restart episode; Home filters this case out in its outer WHERE), else
//     the first unwatched AFTER the anchor in Show order, WRAPPING to the first
//     unwatched from the start once the end is reached. With no anchor every
//     candidate falls to the wrap bucket → first-unwatched.
func resumePointCTE(showFilter, rateClause string) string {
	return `started_shows AS (
	     SELECT sh.id AS show_id, MAX(w.updated_at) AS last_at
	       FROM watch_state w
	       JOIN titles  t  ON t.id = w.title_id AND t.kind = 'episode'
	       JOIN seasons s  ON s.id = t.season_id
	       JOIN shows   sh ON sh.id = s.show_id
	      WHERE w.user_id = ? AND (w.watched = 1 OR w.resume_position_ms > 0)` + showFilter + `
	      GROUP BY sh.id
	 ),
	 episodes AS (
	     SELECT st.show_id, st.last_at, sh.title AS show_title,
	            t.id, t.library_id, t.kind, t.title, t.year, t.identity_key,
	            t.sort_title, t.added_at, t.tmdb_id, t.imdb_id,
	            t.needs_review, t.ambiguous, t.hidden, t.content_rating,
	            t.season_id, t.season_number, t.episode_number, t.episode_label,
	            t.overview, t.enrichment_status, t.enriched_title,
	            w.watched AS w_watched, w.resume_position_ms AS w_resume,
	            w.played_at AS w_played_at,
	            ROW_NUMBER() OVER (
	              PARTITION BY st.show_id
	              ORDER BY (CASE WHEN t.season_number = 0 THEN 1 ELSE 0 END) ASC,
	                       t.season_number ASC, t.episode_number ASC,
	                       t.sort_title ASC, t.id ASC
	            ) AS ord
	       FROM started_shows st
	       JOIN shows   sh ON sh.id = st.show_id AND sh.hidden = 0
	       JOIN seasons s  ON s.show_id = sh.id AND s.hidden = 0
	       JOIN titles  t  ON t.season_id = s.id AND t.kind = 'episode' AND t.hidden = 0
	       LEFT JOIN watch_state w ON w.user_id = ? AND w.title_id = t.id
	 ),
	 anchor AS (
	     SELECT show_id, ord AS anchor_ord, w_resume AS anchor_resume, w_watched AS anchor_watched
	       FROM (
	         SELECT show_id, ord, w_resume, w_watched,
	                ROW_NUMBER() OVER (
	                  PARTITION BY show_id
	                  ORDER BY w_played_at DESC, ord DESC
	                ) AS arn
	           FROM episodes
	          WHERE w_played_at IS NOT NULL
	       )
	      WHERE arn = 1
	 ),
	 picks AS (
	     SELECT e.show_id, e.show_title, e.id, e.library_id, e.kind, e.title, e.year,
	            e.identity_key, e.sort_title, e.added_at, e.tmdb_id, e.imdb_id,
	            e.needs_review, e.ambiguous, e.hidden,
	            e.season_id, e.season_number, e.episode_number, e.episode_label, e.last_at,
	            e.overview, e.enrichment_status, e.enriched_title,
	            e.w_resume, e.w_watched,
	            a.anchor_resume, a.anchor_watched,
	            ROW_NUMBER() OVER (
	              PARTITION BY e.show_id
	              ORDER BY (CASE
	                          WHEN e.ord = a.anchor_ord
	                               AND COALESCE(a.anchor_resume, 0) > 0
	                               AND COALESCE(a.anchor_watched, 0) = 0 THEN 0
	                          WHEN e.ord > a.anchor_ord THEN 1
	                          ELSE 2 END) ASC,
	                       e.ord ASC
	            ) AS pick_rn
	       FROM episodes e
	       LEFT JOIN anchor a ON a.show_id = e.show_id
	      WHERE (e.w_watched IS NULL OR e.w_watched = 0)` + rateClause + `
	 )`
}

// ResumePoint is a Show's computed resume point for one User (ADR-0028), surfaced
// on the Show detail page: the Episode to play next plus the mode that selects its
// controls. Unlike Home's Up Next it KEEPS the in-progress-anchor case (the detail
// page's Continue/Restart). It is the shared picks pick_rn=1 Episode, so it honors
// the same S/E ordering (Specials last) and hidden/Missing/access/rating exclusions.
type ResumePoint struct {
	Title
	// SeasonID is the resume-point Episode's Season, enough for the detail page to
	// build the cross-season show-from-here Queue with this Episode as the head.
	SeasonID string
	// ResumePositionMs is where Continue seeks: the in-progress anchor's stored
	// resume for the in-progress mode, else 0 (a fresh next Episode plays from the
	// start).
	ResumePositionMs int64
	// InProgress is the mode: true when the anchor is still in progress (resume > 0,
	// unwatched) → the detail page offers Continue + Restart; false when the resume
	// point is a fresh next Episode → a single Play.
	InProgress bool
	// DurationMs is the resume-point Episode's playable duration (MAX file
	// duration_ms across its Editions — the same measure the resume position was
	// recorded against). 0 when unknown (no File / duration not probed). The detail
	// page uses it with ResumePositionMs to draw the Continue progress bar and the
	// minutes-remaining label; it is only meaningful in the in-progress mode.
	DurationMs int64
	// Overview / EnrichedTitle / EnrichmentStatus carry the same Episode enrichment
	// the Season's Episode list applies, so the detail block shows the canonical
	// title + synopsis. (EnrichedTitle/Overview live on the embedded Title.)
}

// ResumePoint returns the User's resume point for one Show (found=false when the
// Show is not started or fully watched — the detail page then falls back to the
// Show description). It reads the shared resumePointCTE scoped to showID and keeps
// pick_rn=1 including the in-progress anchor. The AccessFilter applies the caller's
// Rating ceiling (in picks) and Library grant (outer) so an inaccessible/above-
// ceiling candidate drops out exactly as it does for Home's Up Next.
func (db *DB) ResumePoint(userID, showID string, filter AccessFilter) (ResumePoint, bool, error) {
	libClause, libArgs := filter.libraryClause("library_id")
	rateClause, rateArgs := filter.titleRatingClause("e.content_rating")
	// Arg order mirrors the CTE placeholders: userID (started_shows) + showID (its
	// sh.id filter) + userID (episodes LEFT JOIN) + rate args (picks) + lib args (outer).
	args := []any{userID, showID, userID}
	args = append(args, rateArgs...)
	args = append(args, libArgs...)
	var rp ResumePoint
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	var wResume, anchorResume sql.NullInt64
	var anchorWatched sql.NullInt64
	var durationMs sql.NullInt64
	err := db.QueryRow(
		`WITH `+resumePointCTE(" AND sh.id = ?", rateClause)+`
		 SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
		        tmdb_id, imdb_id, needs_review, ambiguous, hidden,
		        season_id, season_number, episode_number, episode_label,
		        overview, enrichment_status, enriched_title,
		        w_resume, anchor_resume, anchor_watched,
		        (SELECT MAX(f.duration_ms) FROM editions ed
		           JOIN files f ON f.edition_id = ed.id
		          WHERE ed.title_id = picks.id) AS duration_ms
		   FROM picks
		  WHERE pick_rn = 1`+libClause,
		args...,
	).Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey, &t.SortTitle,
		&t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden,
		&rp.SeasonID, &t.SeasonNumber, &t.EpisodeNumber, &t.EpisodeLabel,
		&t.Overview, &t.EnrichmentStatus, &t.EnrichedTitle,
		&wResume, &anchorResume, &anchorWatched, &durationMs)
	if errors.Is(err, sql.ErrNoRows) {
		return ResumePoint{}, false, nil
	}
	if err != nil {
		return ResumePoint{}, false, fmt.Errorf("store: reading resume point: %w", err)
	}
	if year.Valid {
		t.Year = int(year.Int64)
	}
	t.NeedsReview = needsReview != 0
	t.Ambiguous = ambiguous != 0
	t.Hidden = hidden != 0
	rp.Title = t
	// In-progress mode is judged on the ANCHOR (not the chosen Episode): the anchor
	// still has a mid-band resume and is unwatched. A wrap pick can itself carry a
	// stale resume yet still be a fresh next Episode, so it must not read as
	// in-progress. Continue seeks the anchor's resume; a next Episode plays from 0.
	rp.InProgress = anchorResume.Valid && anchorResume.Int64 > 0 &&
		!(anchorWatched.Valid && anchorWatched.Int64 == 1)
	if rp.InProgress {
		rp.ResumePositionMs = wResume.Int64
	}
	if durationMs.Valid {
		rp.DurationMs = durationMs.Int64
	}
	return rp, true, nil
}

// RecentlyAdded returns Titles ordered newest-added first across the caller's
// accessible Libraries, excluding hidden (all-Files-Missing) Titles — the
// per-User "Recently Added" Home row (CONTEXT.md). It reuses the (added_at DESC,
// id DESC) ordering of the by-date browse sort. limit caps the row count. The
// AccessFilter applies the caller's library grants + Rating ceiling in SQL (a
// no-op for an Admin); hidden exclusion is always enforced.
func (db *DB) RecentlyAdded(limit int, filter AccessFilter) ([]Title, error) {
	if limit <= 0 {
		limit = 20
	}
	clause, clauseArgs := filter.titleClauses("library_id", "content_rating")
	args := append([]any{}, clauseArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
		        tmdb_id, imdb_id, needs_review, ambiguous, hidden
		   FROM titles WHERE hidden = 0`+clause+` ORDER BY added_at DESC, id DESC LIMIT ?`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing recently added: %w", err)
	}
	defer rows.Close()

	var out []Title
	for rows.Next() {
		t, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
