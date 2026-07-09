package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subfetch"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// External subtitle fetching (ADR-0021, subtitles slice 05). Two POST leaves on
// the title subtree, both open to ANY authenticated User (Member included — the
// deliberate role widening the ADR records):
//
//	POST /titles/{id}/subtitles/search  → candidates for a wanted language
//	POST /titles/{id}/subtitles/fetch   → download + persist a chosen candidate
//
// The provider network lives behind the subfetch.Service seam; a disabled/offline
// provider degrades to an empty candidate list and surfaces nothing to playback.
// A fetched track is a source='fetched' subtitles row that appears in the Title's
// subtitles[] and serves through the existing out-of-band .vtt endpoint unchanged.

// subtitleFetchTimeout bounds a single provider search/download so a hung host
// can't stall the request (the provider clients also carry their own timeouts).
const subtitleFetchTimeout = 30 * time.Second

// subtitleSearchRequest is the POST .../search body: the wanted ISO-639-1 language.
type subtitleSearchRequest struct {
	Language string `json:"language"`
}

// subtitleCandidateJSON is one search result offered to the viewer. It carries the
// opaque provider id + format/forced needed to fetch it later (the client echoes
// the chosen candidate back to .../fetch), plus human copy for the picker.
type subtitleCandidateJSON struct {
	ID              string `json:"id"`
	Language        string `json:"language"`
	Format          string `json:"format"`
	Release         string `json:"release,omitempty"`
	Forced          bool   `json:"forced"`
	HearingImpaired bool   `json:"hearingImpaired"`
	MatchedBy       string `json:"matchedBy,omitempty"`
	Label           string `json:"label"`
}

// subtitleSearchResponse is the POST .../search body: the candidate list (non-nil,
// empty when the provider is disabled/offline or has nothing — a normal outcome).
type subtitleSearchResponse struct {
	Candidates []subtitleCandidateJSON `json:"candidates"`
}

// subtitleFetchRequest is the POST .../fetch body: the wanted language plus the
// candidate the viewer picked (echoed verbatim from the search response, so the
// fetch is stateless — no server-side candidate cache to key by).
type subtitleFetchRequest struct {
	Language  string                `json:"language"`
	Candidate subtitleCandidateJSON `json:"candidate"`
}

// subtitleFetchResponse is the POST .../fetch body: the newly-created fetched track
// as a decision-style subtitle entry, so the client can add it to the captions
// menu and enable it immediately (its .vtt URL is included for a text track).
type subtitleFetchResponse struct {
	Subtitle decisionSubtitleJSON `json:"subtitle"`
}

// handleSubtitleSearch resolves the Title under the caller's scope, builds the
// provider match ref (parsed identity + IMDBID + played-file path for the lazy
// moviehash), and returns the provider's candidates for the wanted language. A
// disabled/offline provider yields an empty list, never an error (graceful
// degradation, ADR-0001).
func handleSubtitleSearch(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		var req subtitleSearchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		lang := subtitle.NormalizeLang(req.Language)
		if lang == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid subtitle language is required", nil)
			return
		}

		ref, ok := resolveFetchRef(w, deps, scope, titleID)
		if !ok {
			return
		}
		if deps.SubFetch == nil {
			writeJSON(w, http.StatusOK, subtitleSearchResponse{Candidates: []subtitleCandidateJSON{}})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), subtitleFetchTimeout)
		defer cancel()
		cands, err := deps.SubFetch.Search(ctx, ref, lang)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "subtitle search failed", nil)
			return
		}

		out := make([]subtitleCandidateJSON, 0, len(cands))
		for _, c := range cands {
			out = append(out, toSubtitleCandidateJSON(c))
		}
		writeJSON(w, http.StatusOK, subtitleSearchResponse{Candidates: out})
	}
}

