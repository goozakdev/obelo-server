package api

import (
	"net/http"

	"github.com/marioquake/juicebox/internal/catalog"
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
	ID               string              `json:"id"`
	Kind             string              `json:"kind"`
	Title            string              `json:"title"`
	Year             int                 `json:"year,omitempty"`
	TMDBID           string              `json:"tmdbId,omitempty"`
	IMDBID           string              `json:"imdbId,omitempty"`
	AddedAt          string              `json:"addedAt,omitempty"`
	ResumePositionMs int64               `json:"resumePositionMs,omitempty"`
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
}

type homeResponse struct {
	ContinueWatching []homeTitleJSON `json:"continueWatching"`
	UpNext           []homeTitleJSON `json:"upNext"`
	RecentlyAdded    []homeTitleJSON `json:"recentlyAdded"`
}

func toHomeTitle(t catalog.HomeTitle) homeTitleJSON {
	j := homeTitleJSON{
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
		j.Track = toTrackContext(*t.Track)
	}
	return j
}

// handleHome returns the calling User's computed Home rows (authenticated).
func handleHome(svc *catalog.Service) http.HandlerFunc {
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
		out := homeResponse{
			ContinueWatching: make([]homeTitleJSON, 0, len(cw.Titles)),
			UpNext:           make([]homeTitleJSON, 0, len(un.Titles)),
			RecentlyAdded:    make([]homeTitleJSON, 0, len(ra.Titles)),
		}
		for _, t := range cw.Titles {
			out.ContinueWatching = append(out.ContinueWatching, toHomeTitle(t))
		}
		for _, t := range un.Titles {
			out.UpNext = append(out.UpNext, toHomeTitle(t))
		}
		for _, t := range ra.Titles {
			out.RecentlyAdded = append(out.RecentlyAdded, toHomeTitle(t))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// homeRowLimit caps how many Titles each Home row carries. The Home surface is a
// browse shortcut, not a paginated list; a bounded slice keeps the payload small.
const homeRowLimit = 20
