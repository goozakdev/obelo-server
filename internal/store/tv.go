package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// TV catalog persistence (issue tv-music/01): the explicit Show → Season parent
// entities and the Episode-as-Title linkage. An Episode is a Title (kind
// 'episode') that owns the existing Edition → File → Stream chain UNCHANGED and
// references its Season → Show. These types mirror the Movie catalog shapes
// (catalog.go) so the scanner writes one resolved Show subtree per Show folder
// and the browse API reads the hierarchy back.

// Show is the top-level browse unit of a TV Library (CONTEXT.md: Show → Season →
// Episode). Identity is (normalized title + year), like a Movie, scoped to the
// Library; identity_key dedups across rescans (ADR-0014).
type Show struct {
	ID          string
	LibraryID   string
	Title       string
	Year        int
	IdentityKey string
	SortTitle   string
	TMDBID      string
	IMDBID      string
	NeedsReview bool
	Hidden      bool
	AddedAt     string
}

// Season groups a Show's Episodes. SeasonNumber 0 is Specials (Season 00 /
// Specials/). IdentityKey re-resolves the Season on rescan.
type Season struct {
	ID           string
	ShowID       string
	SeasonNumber int
	IdentityKey  string
	Hidden       bool
	AddedAt      string
	// EpisodeCount / UnwatchedCount are computed for the browse list (not stored).
	EpisodeCount int
}

// EpisodeTree is one resolved Episode: its Title identity (with the TV ordering
// fields) plus the Edition→File→Stream/Extras/Artwork subtree the scanner built,
// exactly as for a Movie. SeasonNumber/EpisodeNumber/EpisodeLabel carry the
// parsed TV ordering; the store writes them onto the titles row.
type EpisodeTree struct {
	TitleTree
	SeasonNumber  int
	EpisodeNumber int
	EpisodeLabel  string
}

// SeasonTree is one Season and the Episodes resolved within it.
type SeasonTree struct {
	SeasonNumber int
	IdentityKey  string
	Episodes     []EpisodeTree
}

// ShowTree is the complete result of resolving one on-disk Show folder: the Show
// identity plus its Seasons, each with its Episode subtrees. The scanner builds
// this and hands it to UpsertShowTree, which resolves/reuses the Show and Season
// ids by identity and rewrites the Episode subtrees atomically (reusing the same
// per-File identity-stability rules as the Movie upsert).
type ShowTree struct {
	Show    Show
	Seasons []SeasonTree
}

// UpsertShowTree persists one resolved Show folder in a single transaction. The
// Show is keyed by (library_id, identity_key) and each Season by (show_id,
// season_number) so a rescan re-resolves to the same rows instead of duplicating
// (identity stability, ADR-0014). Episode Titles are upserted via the same
// path-stable rules UpsertTitleTree uses (a File keeps its id/added_at by path),
// with the Season linkage + episode ordering written onto the titles row.
func (db *DB) UpsertShowTree(tree ShowTree) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin upsert show tree: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	showID, err := upsertShow(tx, tree.Show)
	if err != nil {
		return err
	}

	// One shared "paths written this tx" set across all Episodes of the Show, so a
	// multi-episode file (two Episode Titles, one path) keeps both File rows
	// instead of the second reclaiming the first (see writeTitleSubtree).
	written := map[string]bool{}
	for _, st := range tree.Seasons {
		seasonID, err := upsertSeason(tx, showID, st)
		if err != nil {
			return err
		}
		for _, ep := range st.Episodes {
			if err := upsertEpisodeTitle(tx, seasonID, ep, written); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert show tree: %w", err)
	}
	return nil
}

