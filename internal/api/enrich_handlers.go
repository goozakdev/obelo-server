package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes for the Enrichment surface (docs/api-contract.md): camelCase. The
// pass result is a small summary the Admin sees after triggering enrichment.

// enrichRequest is the optional JSON body of POST /libraries/{id}/enrich. mode
// "full" re-enriches every visible Title (unlocked-only); absent/"new" enriches
// only Titles never successfully enriched.
type enrichRequest struct {
	Mode string `json:"mode"`
}

type enrichResultJSON struct {
	LibraryID string `json:"libraryId"`
	Total     int    `json:"total"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Disabled  int    `json:"disabled"`
}

// handleEnrich triggers an Enrichment pass over a Library (Admin). By default it
// is the only-new mode (Titles with status 'pending'); pass {"mode":"full"} or
// ?mode=full for a full refresh. The pass runs to completion and the response is
// its summary. An unknown Library is 404 (api-contract.md hide-existence). When
// enrichment is unconfigured the pass is a no-op that reports the candidates as
// 'disabled' (ADR-0001) — it still returns 200 with the disabled count.
//
// While the pass runs it publishes enrichProgress events over the SSE Broker
// (ADR-0016) so a connected client can show an "enriching" indicator and live-
// update its grid, plus a terminal Complete event when the pass finishes. broker
// may be nil (events simply aren't published).
func handleEnrich(svc *enrich.Service, broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrich")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		mode := enrich.ModeNew
		if strings.EqualFold(r.URL.Query().Get("mode"), "full") {
			mode = enrich.ModeFull
		} else if r.ContentLength > 0 {
			var req enrichRequest
			// Best-effort: a malformed body just leaves the default mode.
			if json.NewDecoder(r.Body).Decode(&req) == nil && strings.EqualFold(req.Mode, "full") {
				mode = enrich.ModeFull
			}
		}

		var onProgress func(enrich.Progress)
		if broker != nil {
			onProgress = func(p enrich.Progress) { broker.PublishEnrichProgress(toEnrichEvent(p, false)) }
		}
		res, err := svc.EnrichLibraryProgress(r.Context(), id, mode, onProgress)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "enrichment failed", nil)
			return
		}
		if broker != nil {
			// Terminal event: clients hide the indicator + do a final refetch.
			broker.PublishEnrichProgress(events.EnrichProgress{
				LibraryID: id, Total: res.Total, Done: res.Total,
				Matched: res.Matched, Unmatched: res.Unmatched,
				Failed: res.Failed, Disabled: res.Disabled, Complete: true,
			})
			// An enrichment pass changes a Library's metadata/artwork, so it is
			// also a content-change point: nudge clients to refetch (library-scoped).
			broker.PublishLibraryUpdated(id)
		}
		writeJSON(w, http.StatusOK, enrichResultJSON{
			LibraryID: id,
			Total:     res.Total,
			Matched:   res.Matched,
			Unmatched: res.Unmatched,
			Failed:    res.Failed,
			Disabled:  res.Disabled,
		})
	}
}

// --- Hand-editing + Locked fields (PUT /metadata, DELETE /metadata/locks) ----

// metadataCreditJSON is one cast/crew member in a hand-edit body.
type metadataCreditJSON struct {
	Person    string `json:"person"`
	Role      string `json:"role"`
	Character string `json:"character"`
	Kind      string `json:"kind"`
}

// metadataEditRequest is the body of PUT /titles/{id}/metadata. Every field is a
// pointer (or slice) so a present field is written-and-Locked while an absent one
// is left untouched — a client edits exactly the fields it sends. The field names
// mirror the Title detail's enrichment fields. `title` edits the canonical DISPLAY
// title only (never the parsed identity Title). lockArtwork pins artwork roles
// ('poster' / 'background') so a refresh can't replace the chosen image.
type metadataEditRequest struct {
	Overview       *string               `json:"overview"`
	Tagline        *string               `json:"tagline"`
	Title          *string               `json:"title"`
	ContentRating  *string               `json:"contentRating"`
	ReleaseDate    *string               `json:"releaseDate"`
	RuntimeMinutes *int                  `json:"runtimeMinutes"`
	Studio         *string               `json:"studio"`
	Genres         *[]string             `json:"genres"`
	Cast           *[]metadataCreditJSON `json:"cast"`
	LockArtwork    []string              `json:"lockArtwork"`
}

// handleEditMetadata applies an Admin hand-edit to a Title's descriptive fields
// and Locks each edited field (CONTEXT.md "Locked field"), so re-enrichment never
// overwrites it while still refreshing unlocked fields. Returns the updated Title
// detail (which now lists the edits in lockedFields[]). Unknown Title → 404 (hide
// existence). Identity / watch state are never touched (ADR-0002). Admin-only.
func handleEditMetadata(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		titleID := pathParam(r.URL.Path, "/titles/", "/metadata")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req metadataEditRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		edit := store.MetadataEdit{
			Overview:       req.Overview,
			Tagline:        req.Tagline,
			ContentRating:  req.ContentRating,
			ReleaseDate:    req.ReleaseDate,
			RuntimeMinutes: req.RuntimeMinutes,
			Studio:         req.Studio,
			Name:           req.Title,
			Genres:         req.Genres,
			LockArtwork:    req.LockArtwork,
		}
		if req.Cast != nil {
			cast := make([]store.Credit, 0, len(*req.Cast))
			for _, c := range *req.Cast {
				cast = append(cast, store.Credit{
					Person: c.Person, Role: c.Role, Character: c.Character, Kind: c.Kind,
				})
			}
			edit.Cast = &cast
		}

		d, err := svc.EditMetadata(titleID, edit)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to edit metadata", nil)
			return
		}
		ws, err := svc.WatchStateFor(ident.User.ID, d.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to edit metadata", nil)
			return
		}
		writeJSON(w, http.StatusOK, toTitleDetail(d, ws))
	}
}

// handleReleaseLock releases a Title's Lock on one field (DELETE
// /titles/{id}/metadata/locks/{field}) so the next enrich pass refreshes it again
// (CONTEXT.md "a lock is releasable back to auto"). Releasing an absent lock is a
// no-op. Returns the updated Title detail. Unknown Title → 404. Admin-only.
func handleReleaseLock(svc *catalog.Service, titleID, field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if titleID == "" || field == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		d, err := svc.ReleaseLock(titleID, field)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to release lock", nil)
			return
		}
		ws, err := svc.WatchStateFor(ident.User.ID, d.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to release lock", nil)
			return
		}
		writeJSON(w, http.StatusOK, toTitleDetail(d, ws))
	}
}

// --- Enrichment-match correction (PUT /titles/{id}/enrichmentMatch) ----------

// enrichmentMatchRequest is the body of PUT /titles/{id}/enrichmentMatch: the
// external id an Admin assigns to correct a wrong or missing metadata match. At
// least one id must be present. Setting it re-points the Enrichment lookup anchor
// and re-enriches the Title — it is deliberately DISTINCT from identity fix-match
// and NEVER touches identity_key / watch state (ADR-0002/0014).
type enrichmentMatchRequest struct {
	TMDBID        string `json:"tmdbId"`
	IMDBID        string `json:"imdbId"`
	MusicbrainzID string `json:"musicbrainzId"`
}

// handleEnrichmentMatch sets the external id used for a Title's Enrichment lookup
// and re-enriches just that Title immediately (PRD stories 22, 25). Watch state
// and identity are preserved (ADR-0014); the descriptive fields/artwork refresh
// (unlocked only). On a successful match the Title's enrichmentStatus becomes
// 'matched' and it leaves the attention surface. Returns the updated Title detail.
// At least one external id is required (400 otherwise); an unknown Title → 404
// (hide existence). Admin-only.
func handleEnrichmentMatch(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		titleID := pathParam(r.URL.Path, "/titles/", "/enrichmentMatch")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req enrichmentMatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		m := store.ExternalMatch{
			TMDBID:        strings.TrimSpace(req.TMDBID),
			IMDBID:        strings.TrimSpace(req.IMDBID),
			MusicbrainzID: strings.TrimSpace(req.MusicbrainzID),
		}
		if m.TMDBID == "" && m.IMDBID == "" && m.MusicbrainzID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"at least one external id (tmdbId, imdbId, or musicbrainzId) is required", nil)
			return
		}

		err := enrichSvc.MatchTitle(r.Context(), titleID, m)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set enrichment match", nil)
			return
		}
		// Shared re-enrich tail: read the updated detail, emit the libraryUpdated SSE
		// nudge, and write the Title detail (identical to enrichmentOverride).
		writeReEnrichedDetail(w, cat, broker, ident.User.ID, titleID)
	}
}

// writeReEnrichedDetail is the shared response tail for the apply-Enrichment-
// override endpoints (PUT /enrichmentMatch and PUT /enrichmentOverride): after a
// successful single-Title re-enrich it reads the updated detail unscoped (an Admin
// is all-access), emits the libraryUpdated SSE nudge (ADR-0016) so browse reflects
// the fix live, and writes the Title detail. Centralizing it keeps the two twin
// endpoints behaving identically (both run the same MatchTitle re-enrich).
func writeReEnrichedDetail(w http.ResponseWriter, cat *catalog.Service, broker *events.Broker, userID, titleID string) {
	d, err := cat.GetTitle(access.AllAccess(), titleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply enrichment override", nil)
		return
	}
	if broker != nil {
		broker.PublishLibraryUpdated(d.LibraryID)
	}
	ws, err := cat.WatchStateFor(userID, d.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply enrichment override", nil)
		return
	}
	writeJSON(w, http.StatusOK, toTitleDetail(d, ws))
}

// --- Edit-item: provider search + apply Enrichment override (ADR-0019) -------

// enrichmentCandidateJSON is one provider search result in the Edit-item picker
// (CONTEXT.md "Enrichment override"): enough for an Admin to disambiguate two
// same-named works before applying — the authoritative externalId to pin, the
// source title + year, a thumbnail, and a disambiguation hint.
type enrichmentCandidateJSON struct {
	ExternalID     string `json:"externalId"`
	Title          string `json:"title"`
	Year           int    `json:"year,omitempty"`
	ThumbnailURL   string `json:"thumbnailUrl,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	Kind           string `json:"kind"`
	// TypeLabel is a short record-type badge ("Album · Soundtrack", "Group") that
	// disambiguates same-titled hits (item-editing/search-improvements). Omitted when
	// the source reports none.
	TypeLabel string `json:"typeLabel,omitempty"`
	// Tracklist is an ALBUM candidate's ordered track preview (disc/position/title),
	// so an Admin can confirm the positional map before applying (ADR-0019). Absent
	// for every non-album kind.
	Tracklist []candidateTrackJSON `json:"tracklist,omitempty"`
}

