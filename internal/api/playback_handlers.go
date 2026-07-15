package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marioquake/juicebox/internal/audio"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/playback"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// HTTP transport for the direct-play tier (ADR-0003 tier 1, ADR-0004 progressive
// byte-range). Three routes, all behind requireAuth:
//
//   POST   /api/v1/titles/{id}/playback   negotiate → directPlay decision + session
//   GET    /api/v1/sessions/{id}/stream   progressive byte-range stream of the File
//   DELETE /api/v1/sessions/{id}          end the session
//
// Auth on the stream URL: the bearer header OR the ms_media cookie on GET
// .../stream (a browser <video> cannot set a header; native clients do). An
// EXTERNAL player that can carry neither — VLC, opened on a downloaded .xspf —
// is served instead by the sessionless GET /files/{id}/download, which accepts a
// ?token= query param (see file_handlers.go). Because the stream endpoint is
// plain auth, a User streaming another User's session is hidden as a 404
// (existence-hiding posture, api-contract.md), not a 403.

// --- request shape ----------------------------------------------------------

// playbackRequest is the body of POST /titles/{id}/playback. The Capability
// profile is carried inline (deviceProfile + constraints) — the clientId-
// referenced-profile optimization from the api-contract is deferred to a later
// slice (documented in the playback package). deviceProfile is optional; an empty
// profile cannot direct-play or remux, so it falls through to the transcode tier
// (the server's universal h264/aac HLS fallback) unless the File is structurally
// unplayable (no video stream), which stays an honest error.
type playbackRequest struct {
	DeviceProfile *deviceProfileJSON `json:"deviceProfile"`
	Constraints   *constraintsJSON   `json:"constraints"`
	StartPosition int64              `json:"startPosition"`
	EditionID     string             `json:"editionId"`
	// BurnSubtitleID selects an IMAGE Subtitle track to burn into the video frames
	// (ADR-0020, subtitles/04). It escalates the decision to the transcode tier
	// (which is governed and can return 503 SERVER_BUSY) with the sub burned in. The
	// client sends it both on the initial request (a pre-selected image sub) and on
	// re-negotiation when the viewer picks an image track from the captions menu. An
	// id that is not a burnable image track of the Title is 404.
	BurnSubtitleID string `json:"burnSubtitleId"`
	// AudioStreamID selects the audio Stream to deliver (audio-streams/02, ADR-0022),
	// exactly parallel to BurnSubtitleID: a fresh negotiation, never a session-mutate.
	// The Decision reports that Stream and the delivered bytes carry it. A non-default
	// selection escalates the tier to remux; a codec the client can't decode escalates
	// to a governed transcode (503 SERVER_BUSY at the cap). The client sends it on the
	// initial request (a remembered/pre-selected track) and on the direct-play
	// re-negotiation when the viewer picks a non-default track from the Audio menu. An
	// id that is not an audio Stream of the Title is 404.
	AudioStreamID string `json:"audioStreamId"`
	// VideoStreamID selects the video Stream to deliver (selectable-video/02, ADR-0025),
	// the video parallel of AudioStreamID but following the image-subtitle RESTART model:
	// there is no in-band video rendition, so a non-default pick is a full re-negotiation
	// that escalates to HLS remux (mapping the chosen Stream) — a codec the client can't
	// decode escalates to a governed transcode (503 SERVER_BUSY at the cap; NOT
	// cap-exempt). The switch preserves audioStreamId, burnSubtitleId, and startPosition.
	// The client sends it when the viewer picks a cut from the Video menu; a pick equal to
	// the default keeps the current tier. An id that is not a selectable video Stream of
	// the Title is 404.
	VideoStreamID string `json:"videoStreamId"`
}

type deviceProfileJSON struct {
	Containers          []string         `json:"containers"`
	VideoCodecs         []videoCodecJSON `json:"videoCodecs"`
	AudioCodecs         []string         `json:"audioCodecs"`
	MaxAudioChannels    int              `json:"maxAudioChannels"`
	TextSubtitleFormats []string         `json:"textSubtitleFormats"`
	// HevcInMpegTS: the client (an hls.js/MSE player) accepts a copied HEVC video in
	// MPEG-TS HLS segments; absent/false keeps HEVC on fMP4 (Apple's native player).
	HevcInMpegTS bool `json:"hevcInMpegts"`
}

type videoCodecJSON struct {
	Codec         string   `json:"codec"`
	MaxLevel      string   `json:"maxLevel"`
	MaxResolution string   `json:"maxResolution"`
	HDR           []string `json:"hdr"`
}

type constraintsJSON struct {
	MaxBitrate            int64  `json:"maxBitrate"`
	MaxResolution         string `json:"maxResolution"`
	PreferredAudioLang    string `json:"preferredAudioLang"`
	PreferredSubtitleLang string `json:"preferredSubtitleLang"`
}

func (p *deviceProfileJSON) toDomain() playback.DeviceProfile {
	if p == nil {
		return playback.DeviceProfile{}
	}
	codecs := make([]playback.VideoCodecSupport, 0, len(p.VideoCodecs))
	for _, v := range p.VideoCodecs {
		codecs = append(codecs, playback.VideoCodecSupport{
			Codec:         v.Codec,
			MaxLevel:      v.MaxLevel,
			MaxResolution: v.MaxResolution,
			HDR:           v.HDR,
		})
	}
	return playback.DeviceProfile{
		Containers:          p.Containers,
		VideoCodecs:         codecs,
		AudioCodecs:         p.AudioCodecs,
		MaxAudioChannels:    p.MaxAudioChannels,
		TextSubtitleFormats: p.TextSubtitleFormats,
		HevcInMpegTS:        p.HevcInMpegTS,
	}
}

// textSubtitleFormats returns the profile's declared text-subtitle formats,
// nil-safe: an absent profile declares none, so every text track's delivery URL
// falls back to the WebVTT conversion (ADR-0033).
func (p *deviceProfileJSON) textSubtitleFormats() []string {
	if p == nil {
		return nil
	}
	return p.TextSubtitleFormats
}

func (c *constraintsJSON) toDomain() playback.Constraints {
	if c == nil {
		return playback.Constraints{}
	}
	return playback.Constraints{
		MaxBitrate:            c.MaxBitrate,
		MaxResolution:         c.MaxResolution,
		PreferredAudioLang:    c.PreferredAudioLang,
		PreferredSubtitleLang: c.PreferredSubtitleLang,
	}
}

// --- decision response shape ------------------------------------------------

type decisionEditionJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type decisionStreamJSON struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Channels int    `json:"channels,omitempty"`
}

