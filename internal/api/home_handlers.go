package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// GET /api/v1/home (issue 08, extended tv-music/02): the per-User computed Home
// surface. Three computed views, never stored, all excluding hidden
// (all-Files-Missing) Titles:
//   - Continue Watching (in-progress Titles, 2%–90% band, most-recent first);
//   - Up Next (TV-only: the next unwatched Episode in Show order for each Show
//     the User has started — advances when an Episode crosses the ~90% Watched
//     threshold OR is toggled watched/unwatched manually);
//   - Recently Added (Titles newest-added first).
//
// Continue Watching and Recently Added are additively decorated with parent
// context for an Episode leaf (the `episode` field) so a client renders
// "The Bear · S01E03" rather than a bare title; Movie entries are unchanged.

// homeTitleJSON is a Title in a Home row plus its per-User resume position. It is
// deliberately a summary shape (not the full nested detail) so a Home payload
// stays light; resumePositionMs is 0/omitted for Recently Added / Up Next
// entries. Episode carries the Show/Season/episode parent context for an Episode
// leaf (omitted entirely for a Movie — additive, Movie shape unchanged).
type homeTitleJSON struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Year             int    `json:"year,omitempty"`
	TMDBID           string `json:"tmdbId,omitempty"`
	IMDBID           string `json:"imdbId,omitempty"`
	AddedAt          string `json:"addedAt,omitempty"`
	ResumePositionMs int64  `json:"resumePositionMs,omitempty"`
	// DurationMs is the Title's playable duration; with ResumePositionMs it drives
	// the Continue Watching card's progress bar — the same pairing, and the same
	// value, the Show detail's resumePoint carries. Present on Continue Watching
	// entries only (Up Next / Recently Added draw no bar, so they omit it, as they
	// omit resumePositionMs); also omitted when the duration is unknown.
	DurationMs int64               `json:"durationMs,omitempty"`
	Episode    *episodeContextJSON `json:"episode,omitempty"`
	// Track carries the Artist/Album/disc/track parent context for a Track leaf
	// (kind "track"), so a Home card reads as "Radiohead · OK Computer"; omitted
	// for a Movie/Episode (issue tv-music/03, additive).
	Track *trackContextJSON `json:"track,omitempty"`
	// Enrichment (issue 03): overview/genres + the canonical display title for an
	// Episode/Track. All omitempty so an un-enriched Home card is unchanged.
	Overview     string   `json:"overview,omitempty"`
	Genres       []string `json:"genres,omitempty"`
	DisplayTitle string   `json:"displayTitle,omitempty"`
	// Linked/Available: this row lives in a mirror of another household's Library
	// (ADR-0056 §1, §6). Absent on a local Title — exactly the pair, with exactly
	// the semantics, titleSummaryJSON carries. A Home row is the one place a
	// viewer meets a mirrored Title with no Library screen around it, so the row
	// itself has to say so rather than leave the client to infer it from where it
	// arrived (.scratch/linked-servers issue 14).
	Linked    bool  `json:"linked,omitempty"`
	Available *bool `json:"available,omitempty"`
	// LinkedServer names the sharing Server (ADR-0056 §6). Present only on a
	// linked row — the provenance a member sees on a card with no Library screen
	// around it.
	LinkedServer string `json:"linkedServer,omitempty"`
}

type homeResponse struct {
	ContinueWatching []homeTitleJSON `json:"continueWatching"`
	UpNext           []homeTitleJSON `json:"upNext"`
	RecentlyAdded    []homeTitleJSON `json:"recentlyAdded"`
	// Rows is the same Home surface as one ordered list, with My List and the
	// Discovery rows interleaved (home-discovery-rows 04). The three fields above
	// stay as they were for existing clients; a client that renders Rows ignores them.
	Rows []homeRowJSON `json:"rows"`
}

// homeRowJSON is one Home row. Items are homeTitleJSON for the built-in rows,
// playlistMemberJSON for My List, and titleSummaryJSON / showSummaryJSON for a
// Discovery row of Movies / Shows. Source and Total are on Discovery rows only.
type homeRowJSON struct {
	Kind      string             `json:"kind"`
	Title     string             `json:"title"`
	MediaKind string             `json:"mediaKind,omitempty"`
	Items     any                `json:"items"`
	Source    *homeRowSourceJSON `json:"source,omitempty"`
	Total     int                `json:"total,omitempty"`
}

// homeRowSourceJSON says what a Discovery row was drawn from: enough for a client
// to build the Library browse request behind its "See all", one per LibraryIDs
// entry. Type is the browse filter parameter name (filter[<type>]=<value>).
type homeRowSourceJSON struct {
	Type         string   `json:"type"`
	Value        string   `json:"value"`
	DisplayValue string   `json:"displayValue,omitempty"`
	LibraryIDs   []string `json:"libraryIds"`
}

