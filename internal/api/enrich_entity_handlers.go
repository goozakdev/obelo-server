package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/store"
)

// Edit-item on the browse PARENTS — Show / Artist / Album (item-editing/02,
// ADR-0019). The leaf surface (playback_handlers.go /titles/…) is mirrored here
// for the entity_enrichment-backed parents: a provider search picker
// (enrichmentCandidates), an apply-Enrichment-override (enrichmentOverride, a
// durable pin + single-parent re-enrich), and a hand-edit + Locked-field surface
// (metadata / metadata/locks/{field}). Every route is Admin-only and emits the
// libraryUpdated SSE nudge, exactly like the leaf endpoints. Seasons get NO edit
// affordance (edited at the Show or Episode grain) — no season route is wired.

// entityOverrideJSON is the active durable Enrichment override on a parent, so a
// client can show and undo it. Present only when an Admin has pinned a record.
type entityOverrideJSON struct {
	ExternalID string `json:"externalId"`
	Source     string `json:"source,omitempty"`
	Status     string `json:"status,omitempty"`
}

// entityEnrichmentDetailJSON is the compact parent-enrichment detail the parent
// Edit-item endpoints return (and the browse detail embeds): the descriptive
// fields plus which fields are Locked and which Enrichment override is in effect.
type entityEnrichmentDetailJSON struct {
	EntityType       string              `json:"entityType"`
	EntityID         string              `json:"entityId"`
	Overview         string              `json:"overview,omitempty"`
	Genres           []string            `json:"genres,omitempty"`
	ContentRating    string              `json:"contentRating,omitempty"`
	Network          string              `json:"network,omitempty"`
	EnrichmentStatus string              `json:"enrichmentStatus,omitempty"`
	LockedFields     []string            `json:"lockedFields,omitempty"`
	Override         *entityOverrideJSON `json:"enrichmentOverride,omitempty"`
	// Cascade is the "also apply to children" summary (item-editing/05): present only
	// on an override/Wrong-item apply that ran the cascade (updated N children, sent M
	// to the attention list). Omitted when no cascade ran.
	Cascade *cascadeSummaryJSON `json:"cascade,omitempty"`
}

// cascadeSummaryJSON reports a cascade's outcome to the Admin: how many children got
// a durable Enrichment override, and how many were routed to the attention list.
type cascadeSummaryJSON struct {
	Updated   int `json:"updated"`
	Attention int `json:"attention"`
}

// runEntityCascade applies the parent correction to the parent's children when the
// Admin ticked "also apply to children", returning the summary for the response (nil
// when no cascade was requested). It is BEST-EFFORT: a cascade error is logged into
// the summary as zero effect but never fails the parent correction, which already
// stuck. It must run AFTER the parent override applied and its per-Library lock was
// released (each per-child re-enrich re-acquires it).
func runEntityCascade(r *http.Request, enrichSvc *enrich.Service, cascade bool, entityType, entityID, externalID string) *cascadeSummaryJSON {
	if !cascade {
		return nil
	}
	sum, err := enrichSvc.CascadeEntity(r.Context(), entityType, entityID, externalID)
	if err != nil {
		// A partial cascade already wrote whatever children it reached; surface the
		// counts it accumulated and don't fail the (already-applied) parent correction.
		return &cascadeSummaryJSON{Updated: sum.Updated, Attention: sum.Attention}
	}
	return &cascadeSummaryJSON{Updated: sum.Updated, Attention: sum.Attention}
}

// entityOverride builds the active-override view of a parent's enrichment, or nil
// when no durable override is pinned. Shared by the parent detail read and the
// browse-detail decorators.
func entityOverride(e store.EntityEnrichment) *entityOverrideJSON {
	if !e.ExternalIDLocked || e.ExternalID == "" {
		return nil
	}
	return &entityOverrideJSON{ExternalID: e.ExternalID, Source: e.Source, Status: e.Status}
}

