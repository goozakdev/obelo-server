package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/events"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Wire shapes for the Enrichment surface (docs/api-contract.md): camelCase. The
// pass result is a small summary the Admin sees after triggering enrichment.

// enrichRequest is the optional JSON body of POST /libraries/{id}/enrich. mode
// "full" re-enriches every visible Title (unlocked-only); "recheck" adds the
// settled non-answers to the only-new population (ADR-0051); absent/"new"
// enriches only Titles never successfully enriched.
type enrichRequest struct {
	Mode string `json:"mode"`
}

// enrichMode maps a wire mode string onto an enrich.Mode. An UNRECOGNIZED value
// (including "" and "new") falls back to the default only-new pass rather than
// 400-ing: this handler has always been best-effort about the mode — a malformed
// body leaves the default too — and an older client naming a mode this build does
// not have should get the safe, cheap pass, not an error.
func enrichMode(s string) enrich.Mode {
	switch {
	case strings.EqualFold(s, "full"):
		return enrich.ModeFull
	case strings.EqualFold(s, "recheck"):
		return enrich.ModeRecheck
	}
	return enrich.ModeNew
}

type enrichResultJSON struct {
	LibraryID string `json:"libraryId"`
	Total     int    `json:"total"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Disabled  int    `json:"disabled"`
	// Retrying is the count of leaves left scheduled for another attempt after a
	// transient provider failure (ADR-0048) — not parked, and not the Admin's
	// problem unless the streak escalates.
	Retrying int `json:"retrying"`
	// Mode / FinishedAt describe WHICH pass this summary came from, and are set only
	// where the summary is reported detached from the request that caused it — i.e.
	// as the `lastPass` of a status read.
	Mode       string `json:"mode,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// enrichProgressJSON is how far a running pass has got: the same done/total and
// running counts the enrichProgress SSE stream carries (ADR-0016), so a client
// that missed the ticks — a page reloaded mid-pass — can catch up with one read.
type enrichProgressJSON struct {
	Total     int `json:"total"`
	Done      int `json:"done"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
	Failed    int `json:"failed"`
	Disabled  int `json:"disabled"`
	Retrying  int `json:"retrying"`
}

// Pass states on the wire. A Library is "running" while a pass is queued or
// executing for it and "idle" otherwise; there is no third value, because a pass
// that ended is described by lastPass rather than by a state of its own.
const (
	enrichStateIdle    = "idle"
	enrichStateRunning = "running"
)

// enrichPassJSON is the body of BOTH POST (202) and GET /libraries/{id}/enrich —
// one shape, so "what did my press do?" and "what is happening now?" are answered
// in the same words, exactly as the scan surface answers both with a scan status.
type enrichPassJSON struct {
	LibraryID string `json:"libraryId"`
	State     string `json:"state"`
	// Mode / StartedAt describe the pass in flight; absent when idle.
	Mode      string `json:"mode,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	// Started is true on the POST that actually started this pass, and false when
	// the reply is reporting one that was ALREADY running — the difference between
	// "off it goes" and "you already asked". Omitted from a status read.
	Started  bool                `json:"started,omitempty"`
	Progress *enrichProgressJSON `json:"progress,omitempty"`
	// LastPass is the most recent FINISHED pass over this Library, or absent if none
	// has finished since the server started. In-memory: see enrich.PassStatus.
	LastPass *enrichResultJSON `json:"lastPass,omitempty"`
}

// toEnrichPassJSON projects the in-memory pass status onto the wire shape.
func toEnrichPassJSON(libraryID string, st enrich.PassStatus) enrichPassJSON {
	out := enrichPassJSON{LibraryID: libraryID, State: enrichStateIdle}
	if st.Running {
		out.State = enrichStateRunning
		out.Mode = st.Mode.String()
		out.StartedAt = formatInstant(st.StartedAt)
		out.Progress = &enrichProgressJSON{
			Total: st.Progress.Total, Done: st.Progress.Done,
			Matched: st.Progress.Matched, Unmatched: st.Progress.Unmatched,
			Failed: st.Progress.Failed, Disabled: st.Progress.Disabled,
			Retrying: st.Progress.Retrying,
		}
	}
	if st.Last != nil {
		out.LastPass = &enrichResultJSON{
			LibraryID: libraryID,
			Total:     st.Last.Total, Matched: st.Last.Matched,
			Unmatched: st.Last.Unmatched, Failed: st.Last.Failed,
			Disabled: st.Last.Disabled, Retrying: st.Last.Retrying,
			Mode: st.LastMode.String(), FinishedAt: formatInstant(st.LastFinishedAt),
		}
	}
	return out
}

// enrichPassStatusOf reads a Library's pass status through the wired reader,
// answering "idle, nothing has finished" when none is wired (a narrow unit test).
func enrichPassStatusOf(deps Deps, libraryID string) enrich.PassStatus {
	if deps.EnrichStatus == nil {
		return enrich.PassStatus{}
	}
	return deps.EnrichStatus(libraryID)
}

// libraryMustExist validates a Library id for the enrich surface, writing the 404
// itself when it is absent or unknown (api-contract.md hide-existence). It runs
// BEFORE anything is queued, so an unknown Library is a 404 rather than a 202 for
// a pass that will fail somewhere else five seconds later — the same ordering
// handleScan gets from StartScan validating first.
func libraryMustExist(w http.ResponseWriter, deps Deps, libraryID string) bool {
	if libraryID == "" {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		return false
	}
	if deps.Libraries == nil {
		return true // narrow unit test: nothing to validate against
	}
	ok, err := deps.Libraries.LibraryExists(libraryID)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read library", nil)
		return false
	case !ok:
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return false
	}
	return true
}