// decisionSubtitleJSON is one selectable Subtitle track on a playback decision
// (ADR-0020), the same shape as the catalog's subtitleTrackJSON plus a delivery
// URL. id selects the track; source is embedded|sidecar|fetched; kind is
// text|image; language is ISO-639-1 ("" = Unknown); label is the ready menu
// string. url is the out-of-band WebVTT endpoint for a deliverable TEXT track —
// the client drops it into a <track> (direct play) or, on the HLS tiers, uses it
// until the in-band rendition lands (slice 03). Image tracks (burn-in, slice 04)
// and any text format we can't convert carry no url.
type decisionSubtitleJSON struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Language string `json:"language,omitempty"`
	Forced   bool   `json:"forced"`
	Label    string `json:"label"`
	URL      string `json:"url,omitempty"`
	// Format is the delivery format of the bytes at url — "vtt" (the universal
	// WebVTT conversion) or, when the client's Capability profile declares support
	// for the track's original format (textSubtitleFormats, ADR-0033), the original
	// "srt"/"ass" with styling intact (libmpv renders ASS natively). Absent when
	// there is no url.
	Format string `json:"format,omitempty"`
}

// decisionResponse is the negotiation decision (api-contract.md). streamUrl is the
// session-scoped progressive byte-range URL (direct play) or HLS playlist URL.
// subtitles is every selectable Subtitle track for the played File (ADR-0020),
// replacing the old thin subtitle:{mode,url}; non-nil, empty when the File has none.
type decisionResponse struct {
	SessionID   string              `json:"sessionId"`
	Tier        string              `json:"tier"`
	StreamURL   string              `json:"streamUrl"`
	Edition     decisionEditionJSON `json:"edition"`
	VideoStream decisionStreamJSON  `json:"videoStream"`
	AudioStream *decisionStreamJSON `json:"audioStream,omitempty"`
	// AudioStreams is every selectable audio Stream the played File offers, labeled
	// for the Audio menu (audio-streams/02) — the same projection the catalog exposes
	// per File. audioStream above is the resolved one the delivery carries; this is
	// the full list a client builds its menu from. Non-nil; empty for a silent File.
	AudioStreams []audioStreamJSON `json:"audioStreams"`
	// VideoStreams is every selectable video Stream the played File offers, labeled for
	// the Video menu (selectable-video/01) — the same projection the catalog exposes,
	// but with the default flag re-marked to the capability-then-quality pick the
	// decision resolved (videoStream above), not the container disposition. Non-nil; a
	// single-video File carries a one-element list, so the client shows the Video menu
	// only when there are ≥2.
	VideoStreams     []videoStreamJSON      `json:"videoStreams"`
	Subtitles        []decisionSubtitleJSON `json:"subtitles"`
	EstimatedBitrate int64                  `json:"estimatedBitrate"`
}

// --- handlers ---------------------------------------------------------------

// handleTitleSubtree dispatches every route under "/titles/{id}...". It applies
// auth PER LEAF rather than relying on an outer requireAuth, because one leaf —
// GET {id}/artwork/{role} — must also accept the media cookie (a browser <img>
// cannot send an Authorization header), while every other leaf stays bearer-only
// (requireAuth). POST {id}/playback is the direct-play negotiation; PUT
// {id}/watchState toggles watch state; the artwork leaf is the cookie-capable
// media GET; GET {id} is the browse/read surface. Routing by sub-resource keeps
// the playback POST from being shadowed by the GET-only get-Title handler.
func handleTitleSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/titles/")
		if strings.HasSuffix(rest, "/playback") {
			requireMethod(http.MethodPost, requireAuth(deps.Auth, requireScope(deps.Access, handlePlayback(deps.Playback))))(w, r)
			return
		}
		if id, ok := strings.CutSuffix(rest, "/watchState"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireScope(deps.Access, handleSetWatchState(deps.Playback, deps.Catalog, id))))(w, r)
			return
		}
		// DELETE {id}/metadata/locks/{field}: release a Locked field back to auto
		// (Admin). Matched before /metadata so the longer suffix isn't shadowed.
		if i := strings.Index(rest, "/metadata/locks/"); i > 0 {
			titleID := rest[:i]
			field := rest[i+len("/metadata/locks/"):]
			if strings.Contains(titleID, "/") || field == "" || strings.Contains(field, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodDelete,
				requireAuth(deps.Auth, requireAdmin(handleReleaseLock(deps.Catalog, titleID, field))))(w, r)
			return
		}
		// POST {id}/scan: Targeted scan of this Movie's folder / bare file (Admin,
		// ADR-0030). Offered on the Movie detail page; Episode/Track leaves scan via
		// their Show/Album, so an all-Files-Missing or non-foldered leaf just 409s.
		if id, ok := strings.CutSuffix(rest, "/scan"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleTargetedScan(deps, "title", id))))(w, r)
			return
		}
		// POST {id}/review: dismiss this Title's needs_review flag — the Admin
		// confirms the uncertain identity parse is fine (Movie / Episode / Track).
		if id, ok := strings.CutSuffix(rest, "/review"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleReviewTitle(deps.Catalog, id))))(w, r)
			return
		}
		// PUT {id}/identityCorrection: the Wrong-item destructive correction (Admin,
		// ADR-0019). Re-identifies a Movie as a different work: folder-keyed Match
		// override + re-key + watch-state reset + Locked-field clear + re-enrich. A
		// non-Movie leaf (Episode / Track) is rejected 422 WRONG_KIND by the handler.
		if id, ok := strings.CutSuffix(rest, "/identityCorrection"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handleTitleIdentityCorrection(deps, id))))(w, r)
			return
		}
		// GET {id}/enrichmentCandidates?q=: search the authoritative provider for the
		// Enrichment-override picker (Admin, ADR-0019). Matched before /enrichmentMatch
		// (distinct suffix); read-only, never touches identity.
		if id, ok := strings.CutSuffix(rest, "/enrichmentCandidates"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodGet,
				requireAuth(deps.Auth, requireAdmin(handleEnrichmentCandidates(deps.Enrich))))(w, r)
			return
		}
		// GET {id}/externalPreview?ref=: preview a pasted MusicBrainz/TMDB id-or-URL
		// before applying it (the paste escape hatch, item-editing/search-improvements).
		// Matched before /enrichmentOverride (distinct suffix); read-only.
		if id, ok := strings.CutSuffix(rest, "/externalPreview"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodGet,
				requireAuth(deps.Auth, requireAdmin(handleTitleExternalPreview(deps.Enrich))))(w, r)
			return
		}
		// PUT {id}/enrichmentOverride: apply a picked candidate as a durable Enrichment
		// override + re-enrich JUST this Title (Admin, ADR-0019). Never touches
		// identity_key or watch state; emits a libraryUpdated SSE nudge.
		if id, ok := strings.CutSuffix(rest, "/enrichmentOverride"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handleEnrichmentOverride(deps.Enrich, deps.Catalog, deps.Events))))(w, r)
			return
		}
		// PUT {id}/enrichmentMatch: re-point the external metadata match + re-enrich
		// JUST this Title (Admin). Distinct from identity fix-match — it never touches
		// identity_key or watch state (ADR-0002/0014).
		if id, ok := strings.CutSuffix(rest, "/enrichmentMatch"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handleEnrichmentMatch(deps.Enrich, deps.Catalog, deps.Events))))(w, r)
			return
		}
		// PUT {id}/metadata: hand-edit + Lock descriptive fields (Admin).
		if id, ok := strings.CutSuffix(rest, "/metadata"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handleEditMetadata(deps.Catalog))))(w, r)
			return
		}
		// GET {id}/artworkCandidates?role=: list the provider images for a role so the
		// Admin can pick one (Fix label image picker, ADR-0019). Matched before the
		// /artwork/ media GET (distinct suffix); read-only.
		if id, ok := strings.CutSuffix(rest, "/artworkCandidates"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodGet,
				requireAuth(deps.Auth, requireAdmin(handleTitleArtworkCandidates(deps.Enrich))))(w, r)
			return
		}
		// PUT {id}/artwork: apply a picked provider image to a role + Lock the role
		// (Admin). Matched before the /artwork/ media GET (this has no trailing role).
		if id, ok := strings.CutSuffix(rest, "/artwork"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut,
				requireAuth(deps.Auth, requireAdmin(handlePickTitleArtwork(deps.Enrich, deps.Catalog, deps.Events))))(w, r)
			return
		}
		// POST {id}/artworkUpload?role=…: store an Admin-uploaded image as a role +
		// Lock it (ADR-0026, upload-is-select — the server's first multipart handler).
		// Distinct suffix from /artwork and the /artwork/ media GET, so it isn't
		// shadowed. Admin-only.
		if id, ok := strings.CutSuffix(rest, "/artworkUpload"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireAdmin(handleUploadTitleArtwork(deps.Enrich, deps.Catalog, deps.Events))))(w, r)
			return
		}
		// POST {id}/subtitles/search: "search online" for a subtitle in a language the
		// Title lacks (ADR-0021). Available to ANY authenticated User including a Member
		// (a deliberate widening of the browse+play Member role) — requireAuth +
		// requireScope, NOT requireAdmin. Matched before the /subtitles/{subId}.vtt
		// dispatcher (distinct suffix) so it isn't 404'd as a bad .vtt id.
		if id, ok := strings.CutSuffix(rest, "/subtitles/search"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireScope(deps.Access, handleSubtitleSearch(deps, id))))(w, r)
			return
		}
		// POST {id}/subtitles/fetch: download + persist a chosen candidate as a
		// fetched track (ADR-0021). Any User (Member included) — the pick locks the
		// choice and the new text track appears in the decision like any other.
		if id, ok := strings.CutSuffix(rest, "/subtitles/fetch"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireScope(deps.Access, handleSubtitleFetch(deps, id))))(w, r)
			return
		}
		// GET {id}/subtitles/{subId}.vtt: the out-of-band WebVTT media GET a browser
		// reaches via a <track> src — bearer OR media cookie, like the artwork leaf.
		// Matched before /artwork/ (distinct suffix) so it isn't shadowed by the
		// bare-{id} fallthrough.
		if h, ok := dispatchTitleSubtitle(deps, rest); ok {
			h(w, r)
			return
		}
		// GET {id}/artwork/{role}: the read-only media GET a browser reaches via
		// <img src> — bearer OR media cookie. handleGetTitle dispatches the artwork
		// sub-resource itself, so route it through the cookie-capable middleware.
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			requireMethod(http.MethodGet, requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleGetTitle(deps.Catalog))))(w, r)
			return
		}
		// GET {id}: the JSON detail surface — bearer-only.
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleGetTitle(deps.Catalog))))(w, r)
	}
}

