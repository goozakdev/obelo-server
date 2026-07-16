package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/store"
)

// withArtworkVersion appends an opaque per-entity cache-bust token to an artwork
// URL so a re-fetched image (a newer MAX added_at) busts the browser cache; an
// empty version (no artwork / unknown) leaves the URL untouched. The artwork
// handlers ignore the query string (they serve by id+role), so this is purely a
// client-cache key — the parent-entity analogue of the title summary's
// artworkVersion (realtime-events web slice).
func withArtworkVersion(rawURL, version string) string {
	if version == "" {
		return rawURL
	}
	return rawURL + "?v=" + url.QueryEscape(version)
}

// Wire shapes + handlers for the TV browse hierarchy (issue tv-music/01,
// docs/api-contract.md): a TV Library's GET /libraries/{id}/titles returns Shows;
// GET /shows/{id}/seasons and GET /seasons/{id}/episodes drill down; an Episode's
// GET /titles/{id} adds parent context. camelCase, the standard error envelope,
// and 404-not-403 access control exactly like the Movie endpoints.

// --- Show summary (the TV grid) ---------------------------------------------

type showSummaryJSON struct {
	ID string `json:"id"`
	// LibraryID is the Library this Show belongs to — the client's Show-detail
	// "Back" link returns to its owning Library. Set on the detail; harmless on the
	// grid (each grid Show already carries it).
	LibraryID string `json:"libraryId,omitempty"`
	Kind      string `json:"kind"` // always "show" — lets the client branch in the grid
	Title     string `json:"title"`
	Year      int    `json:"year,omitempty"`
	// NeedsReview flags a Show filed from a partial parse (e.g. a yearless Show).
	NeedsReview bool   `json:"needsReview,omitempty"`
	TMDBID      string `json:"tmdbId,omitempty"`
	IMDBID      string `json:"imdbId,omitempty"`
	// IdentityKey is the Show's stable identity key (ADR-0014). Surfaced so a client
	// — and the Wrong-item tests — can observe a Show identity change (item-editing/04).
	IdentityKey string `json:"identityKey,omitempty"`
	AddedAt     string `json:"addedAt,omitempty"`
	// UnwatchedEpisodeCount is the per-User count of this Show's visible Episodes
	// the caller has NOT watched — the Show-poster watched affordance (issue
	// tv-music/04), analogous to a Movie's resume marker. Set only on the Show
	// grid (GET /libraries/{id}/titles for a TV Library); omitted (0) elsewhere.
	UnwatchedEpisodeCount int `json:"unwatchedEpisodeCount,omitempty"`
	// Enrichment (issue 03): the descriptive fields + fetched artwork the optional
	// Enrichment step decorates a Show with. All omitempty so an un-enriched Show
	// is unchanged. PosterURL/BackgroundURL/LogoURL point at the Show artwork
	// endpoint and are set only when a fetched image exists.
	Overview         string   `json:"overview,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	ContentRating    string   `json:"contentRating,omitempty"`
	Network          string   `json:"network,omitempty"`
	EnrichmentStatus string   `json:"enrichmentStatus,omitempty"`
	PosterURL        string   `json:"posterUrl,omitempty"`
	BackgroundURL    string   `json:"backgroundUrl,omitempty"`
	LogoURL          string   `json:"logoUrl,omitempty"`
	// Edit-item surface (item-editing/02): on the Show DETAIL only, which fields are
	// Locked and which Enrichment override is in effect, so an Admin can see/undo a
	// prior correction. Omitted on the lean grid.
	LockedFields       []string            `json:"lockedFields,omitempty"`
	EnrichmentOverride *entityOverrideJSON `json:"enrichmentOverride,omitempty"`
	// Cast is the Show's series main cast (cast-photos/02), each member carrying the
	// same personId + photoVersion shape a Movie's cast does — so the Show detail
	// renders the same headshot cast strip. On the Show DETAIL only; omitted (empty)
	// on the lean grid and when no cast was captured.
	Cast []creditJSON `json:"cast,omitempty"`
}

// decorateShow overlays a Show summary with its enriched metadata + artwork URLs.
// roles is the set of artwork roles the Show has fetched (so a URL is advertised
// only when an image exists); version is the Show's artwork cache-bust token,
// appended so a re-enriched poster/backdrop reloads in place. A nil/zero
// enrichment leaves the summary lean.
func decorateShow(js *showSummaryJSON, e store.EntityEnrichment, roles map[string]bool, version string) {
	js.Overview = e.Overview
	js.Genres = e.Genres
	js.ContentRating = e.ContentRating
	js.Network = e.Network
	if e.Status != "" && e.Status != "pending" {
		js.EnrichmentStatus = e.Status
	}
	if roles["poster"] {
		js.PosterURL = withArtworkVersion(APIPrefix+"/shows/"+js.ID+"/artwork/poster", version)
	}
	if roles["background"] {
		js.BackgroundURL = withArtworkVersion(APIPrefix+"/shows/"+js.ID+"/artwork/background", version)
	}
	if roles["logo"] {
		js.LogoURL = withArtworkVersion(APIPrefix+"/shows/"+js.ID+"/artwork/logo", version)
	}
}

type showsResponse struct {
	Shows      []showSummaryJSON `json:"shows"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func toShowSummary(s store.Show) showSummaryJSON {
	return showSummaryJSON{
		ID:          s.ID,
		LibraryID:   s.LibraryID,
		Kind:        "show",
		Title:       s.Title,
		Year:        s.Year,
		NeedsReview: s.NeedsReview,
		TMDBID:      s.TMDBID,
		IMDBID:      s.IMDBID,
		IdentityKey: s.IdentityKey,
		AddedAt:     formatTimestamp(s.AddedAt),
	}
}

// --- Season + Episode listings ----------------------------------------------

type seasonJSON struct {
	ID           string `json:"id"`
	ShowID       string `json:"showId"`
	SeasonNumber int    `json:"seasonNumber"`
	// Specials is true for Season 0 so the client can label it "Specials".
	Specials     bool `json:"specials,omitempty"`
	EpisodeCount int  `json:"episodeCount"`
	// PosterURL points at the Season artwork endpoint, set when the Season HAS a
	// poster from either source: a local `Season NN.jpg` in the Show folder
	// (naming-convention.md) or one Enrichment fetched (issue 03). Local wins when
	// both exist; the URL is the same either way, so a client never learns which.
	PosterURL string `json:"posterUrl,omitempty"`
}

type seasonsResponse struct {
	Show    showSummaryJSON `json:"show"`
	Seasons []seasonJSON    `json:"seasons"`
	// ResumePoint is the Show detail page's next-episode block (issue 02, ADR-0028):
	// the resume-point Episode + its mode (in-progress → Continue/Restart, next →
	// Play). Null (omitted) for a not-started or fully-watched Show — the page then
	// shows the Show description, with Play only when not started (the client tells
	// the two null cases apart by the Show's unwatchedEpisodeCount).
	ResumePoint *resumePointJSON `json:"resumePoint,omitempty"`
}

// resumePointJSON is the Show detail page's resume-point block (issue 02): the
// Episode to surface plus the mode that selects its controls. Decorated with the
// same Episode enrichment a Season's Episode list applies (canonical display title,
// synopsis, still). seasonId is enough for the client to build the cross-season
// show-from-here Queue with this Episode as the head.
type resumePointJSON struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"` // "episode"
	SeasonID     string `json:"seasonId"`
	SeasonNumber int    `json:"seasonNumber"`
	// EpisodeNumber / EpisodeLabel form the S/E code the block labels ("S01E03", or
	// a degraded-offline label).
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	EpisodeLabel  string `json:"episodeLabel,omitempty"`
	Title         string `json:"title"`
	Overview      string `json:"overview,omitempty"`
	// ResumePositionMs is where Continue seeks (the in-progress anchor's stored
	// resume); 0 for the next mode (Play from the start).
	ResumePositionMs int64 `json:"resumePositionMs,omitempty"`
	// DurationMs is the Episode's playable duration; with ResumePositionMs it drives
	// the in-progress Continue progress bar + minutes-remaining label. 0/omitted when
	// unknown.
	DurationMs int64 `json:"durationMs,omitempty"`
	// Mode is "inProgress" (Continue + Restart) or "next" (a single Play).
	Mode             string `json:"mode"`
	EnrichmentStatus string `json:"enrichmentStatus,omitempty"`
	StillURL         string `json:"stillUrl,omitempty"`
}