// handleEnrich STARTS an Enrichment pass over a Library (Admin) and returns 202
// Accepted immediately. By default it is the only-new mode (Titles with status
// 'pending'); pass {"mode":"full"} or ?mode=full for a full refresh, or
// {"mode":"recheck"} / ?mode=recheck to re-ask the settled non-answers as well
// (ADR-0051) — the mode the Needs Fixing screen's "Re-check unmatched items"
// button uses after a matching improvement ships.
//
// It used to run the pass INSIDE the request, and the doc comment here said so.
// That was harmless while the only caller was a human with curl and immediately
// fatal once a browser pointed at it: a recheck of a real library is ~15 minutes
// of provider calls, the fetch hung, the operator reloaded the page, and the
// reload cancelled r.Context() and with it the pass. All 724 flagged rows still
// carried a blank enrichment_reason afterwards, which is how we know not one leaf
// had been processed. So: **a pass is started, not awaited** (ADR-0051's
// amendment). The pass runs on the application's background worker — the same one
// the auto-after-scan trigger and the policy-change re-enrich use, serialized by
// the same per-Library lock — so cancelling this request cannot cancel it, and
// progress arrives on the enrichProgress SSE stream (ADR-0016) that already
// existed. This is exactly handleScan's shape, which enrichment could have had
// all along; only the endpoint was holding the request open.
//
// The reply names the mode it started and whether THIS request started it. Three
// answers that used to be silence are now said out loud:
//   - no worker is running (nothing would ever pick the pass up) → 503;
//   - the queue is full → 503, rather than a dropped request and a log line;
//   - a pass is already running for this Library → 202 with started=false and the
//     in-flight status, rather than a duplicate queued behind the same lock.
//
// An unknown Library is 404 (api-contract.md hide-existence), validated before
// anything is queued. When enrichment is unconfigured the pass still runs and
// marks its candidates 'disabled' (ADR-0001) — that is a finished pass, not a
// refusal.
func handleEnrich(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrich")
		if !libraryMustExist(w, deps, id) {
			return
		}

		// The query string wins when it names a mode this build knows; otherwise the
		// body is consulted. Both spellings select the same three modes.
		mode := enrichMode(r.URL.Query().Get("mode"))
		if mode == enrich.ModeNew && r.ContentLength > 0 {
			var req enrichRequest
			// Best-effort: a malformed body just leaves the default mode.
			if json.NewDecoder(r.Body).Decode(&req) == nil {
				mode = enrichMode(req.Mode)
			}
		}

		if deps.EnrichStart == nil {
			writeError(w, http.StatusServiceUnavailable, codeEnrichUnavailable,
				"this server is not running background enrichment passes, so there is nothing to start", nil)
			return
		}
		// done is deliberately nil here: nothing about the HTTP reply waits on the
		// pass. The callback exists for callers that want to await one (tests), which
		// is the same reason StartScan takes one.
		err := deps.EnrichStart(id, mode, nil)
		switch {
		case errors.Is(err, enrich.ErrPassInProgress):
			// Idempotent, exactly like a second POST /scan: report the pass that IS
			// running rather than queueing a second one behind the same lock.
			out := toEnrichPassJSON(id, enrichPassStatusOf(deps, id))
			writeJSON(w, http.StatusAccepted, out)
			return
		case errors.Is(err, enrich.ErrPassWorkerUnavailable):
			writeError(w, http.StatusServiceUnavailable, codeEnrichUnavailable,
				"this server is not running background enrichment passes, so there is nothing to start", nil)
			return
		case errors.Is(err, enrich.ErrPassQueueFull):
			writeError(w, http.StatusServiceUnavailable, codeEnrichBusy,
				"too many enrichment passes are already queued — try again in a moment", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to start the enrichment pass", nil)
			return
		}

		// Report the pass we just started. Reading the status back (rather than
		// synthesizing a reply) means the 202 and the pollable GET can never describe
		// the same pass differently — and by the time this runs the worker may
		// already be publishing progress, which is fine: both are the truth.
		out := toEnrichPassJSON(id, enrichPassStatusOf(deps, id))
		out.State = enrichStateRunning
		if out.Mode == "" {
			out.Mode = mode.String()
		}
		out.Started = true
		writeJSON(w, http.StatusAccepted, out)
	}
}