func toHomeTitle(t catalog.HomeTitle, linked linkedState, albumVersions map[string]string) homeTitleJSON {
	isLinked, available, server := linked.decorate(t.LibraryID)
	j := homeTitleJSON{
		Linked:           isLinked,
		Available:        available,
		LinkedServer:     server,
		ID:               t.ID,
		Kind:             t.Kind,
		Title:            t.Title.Title,
		Year:             t.Year,
		TMDBID:           t.TMDBID,
		IMDBID:           t.IMDBID,
		AddedAt:          formatTimestamp(t.AddedAt),
		ResumePositionMs: t.ResumePositionMs,
		DurationMs:       t.DurationMs,
		Overview:         t.Overview,
		Genres:           t.Genres,
		DisplayTitle:     t.EnrichedTitle,
	}
	if t.Episode != nil {
		j.Episode = toEpisodeContext(*t.Episode)
	}
	if t.Track != nil {
		j.Track = toTrackContext(*t.Track, albumVersions[t.Track.AlbumID])
	}
	return j
}

// handleHome returns the calling User's computed Home rows (authenticated).
func handleHome(deps Deps) http.HandlerFunc {
	svc := deps.Catalog
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		cw, un, ra, err := svc.Home(scope, ident.User.ID, homeRowLimit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to build home", nil)
			return
		}
		// One read of the mirror state for the whole payload — three rows of up to
		// 20 Titles each share it, rather than one lookup per row.
		linked := loadLinkedState(deps)
		// One bulk read of the albums' cover versions for every Track row.
		var albumIDs []string
		for _, row := range [][]catalog.HomeTitle{cw.Titles, un.Titles, ra.Titles} {
			for _, t := range row {
				if t.Track != nil {
					albumIDs = append(albumIDs, t.Track.AlbumID)
				}
			}
		}
		albumVersions, _ := svc.EntityArtworkVersions(store.EntityAlbum, albumIDs)
		out := homeResponse{
			ContinueWatching: make([]homeTitleJSON, 0, len(cw.Titles)),
			UpNext:           make([]homeTitleJSON, 0, len(un.Titles)),
			RecentlyAdded:    make([]homeTitleJSON, 0, len(ra.Titles)),
		}
		for _, t := range cw.Titles {
			out.ContinueWatching = append(out.ContinueWatching, toHomeTitle(t, linked, albumVersions))
		}
		for _, t := range un.Titles {
			out.UpNext = append(out.UpNext, toHomeTitle(t, linked, albumVersions))
		}
		for _, t := range ra.Titles {
			out.RecentlyAdded = append(out.RecentlyAdded, toHomeTitle(t, linked, albumVersions))
		}
		now := time.Now
		if deps.Now != nil {
			now = deps.Now
		}
		out.Rows, err = buildHomeRows(deps, scope, ident.User.ID, now(), out)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to build home", nil)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// buildHomeRows lays the Home rows out in the fixed pattern: Continue Watching,
// Up Next, Discovery[0], My List, Discovery[1], Recently Added, Discovery[2..].
// An empty row is omitted and the rest move up; the pattern itself never reorders.
func buildHomeRows(deps Deps, scope access.Scope, userID string, day time.Time, h homeResponse) ([]homeRowJSON, error) {
	disc, err := deps.Catalog.DiscoveryRows(scope, userID, day, homeRowLimit)
	if err != nil {
		return nil, err
	}
	discRows, err := discoveryRowsJSON(deps, userID, disc)
	if err != nil {
		return nil, err
	}
	myList, err := myListMembers(deps, scope, userID)
	if err != nil {
		return nil, err
	}

	rows := []homeRowJSON{}
	add := func(r homeRowJSON, n int) {
		if n > 0 {
			rows = append(rows, r)
		}
	}
	nextDisc := func() {
		if len(discRows) > 0 {
			rows = append(rows, discRows[0])
			discRows = discRows[1:]
		}
	}
	add(homeRowJSON{Kind: "continueWatching", Title: "Continue Watching", Items: h.ContinueWatching}, len(h.ContinueWatching))
	add(homeRowJSON{Kind: "upNext", Title: "Up Next", Items: h.UpNext}, len(h.UpNext))
	nextDisc()
	add(homeRowJSON{Kind: "myList", Title: "My List", Items: myList}, len(myList))
	nextDisc()
	add(homeRowJSON{Kind: "recentlyAdded", Title: "Recently Added", Items: h.RecentlyAdded}, len(h.RecentlyAdded))
	return append(rows, discRows...), nil
}

// myListMembers is the caller's Watchlist in Watchlist order, resolved through
// their access Scope and Rating ceiling and shaped like the other built-in rows
// (so an Episode carries its parent context), capped to the Home row length.
func myListMembers(deps Deps, scope access.Scope, userID string) ([]homeTitleJSON, error) {
	wl, err := deps.Organize.Watchlist(userID)
	if err != nil {
		return nil, err
	}
	members, err := deps.Organize.PlaylistMembers(scope, userID, wl.ID)
	if err != nil {
		return nil, err
	}
	if len(members) > homeRowLimit {
		members = members[:homeRowLimit]
	}
	titles := make([]store.Title, 0, len(members))
	for _, m := range members {
		titles = append(titles, m.Title)
	}
	var albumIDs []string
	home := deps.Catalog.HomeTitles(titles)
	for _, t := range home {
		if t.Track != nil {
			albumIDs = append(albumIDs, t.Track.AlbumID)
		}
	}
	albumVersions, _ := deps.Catalog.EntityArtworkVersions(store.EntityAlbum, albumIDs)
	linked := loadLinkedState(deps)
	out := make([]homeTitleJSON, 0, len(home))
	for _, t := range home {
		out = append(out, toHomeTitle(t, linked, albumVersions))
	}
	return out, nil
}

// discoveryRowsJSON shapes the engine's rows: Movie rows as browse title
// summaries, Show rows as browse show summaries — the /search precedent.
func discoveryRowsJSON(deps Deps, userID string, rows []catalog.DiscoveryRow) ([]homeRowJSON, error) {
	var titleIDs, showIDs []string
	for _, r := range rows {
		for _, it := range r.Items {
			if r.Kind == "tv" {
				showIDs = append(showIDs, it.ID)
			} else {
				titleIDs = append(titleIDs, it.ID)
			}
		}
	}
	titles, err := deps.Catalog.TitlesByIDs(titleIDs)
	if err != nil {
		return nil, err
	}
	shows, err := deps.Catalog.ShowsByIDs(showIDs)
	if err != nil {
		return nil, err
	}
	showEnr, _ := deps.Catalog.EntityEnrichmentForMany(store.EntityShow, showIDs)
	showRoles, _ := deps.Catalog.EntityArtworkRoles(store.EntityShow, showIDs)
	showVersions, _ := deps.Catalog.EntityArtworkVersions(store.EntityShow, showIDs)
	linked := loadLinkedState(deps)

	out := make([]homeRowJSON, 0, len(rows))
	for _, r := range rows {
		typ := string(r.Source)
		if r.Source == catalog.SourceActor {
			typ = "person" // the filter[person] browse parameter
		}
		row := homeRowJSON{
			Kind:      "discovery",
			Title:     discoveryRowTitle(typ, r.Value, r.DisplayValue, r.Kind),
			MediaKind: r.Kind,
			Total:     r.Total,
			Source:    &homeRowSourceJSON{Type: typ, Value: r.Value, LibraryIDs: r.LibraryIDs},
		}
		if r.DisplayValue != r.Value {
			row.Source.DisplayValue = r.DisplayValue
		}
		if r.Kind == "tv" {
			items := make([]showSummaryJSON, 0, len(r.Items))
			for _, it := range r.Items {
				s, ok := shows[it.ID]
				if !ok {
					continue
				}
				js := toShowSummary(s, linked)
				decorateShow(&js, showEnr[s.ID], showRoles[s.ID], showVersions[s.ID])
				items = append(items, js)
			}
			row.Items = items
		} else {
			members := make([]store.Title, 0, len(r.Items))
			for _, it := range r.Items {
				if t, ok := titles[it.ID]; ok {
					members = append(members, t)
				}
			}
			items, err := decorateMembers(deps, userID, members)
			if err != nil {
				return nil, err
			}
			row.Items = items
		}
		out = append(out, row)
	}
	return out, nil
}

// discoveryRowTitle is the English label the Server computes for a Discovery row.
// A client may localize from the row's kind and source instead.
func discoveryRowTitle(typ, value, display, mediaKind string) string {
	noun := "Movies"
	if mediaKind == "tv" {
		noun = "Shows"
	}
	switch typ {
	case "studio":
		return "From " + value
	case "network":
		return "On " + value
	case "person":
		return "Starring " + display
	case "decade":
		if y, err := strconv.Atoi(value); err == nil {
			if y >= 1920 && y < 2000 {
				return strconv.Itoa(y%100) + "s " + noun
			}
			return value + "s " + noun
		}
	}
	return value + " " + noun
}

// homeRowLimit caps how many Titles each Home row carries. The Home surface is a
// browse shortcut, not a paginated list; a bounded slice keeps the payload small.
const homeRowLimit = 20
