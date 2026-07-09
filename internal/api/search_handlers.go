package api

import (
	"net/http"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/store"
)

// Cross-kind search (issue tv-music/04, docs/api-contract.md /search). One
// authenticated GET returns matches across every browse kind — Movies, Shows,
// Artists, Albums, and (drilling in) Episodes and Tracks — grouped so a client
// can render a sectioned result. Access control is the same as browse: hidden
// (all-Files-Missing) entities are excluded server-side, and an entity outside
// the User's access would be omitted the same way (existence-hiding, 404-not-403
// for a direct fetch). Result rows reuse the existing browse summary DTOs so the
// shapes match what the grid/list endpoints already return.

// searchResponse groups the matches by kind. Each group is an array (never null)
// so a client can iterate every section unconditionally.
type searchResponse struct {
	Movies   []titleSummaryJSON  `json:"movies"`
	Shows    []showSummaryJSON   `json:"shows"`
	Artists  []artistSummaryJSON `json:"artists"`
	Albums   []albumJSON         `json:"albums"`
	Episodes []titleSummaryJSON  `json:"episodes"`
	Tracks   []titleSummaryJSON  `json:"tracks"`
}

// handleSearch serves GET /search?q=&limit=. The query is matched
// case-insensitively as a substring against each entity's display name; an empty
// query yields empty groups (200, not an error). Results carry the calling
// User's watch state on the playable leaves (Movies/Episodes/Tracks) via one
// bulk read, so a result row shows its watched/resume marker like a grid entry.
func handleSearch(svc *catalog.Service) http.HandlerFunc {
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
		q := r.URL.Query()
		query := q.Get("q")
		if query == "" {
			query = q.Get("query")
		}
		res, err := svc.Search(scope, query, parseLimit(q.Get("limit")))
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "search failed", nil)
			return
		}

		// One bulk watch-state read across every playable leaf in the results, so
		// each Movie/Episode/Track row carries resume/watched without an N+1.
		ids := make([]string, 0, len(res.Movies)+len(res.Episodes)+len(res.Tracks))
		for _, t := range res.Movies {
			ids = append(ids, t.ID)
		}
		for _, t := range res.Episodes {
			ids = append(ids, t.ID)
		}
		for _, t := range res.Tracks {
			ids = append(ids, t.ID)
		}
		states, err := svc.WatchStatesForTitles(ident.User.ID, ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "search failed", nil)
			return
		}
		// Enrichment for the playable leaves in two bulk reads, so search results
		// carry overview/genres/status like a browse list (issue 03).
		enr, _ := svc.TitleEnrichmentForMany(ids)
		genres, _ := svc.GenresForTitles(ids)

		// Parent-entity enrichment for the Show/Artist/Album rows, so search
		// summaries decorate them exactly like the browse grids do (issue 03).
		showIDs := make([]string, 0, len(res.Shows))
		for _, s := range res.Shows {
			showIDs = append(showIDs, s.ID)
		}
		artistIDs := make([]string, 0, len(res.Artists))
		for _, a := range res.Artists {
			artistIDs = append(artistIDs, a.ID)
		}
		albumIDs := make([]string, 0, len(res.Albums))
		for _, al := range res.Albums {
			albumIDs = append(albumIDs, al.ID)
		}
		showEnr, _ := svc.EntityEnrichmentForMany(store.EntityShow, showIDs)
		showRoles, _ := svc.EntityArtworkRoles(store.EntityShow, showIDs)
		artistEnr, _ := svc.EntityEnrichmentForMany(store.EntityArtist, artistIDs)
		artistRoles, _ := svc.EntityArtworkRoles(store.EntityArtist, artistIDs)
		albumEnr, _ := svc.EntityEnrichmentForMany(store.EntityAlbum, albumIDs)
		albumRoles, _ := svc.EntityArtworkRoles(store.EntityAlbum, albumIDs)

		out := searchResponse{
			Movies:   make([]titleSummaryJSON, 0, len(res.Movies)),
			Shows:    make([]showSummaryJSON, 0, len(res.Shows)),
			Artists:  make([]artistSummaryJSON, 0, len(res.Artists)),
			Albums:   make([]albumJSON, 0, len(res.Albums)),
			Episodes: make([]titleSummaryJSON, 0, len(res.Episodes)),
			Tracks:   make([]titleSummaryJSON, 0, len(res.Tracks)),
		}
		for _, t := range res.Movies {
			out.Movies = append(out.Movies, toTitleSummary(mergeEnrichment(t, enr), states[t.ID], genres[t.ID]))
		}
		for _, s := range res.Shows {
			js := toShowSummary(s)
			// Search results are a one-shot list (no live refresh), so an artwork
			// cache-bust version isn't needed here — consistent with the Movie rows
			// above, which also omit it.
			decorateShow(&js, showEnr[s.ID], showRoles[s.ID], "")
			out.Shows = append(out.Shows, js)
		}
		for _, a := range res.Artists {
			js := toArtistSummary(a)
			decorateArtist(&js, artistEnr[a.ID], artistRoles[a.ID], "")
			out.Artists = append(out.Artists, js)
		}
		for _, al := range res.Albums {
			js := toAlbumJSON(al)
			decorateAlbum(&js, albumEnr[al.ID], albumRoles[al.ID], "")
			out.Albums = append(out.Albums, js)
		}
		for _, t := range res.Episodes {
			out.Episodes = append(out.Episodes, toTitleSummary(mergeEnrichment(t, enr), states[t.ID], genres[t.ID]))
		}
		for _, t := range res.Tracks {
			out.Tracks = append(out.Tracks, toTitleSummary(mergeEnrichment(t, enr), states[t.ID], genres[t.ID]))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