// buildEntityDetail assembles a parent's enrichment detail (fields + lockedFields +
// active override) for the Edit-item endpoints' response.
func buildEntityDetail(cat *catalog.Service, entityType, entityID string) (entityEnrichmentDetailJSON, error) {
	e, err := cat.EntityEnrichment(entityType, entityID)
	if err != nil {
		return entityEnrichmentDetailJSON{}, err
	}
	locked, err := cat.EntityLockedFields(entityType, entityID)
	if err != nil {
		return entityEnrichmentDetailJSON{}, err
	}
	d := entityEnrichmentDetailJSON{
		EntityType:    entityType,
		EntityID:      entityID,
		Overview:      e.Overview,
		Genres:        e.Genres,
		ContentRating: e.ContentRating,
		Network:       e.Network,
		LockedFields:  locked,
		Override:      entityOverride(e),
	}
	if e.Status != "" && e.Status != "pending" {
		d.EnrichmentStatus = e.Status
	}
	return d, nil
}

// handleEntityEnrichmentCandidates searches the authoritative provider for the
// records that could decorate a browse parent (Show → TMDB tv, Artist/Album →
// MusicBrainz), so an Admin can pick one and apply it as an Enrichment override
// (GET /shows|artists|albums/{id}/enrichmentCandidates?q=…, Admin-only). An album
// candidate carries its tracklist. A blank query returns an empty list; an
// unconfigured/unreachable provider is 503 SEARCH_UNAVAILABLE. Unknown parent →
// 404. Reads only.
func handleEntityEnrichmentCandidates(enrichSvc *enrich.Service, cat *catalog.Service, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(entityType, entityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to search", nil)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		cands, err := enrichSvc.SearchEntityCandidates(r.Context(), entityType, entityID, query, searchOptionsFrom(r))
		switch {
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

// handleEntityExternalPreview resolves a pasted MusicBrainz/TMDB id-or-URL to a single
// preview candidate for a browse parent WITHOUT searching (GET
// /shows|artists|albums/{id}/externalPreview?ref=…, Admin-only) — the parent analogue
// of handleTitleExternalPreview (item-editing/search-improvements). Reads only; the
// apply reuses the existing enrichmentOverride endpoint. Unknown parent → 404.
func handleEntityExternalPreview(enrichSvc *enrich.Service, cat *catalog.Service, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(entityType, entityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to preview", nil)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		c, err := enrichSvc.PreviewEntityExternal(r.Context(), entityType, entityID, ref)
		writeExternalPreview(w, c, err)
	}
}

// handleEntityEnrichmentOverride applies a picked candidate as a durable Enrichment
// override on a browse parent and re-enriches just it (PUT
// /shows|artists|albums/{id}/enrichmentOverride, Admin-only). It pins the
// authoritative external id (persisted, external_id_locked — so future passes look
// up BY it) and refreshes the unlocked fields/artwork from that record. Identity and
// watch state are NEVER touched (ADR-0002/0014); Locked fields are honored. Emits a
// libraryUpdated SSE nudge and returns the updated parent detail. Missing externalId
// → 400; unknown parent → 404.
func handleEntityEnrichmentOverride(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req enrichmentOverrideRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		externalID := strings.TrimSpace(req.ExternalID)
		if externalID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "externalId is required", nil)
			return
		}
		err := enrichSvc.ApplyEntityOverride(r.Context(), entityType, entityID, externalID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply enrichment override", nil)
			return
		}
		// "Also apply to children" (item-editing/05): after the parent pin applied (and
		// its per-Library lock released), best-effort re-resolve the children.
		sum := runEntityCascade(r, enrichSvc, req.Cascade, entityType, entityID, externalID)
		writeEntityDetailAfterCascade(w, cat, broker, entityType, entityID, sum)
	}
}

// entityMetadataEditRequest is the body of PUT /shows|artists|albums/{id}/metadata:
// each present field is hand-edited AND Locked so re-enrichment never overwrites it
// (CONTEXT.md "Locked field"). Pointer fields distinguish "edit to empty" from
// "leave untouched"; lockArtwork pins artwork roles ('poster'/'background'/'cover').
type entityMetadataEditRequest struct {
	Overview      *string   `json:"overview"`
	ContentRating *string   `json:"contentRating"`
	Network       *string   `json:"network"`
	Genres        *[]string `json:"genres"`
	// Title edits the parent's DISPLAY label (a rename) — never its identity_key or
	// the catalog hierarchy (ADR-0002), so it never touches watch state or the active
	// override. Locks "title".
	Title       *string  `json:"title"`
	LockArtwork []string `json:"lockArtwork"`
}