// watchStateRequest is the body of PUT /titles/{id}/watchState: a manual
// watched/unwatched toggle that BYPASSES the ~90% threshold (api-contract.md).
type watchStateRequest struct {
	Watched bool `json:"watched"`
}

// handleSetWatchState manually toggles a Title's watched/unwatched state for the
// caller, bypassing the Watched threshold. It first confirms the Title exists
// (and is visible) so an unknown id is 404 (hide existence) rather than a silent
// write to nothing. Marking watched clears the resume; marking unwatched resets
// both. Returns the resolved watch state.
func handleSetWatchState(svc *playback.Service, cat *catalog.Service, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		var req watchStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		// Existence/visibility check: a watch-state write must target a real Title.
		if _, err := cat.GetTitle(scope, titleID); err != nil {
			if errors.Is(err, catalog.ErrNotFound) {
				writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set watch state", nil)
			return
		}
		if err := svc.SetWatchState(id.User.ID, titleID, req.Watched); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set watch state", nil)
			return
		}
		ws, err := cat.WatchStateFor(id.User.ID, titleID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to set watch state", nil)
			return
		}
		writeJSON(w, http.StatusOK, progressResponse{
			TitleID:          titleID,
			ResumePositionMs: ws.ResumePositionMs,
			Watched:          ws.Watched,
		})
	}
}

// handlePlayback negotiates a Capability profile against a Title's best playable
// Edition, creates a Playback session, and returns the decision with a streamUrl:
// a progressive byte-range URL for directPlay, or an HLS media-playlist URL for
// the directStream (remux) and transcode tiers. A File that is structurally
// unplayable (no video stream) returns the structured TRANSCODE_REQUIRED error
// (501-class). Unknown Title (or all-Missing) → 404 (hide existence).
func handlePlayback(svc *playback.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID := pathParam(r.URL.Path, "/titles/", "/playback")
		if titleID == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}

		var req playbackRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		dec, sess, unsup, busy, err := svc.Negotiate(playback.Request{
			UserID:         id.User.ID,
			DeviceID:       id.Device.ID,
			TitleID:        titleID,
			Profile:        req.DeviceProfile.toDomain(),
			Constraints:    req.Constraints.toDomain(),
			StartPosition:  req.StartPosition,
			EditionID:      req.EditionID,
			BurnSubtitleID: req.BurnSubtitleID,
			AudioStreamID:  req.AudioStreamID,
			VideoStreamID:  req.VideoStreamID,
			Scope:          scope,
		})
		switch {
		case errors.Is(err, playback.ErrTitleNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case errors.Is(err, playback.ErrBurnSubtitleNotFound):
			// The burnSubtitleId did not resolve to a burnable image track — hide the
			// distinction (unknown id / text track / audio-only File) behind a 404.
			writeError(w, http.StatusNotFound, codeNotFound, "subtitle not found", nil)
			return
		case errors.Is(err, playback.ErrAudioStreamNotFound):
			// The audioStreamId named no audio Stream of the Title (unknown/stale id, or
			// one from another File) — hide existence behind a 404 rather than silently
			// delivering the default audio (audio-streams/02).
			writeError(w, http.StatusNotFound, codeNotFound, "audio stream not found", nil)
			return
		case errors.Is(err, playback.ErrVideoStreamNotFound):
			// The videoStreamId named no selectable video Stream of the Title (unknown/stale
			// id, a cover-art still, or one from another File) — hide existence behind a 404
			// rather than silently delivering the default video (selectable-video/02).
			writeError(w, http.StatusNotFound, codeNotFound, "video stream not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "playback negotiation failed", nil)
			return
		case unsup != nil:
			// 501-class: the server understood the request but this tier cannot
			// satisfy it (no remux/transcode in this slice). The reason + detail let
			// the client explain why and a later tier know what to convert.
			writeError(w, http.StatusNotImplemented, codeTranscodeRequired,
				"direct play not possible for this client; a transcode would be required",
				map[string]any{
					"reason": string(unsup.Reason),
					"detail": unsup.Detail,
				})
			return
		case busy != nil:
			// 503 SERVER_BUSY (ADR-0009): a transcode would be required but the server
			// is at its concurrent-transcode cap. Reject-don't-queue — the client may
			// retry at suggestedMaxBitrate (a lower quality that may direct-play/remux
			// or cost less to transcode). Direct play / remux never reach here.
			writeError(w, http.StatusServiceUnavailable, codeServerBusy,
				"server is at its transcode capacity; retry at a lower bitrate",
				map[string]any{
					"retryable":           true,
					"suggestedMaxBitrate": busy.SuggestedMaxBitrate,
				})
			return
		}

		writeJSON(w, http.StatusOK, toDecisionResponse(sess.ID, titleID, dec, req.DeviceProfile.textSubtitleFormats()))
	}
}