// candidateTrackJSON is one track in an album candidate's tracklist preview.
type candidateTrackJSON struct {
	Disc     int    `json:"disc,omitempty"`
	Position int    `json:"position"`
	Title    string `json:"title"`
}

type enrichmentCandidatesJSON struct {
	Candidates []enrichmentCandidateJSON `json:"candidates"`
	// HasMore hints that another page likely exists (a full page came back), so the
	// picker can offer "show more" for a broad common-title query (item-editing/
	// search-improvements). False on a short/last page.
	HasMore bool `json:"hasMore,omitempty"`
}

// toCandidateJSON maps a provider Candidate onto the Edit-item picker wire shape,
// shared by the leaf + parent candidate handlers (and the paste-id preview).
func toCandidateJSON(c enrich.Candidate) enrichmentCandidateJSON {
	jc := enrichmentCandidateJSON{
		ExternalID:     c.ExternalID,
		Title:          c.Title,
		Year:           c.Year,
		ThumbnailURL:   c.ThumbnailURL,
		Disambiguation: c.Disambiguation,
		TypeLabel:      c.TypeLabel,
		Kind:           c.Kind,
	}
	for _, tr := range c.Tracklist {
		jc.Tracklist = append(jc.Tracklist, candidateTrackJSON{
			Disc: tr.Disc, Position: tr.Position, Title: tr.Title,
		})
	}
	return jc
}