// handleSubtitleFetch downloads the chosen candidate, normalizes it to WebVTT,
// caches it identity-keyed under the data dir, and records the locking
// source='fetched' row (subfetch.Service.Pick). It returns the created track as a
// decision-style entry so the client can enable it at once.
func handleSubtitleFetch(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		var req subtitleFetchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		lang := subtitle.NormalizeLang(req.Language)
		if lang == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a valid subtitle language is required", nil)
			return
		}
		if req.Candidate.ID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "a candidate id is required", nil)
			return
		}
		if deps.SubFetch == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "subtitle fetching is unavailable", nil)
			return
		}

		ref, ok := resolveFetchRef(w, deps, scope, titleID)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), subtitleFetchTimeout)
		defer cancel()
		sub, err := deps.SubFetch.Pick(ctx, ref, fromSubtitleCandidateJSON(req.Candidate), lang)
		switch {
		case errors.Is(err, subfetch.ErrProviderDisabled), errors.Is(err, subfetch.ErrNoMatch):
			// The provider went away between search and pick, or the candidate vanished:
			// nothing to deliver, but not a server error.
			writeError(w, http.StatusNotFound, codeNotFound, "subtitle no longer available", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "subtitle fetch failed", nil)
			return
		}

		writeJSON(w, http.StatusOK, subtitleFetchResponse{Subtitle: toFetchedSubtitleJSON(titleID, sub)})
	}
}

// resolveFetchRef loads the Title under scope and derives the provider match ref:
// the parsed title/year, the enrichment-assigned IMDBID, and the first present
// File's path (for the lazy moviehash + size). A missing/out-of-scope Title is a
// 404 (hide existence); a Title with no playable File still fetches by identity.
func resolveFetchRef(w http.ResponseWriter, deps Deps, scope access.Scope, titleID string) (subfetch.FetchRef, bool) {
	d, err := deps.Catalog.GetTitle(scope, titleID)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
		return subfetch.FetchRef{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to load title", nil)
		return subfetch.FetchRef{}, false
	}
	return subfetch.FetchRef{
		TitleID:  d.Title.ID,
		Title:    d.Title.Title,
		Year:     d.Title.Year,
		IMDBID:   d.Title.IMDBID,
		FilePath: firstPresentFilePath(d),
	}, true
}

// firstPresentFilePath returns the on-disk path of the Title's first present File,
// used only to compute the moviehash for release-exact matching. Empty when the
// Title has no present File (the provider falls back to imdb_id / a filename query).
func firstPresentFilePath(d store.TitleDetail) string {
	for _, ed := range d.Editions {
		for _, f := range ed.Files {
			if f.Present {
				return f.Path
			}
		}
	}
	return ""
}

func toSubtitleCandidateJSON(c subfetch.Candidate) subtitleCandidateJSON {
	return subtitleCandidateJSON{
		ID:              c.ID,
		Language:        c.Language,
		Format:          c.Format,
		Release:         c.Release,
		Forced:          c.Forced,
		HearingImpaired: c.HearingImpaired,
		MatchedBy:       c.MatchedBy,
		Label:           subfetch.CandidateLabel(c),
	}
}

func fromSubtitleCandidateJSON(c subtitleCandidateJSON) subfetch.Candidate {
	return subfetch.Candidate{
		ID:              c.ID,
		Language:        c.Language,
		Format:          c.Format,
		Release:         c.Release,
		Forced:          c.Forced,
		HearingImpaired: c.HearingImpaired,
		MatchedBy:       c.MatchedBy,
	}
}

// toFetchedSubtitleJSON renders a freshly-fetched store.Subtitle as a decision-style
// entry, attaching the out-of-band .vtt URL for a text track so the client can
// enable it immediately (mirrors toDecisionSubtitles for the fetched case).
func toFetchedSubtitleJSON(titleID string, s store.Subtitle) decisionSubtitleJSON {
	entry := decisionSubtitleJSON{
		ID:       s.ID,
		Source:   s.Source,
		Kind:     s.Kind,
		Language: s.Language,
		Forced:   s.Forced,
		Label:    subtitle.Label(s.Language, s.Forced),
	}
	if s.Kind == "text" && subtitle.IsTextConvertible(s.Codec) {
		entry.URL = subtitleVTTURL(titleID, s.ID)
	}
	return entry
}
