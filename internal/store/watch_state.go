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
func (db *DB) SaveWatchState(userID, titleID string, resumeMs int64, watched bool) error {
	if resumeMs < 0 {
		resumeMs = 0
	}
	// Millisecond-precision timestamp (strftime %f) so Continue Watching's
	// most-recently-played ordering is stable even for two writes in the same
	// second — datetime('now') is only second-granular, which would tie.
	_, err := db.Exec(
		`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, updated_at)
		 VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		 ON CONFLICT (user_id, title_id) DO UPDATE SET
		   resume_position_ms = excluded.resume_position_ms,
		   watched            = excluded.watched,
		   updated_at         = excluded.updated_at`,
		uuid.NewString(), userID, titleID, resumeMs, boolToInt(watched),
	)
	if err != nil {
		return fmt.Errorf("store: saving watch state: %w", err)
	}
	// Multi-episode file (S01E05-E06): one File maps to TWO Episode Titles
	// (naming-convention.md). It plays once, so a progress/watched write against
	// one Episode propagates the SAME resume + watched to its co-File sibling(s),
	// so "watching it marks both". A Movie / single Episode has no co-File
	// sibling, so this is a no-op for them.
	if err := db.propagateToSiblingEpisodes(userID, titleID, resumeMs, watched); err != nil {
		return err
	}
	return nil
}

// propagateToSiblingEpisodes copies (resume, watched) to every OTHER Episode
// Title that shares a File path with titleID — the two Titles of a multi-episode
// file. The shared-File join is the authority: the scanner gives both Episodes
// the same physical File (different edition rows, same path). Movies and
// single-file Episodes have no such sibling, so nothing is written.
func (db *DB) propagateToSiblingEpisodes(userID, titleID string, resumeMs int64, watched bool) error {
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
			`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, updated_at)
			 VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
			 ON CONFLICT (user_id, title_id) DO UPDATE SET
			   resume_position_ms = excluded.resume_position_ms,
			   watched            = excluded.watched,
			   updated_at         = excluded.updated_at`,
			uuid.NewString(), userID, sib, resumeMs, boolToInt(watched),
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

// UpNext returns the per-User Up Next Home row (CONTEXT.md "Up Next = next
// unwatched Episode in Show order"). It is a COMPUTED view (no stored entity):
// for every Show the User has STARTED — has ≥1 Episode that is watched OR
// in-progress (a resume position) — it surfaces the next unwatched Episode in
// Show order (Season then Episode; Specials = Season 0 sort first), and only
// when such an Episode exists (a fully-watched Show drops out).
//
// "Started a Show" trigger: a Show is started when the User has any watch_state
// row against one of its Episodes (watched OR resume > 0). This is the union of
// the Continue-Watching condition (a mid-band resume) and the Watched-threshold
// condition (an Episode marked watched, by the auto ~90% path or the manual
// toggle) — so Up Next moves consistently whether the prior Episode advanced via
// progress crossing the ceiling or via PUT watchState, with no write-path hook
// (it is purely a query off the same watch_state rows).
//
// Episode ORDERING for the "next" pick: regular Seasons (number ≥ 1) in
// season-then-episode order FIRST, with Specials (Season 0) ordered LAST. The
// season-0-first convention is right for the Season LIST (issue 01's
// SeasonsForShow), but for "what's next" a viewer working through Season 1 must
// get S01E02 next — not a Special — so Up Next deliberately defers Season 0 to
// the end of the progression. Within that, episode_number then sort_title then
// id break ties deterministically.
//
// Shows are ordered most-recently-active first (the Show's latest Episode
// updated_at), the same recency feel as Continue Watching, then by Show id for a
// stable tiebreak. Hidden Episodes/Seasons/Shows are excluded (a Show whose every
// File went Missing drops out of the row, ADR-0008). limit caps the row count.
func (db *DB) UpNext(userID string, limit int, filter AccessFilter) ([]UpNextRow, error) {
	if limit <= 0 {
		limit = 20
	}
	// The candidates CTE projects each surfaced Episode's library_id, so the
	// Library filter is applied on the outer SELECT — a started Show whose next
	// Episode is in an inaccessible Library drops out. The Rating filter is applied
	// inside the CTE on the candidate Episode's content_rating (so an above-ceiling
	// next Episode is skipped). Both empty under all-access. The candidates clause
	// precedes the outer clause in the SQL, so its args come first.
	libClause, libArgs := filter.libraryClause("library_id")
	rateClause, rateArgs := filter.titleRatingClause("t.content_rating")
	args := []any{userID, userID}
	args = append(args, rateArgs...)
	args = append(args, libArgs...)
	args = append(args, limit)
	// started_shows: every Show the User has touched (any Episode with a
	// watch_state row), carrying that Show's most-recent Episode updated_at as the
	// recency key. next_episode: per started Show, the lowest-ordered visible
	// Episode that is NOT watched — that is the Up Next pick. We rank candidates
	// per show (Season then Episode then sort_title then id) and keep rank 1.
	rows, err := db.Query(
		`WITH started_shows AS (
		     SELECT sh.id AS show_id, MAX(w.updated_at) AS last_at
		       FROM watch_state w
		       JOIN titles  t  ON t.id = w.title_id AND t.kind = 'episode'
		       JOIN seasons s  ON s.id = t.season_id
		       JOIN shows   sh ON sh.id = s.show_id
		      WHERE w.user_id = ? AND (w.watched = 1 OR w.resume_position_ms > 0)
		      GROUP BY sh.id
		 ),
		 candidates AS (
		     SELECT sh.id AS show_id, sh.title AS show_title,
		            t.id, t.library_id, t.kind, t.title, t.year, t.identity_key,
		            t.sort_title, t.added_at, t.tmdb_id, t.imdb_id,
		            t.needs_review, t.ambiguous, t.hidden,
		            t.season_number, t.episode_number, t.episode_label,
		            st.last_at,
		            ROW_NUMBER() OVER (
		              PARTITION BY sh.id
		              ORDER BY (CASE WHEN t.season_number = 0 THEN 1 ELSE 0 END) ASC,
		                       t.season_number ASC, t.episode_number ASC,
		                       t.sort_title ASC, t.id ASC
		            ) AS rn
		       FROM started_shows st
		       JOIN shows   sh ON sh.id = st.show_id AND sh.hidden = 0
		       JOIN seasons s  ON s.show_id = sh.id AND s.hidden = 0
		       JOIN titles  t  ON t.season_id = s.id AND t.kind = 'episode' AND t.hidden = 0
		      WHERE NOT EXISTS (
		              SELECT 1 FROM watch_state wt
		               WHERE wt.user_id = ? AND wt.title_id = t.id AND wt.watched = 1
		            )`+rateClause+`
		 )
		 SELECT show_id, show_title, id, library_id, kind, title, year, identity_key,
		        sort_title, added_at, tmdb_id, imdb_id, needs_review, ambiguous, hidden,
		        season_number, episode_number, episode_label, last_at
		   FROM candidates
		  WHERE rn = 1`+libClause+`
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