// resumePointMode maps the store's in-progress flag to the wire mode string.
func resumePointMode(inProgress bool) string {
	if inProgress {
		return "inProgress"
	}
	return "next"
}

// toResumePoint shapes a store.ResumePoint into its wire form, applying the same
// Episode enrichment toEpisodeSummary does (canonical display title, synopsis,
// still). version is the Episode's title-artwork cache-bust token.
func toResumePoint(rp store.ResumePoint, version string) *resumePointJSON {
	js := &resumePointJSON{
		ID:               rp.ID,
		Kind:             rp.Kind,
		SeasonID:         rp.SeasonID,
		SeasonNumber:     rp.SeasonNumber,
		EpisodeNumber:    rp.EpisodeNumber,
		EpisodeLabel:     rp.EpisodeLabel,
		Title:            displayTitle(rp.Title),
		Overview:         rp.Overview,
		ResumePositionMs: rp.ResumePositionMs,
		DurationMs:       rp.DurationMs,
		Mode:             resumePointMode(rp.InProgress),
	}
	if rp.EnrichmentStatus != "" && rp.EnrichmentStatus != "pending" {
		js.EnrichmentStatus = rp.EnrichmentStatus
	}
	if rp.EnrichmentStatus == "matched" {
		js.StillURL = withArtworkVersion(APIPrefix+"/titles/"+rp.ID+"/artwork/poster", version)
	}
	return js
}

