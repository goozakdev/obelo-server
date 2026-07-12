package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes + handlers for the Music browse hierarchy (issue tv-music/03,
// docs/api-contract.md): a Music Library's GET /libraries/{id}/titles returns
// Artists; GET /artists/{id}/albums and GET /albums/{id}/tracks drill down; a
// Track's GET /titles/{id} adds parent context. camelCase, the standard error
// envelope, and 404-not-403 access control exactly like the Movie/TV endpoints.

// --- Artist summary (the Music list) ----------------------------------------

type artistSummaryJSON struct {
	ID string `json:"id"`
	// LibraryID is the Music Library this Artist belongs to — the client's
	// Artist-detail "Back" link returns to its owning Library.
	LibraryID string `json:"libraryId,omitempty"`
	Kind      string `json:"kind"` // always "artist" — lets the client branch in the list
	Name      string `json:"name"`
	// Enrichment (issue 03): bio (overview) + genres + fetched artwork, all
	// omitempty. ArtworkURL is the artist photo; BackgroundURL/LogoURL are the
	// fanart.tv Background + ClearLOGO (like a Show/Movie), each pointing at the
	// Artist artwork endpoint and set only when that role was fetched. Set on the
	// Artist detail; omitted on the lean list.
	Overview         string   `json:"overview,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	EnrichmentStatus string   `json:"enrichmentStatus,omitempty"`
	ArtworkURL       string   `json:"artworkUrl,omitempty"`
	BackgroundURL    string   `json:"backgroundUrl,omitempty"`
	LogoURL          string   `json:"logoUrl,omitempty"`
	// Edit-item surface (item-editing/02): on the Artist DETAIL only.
	LockedFields       []string            `json:"lockedFields,omitempty"`
	EnrichmentOverride *entityOverrideJSON `json:"enrichmentOverride,omitempty"`
}

type artistsResponse struct {
	Artists    []artistSummaryJSON `json:"artists"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

func toArtistSummary(a store.Artist) artistSummaryJSON {
	return artistSummaryJSON{ID: a.ID, LibraryID: a.LibraryID, Kind: "artist", Name: a.Name}
}

// decorateArtist overlays an Artist summary with its enriched bio/genres + image
// URL. roles is the set of artwork roles the Artist has fetched; version is the
// cache-bust token appended so a re-enriched image reloads in place.
func decorateArtist(js *artistSummaryJSON, e store.EntityEnrichment, roles map[string]bool, version string) {
	js.Overview = e.Overview
	js.Genres = e.Genres
	if e.Status != "" && e.Status != "pending" {
		js.EnrichmentStatus = e.Status
	}
	if roles["poster"] {
		js.ArtworkURL = artistArtworkURL(js.ID, "poster", version)
	}
	if roles["background"] {
		js.BackgroundURL = artistArtworkURL(js.ID, "background", version)
	}
	if roles["logo"] {
		js.LogoURL = artistArtworkURL(js.ID, "logo", version)
	}
}

// artistArtworkURL builds the Artist artwork endpoint URL for a role with its
// cache-bust token. Shared by the lean Artist list and the Artist detail so both
// advertise the same image location.
func artistArtworkURL(id, role, version string) string {
	return withArtworkVersion(APIPrefix+"/artists/"+id+"/artwork/"+role, version)
}

// --- Album + Track listings -------------------------------------------------

type albumJSON struct {
	ID       string `json:"id"`
	ArtistID string `json:"artistId"`
	// ArtistName is the parent Artist's display name, for the album header's
	// artist link. Omitempty: absent when the Artist row can't be resolved.
	ArtistName string `json:"artistName,omitempty"`
	Title      string `json:"title"`
	Year       int    `json:"year,omitempty"`
	// HasArtwork is true when a cover is available — a local cover.jpg/folder.jpg
	// OR an Enrichment-fetched cover; the client fetches it from GET
	// /albums/{id}/artwork (local wins). Genres is the enriched genre list.
	HasArtwork bool `json:"hasArtwork,omitempty"`
	// ArtworkVersion is the cover's cache-bust token (newest fetched-cover
	// timestamp), or "" for a local-only cover. The client appends it to the
	// /albums/{id}/artwork URL so a re-enriched cover reloads in place. Omitempty.
	ArtworkVersion   string   `json:"artworkVersion,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	EnrichmentStatus string   `json:"enrichmentStatus,omitempty"`
	TrackCount       int      `json:"trackCount"`
	// Edit-item surface (item-editing/02): on the Album DETAIL only.
	LockedFields       []string            `json:"lockedFields,omitempty"`
	EnrichmentOverride *entityOverrideJSON `json:"enrichmentOverride,omitempty"`
}

type albumsResponse struct {
	Artist artistSummaryJSON `json:"artist"`
	Albums []albumJSON       `json:"albums"`
}

func toAlbumJSON(a store.Album) albumJSON {
	return albumJSON{
		ID:         a.ID,
		ArtistID:   a.ArtistID,
		Title:      a.Title,
		Year:       a.Year,
		HasArtwork: a.ArtworkPath != "",
		TrackCount: a.TrackCount,
	}
}

// decorateAlbum overlays an Album with its enriched genres + status, and flips
// HasArtwork on when Enrichment fetched a cover (local cover already set it).
// version is the fetched cover's cache-bust token ("" for a local-only cover),
// so a re-enriched cover reloads in place.
func decorateAlbum(js *albumJSON, e store.EntityEnrichment, roles map[string]bool, version string) {
	js.Genres = e.Genres
	if e.Status != "" && e.Status != "pending" {
		js.EnrichmentStatus = e.Status
	}
	if roles["cover"] {
		js.HasArtwork = true
	}
	js.ArtworkVersion = version
}

// trackSummaryJSON is a Track in an Album listing: a Title summary plus the Music
// ordering (disc/track number) and the calling User's watch state, so the list
// shows progress markers without an extra round-trip per Track.
type trackSummaryJSON struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // "track"
	Title       string `json:"title"`
	DiscNumber  int    `json:"discNumber,omitempty"`
	TrackNumber int    `json:"trackNumber,omitempty"`
	// DurationMs is the Track's playable length (its file's duration), so the
	// Album track list shows each song's length without a per-Track fetch. 0/absent
	// when no file is indexed yet.
	DurationMs       int64 `json:"durationMs,omitempty"`
	NeedsReview      bool  `json:"needsReview,omitempty"`
	ResumePositionMs int64 `json:"resumePositionMs,omitempty"`
	Watched          bool  `json:"watched,omitempty"`
	// Enrichment (issue 03): Overview synopsis; Title above already prefers a
	// canonical enriched title where the tag title was sparse. Both omitempty.
	Overview         string `json:"overview,omitempty"`
	EnrichmentStatus string `json:"enrichmentStatus,omitempty"`
}

type tracksResponse struct {
	Album  albumJSON          `json:"album"`
	Tracks []trackSummaryJSON `json:"tracks"`
}

func toTrackSummary(t store.Title, ws store.WatchState) trackSummaryJSON {
	js := trackSummaryJSON{
		ID:               t.ID,
		Kind:             t.Kind,
		Title:            displayTitle(t),
		DiscNumber:       t.DiscNumber,
		TrackNumber:      t.TrackNumber,
		NeedsReview:      t.NeedsReview,
		ResumePositionMs: ws.ResumePositionMs,
		Watched:          ws.Watched,
		Overview:         t.Overview,
	}
	if t.EnrichmentStatus != "" && t.EnrichmentStatus != "pending" {
		js.EnrichmentStatus = t.EnrichmentStatus
	}
	return js
}

// trackContextJSON is the parent chain attached to a Track's GET /titles/{id} so
// a client can show "Radiohead · OK Computer · 02" without extra round-trips.
// Omitted entirely for a Movie/Episode.
type trackContextJSON struct {
	ArtistID    string `json:"artistId"`
	ArtistName  string `json:"artistName"`
	AlbumID     string `json:"albumId"`
	AlbumTitle  string `json:"albumTitle"`
	AlbumYear   int    `json:"albumYear,omitempty"`
	DiscNumber  int    `json:"discNumber,omitempty"`
	TrackNumber int    `json:"trackNumber,omitempty"`
}

func toTrackContext(c store.TrackContext) *trackContextJSON {
	return &trackContextJSON{
		ArtistID:    c.ArtistID,
		ArtistName:  c.ArtistName,
		AlbumID:     c.AlbumID,
		AlbumTitle:  c.AlbumTitle,
		AlbumYear:   c.AlbumYear,
		DiscNumber:  c.DiscNumber,
		TrackNumber: c.TrackNumber,
	}
}

// --- Handlers ---------------------------------------------------------------

// handleListArtists returns a cursor-paginated page of a Music Library's
// Artists. It is the Music branch of GET /libraries/{id}/titles (the handler
// picks this when the Library's kind is "music"). Unknown/inaccessible Library
// → 404.
func handleListArtists(svc *catalog.Service, libraryID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		page, err := svc.ListArtists(scope, catalog.ListInput{
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
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list artists", nil)
			return
		}
		// Advertise each Artist's fetched image so the grid shows it (not just
		// initials). The list stays lean — only the artwork URL is overlaid, not
		// the enrichment bio/genres the detail carries — so we look up artwork
		// roles + cache-bust versions in bulk and skip EntityEnrichment entirely.
		artistIDs := make([]string, 0, len(page.Artists))
		for _, a := range page.Artists {
			artistIDs = append(artistIDs, a.ID)
		}
		roles, _ := svc.EntityArtworkRoles(store.EntityArtist, artistIDs)
		versions, _ := svc.EntityArtworkVersions(store.EntityArtist, artistIDs)

		out := artistsResponse{
			Artists:    make([]artistSummaryJSON, 0, len(page.Artists)),
			NextCursor: page.NextCursor,
		}
		for _, a := range page.Artists {
			js := toArtistSummary(a)
			if roles[a.ID]["poster"] {
				js.ArtworkURL = artistArtworkURL(a.ID, "poster", versions[a.ID])
			}
			out.Artists = append(out.Artists, js)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleArtistSubtree dispatches routes under "/artists/{id}...": the albums
// listing (bearer-only) and the Artist artwork GET (cookie-capable, so a browser
// <img> can load a fetched artist image). Mirrors handleAlbumSubtree.
func handleArtistSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/artists/")
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			id := rest[:i]
			role := rest[i+len("/artwork/"):]
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleEntityArtwork(deps.Catalog, store.EntityArtist, id, role))))(w, r)
			return
		}
		// POST {id}/scan: Targeted scan of this Artist's album folders (Admin, ADR-0030).
		if id, ok := strings.CutSuffix(rest, "/scan"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleTargetedScan(deps, "artist", id))))(w, r)
			return
		}
		// Edit-item on an Artist (item-editing/02), Admin-only, before the albums listing.
		if dispatchEntityEditRoutes(w, r, deps, store.EntityArtist, rest) {
			return
		}
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleArtistAlbums(deps.Catalog))))(w, r)
	}
}

// handleArtistAlbums serves GET /artists/{id}/albums. Unknown/inaccessible
// Artist → 404 (hide existence). The path must be exactly /artists/{id}/albums.
func handleArtistAlbums(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		id := pathParam(r.URL.Path, "/artists/", "/albums")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		artist, albums, err := svc.Albums(scope, id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "artist not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list albums", nil)
			return
		}
		artistJS := toArtistSummary(artist)
		if e, err := svc.EntityEnrichment(store.EntityArtist, artist.ID); err == nil {
			roles, _ := svc.EntityArtworkRoles(store.EntityArtist, []string{artist.ID})
			versions, _ := svc.EntityArtworkVersions(store.EntityArtist, []string{artist.ID})
			decorateArtist(&artistJS, e, roles[artist.ID], versions[artist.ID])
			artistJS.EnrichmentOverride = entityOverride(e)
		}
		artistJS.LockedFields, _ = svc.EntityLockedFields(store.EntityArtist, artist.ID)

		// Album enrichment (genres + fetched-cover flag) + cover versions in bulk.
		albumIDs := make([]string, 0, len(albums))
		for _, a := range albums {
			albumIDs = append(albumIDs, a.ID)
		}
		albumEnr, _ := svc.EntityEnrichmentForMany(store.EntityAlbum, albumIDs)
		albumRoles, _ := svc.EntityArtworkRoles(store.EntityAlbum, albumIDs)
		albumVersions, _ := svc.EntityArtworkVersions(store.EntityAlbum, albumIDs)

		out := albumsResponse{
			Artist: artistJS,
			Albums: make([]albumJSON, 0, len(albums)),
		}
		for _, a := range albums {
			js := toAlbumJSON(a)
			decorateAlbum(&js, albumEnr[a.ID], albumRoles[a.ID], albumVersions[a.ID])
			out.Albums = append(out.Albums, js)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleAlbumTracks serves GET /albums/{id}/tracks (disc/track order), decorating
// each Track with the calling User's watch state. Unknown/inaccessible Album →
// 404. It also dispatches the album artwork sub-resource
// (/albums/{id}/artwork).
func handleAlbumTracks(svc *catalog.Service) http.HandlerFunc {
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
		id := pathParam(r.URL.Path, "/albums/", "/tracks")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		album, artist, tracks, err := svc.Tracks(scope, id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "album not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list tracks", nil)
			return
		}
		ids := make([]string, 0, len(tracks))
		for _, t := range tracks {
			ids = append(ids, t.ID)
		}
		states, err := svc.WatchStatesForTitles(ident.User.ID, ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list tracks", nil)
			return
		}
		albumJS := toAlbumJSON(album)
		albumJS.ArtistName = artist.Name
		if e, err := svc.EntityEnrichment(store.EntityAlbum, album.ID); err == nil {
			roles, _ := svc.EntityArtworkRoles(store.EntityAlbum, []string{album.ID})
			versions, _ := svc.EntityArtworkVersions(store.EntityAlbum, []string{album.ID})
			decorateAlbum(&albumJS, e, roles[album.ID], versions[album.ID])
			albumJS.EnrichmentOverride = entityOverride(e)
		}
		albumJS.LockedFields, _ = svc.EntityLockedFields(store.EntityAlbum, album.ID)
		// Per-Track durations decorate the list so each row can show its length; a
		// lookup failure degrades to no durations rather than failing the listing.
		durations, _ := svc.TrackDurations(album.ID)
		out := tracksResponse{
			Album:  albumJS,
			Tracks: make([]trackSummaryJSON, 0, len(tracks)),
		}
		for _, t := range tracks {
			ts := toTrackSummary(t, states[t.ID])
			ts.DurationMs = durations[t.ID]
			out.Tracks = append(out.Tracks, ts)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleAlbumSubtree dispatches every route under "/albums/{id}...". It applies
// auth PER LEAF (like the /titles/ dispatcher): the tracks listing is bearer-only
// (requireAuth), while the artwork GET must also accept the media cookie (a
// browser <img> cannot send an Authorization header), so it runs through
// requireAuthAllowCookie. Routing by sub-resource keeps the two leaves distinct.
func handleAlbumSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/albums/")
		// GET {id}/artwork: the local cover media GET (cookie-capable). Only GET is the
		// cover — a PUT {id}/artwork is the Fix-label image pick, handled by
		// dispatchEntityEditRoutes below, so guard on the method to avoid shadowing it.
		if id, ok := strings.CutSuffix(rest, "/artwork"); ok && r.Method == http.MethodGet {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleAlbumArtwork(deps.Catalog, id))))(w, r)
			return
		}
		// POST {id}/scan: Targeted scan of this Album's folder(s) (Admin, ADR-0030).
		if id, ok := strings.CutSuffix(rest, "/scan"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleTargetedScan(deps, "album", id))))(w, r)
			return
		}
		// Edit-item on an Album (item-editing/02), Admin-only, before the tracks listing.
		if dispatchEntityEditRoutes(w, r, deps, store.EntityAlbum, rest) {
			return
		}
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleAlbumTracks(deps.Catalog))))(w, r)
	}
}

// handleAlbumArtwork serves an Album's local cover image bytes (cover.jpg /
// folder.jpg). Local-on-disk wins; no external fetch (ADR-0001). An Album with no
// local cover → 404 (the client falls back to a placeholder; embedded cover art
// extraction is a later concern).
func handleAlbumArtwork(svc *catalog.Service, albumID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if albumID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		art, err := svc.AlbumArtwork(scope, albumID)
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
