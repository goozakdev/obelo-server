package api

import (
	"encoding/json"
	"net/http"
)

// errorBody is the standard error envelope from docs/api-contract.md:
//
//	{ "error": { "code": "STRING_ENUM", "message": "...", "details": { } } }
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Error codes (the machine-readable STRING_ENUM). Grow this set as endpoints
// land; keeping them centralized keeps the vocabulary consistent across the
// API surface.
const (
	codeNotFound         = "NOT_FOUND"
	codeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	codeInternal         = "INTERNAL"
	codeBadRequest       = "BAD_REQUEST"
	codeUnauthorized     = "UNAUTHORIZED"
	codeForbidden        = "FORBIDDEN"
	codeFolderOverlap    = "FOLDER_OVERLAP"
	// codeNoFiles (409): a Targeted scan (ADR-0030) of an entity whose Files are all
	// Missing — there is nothing on disk to walk (hidden-entity resurrection is out
	// of scope for v1).
	codeNoFiles      = "NO_FILES"
	codeSetupClosed  = "SETUP_CLOSED"
	codeInvalidClaim = "INVALID_CLAIM_TOKEN"
	codeInvalidLogin = "INVALID_CREDENTIALS"
	// User-management (Admin-scope /users): a username collision and the
	// last-Admin guard each surface as a 409 with one of these codes.
	codeUsernameTaken = "USERNAME_TAKEN"
	codeLastAdmin     = "LAST_ADMIN"
	// Library-access grants (PUT /users/{id}/libraryAccess), both 422: granting to
	// an Admin, and naming a Library that does not exist.
	codeAdminGrant     = "ADMIN_GRANT"
	codeUnknownLibrary = "UNKNOWN_LIBRARY"
	// Rating ceiling (PUT /users/{id}/ratingCeiling), both 422: setting a ceiling
	// on an Admin, and an unknown rating label.
	codeAdminCeiling  = "ADMIN_CEILING"
	codeUnknownRating = "UNKNOWN_RATING"
	// codeUnknownTitle (422): a Collection item-add (POST /collections/{id}/items)
	// named a Title that does not exist; the whole add is rejected and the
	// membership set is left unchanged (mirrors UNKNOWN_LIBRARY for grants).
	codeUnknownTitle = "UNKNOWN_TITLE"
	// codeKindMismatch (422): a Playlist item-append (POST /playlists/{id}/items)
	// named a Title whose media kind does not match the Playlist's already-fixed
	// kind (a Movie into a music Playlist, etc.); the append is rejected and the
	// Playlist kind is left unchanged (collections-playlists 03).
	codeKindMismatch = "KIND_MISMATCH"
	// codeItemSetMismatch (422): a Playlist reorder (PUT /playlists/{id}/items)
	// gave an itemIds payload that does not EXACTLY match the Playlist's current
	// item ids (a missing, foreign/unknown, or duplicated id). The reorder is
	// rejected as a no-op and the existing order is left unchanged
	// (collections-playlists 04).
	codeItemSetMismatch = "ITEM_SET_MISMATCH"
	// codeSystemPlaylist (422): a rename (PUT /playlists/{id}) or delete
	// (DELETE /playlists/{id}) targeted a system Playlist (the Watchlist) that the
	// User owns but may not rename or delete — it belongs to the system, not the
	// User. The write is rejected and the Playlist is left unchanged.
	codeSystemPlaylist = "SYSTEM_PLAYLIST"
	// codeTranscodeRequired: the client cannot direct-play the File and this slice
	// has no remux/transcode tier, so playback negotiation returns it (501-class).
	// details carries { reason, detail } explaining the first blocking attribute.
	codeTranscodeRequired = "TRANSCODE_REQUIRED"
	// codeServerBusy: a playback would require a transcode but the server is at its
	// concurrent-transcode cap (ADR-0009), so it rejects rather than queues (503).
	// details carries { retryable: true, suggestedMaxBitrate } so the client can
	// retry at a lower quality. Direct play / remux never produce it.
	codeServerBusy = "SERVER_BUSY"
	// codeServiceUnavailable: a dependency needed for the request is not wired
	// (503). Today only the subtitle-fetch handlers use it, when the SubFetch
	// service is absent — a wiring gap, not a client fault.
	codeServiceUnavailable = "SERVICE_UNAVAILABLE"
	// Metadata-provider settings (Admin-scope /settings/metadata-providers,
	// metadata-providers 02). All 422 — the request was well-formed JSON but names
	// an unknown provider or an invalid configuration:
	//   codeProviderUnknown       — a slug not in the static provider registry.
	//   codeProviderKeyRequired   — enabling a key-requiring provider with no key
	//                               on file and none supplied in the request.
	//   codeProviderInvalidBaseURL — a base-URL override that is not a well-formed
	//                               absolute http(s) URL.
	//   codeProviderInvalidLanguage — a metadataLanguage set to the empty string.
	//   codeProviderInvalidSetting — a behavior knob (enrichIntervalSeconds /
	//                               musicBrainzRateLimitMs) given a negative value
	//                               (enrichment-runtime-settings).
	codeProviderUnknown         = "PROVIDER_UNKNOWN"
	codeProviderKeyRequired     = "PROVIDER_KEY_REQUIRED"
	codeProviderInvalidBaseURL  = "PROVIDER_INVALID_BASE_URL"
	codeProviderInvalidLanguage = "PROVIDER_INVALID_LANGUAGE"
	codeProviderInvalidSetting  = "PROVIDER_INVALID_SETTING"
	// codeProviderNotAuthoritative (422): a Library's Enrichment policy tried to point
	// its Authoritative provider at a slug that is not a USABLE Full provider of the
	// Library's kind — unknown, artwork-only, wrong-kind, or not yet keyed (ADR-0027).
	codeProviderNotAuthoritative = "PROVIDER_NOT_AUTHORITATIVE"

	// codeSearchUnavailable: an Edit-item provider search (Enrichment override,
	// ADR-0019) could not run — the authoritative provider for the item's kind is
	// unconfigured/disabled, or the source was unreachable. Returned 503 so the
	// Edit-item box reports why instead of hanging; the correction itself is
	// unaffected (item-editing/01).
	codeSearchUnavailable = "SEARCH_UNAVAILABLE"
	// codeWrongKind (422): a Wrong-item identity correction (PUT
	// /titles|shows/{id}/identityCorrection, ADR-0019) was requested on a kind that
	// has no folder-keyed identity anchor — an Episode, or a music leaf (Track).
	// Wrong-item is Movie/Show only; music identity is tag-anchored and Episodes have
	// no per-episode override anchor (item-editing/04).
	codeWrongKind = "WRONG_KIND"
	// Artwork upload validation (POST /…/artworkUpload, ADR-0026). An upload whose
	// sniffed type is not JPEG/PNG/WebP is 415; one over the 16 MiB cap is 413. Both
	// leave the current image unchanged so a bad file never blanks the artwork.
	codeUnsupportedMedia = "UNSUPPORTED_MEDIA_TYPE"
	codePayloadTooLarge  = "PAYLOAD_TOO_LARGE"
)

// decodeJSON reads the request body as JSON into dst. It returns false (after
// writing a BAD_REQUEST envelope) on malformed input, so handlers can early
// return. The body size is bounded to guard against oversized payloads.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON body", nil)
		return false
	}
	return true
}

// requireMethod wraps h so that requests using any other method receive the
// standard 405 envelope with an Allow header, rather than net/http's plain-text
// default. We dispatch method inside the handler because a catch-all "/" route
// otherwise shadows ServeMux's built-in method-mismatch handling.
func requireMethod(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
			return
		}
		h(w, r)
	}
}

// writeError serializes the standard error envelope with the given HTTP status.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorBody{Error: errorPayload{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// writeJSON writes v as JSON with the given status and the JSON content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Encoding into the response is best-effort: if it fails the status line is
	// already sent, so there's nothing more we can do but stop.
	_ = json.NewEncoder(w).Encode(v)
}