// handleEnrichStatus answers whether an Enrichment pass is running over a Library
// — its mode, how far it has got, and the summary of the most recent finished one
// (GET /libraries/{id}/enrich, Admin-only; unknown Library → 404).
//
// It is what lets a RELOADED page rejoin a pass instead of showing an idle button,
// which is the failure ADR-0051's amendment was written from: the operator saw
// nothing happen, reloaded, and killed their own pass. Progress still streams over
// SSE; this is the "I just arrived, what did I miss?" read, and the SSE stream is
// how the answer stays current afterwards.
//
// The status is held IN MEMORY (see enrich.PassStatus), so a Library reads idle
// with no lastPass after a restart. That is the honest answer: a status that
// survived the process would be claiming a pass had.
func handleEnrichStatus(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrich")
		if !libraryMustExist(w, deps, id) {
			return
		}
		writeJSON(w, http.StatusOK, toEnrichPassJSON(id, enrichPassStatusOf(deps, id)))
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
	// ReleaseID is the exact EDITION a pasted MusicBrainz /release/ URL named
	// (ADR-0052). The preview resolves that release to its parent release-group —
	// externalId, which is what an album IS — and returns the release here so the
	// apply can keep both. The client sends it straight back as the override's
	// releaseId; absent means the Admin named no edition. Album previews only.
	ReleaseID string `json:"releaseId,omitempty"`
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
//
// The thumbnail is rewritten to a same-origin /providerImage reference: the wire
// field is the URL the picker puts in an <img src>, and pointing that at TMDB or
// the Cover Art Archive is what handed those hosts the admin's IP (ADR-0001, see
// provider_image.go). Doing it HERE rather than in each provider means the enrich
// package keeps emitting honest provider URLs — which is what the pick/apply path
// still needs — and no emit site can be forgotten.
func toCandidateJSON(p *providerImageProxy, c enrich.Candidate) enrichmentCandidateJSON {
	jc := enrichmentCandidateJSON{
		ExternalID:     c.ExternalID,
		Title:          c.Title,
		Year:           c.Year,
		ThumbnailURL:   p.proxyURL(c.ThumbnailURL),
		Disambiguation: c.Disambiguation,
		TypeLabel:      c.TypeLabel,
		Kind:           c.Kind,
		ReleaseID:      c.ReleaseID,
	}
	for _, tr := range c.Tracklist {
		jc.Tracklist = append(jc.Tracklist, candidateTrackJSON{
			Disc: tr.Disc, Position: tr.Position, Title: tr.Title,
		})
	}
	return jc
}

// searchOptionsFrom reads the optional narrowing + paging query params the pickers
// send (item-editing/search-improvements, needs-fixing/06): `artist` AND-narrows a
// music album/track search to a specific artist (pre-filled from the item's parsed
// artist), `release` AND-narrows a TRACK search to the album the recording sits on,
// and `page` (0-based) offsets a broad query by whole pages so "show more" works.
// Each is blank when absent, which is exactly "no narrowing", so a video search —
// which sends neither — is unchanged. The page size is SearchCandidateLimit — the
// same cap the service applies.
func searchOptionsFrom(r *http.Request) enrich.SearchOptions {
	q := r.URL.Query()
	page := 0
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("page"))); err == nil && n > 0 {
		page = n
	}
	return enrich.SearchOptions{
		Artist:  strings.TrimSpace(q.Get("artist")),
		Release: strings.TrimSpace(q.Get("release")),
		Limit:   enrich.SearchCandidateLimit,
		Offset:  page * enrich.SearchCandidateLimit,
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
func handleEnrichmentCandidates(enrichSvc *enrich.Service, images *providerImageProxy) http.HandlerFunc {
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

		writeJSON(w, http.StatusOK, toCandidatesJSON(images, cands))
	}
}