// handleEntityMetadata applies an Admin hand-edit to a browse parent's descriptive
// fields and Locks each edited field (PUT /shows|artists|albums/{id}/metadata,
// Admin-only) — the parent analogue of handleEditMetadata. Returns the updated
// parent detail (listing the edits in lockedFields[]). Unknown parent → 404.
func handleEntityMetadata(cat *catalog.Service, broker *events.Broker, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req entityMetadataEditRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		edit := store.EntityMetadataEdit{
			Overview:      req.Overview,
			ContentRating: req.ContentRating,
			Network:       req.Network,
			Genres:        req.Genres,
			Name:          req.Title,
			LockArtwork:   req.LockArtwork,
		}
		err := cat.EditEntityMetadata(entityType, entityID, edit)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to edit metadata", nil)
			return
		}
		writeEntityDetailAfterEdit(w, cat, broker, entityType, entityID)
	}
}

// handleReleaseEntityLock releases a browse parent's Lock on one field (DELETE
// /shows|artists|albums/{id}/metadata/locks/{field}, Admin-only) so the next enrich
// pass refreshes it. Releasing an absent lock is a no-op. Unknown parent → 404.
func handleReleaseEntityLock(cat *catalog.Service, broker *events.Broker, entityType, entityID, field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := cat.ReleaseEntityLock(entityType, entityID, field)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to release lock", nil)
			return
		}
		writeEntityDetailAfterEdit(w, cat, broker, entityType, entityID)
	}
}