func toDecisionResponse(sessionID, titleID string, d playback.Decision, clientSubtitleFormats []string) decisionResponse {
	// The streamUrl depends on the tier (ADR-0004): direct play is a progressive
	// byte-range stream; directStream (remux) and transcode are an HLS playlist the
	// client loads with hls.js / native HLS. On the HLS tiers, when the File offers
	// at least one deliverable text subtitle, the client is pointed at a MASTER
	// playlist (one video rendition + an in-band SUBTITLES group, ADR-0020 / the
	// ADR-0004 amendment) instead of the bare media playlist; with no deliverable
	// text subtitle the media playlist is served directly, unchanged.
	streamURL := APIPrefix + "/sessions/" + sessionID + "/stream"
	if d.Tier != playback.TierDirectPlay {
		file := playbackHLSPlaylist
		// The client is pointed at the MASTER playlist when the session carries an
		// in-band group: a demuxed multi-audio AUDIO group (audio-streams/03) and/or a
		// deliverable text SUBTITLES group (ADR-0020). With neither, the bare video media
		// playlist is served directly, unchanged.
		if playback.IsDemuxed(d) || hasDeliverableTextSubtitle(d.Subtitles) {
			file = playbackHLSMaster
		}
		streamURL = APIPrefix + "/sessions/" + sessionID + "/hls/" + file
	}
	resp := decisionResponse{
		SessionID: sessionID,
		Tier:      string(d.Tier),
		StreamURL: streamURL,
		Edition:   decisionEditionJSON{ID: d.Edition.ID, Name: d.Edition.Name},
		VideoStream: decisionStreamJSON{
			Index:    d.VideoStream.Index,
			Codec:    d.VideoStream.Codec,
			Language: d.VideoStream.Language,
			Width:    d.VideoStream.Width,
			Height:   d.VideoStream.Height,
		},
		AudioStreams: toAudioStreams(d.File.Streams),
		// Re-flag the default to the resolved video Stream (the capability-then-quality
		// pick), which may differ from the container is_default disposition on a multi-
		// video File (selectable-video/01).
		VideoStreams:     toVideoStreams(d.File.Streams, d.VideoStream.ID),
		Subtitles:        toDecisionSubtitles(titleID, d.Subtitles, clientSubtitleFormats),
		EstimatedBitrate: d.EstimatedBitrate,
	}
	// An audio Stream is present for every real Movie File; omit it only for the
	// (rare) silent file so the response stays honest.
	if d.AudioStream.Kind == "audio" || d.AudioStream.Codec != "" {
		resp.AudioStream = &decisionStreamJSON{
			Index:    d.AudioStream.Index,
			Codec:    d.AudioStream.Codec,
			Language: d.AudioStream.Language,
			Channels: d.AudioStream.Channels,
		}
	}
	return resp
}

// toDecisionSubtitles maps the domain Subtitle-track list onto the decision wire
// shape, attaching the out-of-band delivery URL to each deliverable TEXT track
// (identity-scoped, cacheable — mirrors the artwork endpoint). The URL's format is
// negotiated per track (ADR-0033): a client whose Capability profile declares the
// track's ORIGINAL format in textSubtitleFormats gets the original (.srt/.ass,
// styling intact — libmpv territory); everyone else gets the WebVTT conversion.
// Image tracks and any text track we can't convert carry no url (burn-in /
// unsupported-format is a later concern). Always non-nil so the JSON is a `[]`,
// never null.
func toDecisionSubtitles(titleID string, tracks []playback.SubtitleTrack, clientFormats []string) []decisionSubtitleJSON {
	accepts := map[string]bool{}
	for _, f := range clientFormats {
		// Normalize the client's tokens the same way track codecs fold ("subrip"→
		// srt, "ssa"→ass, "webvtt"→vtt) so a human-written profile matches.
		if canon := subtitle.TextFormat(f); canon != "" {
			accepts[canon] = true
		}
	}
	out := make([]decisionSubtitleJSON, 0, len(tracks))
	for _, t := range tracks {
		entry := decisionSubtitleJSON{
			ID:       t.ID,
			Source:   t.Source,
			Kind:     t.Kind,
			Language: t.Language,
			Forced:   t.Forced,
			Label:    subtitle.Label(t.Language, t.Forced),
		}
		if t.Kind == "text" && t.Convertible {
			if t.Format != "" && t.Format != "vtt" && accepts[t.Format] {
				entry.URL = subtitleOriginalURL(titleID, t.ID, t.Format)
				entry.Format = t.Format
			} else {
				entry.URL = subtitleVTTURL(titleID, t.ID)
				entry.Format = "vtt"
			}
		}
		out = append(out, entry)
	}
	return out
}