// toCandidatesJSON shapes a provider candidate page into the picker wire response,
// setting HasMore when a full page came back (another page likely exists).
func toCandidatesJSON(p *providerImageProxy, cands []enrich.Candidate) enrichmentCandidatesJSON {
	out := enrichmentCandidatesJSON{
		Candidates: make([]enrichmentCandidateJSON, 0, len(cands)),
		HasMore:    len(cands) >= enrich.SearchCandidateLimit,
	}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, toCandidateJSON(p, c))
	}
	return out
}

// externalPreviewRequest maps a preview/apply error onto the response. Shared by the
// leaf + parent paste-id preview handlers so both surface the same statuses: an
// unreadable paste → 400, a wrong-kind URL → 400, a disabled provider → 503, and a
// stale/unknown id → 404 ("not found", never a hang or 500).
func writeExternalPreview(w http.ResponseWriter, p *providerImageProxy, c enrich.Candidate, err error) {
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
		writeJSON(w, http.StatusOK, toCandidateJSON(p, c))
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
func handleTitleExternalPreview(enrichSvc *enrich.Service, images *providerImageProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/externalPreview")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		c, err := enrichSvc.PreviewTitleExternal(r.Context(), titleID, ref)
		writeExternalPreview(w, images, c, err)
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
	// ReleaseID pins WHICH EDITION of an Album decorates its tracks (ADR-0052): the
	// MusicBrainz release a pasted /release/ URL names, carried back by the preview
	// beside the release-group it resolved to. Album only, and never identity — the
	// release-group remains what an album IS (ADR-0038), so pinning an edition re-keys
	// nothing. OMITTED CLEARS a previously chosen edition, because a picked search
	// candidate or a pasted /release-group/ URL is the Admin naming a less specific
	// thing, and a stale edition under a new group would decorate from a stranger's
	// tracklist. Ignored on a leaf Title and on a Show/Artist — a Track has no
	// tracklist of its own, and neither of those kinds has editions.
	ReleaseID string `json:"releaseId,omitempty"`
	// Season / Episode pin WHICH provider episode decorates an Episode, for the
	// lookup only. Sent when the Admin picked a specific episode after picking the
	// series — the fix for a file the provider numbers differently from the disk.
	// Omitted (Episode 0) leaves any existing pin alone; identity and watch state
	// are never touched either way (ADR-0002/0014).
	Season  int `json:"season,omitempty"`
	Episode int `json:"episode,omitempty"`
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
		// When the Admin picked a specific episode, pin it too — that is the whole
		// point on a series whose provider numbering doesn't match the files.
		var err error
		if req.Episode > 0 {
			err = enrichSvc.ApplyEpisodeOverride(r.Context(), titleID, externalID, req.Season, req.Episode)
		} else {
			err = enrichSvc.ApplyOverride(r.Context(), titleID, externalID)
		}
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
// image picker (Fix label, ADR-0019): the URL to pick, a same-origin URL to PREVIEW
// it with, and the source's dimensions (0 when unreported) so the picker can hint
// resolution.
//
// url and thumbnailUrl are two different jobs that used to be one field, and
// splitting them is the point. url is the candidate's IDENTITY: it goes back in the
// PUT /artwork body, the server downloads it, and the picker compares it to mark
// which image is applied — so it stays the provider's own URL, unchanged.
// thumbnailUrl is what the grid puts in an <img src>, and it is proxied through
// this server so displaying the grid does not hand TMDB / the Cover Art Archive /
// fanart.tv the admin's IP address (ADR-0001, provider_image.go). Do not collapse
// them back together: making url the proxy URL would mean the pick path had to
// unwrap its own proxy references, and making thumbnailUrl the raw URL would put
// the browser straight back in touch with the third party.
type artworkCandidateJSON struct {
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	Source       string `json:"source,omitempty"`
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

func toArtworkCandidatesJSON(p *providerImageProxy, role string, cands []enrich.ArtworkCandidate) artworkCandidatesJSON {
	out := artworkCandidatesJSON{Role: role, Candidates: make([]artworkCandidateJSON, 0, len(cands))}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, artworkCandidateJSON{
			URL: c.URL, ThumbnailURL: p.proxyURL(c.URL),
			Width: c.Width, Height: c.Height, Source: c.Source,
		})
	}
	return out
}

// handleTitleArtworkCandidates lists the provider images offered for a leaf Title's
// role, so an Admin can pick a specific poster/background (GET
// /titles/{id}/artworkCandidates?role=…, Admin-only). Same SEARCH_UNAVAILABLE (503)
// semantics as the record search when the provider is unconfigured/unreachable.
// Unknown Title → 404. A missing/invalid role → 400. Reads only.
func handleTitleArtworkCandidates(enrichSvc *enrich.Service, images *providerImageProxy) http.HandlerFunc {
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
		writeJSON(w, http.StatusOK, toArtworkCandidatesJSON(images, role, cands))
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

// (The enrich.Progress → events.EnrichProgress mapping used to live here, for a
// manual pass this handler ran itself. There is exactly ONE producer of
// enrichProgress now — app.runEnrichPass, on the background worker every trigger
// including this one feeds — so the mapping lives there and cannot drift between
// a manual pass and an automatic one.)

// --- Library-scoped candidate search (the Unmatched-file case) --------------

// searchKindForLibrary maps a Library's media kind onto the entity kind a
// library-scoped provider search should look up. It is the same mapping
// handleFixMatch already uses to resolve an id-only fix (movie → movie, tv →
// show), extended with music → album, because a Music fix-match is anchored to an
// album folder and an album is what the Admin is identifying. "" for an unknown
// kind, which the callers turn into a 404.
func searchKindForLibrary(libraryKind string) string {
	switch libraryKind {
	case "movie":
		return "movie"
	case "tv":
		return "show"
	case "music":
		return "album"
	default:
		return ""
	}
}

// handleLibraryEnrichmentCandidates searches the authoritative provider for a
// Library rather than for an existing item (GET
// /libraries/{id}/enrichmentCandidates?q=…, Admin-only).
//
// It exists for the one row type that cannot use the per-item search: an
// **Unmatched** file has, by definition, no Title to anchor
// /titles/{id}/enrichmentCandidates to. That is why its only correction tool used
// to be a raw-id form — the row that most needs a search was the one row that
// could not have one. The searched kind comes from the Library's media kind.
//
// Everything else matches the per-item route: blank query → empty 200, capped page
// size, same-origin thumbnail rewrite, 503 SEARCH_UNAVAILABLE when the provider is
// unconfigured or unreachable, unknown Library → 404 (hide-existence). Reads only.
func handleLibraryEnrichmentCandidates(enrichSvc *enrich.Service, images *providerImageProxy, cat *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libraryID := pathParam(r.URL.Path, "/libraries/", "/enrichmentCandidates")
		kind, ok := librarySearchKind(w, cat, libraryID)
		if !ok {
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		cands, err := enrichSvc.SearchCandidates(r.Context(), kind, query, searchOptionsFrom(r))
		switch {
		case errors.Is(err, enrich.ErrSearchUnavailable):
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider search is unavailable for this library — the provider is unconfigured or disabled", nil)
			return
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
				"metadata provider search failed — the source may be unreachable", nil)
			return
		}
		writeJSON(w, http.StatusOK, toCandidatesJSON(images, cands))
	}
}

// handleLibraryExternalPreview is the paste-an-id escape hatch for the same
// Library-scoped case (GET /libraries/{id}/externalPreview?ref=…, Admin-only), so
// an Unmatched row accepts a pasted provider URL exactly like every per-item
// picker does. Reads only; the apply still goes through fix-match.
func handleLibraryExternalPreview(enrichSvc *enrich.Service, images *providerImageProxy, cat *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libraryID := pathParam(r.URL.Path, "/libraries/", "/externalPreview")
		kind, ok := librarySearchKind(w, cat, libraryID)
		if !ok {
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		c, err := enrichSvc.PreviewExternalForKind(r.Context(), kind, ref)
		writeExternalPreview(w, images, c, err)
	}
}

// librarySearchKind resolves the Library's searchable entity kind, writing the 404
// itself when the id is absent, unknown, or of a kind with no provider search. The
// bool reports whether the caller may continue.
func librarySearchKind(w http.ResponseWriter, cat *catalog.Service, libraryID string) (string, bool) {
	if libraryID == "" {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		return "", false
	}
	libKind, err := cat.LibraryKind(libraryID)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return "", false
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to search", nil)
		return "", false
	}
	kind := searchKindForLibrary(libKind)
	if kind == "" {
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return "", false
	}
	return kind, true
}

