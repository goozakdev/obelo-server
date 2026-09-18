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
	// The file matcher's four refusals (ADR-0044). All three are ACTIONABLE by
	// contract, because the screen they serve is a screen full of pending work and
	// a bare status leaves the Admin with no move:
	//   codeScanRunning   (409) — a scan holds the Library's lock (ADR-0031), so the
	//                             Apply refused and wrote NOTHING. Retry when it ends.
	//   codeSlotCollision (409) — the arrangement would resolve two distinct Titles
	//                             onto one Slot. details carries { slot: {group, slot},
	//                             paths: [...] } naming the Slot and every File
	//                             claiming it, so the screen can offer the three
	//                             fixes: merge them as parts, move one, unassign one.
	//                             Only ever a parse-vs-parse collision the matcher
	//                             did not create — a Placement's own collision is
	//                             settled by displacing the parsed File.
	//   codeOutsideShow   (422) — a decision named a File that does not live under
	//                             the container being rearranged. Refused whole: a
	//                             decision is stored per (library, path) with no
	//                             container on it, so accepting one would let an
	//                             Apply on one Show silently claim another's File.
	//   codeEmptySlot     (422) — a record was pinned onto a Slot no File fills. A
	//                             record decorates something, and an empty Slot has
	//                             no Title to carry the pin — so accepting it would
	//                             report success and store nothing.
	codeScanRunning   = "SCAN_RUNNING"
	codeSlotCollision = "SLOT_COLLISION"
	codeOutsideShow   = "OUTSIDE_SHOW"
	codeEmptySlot     = "EMPTY_SLOT"
	// codeNoFiles (409): a Targeted scan (ADR-0030) of an entity whose Files are all
	// Missing — there is nothing on disk to walk (hidden-entity resurrection is out
	// of scope for v1).
	codeNoFiles      = "NO_FILES"
	codeSetupClosed  = "SETUP_CLOSED"
	codeInvalidClaim = "INVALID_CLAIM_TOKEN"
	codeInvalidLogin = "INVALID_CREDENTIALS"
	// Device authorization grant (ADR-0036). The first four are the RFC 8628 poll
	// states, respelled into this envelope's SCREAMING_SNAKE vocabulary — the
	// state machine is the RFC's, the wire spelling is ours, because a client
	// switching on error.code should not have to know that four of the values it
	// matches came from a different document than the rest.
	//
	//   codeAuthorizationPending (400) — not approved yet; keep polling.
	//   codeSlowDown             (400) — polling faster than the granted interval.
	//   codeExpiredToken         (400) — the code aged out; start over on the TV.
	//   codeInvalidDeviceCode    (400) — no such device code, or already redeemed.
	//   codeInvalidUserCode      (404) — the human-typed code is not live. One
	//                                    answer for unknown/expired/used, so the
	//                                    live code space cannot be mapped by
	//                                    watching which reply comes back.
	//   codeTooManyAttempts      (429) — the CALLER is over one of its own limits:
	//                                    approve's brute-force counter, or the
	//                                    per-address quota on starting a flow.
	//                                    Carries Retry-After on the second.
	//   codeDeviceAuthBusy       (503) — the SERVER has no code space left. Not the
	//                                    caller's doing, and a retry may work at
	//                                    any moment; see device_auth_handlers.go
	//                                    for why these two are not one code.
	codeAuthorizationPending = "AUTHORIZATION_PENDING"
	codeSlowDown             = "SLOW_DOWN"
	codeExpiredToken         = "EXPIRED_TOKEN"
	codeInvalidDeviceCode    = "INVALID_DEVICE_CODE"
	codeInvalidUserCode      = "INVALID_USER_CODE"
	codeTooManyAttempts      = "TOO_MANY_ATTEMPTS"
	codeDeviceAuthBusy       = "DEVICE_AUTH_BUSY"
	// User-management (Admin-scope /users): a username collision and the
	// last-Admin guard each surface as a 409 with one of these codes.
	codeUsernameTaken = "USERNAME_TAKEN"
	codeLastAdmin     = "LAST_ADMIN"
	// codeRoleChange (422): a role change would cross the `remote` boundary — a
	// linked Server promoted to a person, or a person demoted to one (ADR-0054).
	// A remote User is created remote and dies remote. Reserved with its guard
	// (auth.CheckRoleChange) ahead of the role-change endpoint that will need it,
	// so the rule cannot be forgotten when that endpoint arrives.
	codeRoleChange = "ROLE_CHANGE"
	// Linking, the sharing side (ADR-0055, .scratch/linked-servers issue 03):
	//
	//   codeNotRemoteUser (422) — POST /users/{id}/invite named a User that is not
	//                             a linked Server. An invite is the `remote` role's
	//                             ONLY credential and no other role has any use for
	//                             one; minting for a Member would hand out a second,
	//                             passwordless way into a person's account.
	//   codeInvalidOrigin (422) — an origin in the mint body is not a bare absolute
	//                             http(s) address. Refused rather than repaired: a
	//                             path prefix or a typo becomes an invite that can
	//                             only fail on somebody else's machine, with nothing
	//                             on this side to explain why.
	//   codeInvalidInvite (400) — POST /auth/link/redeem presented a code that is
	//                             unknown, expired or already spent. ONE answer for
	//                             all three (and for a missing server id), so the
	//                             live invite space cannot be mapped by watching
	//                             which reply comes back — the same collapse
	//                             INVALID_USER_CODE makes.
	//   codeLinkProtocol  (409) — the two Servers stamp different
	//                             linkProtocolVersions. details carries
	//                             { supported, requested } — this server's and the
	//                             caller's — so the redeeming side can say which of
	//                             the two needs an upgrade (ADR-0055 §3). Checked
	//                             BEFORE anything is redeemed, so a mismatch never
	//                             costs the Admin their invite.
	//   codeResync        (410) — GET /libraries/{id}/export was handed a `since`
	//                             older than the retention of soft-deleted rows, so
	//                             the sharer can no longer promise the feed still
	//                             carries every tombstone the mirror missed. The
	//                             home Server answers it with a full pull
	//                             (ADR-0056 §4). A 410 and not a 400: the request
	//                             was well formed and it is the POSITION that is
	//                             gone.
	codeNotRemoteUser = "NOT_REMOTE_USER"
	codeInvalidOrigin = "INVALID_ORIGIN"
	codeInvalidInvite = "INVALID_INVITE"
	codeLinkProtocol  = "LINK_PROTOCOL"
	codeResync        = "RESYNC"
	// Linking, the RECEIVING side (ADR-0055 §2–§5, ADR-0056 §6,
	// .scratch/linked-servers issue 06) — the Admin who pastes the string:
	//
	//   codeBadInvite         (400) — POST /links (or /rekey) was handed something
	//                                 that is not a readable invite: the wrong
	//                                 scheme, base64 that does not decode, a field
	//                                 missing. ONE code for every shape failure,
	//                                 because the operator's move is the same in all
	//                                 of them — ask for the string again. It also
	//                                 carries the sharer's own INVALID_INVITE
	//                                 refusal, which means the string was fine and
	//                                 the CODE is spent or gone.
	//   codeInviteExpired     (410) — a well-formed invite whose 24 hours ran out
	//                                 (ADR-0055 §1). Gone rather than Bad Request
	//                                 because nothing about the request was wrong;
	//                                 the thing it names is no longer there.
	//   codeLinkUnreachable   (503) — none of the addresses in the invite answered,
	//                                 over either dialer (ADR-0055 §5). Not the
	//                                 caller's fault and retryable, which is what
	//                                 separates it from every 4xx here. ADR-0056 §6
	//                                 reuses it for a play against an unreachable
	//                                 Link.
	//   codeLinkRevoked       (409) — POST /links/{id}/sync reached the sharer and was
	//                                 told the credential is dead (their 401). The
	//                                 mirror stays; the fix is a fresh invite, not a
	//                                 retry, which is why it is a conflict and not
	//                                 the 503 an unreachable Link gets (ADR-0056 §6).
	//   codeLinkServerMismatch (409) — POST /links/{id}/rekey was given an invite
	//                                 for a DIFFERENT Server. A Link is bound to one
	//                                 peer for its whole life (the mirror is keyed by
	//                                 that Server's ids), so this is refused rather
	//                                 than silently repointed.
	//
	// LINK_PROTOCOL above is shared with the sharing side and answered here too,
	// with details named from THIS side — { theirs, ours, upgrade } — because the
	// asking Server is the one that has to say which household needs an upgrade.
	codeBadInvite          = "BAD_INVITE"
	codeInviteExpired      = "INVITE_EXPIRED"
	codeLinkUnreachable    = "LINK_UNREACHABLE"
	codeLinkServerMismatch = "LINK_SERVER_MISMATCH"
	codeLinkRevoked        = "LINK_REVOKED"
	// codeLinkedLibrary (409): a write aimed at a Library that is a MIRROR of
	// another household's (ADR-0056 §1). Not 403 and not 404: the caller is an
	// Admin, the Library is theirs to see and grant, and the resource plainly
	// exists — it is the STATE of it that refuses, which is what Conflict means.
	// The sharer's Server is the identity authority for its own files (ADR-0002,
	// ADR-0019), so nothing here may re-derive, correct or enrich what arrived; a
	// correction belongs on the machine that owns the files.
	//
	// The one exception is renaming: PATCH /libraries/{id} still takes a `name`,
	// because what this household calls the shelf is this household's business.
	codeLinkedLibrary = "LINKED_LIBRARY"
	// Library-access grants (PUT /users/{id}/libraryAccess), both 422: granting to
	// an Admin, and naming a Library that does not exist.
	codeAdminGrant     = "ADMIN_GRANT"
	codeUnknownLibrary = "UNKNOWN_LIBRARY"
	// codeLinkedGrant (422): the target of a grant is a `remote` User (a linked
	// Server) and the set names a Library that itself arrived over a Link. A
	// mirror is never re-shared onward (ADR-0054 §4, ADR-0056 §7) — the owner of
	// the files decided who sees them. Same shape as UNKNOWN_LIBRARY: the whole
	// set is rejected and the prior grants stand. Distinct from LINKED_LIBRARY
	// (409), which refuses a WRITE to the mirror itself; nothing is being written
	// to the Library here, and the Library is not in conflict — the pairing of it
	// with this User is what cannot exist.
	codeLinkedGrant = "LINKED_GRANT"
	// Rating ceiling (PUT /users/{id}/ratingCeiling), both 422: setting a ceiling
	// on an Admin, and an unknown rating label.
	codeAdminCeiling  = "ADMIN_CEILING"
	codeUnknownRating = "UNKNOWN_RATING"
	// codeUnknownResolution (422): a Playback ceiling
	// (PUT /users/{id}/playbackCeiling) named a maxResolution that is not a
	// settable rung (ADR-0054 §2). ADMIN_CEILING is reused for an Admin target —
	// the two ceilings refuse an Admin for the same reason, and the message says
	// which one was refused.
	codeUnknownResolution = "UNKNOWN_RESOLUTION"
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
	// codeStreamLimit: the User already holds as many unended Playback sessions as
	// their Playback ceiling's maxStreams allows (429, ADR-0054 §2). details carries
	// { active, limit } so the client can say "2 of 2 streams in use". Distinct from
	// SERVER_BUSY in both cause and cure: SERVER_BUSY is the server-wide transcode
	// budget and a lower bitrate may get in, whereas this is the User's own cap and
	// only ending one of their streams frees a slot — so a retry at any quality
	// fails identically. Every tier counts, direct play included.
	codeStreamLimit = "STREAM_LIMIT"
	// codeServiceUnavailable: a dependency needed for the request is not wired
	// (503). Today only the subtitle-fetch handlers use it, when the SubFetch
	// service is absent — a wiring gap, not a client fault.
	codeServiceUnavailable = "SERVICE_UNAVAILABLE"
	// Starting a background Enrichment pass (POST /libraries/{id}/enrich), both 503.
	// They exist because both states used to be SILENT — the enqueue was a no-op
	// with no worker and a log-and-drop when full — so an operator pressing the
	// "Re-check unmatched items" button got a spinner and nothing else, forever.
	// ADR-0051's amendment: a press that started nothing must say so.
	//   codeEnrichUnavailable — no background pass worker is running on this server,
	//                           so nothing would ever pick the pass up.
	//   codeEnrichBusy        — the pass queue is at capacity. Retryable: the caller
	//                           can press it again shortly.
	codeEnrichUnavailable = "ENRICH_UNAVAILABLE"
	codeEnrichBusy        = "ENRICH_BUSY"
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
	// Installing an Installed plugin (ADR-0058, .scratch/plugin-system issue 10).
	// FIVE codes rather than one, because they are five different things for the
	// Admin to do next and a single PLUGIN_REFUSED would make the screen say "it
	// did not work" five times in the same words:
	//
	//   codePluginInvalidManifest (422) — no manifest, not JSON, or a manifest
	//                               claiming something a manifest may not claim.
	//   codePluginAPIVersion (422)  — an apiVersion this server does not speak. Its
	//                               message ALWAYS names WHICH SIDE to upgrade
	//                               (ADR-0058 decision 8, the ADR-0055 posture).
	//   codePluginDuplicate (409)   — the id is already claimed, by an Installed
	//                               plugin or by a Built-in this binary ships.
	//   codePluginInvalidModule (422) — no module, or one that will not compile or
	//                               instantiate in this server's sandbox.
	//   codePluginSourceRefused (422) — a pasted URL this server will not fetch a
	//                               plugin from (not absolute http(s), unresolvable,
	//                               resolving into this server's own network) or one
	//                               that answered with something unusable.
	//
	// codePluginUnknown (404) is the lifecycle half: enable / disable / re-enable /
	// uninstall naming a Plugin that is not installed.
	//
	// codePluginInvalidSettings (400) is the manifest-declared settings schema
	// (.scratch/plugin-system issue 13): a save that did not satisfy the fields the
	// Plugin's own manifest declared — a required one left empty, an enum value
	// outside the declared set, an integer out of range, a URL that is not one. It
	// is the one plugin refusal that is PER FIELD, so its details carry
	// { fields: [ { key, message } ] } and the message restates the first of them,
	// which is what lets the form put each sentence under the control that caused
	// it instead of one prose line above the whole panel.
	//
	// codePluginSignature (422) is the pinned-publisher policy (.scratch/plugin-
	// system issue 15): this server has publisher keys pinned and the plugin does
	// not satisfy them. A SERVER WITH NOTHING PINNED NEVER PRODUCES IT — that is
	// the shipped state, and in it nothing about installing changes. Its message
	// always names the publisher the plugin CLAIMED, because "no signature at all",
	// "a publisher nobody pinned", "a signature over other files" and "a signature
	// that does not verify" want four different things done about them and the
	// claimed name is what tells them apart.
	codePluginInvalidManifest = "PLUGIN_INVALID_MANIFEST"
	codePluginAPIVersion      = "PLUGIN_API_VERSION"
	codePluginDuplicate       = "PLUGIN_DUPLICATE"
	codePluginInvalidModule   = "PLUGIN_INVALID_MODULE"
	codePluginSourceRefused   = "PLUGIN_SOURCE_REFUSED"
	codePluginUnknown         = "PLUGIN_UNKNOWN"
	codePluginInvalidSettings = "PLUGIN_INVALID_SETTINGS"
	codePluginSignature       = "PLUGIN_SIGNATURE"

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
	// Tailnet remote-access settings (Admin-scope /settings/tailscale, ADR-0043).
	// Both 422 — well-formed JSON naming a value that cannot work:
	//   codeTailnetInvalidHostname — a hostname that is not a legal DNS label. It
	//                               is validated HERE, strictly, because it becomes
	//                               one at join time inside somebody else's code,
	//                               where the failure is a message written for
	//                               somebody else's user.
	//   codeTailnetInvalidControlURL — a coordination-server URL that is not a
	//                               well-formed absolute http(s) URL.
	codeTailnetInvalidHostname   = "TAILNET_INVALID_HOSTNAME"
	codeTailnetInvalidControlURL = "TAILNET_INVALID_CONTROL_URL"
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
