package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// fixcontext.go answers the question every Admin "Needs Fixing" row has to answer
// before the Admin can act on it: WHICH item is this, and WHERE does it live on
// disk? The attention lists themselves (needsreview.go, enrich.go's
// TitlesNeedingMatch) select the flagged rows; neither carries enough to identify
// one on sight. An Episode is the worst case — its own title is just an episode
// name ("Pilot"), which two different Shows can share — so a row that prints only
// the Title is unactionable without opening it.
//
// This is deliberately ONE batch read shared by both lists rather than widening
// each list's own query: the two lists disagree on almost everything else (one is
// identity-parse state, one is enrichment state) but need exactly the same
// "which item, which file" bundle, and a shared read keeps them from drifting.

// FixContext is the identifying context for one flagged Title: a representative
// present file, plus whichever parent breadcrumb its kind has. A Movie has only a
// Path; an Episode adds its Show + season/episode numbers; a Track adds its Artist
// + Album + disc/track numbers. Zero values mean "not that kind" (or, for Path,
// that every File of the Title is Missing).
type FixContext struct {
	// Path is a representative PRESENT file for the Title — "" when all of its
	// Files are Missing (ADR-0008 soft delete), which is itself worth showing.
	Path string

	// TV breadcrumb, populated only for an Episode.
	ShowID        string
	ShowTitle     string
	SeasonNumber  int
	EpisodeNumber int
	EpisodeLabel  string

	// Music breadcrumb, populated only for a Track.
	ArtistID    string
	ArtistName  string
	AlbumID     string
	AlbumTitle  string
	DiscNumber  int
	TrackNumber int

	// What Enrichment actually matched this item to — the evidence an Admin needs
	// to CONFIRM a flagged item rather than just read its name. A needs-review item
	// is flagged for an uncertain parse (most often a missing year), so the parsed
	// row alone can never say whether the filing is right; the matched record can.
	//
	// EnrichedTitle is the canonical display title Enrichment found (an Episode's
	// real name for a date-numbered file); ReleaseDate is its release date, which
	// supplies the very year a `no-year` item is missing; EnrichmentStatus says
	// whether there is a match to trust at all.
	EnrichedTitle    string
	ReleaseDate      string
	EnrichmentStatus string
}

// TitleFixContexts returns the FixContext for each of `ids` that names an existing
// Title, keyed by Title id. Ids with no row are simply absent from the map (the
// caller renders what it has rather than failing a whole list over one stale id).
//
// The parent joins are LEFT joins on purpose: a Movie has neither a Season nor an
// Album, and must still come back with its Path. The path sub-select mirrors
// TitlesNeedingReview's — lowest path among the Title's present Files — so a Title
// shows the same representative file on every surface.
func (db *DB) TitleFixContexts(ids []string) (map[string]FixContext, error) {
	out := map[string]FixContext{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(
		`SELECT t.id,
		        (SELECT f.path FROM editions e JOIN files f ON f.edition_id = e.id
		          WHERE e.title_id = t.id AND f.present = 1
		          ORDER BY f.path LIMIT 1) AS path,
		        t.season_number, t.episode_number, t.episode_label,
		        sh.id, sh.title,
		        t.disc_number, t.track_number,
		        al.id, al.title, ar.id, ar.name,
		        t.enriched_title, t.release_date, t.enrichment_status
		   FROM titles t
		   LEFT JOIN seasons se ON se.id = t.season_id
		   LEFT JOIN shows   sh ON sh.id = se.show_id
		   LEFT JOIN albums  al ON al.id = t.album_id
		   LEFT JOIN artists ar ON ar.id = al.artist_id
		  WHERE t.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading title fix contexts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c FixContext
		// Every joined column is nullable for a kind that has no such parent.
		var path, showID, showTitle, albumID, albumTitle, artistID, artistName sql.NullString
		if err := rows.Scan(
			&id, &path,
			&c.SeasonNumber, &c.EpisodeNumber, &c.EpisodeLabel,
			&showID, &showTitle,
			&c.DiscNumber, &c.TrackNumber,
			&albumID, &albumTitle, &artistID, &artistName,
			&c.EnrichedTitle, &c.ReleaseDate, &c.EnrichmentStatus,
		); err != nil {
			return nil, fmt.Errorf("store: scanning title fix context: %w", err)
		}
		c.Path = path.String
		c.ShowID, c.ShowTitle = showID.String, showTitle.String
		c.AlbumID, c.AlbumTitle = albumID.String, albumTitle.String
		c.ArtistID, c.ArtistName = artistID.String, artistName.String
		out[id] = c
	}
	return out, rows.Err()
}

// ShowFixContexts is TitleFixContexts for Shows, which are not Titles and so have
// no row in the query above. A Show's representative file is one present Episode
// file (the same one ShowsNeedingReview picks), which is what the Admin needs to
// see the folder the Show was filed from.
func (db *DB) ShowFixContexts(ids []string) (map[string]FixContext, error) {
	out := map[string]FixContext{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// A Show's enrichment lives in entity_enrichment, not on the row — LEFT JOINed
	// so a never-enriched Show still comes back with its path. There is no enriched
	// year for a Show (entity_enrichment carries no release date), so a Show row's
	// confirming evidence is its poster and match status, not a year.
	rows, err := db.Query(
		`SELECT sh.id, sh.title,
		        (SELECT f.path FROM titles t
		           JOIN seasons se ON t.season_id = se.id
		           JOIN editions e ON e.title_id = t.id
		           JOIN files f ON f.edition_id = e.id
		          WHERE se.show_id = sh.id AND f.present = 1
		          ORDER BY f.path LIMIT 1) AS path,
		        ee.enrichment_status
		   FROM shows sh
		   LEFT JOIN entity_enrichment ee
		          ON ee.entity_type = 'show' AND ee.entity_id = sh.id
		  WHERE sh.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading show fix contexts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c FixContext
		var path, status sql.NullString
		if err := rows.Scan(&c.ShowID, &c.ShowTitle, &path, &status); err != nil {
			return nil, fmt.Errorf("store: scanning show fix context: %w", err)
		}
		c.Path = path.String
		// A Show that has never been enriched has no entity_enrichment row at all,
		// where a Title's column defaults to 'pending'. Normalize, so a reader can
		// treat the two kinds alike and "" always means "the server didn't say".
		c.EnrichmentStatus = status.String
		if c.EnrichmentStatus == "" {
			c.EnrichmentStatus = "pending"
		}
		out[c.ShowID] = c
	}
	return out, rows.Err()
}