func upsertShow(tx *sql.Tx, s Show) (string, error) {
	var showID string
	err := tx.QueryRow(
		`SELECT id FROM shows WHERE library_id = ? AND identity_key = ?`,
		s.LibraryID, s.IdentityKey,
	).Scan(&showID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		showID = s.ID
		if _, err := tx.Exec(
			`INSERT INTO shows (id, library_id, title, year, identity_key, sort_title,
			    tmdb_id, imdb_id, needs_review, hidden)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			showID, s.LibraryID, s.Title, nullableYear(s.Year), s.IdentityKey, s.SortTitle,
			s.TMDBID, s.IMDBID, boolToInt(s.NeedsReview),
		); err != nil {
			return "", fmt.Errorf("store: inserting show: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("store: resolving show identity: %w", err)
	default:
		// Existing Show: refresh descriptive fields. hidden is recomputed below.
		// needs_review is recomputed from the parse EXCEPT on a row an Admin has
		// dismissed (reviewed = 1), where it stays cleared (migration 0012, mirrors
		// the Title rule in writeTitleRow). reviewed is never written by the scanner.
		if _, err := tx.Exec(
			`UPDATE shows SET title = ?, year = ?, sort_title = ?, tmdb_id = ?, imdb_id = ?,
			    needs_review = CASE WHEN reviewed = 1 THEN 0 ELSE ? END,
			    hidden = 0 WHERE id = ?`,
			s.Title, nullableYear(s.Year), s.SortTitle, s.TMDBID, s.IMDBID,
			boolToInt(s.NeedsReview), showID,
		); err != nil {
			return "", fmt.Errorf("store: updating show: %w", err)
		}
	}
	return showID, nil
}

func upsertSeason(tx *sql.Tx, showID string, st SeasonTree) (string, error) {
	var seasonID string
	err := tx.QueryRow(
		`SELECT id FROM seasons WHERE show_id = ? AND season_number = ?`,
		showID, st.SeasonNumber,
	).Scan(&seasonID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		seasonID = uuid.NewString()
		if _, err := tx.Exec(
			`INSERT INTO seasons (id, show_id, season_number, identity_key, hidden)
			 VALUES (?, ?, ?, ?, 0)`,
			seasonID, showID, st.SeasonNumber, st.IdentityKey,
		); err != nil {
			return "", fmt.Errorf("store: inserting season: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("store: resolving season: %w", err)
	default:
		if _, err := tx.Exec(
			`UPDATE seasons SET identity_key = ?, hidden = 0 WHERE id = ?`,
			st.IdentityKey, seasonID,
		); err != nil {
			return "", fmt.Errorf("store: updating season: %w", err)
		}
	}
	return seasonID, nil
}

// upsertEpisodeTitle writes one Episode Title (kind 'episode') and its subtree.
// It reuses the exact Movie subtree-rewrite logic (writeTitleSubtree) so a File
// keeps its id/added_at by path across rescans; it then sets the Season linkage
// and episode ordering onto the titles row.
func upsertEpisodeTitle(tx *sql.Tx, seasonID string, ep EpisodeTree, written map[string]bool) error {
	titleID, err := writeTitleRow(tx, ep.TitleTree, episodeColumns{
		seasonID:      seasonID,
		seasonNumber:  ep.SeasonNumber,
		episodeNumber: ep.EpisodeNumber,
		episodeLabel:  ep.EpisodeLabel,
	})
	if err != nil {
		return err
	}
	return writeTitleSubtree(tx, titleID, ep.TitleTree, written)
}

// --- Browse reads ----------------------------------------------------------

// ShowPage is one page of a cursor-paginated Show listing.
type ShowPage struct {
	Shows   []Show
	HasMore bool
}

// ListShows returns one page of a TV Library's Shows, ordered by sort_title then
// id (the same stable order Titles use), excluding hidden Shows, seeking strictly
// past the cursor. It fetches limit+1 to detect HasMore. Each Show is the
// top-level grid entry for a TV Library (the analogue of a Movie Title).
func (db *DB) ListShows(libraryID string, cursor *TitleCursor, limit int, genre string, filter AccessFilter) (ShowPage, error) {
	if limit <= 0 {
		limit = 20
	}
	args := []any{libraryID}
	where := "library_id = ? AND hidden = 0"
	// filter[genre]: keep only Shows carrying that enriched genre (issue 03).
	if genre != "" {
		where += " AND id IN (SELECT entity_id FROM entity_genres WHERE entity_type = 'show' AND genre = ?)"
		args = append(args, genre)
	}
	if cursor != nil {
		where += " AND (sort_title, id) > (?, ?)"
		args = append(args, cursor.SortKey, cursor.ID)
	}
	// Rating ceiling (access-control 04): hide a Show whose enriched content_rating
	// is above the caller's ceiling. The Library dimension is enforced by the
	// service guard (the library is fixed by the path). Empty under all-access.
	rateClause, rateArgs := filter.showRatingClause("id")
	where += rateClause
	args = append(args, rateArgs...)
	query := `SELECT id, library_id, title, year, identity_key, sort_title,
	                 tmdb_id, imdb_id, needs_review, hidden, added_at
	            FROM shows WHERE ` + where + ` ORDER BY sort_title ASC, id ASC LIMIT ?`
	args = append(args, limit+1)

	rows, err := db.Query(query, args...)
	if err != nil {
		return ShowPage{}, fmt.Errorf("store: listing shows: %w", err)
	}
	defer rows.Close()

	var out []Show
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return ShowPage{}, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return ShowPage{}, err
	}
	page := ShowPage{}
	if len(out) > limit {
		page.HasMore = true
		out = out[:limit]
	}
	page.Shows = out
	return page, nil
}

// UnwatchedEpisodeCounts returns, for each requested Show, how many of its
// visible Episodes the User has NOT watched (issue tv-music/04: the Show-poster
// watched affordance — the TV analogue of a Movie's resume bar). An Episode
// counts as unwatched unless the User has a watch_state row with watched=1 for
// it. A Show with no visible Episodes, or with every Episode watched, simply has
// no entry in the map (count 0). showIDs is the page being decorated, so the
// query is bounded; an empty input returns an empty map without a query.
func (db *DB) UnwatchedEpisodeCounts(userID string, showIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(showIDs) == 0 {
		return out, nil
	}
	// Parameter order matches the SQL: the IN (...) Show ids first, then the
	// userID used by the NOT EXISTS watch_state correlation.
	placeholders := make([]string, len(showIDs))
	args := make([]any, 0, len(showIDs)+1)
	for i, id := range showIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, userID)
	rows, err := db.Query(
		`SELECT sh.id, COUNT(*)
		   FROM shows sh
		   JOIN seasons s ON s.show_id = sh.id AND s.hidden = 0
		   JOIN titles  t ON t.season_id = s.id AND t.kind = 'episode' AND t.hidden = 0
		  WHERE sh.id IN (`+strings.Join(placeholders, ",")+`)
		    AND NOT EXISTS (
		          SELECT 1 FROM watch_state wt
		           WHERE wt.user_id = ? AND wt.title_id = t.id AND wt.watched = 1
		        )
		  GROUP BY sh.id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: counting unwatched episodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: scanning unwatched count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ShowByID returns one Show, or ErrNotFound. The catalog service guards the
// result against the caller's access Scope — a Show in a Library not granted to
// the caller (or above their Rating ceiling) is hidden as 404 — and uses this for
// the seasons listing.
func (db *DB) ShowByID(id string) (Show, error) {
	row := db.QueryRow(
		`SELECT id, library_id, title, year, identity_key, sort_title,
		        tmdb_id, imdb_id, needs_review, hidden, added_at
		   FROM shows WHERE id = ?`, id)
	s, err := scanShow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Show{}, ErrNotFound
	}
	if err != nil {
		return Show{}, fmt.Errorf("store: reading show: %w", err)
	}
	return s, nil
}

// SeasonsForShow returns a Show's Seasons in season-number order (Specials =
// Season 0 sorts first), excluding hidden Seasons, each with its visible-Episode
// count. Returns ErrNotFound when the Show does not exist.
func (db *DB) SeasonsForShow(showID string) ([]Season, error) {
	if _, err := db.ShowByID(showID); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT s.id, s.show_id, s.season_number, s.identity_key, s.hidden, s.added_at,
		        (SELECT COUNT(*) FROM titles t WHERE t.season_id = s.id AND t.hidden = 0) AS ep_count
		   FROM seasons s WHERE s.show_id = ? AND s.hidden = 0
		  ORDER BY s.season_number ASC, s.id ASC`, showID)
	if err != nil {
		return nil, fmt.Errorf("store: listing seasons: %w", err)
	}
	defer rows.Close()
	var out []Season
	for rows.Next() {
		var s Season
		var hidden int
		if err := rows.Scan(&s.ID, &s.ShowID, &s.SeasonNumber, &s.IdentityKey, &hidden,
			&s.AddedAt, &s.EpisodeCount); err != nil {
			return nil, fmt.Errorf("store: scanning season: %w", err)
		}
		s.Hidden = hidden != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// SeasonByID returns one Season, or ErrNotFound.
func (db *DB) SeasonByID(id string) (Season, error) {
	var s Season
	var hidden int
	err := db.QueryRow(
		`SELECT id, show_id, season_number, identity_key, hidden, added_at
		   FROM seasons WHERE id = ?`, id,
	).Scan(&s.ID, &s.ShowID, &s.SeasonNumber, &s.IdentityKey, &hidden, &s.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, fmt.Errorf("store: reading season: %w", err)
	}
	s.Hidden = hidden != 0
	return s, nil
}

// EpisodesForSeason returns a Season's Episodes (Titles) in (episode_number, then
// title) order, excluding hidden ones. Returns ErrNotFound when the Season does
// not exist. The Title rows carry the TV ordering fields so the browse API can
// label them.
func (db *DB) EpisodesForSeason(seasonID string) ([]Title, error) {
	if _, err := db.SeasonByID(seasonID); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
		        tmdb_id, imdb_id, needs_review, ambiguous, hidden,
		        season_number, episode_number, episode_label,
		        overview, enrichment_status, enriched_title
		   FROM titles WHERE season_id = ? AND hidden = 0
		  ORDER BY episode_number ASC, sort_title ASC, id ASC`, seasonID)
	if err != nil {
		return nil, fmt.Errorf("store: listing episodes: %w", err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		t, err := scanEpisodeTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EpisodeContext is the parent chain of an Episode Title (Show / Season /
// episode), attached to the Episode's GET /titles/{id} detail so a client can
// render "The Bear · S01E03" without extra round-trips.
type EpisodeContext struct {
	ShowID        string
	ShowTitle     string
	ShowYear      int
	SeasonID      string
	SeasonNumber  int
	EpisodeNumber int
	EpisodeLabel  string
}

// EpisodeContextForTitle returns the parent context for an Episode Title, or
// ErrNotFound when the Title is not an Episode (no season linkage). Movies return
// ErrNotFound here and the caller simply omits the context.
func (db *DB) EpisodeContextForTitle(titleID string) (EpisodeContext, error) {
	var c EpisodeContext
	var year sql.NullInt64
	err := db.QueryRow(
		`SELECT t.episode_number, t.episode_label,
		        s.id, s.season_number, sh.id, sh.title, sh.year
		   FROM titles t
		   JOIN seasons s ON s.id = t.season_id
		   JOIN shows   sh ON sh.id = s.show_id
		  WHERE t.id = ?`, titleID,
	).Scan(&c.EpisodeNumber, &c.EpisodeLabel, &c.SeasonID, &c.SeasonNumber,
		&c.ShowID, &c.ShowTitle, &year)
	if errors.Is(err, sql.ErrNoRows) {
		return EpisodeContext{}, ErrNotFound
	}
	if err != nil {
		return EpisodeContext{}, fmt.Errorf("store: reading episode context: %w", err)
	}
	if year.Valid {
		c.ShowYear = int(year.Int64)
	}
	return c, nil
}

// RecomputeHiddenShows recomputes each Season's and Show's hidden flag from the
// visibility of the Titles beneath them, after a scan's MarkFilesMissing +
// RecomputeHiddenTitles pass. A Season with no visible Episode is hidden; a Show
// with no visible Season is hidden — so a Show whose every file went Missing
// drops out of the grid but stays fetchable (ADR-0008).
func (db *DB) RecomputeHiddenShows(libraryID string) error {
	if _, err := db.Exec(
		`UPDATE seasons SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM titles t WHERE t.season_id = seasons.id AND t.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE show_id IN (SELECT id FROM shows WHERE library_id = ?)`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden seasons: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE shows SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM seasons s WHERE s.show_id = shows.id AND s.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden shows: %w", err)
	}
	return nil
}

func scanShow(s scanner) (Show, error) {
	var sh Show
	var year sql.NullInt64
	var needsReview, hidden int
	if err := s.Scan(&sh.ID, &sh.LibraryID, &sh.Title, &year, &sh.IdentityKey,
		&sh.SortTitle, &sh.TMDBID, &sh.IMDBID, &needsReview, &hidden, &sh.AddedAt); err != nil {
		return Show{}, err
	}
	if year.Valid {
		sh.Year = int(year.Int64)
	}
	sh.NeedsReview = needsReview != 0
	sh.Hidden = hidden != 0
	return sh, nil
}

// scanEpisodeTitle scans a titles row including the TV ordering columns plus the
// enriched display fields (overview / enriched_title / status) so the Season's
// Episode list can show a canonical title + synopsis without a per-Episode fetch.
func scanEpisodeTitle(s scanner) (Title, error) {
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
		&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden,
		&t.SeasonNumber, &t.EpisodeNumber, &t.EpisodeLabel,
		&t.Overview, &t.EnrichmentStatus, &t.EnrichedTitle); err != nil {
		return Title{}, err
	}
	if year.Valid {
		t.Year = int(year.Int64)
	}
	t.NeedsReview = needsReview != 0
	t.Ambiguous = ambiguous != 0
	t.Hidden = hidden != 0
	return t, nil
}