// --- Episode picking (which provider episode decorates this file) -----------

type seasonSummaryJSON struct {
	Season       int `json:"season"`
	EpisodeCount int `json:"episodeCount"`
}

type episodeCandidateJSON struct {
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Name     string `json:"name"`
	Overview string `json:"overview,omitempty"`
	AirDate  string `json:"airDate,omitempty"`
	// StillURL is a same-origin /providerImage reference, never the provider's own
	// host — the same rule every other candidate thumbnail follows (ADR-0001).
	StillURL string `json:"stillUrl,omitempty"`
}

type episodeCandidatesJSON struct {
	// Seasons is sent only on the first request (no explicit `season`), so a client
	// fetches the season list once and then pages episodes within it.
	Seasons  []seasonSummaryJSON    `json:"seasons,omitempty"`
	Season   int                    `json:"season"`
	Episodes []episodeCandidateJSON `json:"episodes"`
}

// handleEpisodeCandidates lists a picked series' episodes so an Admin can choose
// the exact one a file should be decorated from (GET
// /titles/{id}/episodeCandidates?externalId=&season=, Admin-only).
//
// This is the second half of correcting a TV episode. Picking the series alone was
// never enough: the lookup is /tv/{show}/season/{S}/episode/{E} with S and E taken
// from the FILENAME, so a file the provider counts in a different season stayed
// unmatchable however many times the series was re-picked. Choosing the episode
// here writes a lookup-only pin; identity and watch state are untouched.
//
// `season` defaults to the Title's own parsed season, so the common case — right
// season, wrong episode — opens on the right list. 503 SEARCH_UNAVAILABLE when the
// provider cannot list episodes; unknown Title → 404.
func handleEpisodeCandidates(enrichSvc *enrich.Service, images *providerImageProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/episodeCandidates")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		showID := strings.TrimSpace(r.URL.Query().Get("externalId"))
		if showID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"externalId (the series to list episodes from) is required", nil)
			return
		}

		// A season the caller names wins; otherwise the service defaults to the one
		// this file is already filed under — the list the Admin most likely wants.
		var season *int
		if explicit := strings.TrimSpace(r.URL.Query().Get("season")); explicit != "" {
			n, convErr := strconv.Atoi(explicit)
			if convErr != nil {
				writeError(w, http.StatusBadRequest, codeBadRequest, "season must be a number", nil)
				return
			}
			season = &n
		}

		picker, err := enrichSvc.EpisodePickerData(r.Context(), titleID, showID, season)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeEpisodeCandidatesError(w, err)
			return
		}

		out := episodeCandidatesJSON{Season: picker.Season}
		for _, sn := range picker.Seasons {
			out.Seasons = append(out.Seasons, seasonSummaryJSON{Season: sn.Season, EpisodeCount: sn.EpisodeCount})
		}
		out.Episodes = make([]episodeCandidateJSON, 0, len(picker.Episodes))
		for _, e := range picker.Episodes {
			out.Episodes = append(out.Episodes, episodeCandidateJSON{
				Season:   e.Season,
				Episode:  e.Episode,
				Name:     e.Name,
				Overview: e.Overview,
				AirDate:  e.AirDate,
				StillURL: images.proxyURL(e.StillURL),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// writeEpisodeCandidatesError maps a listing failure onto the same statuses the
// other picker reads use, so the box reports why rather than hanging.
func writeEpisodeCandidatesError(w http.ResponseWriter, err error) {
	if errors.Is(err, enrich.ErrSearchUnavailable) {
		writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
			"this metadata provider cannot list episodes — episode picking is unavailable", nil)
		return
	}
	writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
		"listing episodes failed — the metadata provider may be unreachable", nil)
}