// handleSessionSubtree dispatches the /sessions/{id}... routes, applying auth PER
// LEAF rather than relying on an outer requireAuth, because one leaf — GET
// {id}/stream — must also accept the media cookie (a browser <video> cannot send
// an Authorization header), while POST {id}/progress and DELETE {id} stay
// bearer-only (requireAuth). GET {id}/stream serves the File bytes; POST
// {id}/progress reports progress + keepalive; DELETE {id} ends the session.
// Method/shape mismatches get the standard envelopes.
func handleSessionSubtree(deps Deps) http.HandlerFunc {
	svc := deps.Playback
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
		if rest == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if id, ok := strings.CutSuffix(rest, "/stream"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			// The read-only media GET a browser reaches via <video src> — bearer OR
			// media cookie. Ownership is still enforced inside handleSessionStream.
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, handleSessionStream(svc, id)))(w, r)
			return
		}
		// GET {id}/hls/{file}: the HLS artifacts for a directStream/transcode session
		// (ADR-0004). Like /stream these are read-only GETs the browser's hls.js/
		// <video> fetches WITHOUT an Authorization header, so they accept the media
		// cookie OR bearer; ownership is enforced inside the handler. {file} must be a
		// single path element. The subtitle artifacts (the master playlist + the
		// in-band SUBTITLES rendition's playlists/segments, ADR-0020/slice 03) are
		// served by a distinct handler that resolves the session's Subtitle tracks;
		// the video media playlist + .ts segments stay on the runtime path unchanged.
		if id, file, ok := cutHLS(rest); ok {
			if id == "" || file == "" || strings.Contains(file, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			// Dispatch the HLS artifact by filename. The MASTER playlist is now the
			// unified builder (video variant + AUDIO group + SUBTITLES group,
			// audio-streams/03); the demuxed audio renditions (audio_<id>.*) and the
			// in-band subtitle renditions (subs_*) each have their own handler; everything
			// else (index.m3u8, .ts) is the video runtime path.
			switch {
			case file == playbackHLSMaster:
				requireMethod(http.MethodGet,
					requireAuthAllowCookie(deps.Auth, handleSessionMasterHLS(deps, id)))(w, r)
			case isAudioHLSFile(file):
				requireMethod(http.MethodGet,
					requireAuthAllowCookie(deps.Auth, handleSessionAudioHLS(deps, id, file)))(w, r)
			case isSubtitleHLSFile(file):
				requireMethod(http.MethodGet,
					requireAuthAllowCookie(deps.Auth, handleSessionSubtitleHLS(deps, id, file)))(w, r)
			default:
				requireMethod(http.MethodGet,
					requireAuthAllowCookie(deps.Auth, handleSessionHLS(svc, id, file)))(w, r)
			}
			return
		}
		if id, ok := strings.CutSuffix(rest, "/progress"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost, requireAuth(deps.Auth, handleSessionProgress(svc, id)))(w, r)
			return
		}
		// Bare /sessions/{id} → DELETE ends it.
		if strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		requireMethod(http.MethodDelete, requireAuth(deps.Auth, handleEndSession(svc, rest)))(w, r)
	}
}

// progressRequest is the body of POST /sessions/{id}/progress: a raw position
// and the play state. The client reports only this; the SERVER applies the
// Watched threshold (api-contract.md). state is accepted for keepalive but does
// not change the threshold math.
type progressRequest struct {
	PositionMs int64  `json:"positionMs"`
	State      string `json:"state"`
	// AudioStreamID, when set, records an in-band audio pick the player just made
	// (audio-streams/05, ADR-0023). An in-band HLS audio switch is client-side and
	// never re-negotiates, so the player reports it here — alongside progress, on the
	// same watch-state surface — to have it Remembered for the next play. It does not
	// affect the resume/watched threshold; an unknown id is ignored. Empty on an
	// ordinary progress tick.
	AudioStreamID string `json:"audioStreamId"`
	// VideoStreamID is the exact video parallel of AudioStreamID, for players that
	// switch video tracks in-container without re-negotiating (libmpv on direct
	// play; an HLS video switch is a RESTART whose negotiation already records the
	// pick, ADR-0025). When set, the pick is Remembered for the next play;
	// best-effort, an unknown id is ignored. Empty on an ordinary progress tick.
	VideoStreamID string `json:"videoStreamId"`
}

// progressResponse echoes the server-resolved watch state after applying the
// threshold, so a client can update its UI (resume marker, watched badge)
// without guessing.
type progressResponse struct {
	TitleID          string `json:"titleId"`
	ResumePositionMs int64  `json:"resumePositionMs"`
	Watched          bool   `json:"watched"`
}

// handleSessionProgress applies a raw progress report against the session
// (owner-only). It is the keepalive (Touches the session) and the point where
// the server applies the Watched threshold against the session File's duration —
// the client never computes "watched". A reaped/ended/foreign session is 404
// (hide existence). On success it returns the resolved watch state.
func handleSessionProgress(svc *playback.Service, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req progressRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		out, err := svc.ReportProgress(id.User.ID, sessionID, req.PositionMs, req.AudioStreamID, req.VideoStreamID)
		switch {
		case errors.Is(err, playback.ErrSessionNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to record progress", nil)
			return
		}
		writeJSON(w, http.StatusOK, progressResponse{
			TitleID:          out.TitleID,
			ResumePositionMs: out.ResumePositionMs,
			Watched:          out.Watched,
		})
	}
}

// handleSessionStream serves the session's File over HTTP with byte-range support
// so the client can seek. http.ServeContent handles Range, 206 Partial Content,
// If-Range, and HEAD correctly, mapping a seek to a byte offset — no new playback
// decision is needed for seeking (ADR-0004). Only the session's owner may stream
// it; another User (or an unknown/ended session id) gets 404 (hide existence).
func handleSessionStream(svc *playback.Service, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		sess, ok := svc.Sessions().Get(sessionID)
		if !ok || sess.UserID != id.User.ID {
			// Unknown, ended, or not-yours: hide existence with a 404.
			writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
			return
		}
		f, err := openSessionFile(sess.FilePath)
		if err != nil {
			// The File negotiated fine but is gone/unreadable on disk now: 404.
			writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
			return
		}
		defer f.file.Close()
		// ServeContent uses the name only for content-type sniffing and the modtime
		// for caching/If-Modified-Since; it does the heavy lifting of Range + 206.
		http.ServeContent(w, r, f.name, f.modTime, f.file)
	}
}

// playbackHLSPlaylist is the media-playlist filename under the /hls/ route. It
// mirrors transcode.PlaylistName but is duplicated as a small constant so the api
// package does not import transcode just for a string (the route is a transport
// concern, the muxer config a domain one).
const playbackHLSPlaylist = "index.m3u8"

// cutHLS splits a /sessions subtree remainder of the form "{id}/hls/{file}" into
// its session id and file name. It returns ok=false when the path is not an HLS
// route, so the dispatcher falls through to the bare-{id} DELETE handling.
func cutHLS(rest string) (id, file string, ok bool) {
	i := strings.Index(rest, "/hls/")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len("/hls/"):], true
}