// searchOptionsFrom reads the optional artist-narrowing + paging query params the
// Edit-item picker sends (item-editing/search-improvements): `artist` AND-narrows a
// music album/track search to a specific artist (pre-filled from the item's parsed
// artist), and `page` (0-based) offsets a broad query by whole pages so "show more"
// works. The page size is SearchCandidateLimit — the same cap the service applies.
func searchOptionsFrom(r *http.Request) enrich.SearchOptions {
	q := r.URL.Query()
	page := 0
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("page"))); err == nil && n > 0 {
		page = n
	}
	return enrich.SearchOptions{
		Artist: strings.TrimSpace(q.Get("artist")),
		Limit:  enrich.SearchCandidateLimit,
		Offset: page * enrich.SearchCandidateLimit,
	}
}

// handleEnrichmentCandidates searches the authoritative metadata provider for the
// records that could decorate a leaf Title, so an Admin can pick the right one and
// apply it as an Enrichment override (GET /titles/{id}/enrichmentCandidates?q=…,
// Admin-only). The searched kind is the Title's own kind (Movie/Episode → TMDB,
// Track → MusicBrainz). A blank query returns an empty list (200). When the
// provider is unconfigured/disabled or unreachable the response is 503
// SEARCH_UNAVAILABLE so the box reports why instead of hanging (results are capped
// server-side). Unknown Title → 404 (hide existence). Reads only — identity and
// watch state are untouched.
func handleEnrichmentCandidates(enrichSvc *enrich.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/enrichmentCandidates")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		// The service owns the lean existence+kind read (no join-heavy detail fetch):
		// an unknown Title is store.ErrNotFound → 404 (hide existence).
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		cands, err := enrichSvc.SearchTitleCandidates(r.Context(), titleID, query, searchOptionsFrom(r))
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case errors.Is(err, enrich.ErrSearchUnavailable):
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider search is unavailable for this item — the provider is unconfigured or disabled", nil)
			return
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider search failed — the source may be unreachable", nil)
			return
		}

		writeJSON(w, http.StatusOK, toCandidatesJSON(cands))
	}
}