func toSeasonJSON(s store.Season) seasonJSON {
	return seasonJSON{
		ID:           s.ID,
		ShowID:       s.ShowID,
		SeasonNumber: s.SeasonNumber,
		Specials:     s.SeasonNumber == 0,
		EpisodeCount: s.EpisodeCount,
	}
}

// episodeSummaryJSON is an Episode in a Season listing: a Title summary plus the
// TV ordering (season/episode number + the degraded-offline label) and the
// calling User's watch state (resume + watched), so the list shows progress
// markers without an extra round-trip per Episode.
type episodeSummaryJSON struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // "episode"
	Title         string `json:"title"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	// EpisodeLabel is a date / "Episode N" for a degraded-offline episode, else "".
	EpisodeLabel     string `json:"episodeLabel,omitempty"`
	NeedsReview      bool   `json:"needsReview,omitempty"`
	ResumePositionMs int64  `json:"resumePositionMs,omitempty"`
	Watched          bool   `json:"watched,omitempty"`
	AddedAt          string `json:"addedAt,omitempty"`
	// Enrichment (issue 03): Title above already prefers the canonical enriched
	// display title; Overview is the episode synopsis; StillURL points at the
	// episode still (the title artwork endpoint). All omitempty.
	Overview         string `json:"overview,omitempty"`
	EnrichmentStatus string `json:"enrichmentStatus,omitempty"`
	StillURL         string `json:"stillUrl,omitempty"`
}

type episodesResponse struct {
	Season   seasonJSON           `json:"season"`
	Episodes []episodeSummaryJSON `json:"episodes"`
}

func toEpisodeSummary(t store.Title, ws store.WatchState, version string) episodeSummaryJSON {
	js := episodeSummaryJSON{
		ID:               t.ID,
		Kind:             t.Kind,
		Title:            displayTitle(t),
		SeasonNumber:     t.SeasonNumber,
		EpisodeNumber:    t.EpisodeNumber,
		EpisodeLabel:     t.EpisodeLabel,
		NeedsReview:      t.NeedsReview,
		ResumePositionMs: ws.ResumePositionMs,
		Watched:          ws.Watched,
		AddedAt:          formatTimestamp(t.AddedAt),
		Overview:         t.Overview,
	}
	if t.EnrichmentStatus != "" && t.EnrichmentStatus != "pending" {
		js.EnrichmentStatus = t.EnrichmentStatus
	}
	if t.EnrichmentStatus == "matched" {
		// The episode still is served as its poster-role title artwork; version it
		// (newest artwork added_at) so a re-enriched still reloads in place.
		js.StillURL = withArtworkVersion(APIPrefix+"/titles/"+t.ID+"/artwork/poster", version)
	}
	return js
}

// displayTitle is the title a client should show for a leaf: the canonical
// enriched title when present (e.g. a real episode name for a date-based
// episode), else the parsed title. Identity is unaffected — this is display only.
func displayTitle(t store.Title) string {
	if t.EnrichedTitle != "" {
		return t.EnrichedTitle
	}
	return t.Title
}

// --- Handlers ---------------------------------------------------------------

// handleListShows returns a cursor-paginated page of a TV Library's Shows. It is
// the TV branch of GET /libraries/{id}/titles (the handler picks this when the
// Library's kind is "tv"). Unknown/inaccessible Library → 404.
func handleListShows(svc *catalog.Service, libraryID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		page, err := svc.ListShows(scope, catalog.ListInput{
			LibraryID: libraryID,
			Cursor:    q.Get("cursor"),
			Limit:     parseLimit(q.Get("limit")),
			Genre:     q.Get("filter[genre]"),
		})
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case errors.Is(err, catalog.ErrBadCursor):
			writeError(w, http.StatusBadRequest, codeBadRequest, "invalid cursor", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list shows", nil)
			return
		}
		// Decorate each Show poster with the caller's unwatched-Episode count (the
		// watched affordance, issue tv-music/04) via one bulk read. The grid runs
		// behind requireAuth, so the identity is present; if it is somehow absent we
		// still render the grid (counts simply omitted).
		counts := map[string]int{}
		if ident, ok := identityFrom(r.Context()); ok {
			ids := make([]string, 0, len(page.Shows))
			for _, s := range page.Shows {
				ids = append(ids, s.ID)
			}
			if c, err := svc.UnwatchedEpisodeCounts(ident.User.ID, ids); err == nil {
				counts = c
			}
		}
		// Enrichment for the page in two bulk reads (no N+1): the descriptive
		// fields + which artwork roles each Show has fetched.
		ids := make([]string, 0, len(page.Shows))
		for _, s := range page.Shows {
			ids = append(ids, s.ID)
		}
		enr, _ := svc.EntityEnrichmentForMany(store.EntityShow, ids)
		roles, _ := svc.EntityArtworkRoles(store.EntityShow, ids)
		versions, _ := svc.EntityArtworkVersions(store.EntityShow, ids)

		out := showsResponse{
			Shows:      make([]showSummaryJSON, 0, len(page.Shows)),
			NextCursor: page.NextCursor,
		}
		for _, s := range page.Shows {
			js := toShowSummary(s)
			js.UnwatchedEpisodeCount = counts[s.ID]
			decorateShow(&js, enr[s.ID], roles[s.ID], versions[s.ID])
			out.Shows = append(out.Shows, js)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleShowSubtree dispatches routes under "/shows/{id}...": the seasons listing
// (bearer-only) and the Show artwork GET (cookie-capable, so a browser <img> can
// load a fetched Show poster/backdrop). Mirrors handleAlbumSubtree.
func handleShowSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/shows/")
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			id := rest[:i]
			role := rest[i+len("/artwork/"):]
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleEntityArtwork(deps.Catalog, store.EntityShow, id, role))))(w, r)
			return
		}
		// POST {id}/review: dismiss this Show's needs_review flag (Admin).
		if id, ok := strings.CutSuffix(rest, "/review"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleReviewShow(deps.Catalog, id))))(w, r)
			return
		}
		// PUT {id}/identityCorrection: the Wrong-item destructive correction on a Show
		// (Admin, ADR-0019) — folder-keyed Match override + re-key + Episode watch-state
		// reset + Locked-field clear + re-enrich. Matched before the shared Edit-item
		// routes (distinct suffix) and the seasons fall-through.
		if id, ok := strings.CutSuffix(rest, "/identityCorrection"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handleShowIdentityCorrection(deps, id))))(w, r)
			return
		}
		// POST {id}/scan: Targeted scan of this Show's folder (Admin, ADR-0030).
		if id, ok := strings.CutSuffix(rest, "/scan"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleTargetedScan(deps, "show", id))))(w, r)
			return
		}
		// Edit-item on a Show (item-editing/02): Fix-info search/override + hand-edit +
		// lock-release. Admin-only; served before the seasons listing fall-through.
		if dispatchEntityEditRoutes(w, r, deps, store.EntityShow, rest) {
			return
		}
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleShowSeasons(deps.Catalog))))(w, r)
	}
}

// handleSeasonSubtree dispatches routes under "/seasons/{id}...": the episodes
// listing (bearer-only) and the Season artwork GET (cookie-capable).
func handleSeasonSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/seasons/")
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			id := rest[:i]
			role := rest[i+len("/artwork/"):]
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleEntityArtwork(deps.Catalog, store.EntitySeason, id, role))))(w, r)
			return
		}
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleSeasonEpisodes(deps.Catalog))))(w, r)
	}
}

// handleEntityArtwork serves a browse parent's fetched artwork bytes (Show/Season/
// Artist + role). Local artwork, where a parent has any, is served ahead of this
// by the caller; this is the fetched fallback. Unknown entity/role → 404.
func handleEntityArtwork(svc *catalog.Service, entityType, entityID, role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if entityID == "" || role == "" || strings.Contains(role, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		art, err := svc.EntityArtwork(scope, entityType, entityID, role)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "artwork not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get artwork", nil)
			return
		}
		http.ServeFile(w, r, svc.ResolveArtworkPath(art.Path))
	}
}

// handlePersonSubtree dispatches routes under "/people/{personRef}...": the cast
// headshot GET (cookie-capable). The personRef is provider-namespaced ("tmdb:<id>",
// url-encoded by the client and already decoded in r.URL.Path). Only the artwork
// leaf exists in this slice — a person has no browse detail (cast-photos/01).
func handlePersonSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/people/")
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			personRef := rest[:i]
			role := rest[i+len("/artwork/"):]
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handlePersonArtwork(deps.Catalog, personRef, role))))(w, r)
			return
		}
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	}
}

// handlePersonArtwork serves a cast member's fetched headshot bytes for a role
// (profile). Access follows the Titles that credit the person, so a person only
// reachable through a Library the viewer lacks is hidden as 404 — exactly like an
// out-of-scope title poster. An unknown/photoless ref → 404 (the client shows the
// placeholder). Streams the cached file with http.ServeFile (cast-photos/01).
func handlePersonArtwork(svc *catalog.Service, personRef, role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if personRef == "" || role == "" || strings.Contains(role, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		art, err := svc.PersonArtwork(scope, personRef, role)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "artwork not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get artwork", nil)
			return
		}
		http.ServeFile(w, r, svc.ResolveArtworkPath(art.Path))
	}
}

// handleShowSeasons serves GET /shows/{id}/seasons. Unknown/inaccessible Show →
// 404 (hide existence). The path must be exactly /shows/{id}/seasons.
func handleShowSeasons(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/shows/", "/seasons")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		show, seasons, err := svc.Seasons(scope, id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "show not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list seasons", nil)
			return
		}
		showJS := toShowSummary(show)
		// The per-User unwatched-Episode count on the DETAIL (not just the grid, issue
		// tv-music/04): the resume point is null for both a not-started and a fully-
		// watched Show, so the client tells them apart by this count — > 0 (with a
		// null resume point) means not started → show Play; 0 means fully watched → no
		// Play. The Show-detail route runs behind requireAuth, so identity is present.
		var resumePoint *resumePointJSON
		if ident, ok := identityFrom(r.Context()); ok {
			if counts, err := svc.UnwatchedEpisodeCounts(ident.User.ID, []string{show.ID}); err == nil {
				showJS.UnwatchedEpisodeCount = counts[show.ID]
			}
			// The resume point (issue 02, ADR-0028): the detail page's next-episode block,
			// the same computation as Home's Up Next but keeping the in-progress case. Null
			// for a not-started/fully-watched Show. Decorated with the Episode's still
			// cache-bust version, matching a Season's Episode enrichment.
			if rp, found, err := svc.ShowResumePoint(scope, ident.User.ID, show.ID); err == nil && found {
				versions, _ := svc.ArtworkVersionsForTitles([]string{rp.ID})
				resumePoint = toResumePoint(rp, versions[rp.ID])
			}
		}
		if e, err := svc.EntityEnrichment(store.EntityShow, show.ID); err == nil {
			roles, _ := svc.EntityArtworkRoles(store.EntityShow, []string{show.ID})
			versions, _ := svc.EntityArtworkVersions(store.EntityShow, []string{show.ID})
			decorateShow(&showJS, e, roles[show.ID], versions[show.ID])
			showJS.EnrichmentOverride = entityOverride(e)
		}
		showJS.LockedFields, _ = svc.EntityLockedFields(store.EntityShow, show.ID)
		// The Show's series main cast (cast-photos/02): the same personId + photoVersion
		// shape a Movie carries, so the detail renders the same headshot cast strip. An
		// un-enriched Show (or one with no captured cast) simply omits it.
		if credits, err := svc.EntityCredits(store.EntityShow, show.ID); err == nil && len(credits) > 0 {
			showJS.Cast = toCreditsJSON(credits)
		}

		// Season posters + their cache-bust versions in two bulk reads.
		seasonIDs := make([]string, 0, len(seasons))
		for _, s := range seasons {
			seasonIDs = append(seasonIDs, s.ID)
		}
		seasonRoles, _ := svc.EntityArtworkRoles(store.EntitySeason, seasonIDs)
		seasonVersions, _ := svc.EntityArtworkVersions(store.EntitySeason, seasonIDs)

		out := seasonsResponse{
			Show:        showJS,
			Seasons:     make([]seasonJSON, 0, len(seasons)),
			ResumePoint: resumePoint,
		}
		for _, s := range seasons {
			js := toSeasonJSON(s)
			if seasonRoles[s.ID]["poster"] {
				js.PosterURL = withArtworkVersion(APIPrefix+"/seasons/"+s.ID+"/artwork/poster", seasonVersions[s.ID])
			}
			out.Seasons = append(out.Seasons, js)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleSeasonEpisodes serves GET /seasons/{id}/episodes, decorating each Episode
// with the calling User's watch state. Unknown/inaccessible Season → 404.
func handleSeasonEpisodes(svc *catalog.Service) http.HandlerFunc {
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
		id := pathParam(r.URL.Path, "/seasons/", "/episodes")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		season, episodes, err := svc.Episodes(scope, id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "season not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list episodes", nil)
			return
		}
		ids := make([]string, 0, len(episodes))
		for _, e := range episodes {
			ids = append(ids, e.ID)
		}
		states, err := svc.WatchStatesForTitles(ident.User.ID, ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list episodes", nil)
			return
		}
		// Episode stills are title-keyed artwork, so their cache-bust versions come
		// from the title artwork table (one bulk read, no N+1).
		versions, _ := svc.ArtworkVersionsForTitles(ids)
		out := episodesResponse{
			Season:   toSeasonJSON(season),
			Episodes: make([]episodeSummaryJSON, 0, len(episodes)),
		}
		for _, e := range episodes {
			out.Episodes = append(out.Episodes, toEpisodeSummary(e, states[e.ID], versions[e.ID]))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// episodeContextJSON is the parent chain attached to an Episode's GET /titles/{id}
// so a client can show "The Bear · S01E03" without extra round-trips. Omitted
// entirely for a Movie.
type episodeContextJSON struct {
	ShowID        string `json:"showId"`
	ShowTitle     string `json:"showTitle"`
	ShowYear      int    `json:"showYear,omitempty"`
	SeasonID      string `json:"seasonId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	EpisodeLabel  string `json:"episodeLabel,omitempty"`
}

func toEpisodeContext(c store.EpisodeContext) *episodeContextJSON {
	return &episodeContextJSON{
		ShowID:        c.ShowID,
		ShowTitle:     c.ShowTitle,
		ShowYear:      c.ShowYear,
		SeasonID:      c.SeasonID,
		SeasonNumber:  c.SeasonNumber,
		EpisodeNumber: c.EpisodeNumber,
		EpisodeLabel:  c.EpisodeLabel,
	}
}