// handleEntityArtworkCandidates lists the provider images offered for a browse
// parent's role, so an Admin can pick one (GET
// /shows|artists|albums/{id}/artworkCandidates?role=…, Admin-only). Same
// SEARCH_UNAVAILABLE (503) semantics as the record search. Unknown parent → 404;
// missing/invalid role → 400. Reads only.
func handleEntityArtworkCandidates(enrichSvc *enrich.Service, cat *catalog.Service, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(entityType, entityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list images", nil)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		role := strings.TrimSpace(r.URL.Query().Get("role"))
		if !validArtworkRole(role) {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid role (poster, background, cover, logo) is required", nil)
			return
		}
		cands, err := enrichSvc.ListEntityArtworkCandidates(r.Context(), entityType, entityID, role)
		switch {
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

// handleEntityPickArtwork applies a picked provider image to a browse parent's role
// and Locks that role (PUT /shows|artists|albums/{id}/artwork, Admin-only) — the
// parent analogue of handlePickTitleArtwork. Local artwork still wins. Emits a
// libraryUpdated SSE nudge and returns the updated parent detail. Missing role/url →
// 400; unknown parent → 404.
func handleEntityPickArtwork(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(entityType, entityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set artwork", nil)
			return
		}
		if !ok {
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
		if err := enrichSvc.PickEntityArtwork(r.Context(), entityType, entityID, req.Role, strings.TrimSpace(req.URL)); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set artwork", nil)
			return
		}
		writeEntityDetailAfterEdit(w, cat, broker, entityType, entityID)
	}
}

// handleUploadEntityArtwork stores an Admin-uploaded image as a browse parent's
// role and Locks it (POST /shows|artists|albums/{id}/artworkUpload?role=…,
// Admin-only, multipart) — the parent analogue of handleUploadTitleArtwork
// (ADR-0026). The Uploaded image outranks a local parent image at serve time.
// Emits a libraryUpdated SSE nudge and returns the updated parent detail. Bad role
// → 400; bad file → 413/415; unknown parent → 404.
func handleUploadEntityArtwork(enrichSvc *enrich.Service, cat *catalog.Service, broker *events.Broker, entityType, entityID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(entityType, entityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to store uploaded artwork", nil)
			return
		}
		if !ok {
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
		if err := enrichSvc.UploadEntityArtwork(entityType, entityID, role, data, contentType); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to store uploaded artwork", nil)
			return
		}
		writeEntityDetailAfterEdit(w, cat, broker, entityType, entityID)
	}
}

// writeEntityDetailAfterEdit is the shared response tail for the parent Edit-item
// endpoints: read the updated parent detail, emit the libraryUpdated SSE nudge so
// browse reflects the correction live (ADR-0016), and write the detail JSON.
func writeEntityDetailAfterEdit(w http.ResponseWriter, cat *catalog.Service, broker *events.Broker, entityType, entityID string) {
	writeEntityDetailAfterCascade(w, cat, broker, entityType, entityID, nil)
}

// writeEntityDetailAfterCascade is writeEntityDetailAfterEdit with an optional cascade
// summary attached (item-editing/05): the Fix-info / Wrong-item apply paths pass the
// "also apply to children" result so the Admin sees N updated / M sent to attention.
func writeEntityDetailAfterCascade(w http.ResponseWriter, cat *catalog.Service, broker *events.Broker, entityType, entityID string, sum *cascadeSummaryJSON) {
	d, err := buildEntityDetail(cat, entityType, entityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read updated item", nil)
		return
	}
	d.Cascade = sum
	if broker != nil {
		if libraryID, err := cat.LibraryOfEntity(entityType, entityID); err == nil && libraryID != "" {
			broker.PublishLibraryUpdated(libraryID)
		}
	}
	writeJSON(w, http.StatusOK, d)
}

// dispatchEntityEditRoutes handles the Edit-item sub-resources under a browse
// parent's subtree (/shows/{id}…, /artists/{id}…, /albums/{id}…): candidates,
// override, metadata, and metadata/locks/{field}. It returns true when it matched
// (and served) a route, so the subtree dispatcher can fall through to its browse
// listing otherwise. Every route is Admin-only. entityType is the store entity kind.
func dispatchEntityEditRoutes(w http.ResponseWriter, r *http.Request, deps Deps, entityType, rest string) bool {
	// DELETE {id}/metadata/locks/{field}: release a Locked field (matched before
	// /metadata so the longer suffix isn't shadowed).
	if i := strings.Index(rest, "/metadata/locks/"); i > 0 {
		id := rest[:i]
		field := rest[i+len("/metadata/locks/"):]
		if strings.Contains(id, "/") || field == "" || strings.Contains(field, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodDelete,
			requireAuth(deps.Auth, requireAdmin(handleReleaseEntityLock(deps.Catalog, deps.Events, entityType, id, field))))(w, r)
		return true
	}
	if id, ok := strings.CutSuffix(rest, "/enrichmentCandidates"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodGet,
			requireAuth(deps.Auth, requireAdmin(handleEntityEnrichmentCandidates(deps.Enrich, deps.Catalog, entityType, id))))(w, r)
		return true
	}
	// GET {id}/externalPreview?ref=: preview a pasted MusicBrainz/TMDB id-or-URL before
	// applying it on a parent (paste escape hatch, item-editing/search-improvements).
	if id, ok := strings.CutSuffix(rest, "/externalPreview"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodGet,
			requireAuth(deps.Auth, requireAdmin(handleEntityExternalPreview(deps.Enrich, deps.Catalog, entityType, id))))(w, r)
		return true
	}
	// GET {id}/artworkCandidates?role=: list provider images for a role (Fix-label
	// picker). Matched before /artwork (distinct suffix; not a suffix of it).
	if id, ok := strings.CutSuffix(rest, "/artworkCandidates"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodGet,
			requireAuth(deps.Auth, requireAdmin(handleEntityArtworkCandidates(deps.Enrich, deps.Catalog, entityType, id))))(w, r)
		return true
	}
	// PUT {id}/artwork: apply a picked provider image to a role + Lock it (Fix label).
	if id, ok := strings.CutSuffix(rest, "/artwork"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodPut,
			requireAuth(deps.Auth, requireAdmin(handleEntityPickArtwork(deps.Enrich, deps.Catalog, deps.Events, entityType, id))))(w, r)
		return true
	}
	// POST {id}/artworkUpload?role=…: store an Admin-uploaded image as a role + Lock
	// it (ADR-0026, upload-is-select). Distinct suffix from /artwork and its media
	// GET, so it isn't shadowed. Admin-only, multipart.
	if id, ok := strings.CutSuffix(rest, "/artworkUpload"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodPost,
			requireAuth(deps.Auth, requireAdmin(handleUploadEntityArtwork(deps.Enrich, deps.Catalog, deps.Events, entityType, id))))(w, r)
		return true
	}
	if id, ok := strings.CutSuffix(rest, "/enrichmentOverride"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodPut,
			requireAuth(deps.Auth, requireAdmin(handleEntityEnrichmentOverride(deps.Enrich, deps.Catalog, deps.Events, entityType, id))))(w, r)
		return true
	}
	if id, ok := strings.CutSuffix(rest, "/metadata"); ok {
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return true
		}
		requireMethod(http.MethodPut,
			requireAuth(deps.Auth, requireAdmin(handleEntityMetadata(deps.Catalog, deps.Events, entityType, id))))(w, r)
		return true
	}
	return false
}