// toCandidatesJSON shapes a provider candidate page into the picker wire response,
// setting HasMore when a full page came back (another page likely exists).
func toCandidatesJSON(cands []enrich.Candidate) enrichmentCandidatesJSON {
	out := enrichmentCandidatesJSON{
		Candidates: make([]enrichmentCandidateJSON, 0, len(cands)),
		HasMore:    len(cands) >= enrich.SearchCandidateLimit,
	}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, toCandidateJSON(c))
	}
	return out
}

// externalPreviewRequest maps a preview/apply error onto the response. Shared by the
// leaf + parent paste-id preview handlers so both surface the same statuses: an
// unreadable paste → 400, a wrong-kind URL → 400, a disabled provider → 503, and a
// stale/unknown id → 404 ("not found", never a hang or 500).
func writeExternalPreview(w http.ResponseWriter, c enrich.Candidate, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "item not found", nil)
	case errors.Is(err, enrich.ErrExternalRefInvalid):
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"that doesn't look like a MusicBrainz/TMDB id or URL", nil)
	case errors.Is(err, enrich.ErrExternalRefKindMismatch):
		writeError(w, http.StatusBadRequest, codeBadRequest, kindMismatchMessage(err), nil)
	case errors.Is(err, enrich.ErrExternalRefUnsupportedKind):
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"that MusicBrainz link is the wrong kind of record — paste a release-group (album), artist, or recording (track) id or URL", nil)
	case errors.Is(err, enrich.ErrSearchUnavailable):
		writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
			"metadata provider lookup is unavailable for this item — the provider is unconfigured or disabled", nil)
	case errors.Is(err, enrich.ErrNoMatch):
		writeError(w, http.StatusNotFound, codeNotFound,
			"no record found for that id — it may be wrong, stale, or merged away", nil)
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
			"metadata provider lookup failed — the source may be unreachable", nil)
	default:
		writeJSON(w, http.StatusOK, toCandidateJSON(c))
	}
}