// handleSessionHLS serves a directStream session's HLS media playlist or one of
// its segments out of the session scratch (ADR-0004). It lazily starts the remux
// on the first request and briefly waits for ffmpeg to flush a not-yet-written
// playlist/segment rather than hard-404 a file the playlist lists. Only the
// session owner may fetch (another User / unknown / ended session, and a
// direct-play session that has no HLS resource, are all hidden as 404). The
// Content-Type is set per file kind so hls.js and native players parse correctly.
func handleSessionHLS(svc *playback.Service, sessionID, file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var (
			data []byte
			err  error
		)
		if file == playbackHLSPlaylist {
			data, err = svc.HLSPlaylist(id.User.ID, sessionID)
		} else {
			data, err = svc.HLSSegment(id.User.ID, sessionID, file)
		}
		switch {
		case errors.Is(err, playback.ErrSessionNotFound), errors.Is(err, playback.ErrNotHLS):
			// Unknown/ended/foreign session, or a direct-play session with no HLS
			// resource: hide existence with a 404.
			writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
			return
		case os.IsNotExist(err):
			// The file the playlist references never materialized within the wait
			// window (or an unknown segment name) — a genuine 404.
			writeError(w, http.StatusNotFound, codeNotFound, "segment not available", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to serve HLS media", nil)
			return
		}
		w.Header().Set("Content-Type", hlsContentType(file))
		// Scratch segments are session-ephemeral; let the player cache an already
		// fetched immutable segment but never a stale playlist.
		if file == playbackHLSPlaylist {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// hlsContentType maps an HLS file name to its media type: the m3u8 playlist, a
// TS video segment, or a WebVTT subtitle segment (the in-band SUBTITLES rendition,
// slice 03). Anything else defaults to octet-stream (defensive).
func hlsContentType(file string) string {
	switch {
	case strings.HasSuffix(file, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(file, ".ts"):
		return "video/mp2t"
	// fMP4 (CMAF) delivery for a copied-HEVC session (ADR-0024): the fragmented-MP4
	// media segments (.m4s) and the initialization segment (init.mp4 / audio_<id>_
	// init.mp4) are both MP4 — Safari needs the video/mp4 type to play them.
	case strings.HasSuffix(file, ".m4s"), strings.HasSuffix(file, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(file, ".vtt"):
		return "text/vtt; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// In-band HLS subtitle delivery (ADR-0020, slice 03). On the HLS tiers a text
// Subtitle track rides in-band: the session's streamUrl points at a MASTER
// playlist that references the video media playlist (index.m3u8) plus one
// #EXT-X-MEDIA:TYPE=SUBTITLES rendition per deliverable text track. Each rendition
// is a VOD subtitle media playlist of WebVTT segments cut to the video cadence.
// All three artifacts live under the same session-scoped /hls route, dispatched
// by filename:
//
//	master.m3u8            → the master playlist
//	subs_<subId>.m3u8      → one rendition's media playlist
//	subs_<subId>_<NNN>.vtt → that rendition's Nth WebVTT segment
//
// The video media playlist (index.m3u8) and .ts segments stay on the runtime path.
const (
	// playbackHLSMaster is the master-playlist filename served under /hls when an
	// HLS session carries a deliverable text subtitle (ADR-0020). A bare media
	// playlist (index.m3u8) is served otherwise.
	playbackHLSMaster = "master.m3u8"
	// hlsSubtitlePrefix namespaces every in-band subtitle artifact so the /hls
	// dispatcher can tell them from the video media playlist + .ts segments.
	hlsSubtitlePrefix = "subs_"
	// hlsAudioPrefix namespaces every demuxed in-band AUDIO rendition artifact
	// (audio-streams/03) so the /hls dispatcher tells audio_<streamId>.m3u8 /
	// audio_<streamId>_NNN.ts from the video variant and the subtitle renditions. It
	// mirrors transcode.AudioRenditionPlaylist's prefix, duplicated as a small
	// constant so the api package does not import transcode just for a string (the
	// same reason playbackHLSPlaylist duplicates transcode.PlaylistName).
	hlsAudioPrefix = "audio_"
	// hlsSegmentSeconds mirrors transcode.SegmentSeconds — the HLS segment cadence
	// the subtitle rendition is cut to. Duplicated as a small constant so the api
	// package does not import transcode just for a number (the same reason
	// playbackHLSPlaylist duplicates transcode.PlaylistName).
	hlsSegmentSeconds = 4
)

// isSubtitleHLSFile reports whether an /hls/{file} name is an in-band subtitle
// rendition artifact (a subs_ playlist/segment). The master playlist is now the
// UNIFIED builder (audio-streams/03) dispatched separately, so it is no longer a
// subtitle-only artifact here.
func isSubtitleHLSFile(file string) bool {
	return strings.HasPrefix(file, hlsSubtitlePrefix)
}

// isAudioHLSFile reports whether an /hls/{file} name is a demuxed in-band audio
// rendition artifact (an audio_ playlist/segment, audio-streams/03).
func isAudioHLSFile(file string) bool {
	return strings.HasPrefix(file, hlsAudioPrefix)
}

// subtitlePlaylistName / subtitleSegmentName are the session-relative filenames
// for a track's in-band rendition, kept next to their parsers so the naming
// convention lives in one place.
func subtitlePlaylistName(subID string) string {
	return hlsSubtitlePrefix + subID + ".m3u8"
}

func subtitleSegmentName(subID string, index int) string {
	return hlsSubtitlePrefix + subID + "_" + pad3(index) + ".vtt"
}

// pad3 formats a non-negative segment index as a zero-padded 3-digit string
// (mirrors the %03d the video segments use), so subtitle segment names sort and
// parse the same way.
func pad3(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// parseSubtitlePlaylistFile extracts the subtitle-row/Stream id from a
// subs_<subId>.m3u8 rendition-playlist name, or ok=false when the name isn't one.
// A subId is a UUID (no underscore), so the segment form (which carries a
// _<NNN> before .vtt) is unambiguously distinguished by its .vtt suffix.
func parseSubtitlePlaylistFile(file string) (subID string, ok bool) {
	if !strings.HasPrefix(file, hlsSubtitlePrefix) || !strings.HasSuffix(file, ".m3u8") {
		return "", false
	}
	subID = file[len(hlsSubtitlePrefix) : len(file)-len(".m3u8")]
	return subID, subID != ""
}

// parseSubtitleSegmentFile splits a subs_<subId>_<NNN>.vtt segment name into its
// subId and zero-based index. The subId is a UUID (no underscore), so the LAST
// underscore separates it from the index.
func parseSubtitleSegmentFile(file string) (subID string, index int, ok bool) {
	if !strings.HasPrefix(file, hlsSubtitlePrefix) || !strings.HasSuffix(file, ".vtt") {
		return "", 0, false
	}
	tail := file[len(hlsSubtitlePrefix) : len(file)-len(".vtt")]
	us := strings.LastIndex(tail, "_")
	if us <= 0 {
		return "", 0, false
	}
	idx, err := strconv.Atoi(tail[us+1:])
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return tail[:us], idx, true
}

// hasDeliverableTextSubtitle reports whether any track can be served as an in-band
// WebVTT rendition (a text track we can convert). It gates whether the decision's
// streamUrl points at a master playlist vs the bare media playlist.
func hasDeliverableTextSubtitle(tracks []playback.SubtitleTrack) bool {
	for _, t := range tracks {
		if t.Kind == "text" && t.Convertible {
			return true
		}
	}
	return false
}

// hlsSubtitleSegmentCount is how many WebVTT segments cover a File of durationMs
// at the HLS cadence — ceil(duration / segLen), the same count the video media
// playlist lists so subtitle segment N aligns with video segment N. An unknown
// duration yields 0 (an empty but valid rendition playlist).
func hlsSubtitleSegmentCount(durationMs int64) int {
	if durationMs <= 0 {
		return 0
	}
	segMs := int64(hlsSegmentSeconds) * 1000
	return int((durationMs + segMs - 1) / segMs)
}

// handleSessionSubtitleHLS serves the in-band subtitle artifacts for an HLS
// session (ADR-0020): the master playlist, a rendition's media playlist, or a
// rendition's WebVTT segment. It resolves the owner-checked SessionSubtitleContext
// (ErrSessionNotFound/ErrNotHLS → 404, hide existence; a vanished Title/File →
// 404), then produces the requested artifact. WebVTT segments reuse the same
// track-locating + conversion path as the out-of-band endpoint (subtitleVTT),
// then slice the whole-file WebVTT to the requested segment window.
func handleSessionSubtitleHLS(deps Deps, sessionID, file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		sctx, err := deps.Playback.SessionSubtitleContext(id.User.ID, sessionID)
		switch {
		case errors.Is(err, playback.ErrSessionNotFound), errors.Is(err, playback.ErrNotHLS), errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to serve HLS subtitle", nil)
			return
		}

		switch {
		case isSubtitlePlaylist(file):
			subID, _ := parseSubtitlePlaylistFile(file)
			count := hlsSubtitleSegmentCount(sctx.DurationMs)
			data := subtitle.SubtitleMediaPlaylist(count, hlsSegmentSeconds, func(i int) string {
				return subtitleSegmentName(subID, i)
			})
			writeHLSSubtitle(w, file, data, true)
		case isSubtitleSegment(file):
			subID, index, _ := parseSubtitleSegmentFile(file)
			full, err := wholeSubtitleVTT(r.Context(), sctx, subID)
			switch {
			case errors.Is(err, errSubtitleNotText), errors.Is(err, errSubtitleNotFound):
				writeError(w, http.StatusNotFound, codeNotFound, "subtitle not found", nil)
				return
			case err != nil:
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to render subtitle", nil)
				return
			}
			writeHLSSubtitle(w, file, subtitle.SegmentVTT(full, index, hlsSegmentSeconds), false)
		default:
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		}
	}
}

// wholeSubtitleVTT returns a track's whole-file WebVTT, cached in the session
// scratch dir so the (embedded) ffmpeg extraction runs ONCE per (session, track)
// rather than on every segment request during playback — a subtitle rendition is
// segmented on demand, so an uncached embedded track would re-extract the entire
// File for each of its (potentially hundreds of) segments. The cache file is
// removed with the session (its scratch dir), so it never outlives the stream and
// stays consistent with a rescan (a new session re-derives it). A missing scratch
// dir (defensive) falls back to producing it fresh each call.
func wholeSubtitleVTT(ctx context.Context, sctx playback.SessionSubtitleContext, subID string) ([]byte, error) {
	if sctx.ScratchDir == "" {
		return subtitleVTT(ctx, sctx.Detail, subID)
	}
	cachePath := filepath.Join(sctx.ScratchDir, "subfull_"+subID+".vtt")
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}
	data, err := subtitleVTT(ctx, sctx.Detail, subID)
	if err != nil {
		return nil, err
	}
	// Best-effort cache write via a temp file + atomic rename, so a concurrent
	// segment request never reads a half-written file. A write failure is non-fatal
	// — the bytes are already in hand for this request.
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		_ = os.Rename(tmp, cachePath)
	} else {
		_ = os.Remove(tmp)
	}
	return data, nil
}

// isSubtitlePlaylist / isSubtitleSegment classify a subs_ artifact by suffix (a
// rendition media playlist vs a WebVTT segment).
func isSubtitlePlaylist(file string) bool {
	_, ok := parseSubtitlePlaylistFile(file)
	return ok
}

func isSubtitleSegment(file string) bool {
	_, _, ok := parseSubtitleSegmentFile(file)
	return ok
}

// masterRenditions maps the session's deliverable TEXT tracks to master-playlist
// #EXT-X-MEDIA renditions, in the SAME order the decision offered them (embedded
// then sidecar/fetched), so the client can map its menu entries to renditions by
// order. Image tracks and unconvertible text carry no in-band rendition.
func masterRenditions(tracks []playback.SubtitleTrack) []subtitle.Rendition {
	var rs []subtitle.Rendition
	for _, t := range tracks {
		if t.Kind != "text" || !t.Convertible {
			continue
		}
		rs = append(rs, subtitle.Rendition{
			URI:      subtitlePlaylistName(t.ID),
			Name:     subtitle.Label(t.Language, t.Forced),
			Language: t.Language,
			Forced:   t.Forced,
		})
	}
	return rs
}

// writeHLSSubtitle writes an in-band subtitle artifact with its media type. The
// playlists (master + rendition) are marked no-cache (they must not go stale);
// the WebVTT segments are deterministic per (session, track, index) but the
// session is ephemeral, so they carry no explicit caching, mirroring the .ts
// segments.
func writeHLSSubtitle(w http.ResponseWriter, file string, data []byte, noCache bool) {
	w.Header().Set("Content-Type", hlsContentType(file))
	if noCache {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleSessionMasterHLS serves the UNIFIED master playlist for an HLS session
// (audio-streams/03): the video variant + a demuxed AUDIO group (one rendition per
// audio Stream of a multi-audio File) + the in-band SUBTITLES group. It resolves
// both the owner-checked audio and subtitle contexts (same ownership + HLS gates)
// and composes them via audio.MasterPlaylist. A single-audio session simply carries
// no AUDIO group; a session with neither group still returns a valid (bare) master.
func handleSessionMasterHLS(deps Deps, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		actx, err := deps.Playback.SessionAudioContext(id.User.ID, sessionID)
		switch {
		case errors.Is(err, playback.ErrSessionNotFound), errors.Is(err, playback.ErrNotHLS), errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to serve HLS master", nil)
			return
		}
		// Subtitle renditions share the same session; resolve them for the SUBTITLES
		// group. It cannot fail differently than the audio context did (both read the
		// same session + Title), so a rare error here degrades to an audio-only master
		// rather than failing the whole request.
		var subRends []subtitle.Rendition
		if sctx, serr := deps.Playback.SessionSubtitleContext(id.User.ID, sessionID); serr == nil {
			subRends = masterRenditions(sctx.Tracks)
		}
		// A copied-HEVC fMP4 session advertises a CODECS attribute so Safari accepts the
		// HEVC variant (ADR-0024); h264/MPEG-TS sessions pass "" (unchanged). The rest
		// of the variant attributes are honest values from the played File — for an HDR
		// stream, VIDEO-RANGE (+ RESOLUTION/FRAME-RATE) is what stops Safari from
		// killing the variant with a bare decode error.
		variant := audio.Variant{
			Bandwidth:  actx.Bandwidth,
			Width:      actx.Width,
			Height:     actx.Height,
			FrameRate:  actx.FrameRate,
			VideoRange: actx.VideoRange,
		}
		if actx.FMP4 {
			variant.Codecs = masterVideoCodecs(actx.VideoCodec)
		}
		data := audio.MasterPlaylist(playbackHLSPlaylist, variant, masterAudioRenditions(actx.Renditions), subRends)
		writeHLSSubtitle(w, playbackHLSMaster, data, true)
	}
}

// masterVideoCodecs builds the CODECS attribute for a copied-HEVC fMP4 master's
// video variant (ADR-0024): the RFC 6381 HEVC codec string paired with the AAC audio
// the renditions carry (every rendition transcodes to AAC). Safari requires CODECS to
// accept an HEVC fMP4 variant, and it must name the hvc1 sample-entry brand the
// transcode forces (-tag:v hvc1). The precise HEVC profile/level is not stored, so a
// generic Main-profile string is used — Safari validates the brand against the init
// segment and is lenient on the level. A non-HEVC codec (defensive) yields "" so no
// CODECS is emitted rather than a wrong one.
func masterVideoCodecs(videoCodec string) string {
	if !strings.EqualFold(strings.TrimSpace(videoCodec), "hevc") {
		return ""
	}
	// hvc1.1.6.L153.B0 = HEVC Main profile, level 5.1 — a broadly-compatible generic;
	// mp4a.40.2 = AAC-LC (the audio renditions' output).
	return "hvc1.1.6.L153.B0,mp4a.40.2"
}

// masterAudioRenditions maps the session's demuxed audio Streams to master-playlist
// #EXT-X-MEDIA:TYPE=AUDIO renditions, each URI'd at its rendition media playlist
// (audio_<streamId>.m3u8). The resolved default Stream is marked Default so the
// player turns it on. Empty when the session is not demuxed (single-audio).
func masterAudioRenditions(rends []playback.AudioRenditionInfo) []audio.Rendition {
	var rs []audio.Rendition
	for _, r := range rends {
		rs = append(rs, audio.Rendition{
			URI:      audioRenditionPlaylistName(r.StreamID),
			Name:     r.Label,
			Language: r.Language,
			Default:  r.Default,
		})
	}
	return rs
}

// handleSessionAudioHLS serves a demuxed audio rendition's media playlist or one of
// its segments (audio-streams/03): audio_<streamId>.m3u8 → the rendition playlist,
// audio_<streamId>_<NNN>.ts → a rendition segment. It routes by the stream id parsed
// from the filename to that rendition's lazy runtime (started on first request). An
// unknown id / non-demuxed session is 404 (hide existence), the same as an unknown
// video segment.
func handleSessionAudioHLS(deps Deps, sessionID, file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var (
			data []byte
			err  error
		)
		switch {
		case isAudioPlaylist(file):
			streamID, _ := parseAudioPlaylistFile(file)
			data, err = deps.Playback.HLSAudioRendition(id.User.ID, sessionID, streamID)
		case isAudioSegment(file):
			streamID, _ := parseAudioSegmentFile(file)
			data, err = deps.Playback.HLSAudioSegment(id.User.ID, sessionID, streamID, file)
		default:
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch {
		case errors.Is(err, playback.ErrSessionNotFound), errors.Is(err, playback.ErrNotHLS),
			errors.Is(err, playback.ErrNoAudioRendition), errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
			return
		case os.IsNotExist(err):
			writeError(w, http.StatusNotFound, codeNotFound, "segment not available", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to serve HLS audio", nil)
			return
		}
		w.Header().Set("Content-Type", hlsContentType(file))
		// The rendition playlist must never go stale; the immutable segments may cache
		// like the video .ts segments.
		if isAudioPlaylist(file) {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// audioRenditionPlaylistName is the session-relative media-playlist filename for a
// demuxed audio Stream's rendition (mirrors transcode.AudioRenditionPlaylist).
func audioRenditionPlaylistName(streamID string) string {
	return hlsAudioPrefix + streamID + ".m3u8"
}

// parseAudioPlaylistFile extracts the audio-Stream id from an audio_<streamId>.m3u8
// rendition-playlist name, or ok=false when the name isn't one. A streamID is a UUID
// (no underscore), so the .ts segment form (which carries a trailing _<NNN>) is
// distinguished by suffix, exactly like the subtitle rendition parsing.
func parseAudioPlaylistFile(file string) (streamID string, ok bool) {
	if !strings.HasPrefix(file, hlsAudioPrefix) || !strings.HasSuffix(file, ".m3u8") {
		return "", false
	}
	streamID = file[len(hlsAudioPrefix) : len(file)-len(".m3u8")]
	return streamID, streamID != ""
}

// parseAudioSegmentFile extracts the audio-Stream id from a rendition SEGMENT name so
// the request routes to that rendition's runtime (which serves the file by its exact
// name). It recognizes the MPEG-TS segment audio_<id>_<NNN>.ts, its fMP4 (CMAF)
// analogues audio_<id>_<NNN>.m4s, and the fMP4 initialization segment
// audio_<id>_init.mp4 (ADR-0024). A streamID is a UUID (no underscore), so the LAST
// underscore separates the id from the index (or the literal "init"). The index value
// is not needed — only the id and validity.
func parseAudioSegmentFile(file string) (streamID string, ok bool) {
	if !strings.HasPrefix(file, hlsAudioPrefix) {
		return "", false
	}
	body := file[len(hlsAudioPrefix):]
	// fMP4 initialization segment: audio_<id>_init.mp4.
	if s := strings.TrimSuffix(body, "_init.mp4"); s != body {
		return s, s != ""
	}
	// Media segment: audio_<id>_<NNN>.ts (MPEG-TS) or .m4s (fMP4).
	var tail string
	switch {
	case strings.HasSuffix(body, ".ts"):
		tail = strings.TrimSuffix(body, ".ts")
	case strings.HasSuffix(body, ".m4s"):
		tail = strings.TrimSuffix(body, ".m4s")
	default:
		return "", false
	}
	us := strings.LastIndex(tail, "_")
	if us <= 0 {
		return "", false
	}
	if _, err := strconv.Atoi(tail[us+1:]); err != nil {
		return "", false
	}
	return tail[:us], true
}

// isAudioPlaylist / isAudioSegment classify an audio_ artifact by suffix.
func isAudioPlaylist(file string) bool {
	_, ok := parseAudioPlaylistFile(file)
	return ok
}

func isAudioSegment(file string) bool {
	_, ok := parseAudioSegmentFile(file)
	return ok
}

// handleEndSession ends a Playback session (the clean stop, DELETE
// /sessions/{id}). Only the owner may end it; an unknown/ended id or another
// User's session is 404 (hide existence). On success it returns 204 No Content.
func handleEndSession(svc *playback.Service, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		sess, ok := svc.Sessions().Get(sessionID)
		if !ok || sess.UserID != id.User.ID {
			writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
			return
		}
		svc.Sessions().End(sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}
