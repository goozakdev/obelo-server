package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/match"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes + handlers for the fix-match Match override (ADR-0002/0014). An
// Admin re-points a folder's identity; the override persists across rescans and,
// once its folder is renamed/moved, is surfaced as orphaned (never lost).

// --- POST /libraries/{id}/fix-match -----------------------------------------

type fixMatchRequest struct {
	// FolderPath is the on-disk folder (or bare-file path) the override anchors
	// to. Required, absolute.
	FolderPath string `json:"folderPath"`
	// The corrected identity: a Title (+ optional Year), or an embedded external
	// id. At least one identity signal is required.
	Title  string `json:"title"`
	Year   int    `json:"year"`
	TMDBID string `json:"tmdbId"`
	IMDBID string `json:"imdbId"`
}

type matchOverrideJSON struct {
	ID          string `json:"id"`
	FolderPath  string `json:"folderPath"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	TMDBID      string `json:"tmdbId,omitempty"`
	IMDBID      string `json:"imdbId,omitempty"`
	IdentityKey string `json:"identityKey"`
	// Orphaned is the attention flag: the anchor folder no longer exists on disk.
	Orphaned  bool   `json:"orphaned,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

func toMatchOverride(o store.MatchOverride) matchOverrideJSON {
	return matchOverrideJSON{
		ID:          o.ID,
		FolderPath:  o.FolderPath,
		Title:       o.Title,
		Year:        o.Year,
		TMDBID:      o.TMDBID,
		IMDBID:      o.IMDBID,
		IdentityKey: o.IdentityKey,
		Orphaned:    o.Orphaned,
		CreatedAt:   formatTimestamp(o.CreatedAt),
	}
}

// handleFixMatch records an Admin identity correction for a folder (Admin-only).
// Unknown Library → 404; bad input → 400. The override takes effect on the next
// scan and persists across rescans.
func handleFixMatch(svc *match.Service, enr *enrich.Service, cat *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/fix-match")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req fixMatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		// When the Admin supplied only an external id, resolve the canonical title +
		// year from it (the id alone identifies the work) so they need not type them.
		// Only the video kinds use TMDB/IMDB ids; a Music library's identity is its
		// tags, so it is skipped (the Admin gives an album title there). Best-effort:
		// if enrichment is off / the id doesn't resolve / the provider is unreachable,
		// we proceed with whatever was given (the id still anchors identity).
		var kind string
		switch k, _ := cat.LibraryKind(id); k {
		case "movie":
			kind = "movie"
		case "tv":
			kind = "show"
		}
		if kind != "" && enr != nil && strings.TrimSpace(req.Title) == "" && (req.TMDBID != "" || req.IMDBID != "") {
			if title, year, ok, err := enr.ResolveIdentity(r.Context(), enrich.TitleRef{
				Kind: kind, TMDBID: req.TMDBID, IMDBID: req.IMDBID,
			}); err == nil && ok {
				req.Title = title
				if req.Year == 0 {
					req.Year = year
				}
			}
		}
		ov, err := svc.FixMatch(match.FixMatchInput{
			LibraryID:  id,
			FolderPath: req.FolderPath,
			Title:      req.Title,
			Year:       req.Year,
			TMDBID:     req.TMDBID,
			IMDBID:     req.IMDBID,
		})
		switch {
		case errors.Is(err, match.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case errors.Is(err, match.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to record override", nil)
			return
		}
		writeJSON(w, http.StatusOK, toMatchOverride(ov))
	}
}

// --- GET /libraries/{id}/overrides (Admin attention surface) ----------------

type overridesResponse struct {
	Overrides []matchOverrideJSON `json:"overrides"`
}

// handleListOverrides returns a Library's Match overrides — including orphaned
// ones (folder renamed/moved), the Admin attention surface (Admin-only).
func handleListOverrides(svc *match.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/overrides")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		list, err := svc.List(id)
		switch {
		case errors.Is(err, match.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list overrides", nil)
			return
		}
		out := overridesResponse{Overrides: make([]matchOverrideJSON, 0, len(list))}
		for _, o := range list {
			out.Overrides = append(out.Overrides, toMatchOverride(o))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