// kindMismatchMessage turns a wrong-kind paste into an actionable message naming what
// the pasted link points to versus what the item needs, so the Admin knows which link
// to grab (e.g. a release-group link pasted on an Artist). Falls back to a generic
// message if the error doesn't carry the kinds.
func kindMismatchMessage(err error) string {
	var m *enrich.ExternalRefKindMismatchError
	if !errors.As(err, &m) {
		return "that id is for a different kind of record than this item"
	}
	return fmt.Sprintf("that looks like %s, but this item is %s — paste %s instead",
		pastedRefNoun[m.Got], itemKindNoun[m.Want], wantedRefInstruction[m.Want])
}

// Friendly labels for the wrong-kind paste message, keyed by item kind. Music kinds
// name the MusicBrainz entity a link points to (an album is a release-group, a track a
// recording); video kinds name the TMDB record.
var (
	pastedRefNoun = map[string]string{
		"album":  "a MusicBrainz album (release-group) link",
		"artist": "a MusicBrainz artist link",
		"track":  "a MusicBrainz track (recording) link",
		"movie":  "a TMDB movie link",
		"show":   "a TMDB TV-show link",
	}
	itemKindNoun = map[string]string{
		"album":   "an album",
		"artist":  "an artist",
		"track":   "a track",
		"movie":   "a movie",
		"show":    "a TV show",
		"season":  "a season",
		"episode": "an episode",
	}
	wantedRefInstruction = map[string]string{
		"album":   "a release-group (album) id or URL",
		"artist":  "an artist id or URL",
		"track":   "a recording (track) id or URL",
		"movie":   "a TMDB movie id or URL",
		"show":    "a TMDB TV-show id or URL",
		"season":  "a TMDB TV-show id or URL",
		"episode": "a TMDB TV-show id or URL",
	}
)

// handleTitleExternalPreview resolves a pasted MusicBrainz/TMDB id-or-URL to a single
// preview candidate for a leaf Title WITHOUT searching (GET
// /titles/{id}/externalPreview?ref=…, Admin-only) — the "paste an id when search isn't
// enough" escape hatch (item-editing/search-improvements). The Admin sees the record's
// title/year before applying it via the existing enrichmentOverride endpoint, so a
// typo'd or stale id previews as 404 rather than being pinned blind. Reads only.
func handleTitleExternalPreview(enrichSvc *enrich.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/externalPreview")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		c, err := enrichSvc.PreviewTitleExternal(r.Context(), titleID, ref)
		writeExternalPreview(w, c, err)
	}
}

// enrichmentOverrideRequest is the body of PUT
// /titles|shows|artists|albums/{id}/enrichmentOverride: the authoritative externalId
// of the candidate the Admin picked. The server maps it to the right id column for
// the item's kind (Movie/Episode/Show → TMDB, Track/Artist/Album → MusicBrainz), so
// the client passes only what the picker returned. Cascade requests the "also apply
// to children" cascade (item-editing/05): honored only on a parent that HAS children
// (Album→tracks, Artist→albums→tracks, Show→episodes) — ignored on a childless leaf.
type enrichmentOverrideRequest struct {
	ExternalID string `json:"externalId"`
	Cascade    bool   `json:"cascade"`
}

// handleEnrichmentOverride applies a picked candidate as a durable Enrichment
// override on a leaf Title and re-enriches just that Title (PUT
// /titles/{id}/enrichmentOverride, Admin-only). It pins the authoritative external
// id (persisted, so future passes look up BY the pinned id rather than re-searching)
// and refreshes the unlocked descriptive fields/artwork from that record — reusing
// the MatchTitle primitive. Identity_key and every User's watch state are NEVER
// touched (ADR-0002/0014); Locked fields are honored. On success it emits a
// libraryUpdated SSE nudge (ADR-0016) so browse reflects the fix live, and returns
// the updated Title detail. Missing externalId → 400; unknown Title → 404.
func handleEnrichmentOverride(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		titleID := pathParam(r.URL.Path, "/titles/", "/enrichmentOverride")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req enrichmentOverrideRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		externalID := strings.TrimSpace(req.ExternalID)
		if externalID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "externalId is required", nil)
			return
		}
		// req.Cascade is intentionally ignored here: a leaf (Movie/Episode/Track) has no
		// children, so there is nothing to cascade to (item-editing/05). The checkbox is
		// shown only on parents (Album/Show/Artist), whose entity endpoint runs it.

		// The service derives the id column from the Title's own kind (lean read) and
		// re-enriches; an unknown Title is store.ErrNotFound → 404 (hide existence).
		err := enrichSvc.ApplyOverride(r.Context(), titleID, externalID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply enrichment override", nil)
			return
		}
		// Shared re-enrich tail: read the updated detail, emit the libraryUpdated SSE
		// nudge, and write the Title detail (identical to enrichmentMatch).
		writeReEnrichedDetail(w, cat, broker, ident.User.ID, titleID)
	}
}

// --- Edit-item: image picker (list candidates + pick + role lock) -----------

// artworkCandidateJSON is one image the provider offers for a role in the Edit-item
// image picker (Fix label, ADR-0019): the URL to preview + pick, and the source's
// dimensions (0 when unreported) so the picker can hint resolution.
type artworkCandidateJSON struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Source string `json:"source,omitempty"`
}

type artworkCandidatesJSON struct {
	Role       string                 `json:"role"`
	Candidates []artworkCandidateJSON `json:"candidates"`
}

// pickArtworkRequest is the body of PUT /titles|shows|artists|albums/{id}/artwork:
// the role to set and the URL of the provider image the Admin picked (one the
// list-candidates endpoint returned).
type pickArtworkRequest struct {
	Role string `json:"role"`
	URL  string `json:"url"`
}

// validArtworkRole reports whether role is a pickable artwork role. Leaves + video
// parents carry poster/background/logo; an Album carries a cover. The set is closed
// so a pick can't lock an arbitrary field.
func validArtworkRole(role string) bool {
	switch role {
	case "poster", "background", "cover", "logo":
		return true
	default:
		return false
	}
}

func toArtworkCandidatesJSON(role string, cands []enrich.ArtworkCandidate) artworkCandidatesJSON {
	out := artworkCandidatesJSON{Role: role, Candidates: make([]artworkCandidateJSON, 0, len(cands))}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, artworkCandidateJSON{
			URL: c.URL, Width: c.Width, Height: c.Height, Source: c.Source,
		})
	}
	return out
}

// handleTitleArtworkCandidates lists the provider images offered for a leaf Title's
// role, so an Admin can pick a specific poster/background (GET
// /titles/{id}/artworkCandidates?role=…, Admin-only). Same SEARCH_UNAVAILABLE (503)
// semantics as the record search when the provider is unconfigured/unreachable.
// Unknown Title → 404. A missing/invalid role → 400. Reads only.
func handleTitleArtworkCandidates(enrichSvc *enrich.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/artworkCandidates")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		role := strings.TrimSpace(r.URL.Query().Get("role"))
		if !validArtworkRole(role) {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid role (poster, background, cover, logo) is required", nil)
			return
		}
		cands, err := enrichSvc.ListTitleArtworkCandidates(r.Context(), titleID, role)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case errors.Is(err, enrich.ErrSearchUnavailable):
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider images are unavailable for this item — the provider is unconfigured or disabled", nil)
			return
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider image lookup failed — the source may be unreachable", nil)
			return
		}
		writeJSON(w, http.StatusOK, toArtworkCandidatesJSON(role, cands))
	}
}

// handlePickTitleArtwork applies a picked provider image to a leaf Title's role and
// Locks that role (PUT /titles/{id}/artwork, Admin-only). The server downloads +
// caches the image, sets it as the role's image, and Locks the role so a re-enrich
// keeps it; local artwork still wins. Emits a libraryUpdated SSE nudge and returns
// the updated Title detail. Missing role/url → 400; unknown Title → 404.
func handlePickTitleArtwork(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		titleID := pathParam(r.URL.Path, "/titles/", "/artwork")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req pickArtworkRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if !validArtworkRole(req.Role) || strings.TrimSpace(req.URL) == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid role and an image url are required", nil)
			return
		}
		err := enrichSvc.PickTitleArtwork(r.Context(), titleID, req.Role, strings.TrimSpace(req.URL))
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set artwork", nil)
			return
		}
		writeReEnrichedDetail(w, cat, broker, ident.User.ID, titleID)
	}
}

// maxArtworkUploadBytes caps an uploaded image at 16 MiB (ADR-0026; the same
// limit the ArtworkFetcher enforces on a downloaded provider image). uploadSlack
// gives the multipart framing headroom above the file cap so a file of exactly
// the limit still passes; the precise per-file check is enforced after reading.
const (
	maxArtworkUploadBytes = 16 << 20
	uploadSlack           = 1 << 20
)

// allowedImageType reports whether a sniffed content type is an accepted artwork
// upload format — JPEG, PNG, or WebP (ADR-0026). Everything else (SVG, GIF, HEIC,
// animated, PDF) is refused so a format that won't render everywhere never
// becomes catalog art. Detection is by content sniff, not the client's header.
func allowedImageType(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

// readUploadedImage parses the single "image" part of a multipart artwork upload,
// enforcing the 16 MiB cap and the JPEG/PNG/WebP allowlist, and returns the bytes
// plus their sniffed content type. On any rejection it writes the error envelope
// (413 too large, 415 wrong type, 400 malformed) and returns ok=false so the
// handler leaves the current image unchanged (ADR-0026). It never touches state.
func readUploadedImage(w http.ResponseWriter, r *http.Request) (data []byte, contentType string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtworkUploadBytes+uploadSlack)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				"image is too large — the limit is 16 MiB", nil)
			return nil, "", false
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "a multipart image upload is required", nil)
		return nil, "", false
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "an image file part (\"image\") is required", nil)
		return nil, "", false
	}
	defer file.Close()
	data, err = io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the uploaded image could not be read", nil)
		return nil, "", false
	}
	if len(data) > maxArtworkUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			"image is too large — the limit is 16 MiB", nil)
		return nil, "", false
	}
	contentType = http.DetectContentType(data)
	if !allowedImageType(contentType) {
		writeError(w, http.StatusUnsupportedMediaType, codeUnsupportedMedia,
			"unsupported image type — use JPEG, PNG, or WebP", nil)
		return nil, "", false
	}
	return data, contentType, true
}

// handleUploadTitleArtwork stores an Admin-uploaded image as a leaf Title's role
// and Locks that role (POST /titles/{id}/artworkUpload?role=…, Admin-only,
// multipart). Uploading IS selecting (ADR-0026): the bytes go to the artwork cache
// (never the library folder — ADR-0007), fill the role, and Lock it — no separate
// select. An Uploaded image outranks Local + Fetched at serve time. Emits a
// libraryUpdated SSE nudge and returns the updated Title detail. Bad role → 400;
// bad file → 413/415; unknown Title → 404.
func handleUploadTitleArtwork(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		titleID := pathParam(r.URL.Path, "/titles/", "/artworkUpload")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		role := strings.TrimSpace(r.URL.Query().Get("role"))
		if !validArtworkRole(role) {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid role (poster, background, cover, logo) is required", nil)
			return
		}
		data, contentType, ok := readUploadedImage(w, r)
		if !ok {
			return
		}
		err := enrichSvc.UploadTitleArtwork(titleID, role, data, contentType)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to store uploaded artwork", nil)
			return
		}
		writeReEnrichedDetail(w, cat, broker, ident.User.ID, titleID)
	}
}

// toEnrichEvent maps an enrich.Progress snapshot onto the SSE payload shape,
// shared by the manual handler and (via the app worker) the auto/scheduled path.
func toEnrichEvent(p enrich.Progress, complete bool) events.EnrichProgress {
	return events.EnrichProgress{
		LibraryID: p.LibraryID,
		Total:     p.Total,
		Done:      p.Done,
		Matched:   p.Matched,
		Unmatched: p.Unmatched,
		Failed:    p.Failed,
		Disabled:  p.Disabled,
		Complete:  complete,
	}
}
