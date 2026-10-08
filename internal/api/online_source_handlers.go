package api

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/onlinesource"
	"github.com/goozakdev/obelo-server/internal/playback"
)

// Online sources (ADR-0068, .scratch/online-sources issue 01).
//
//	GET  /onlineSources                                the tile list, from the registry
//	GET  /onlineSources/{id}/icon                      the tile image from the Plugin's package (bearer OR cookie)
//	GET  /onlineSources/{id}/rows                      the source page (runs rows())
//	GET  /onlineSources/{id}/rows/{rowId}?cursor=      the next page of one row (runs row())
//	GET  /onlineSources/{id}/items/{itemId}/thumbnail  Server-proxied bytes (bearer OR cookie)
//	POST /onlineSources/{id}/items/{itemId}/playback   resolve() once, open a session
//
// A source has its own endpoints and is never part of the home response, so a
// client that does not call them sees no tiles and no Plugin is called to draw the
// home screen.
//
// INTERIM ACCESS RULE, until per-User grants land: every one of these is
// Admin-only. A non-Admin and any Remote-role caller gets an empty tile list and a
// 404 from everything else, so a capped Member or a linked server cannot see a
// source before grants exist. The 404 hides existence, as it does for a Library.

const (
	onlineSourcesPrefix = "/onlineSources/"
	// codeSourceUnavailable (502): the Online source, or the host serving its
	// media, failed or refused. Distinct from NOT_FOUND so a client can offer the
	// retry the source page's "isn't responding" state shows.
	codeSourceUnavailable = "SOURCE_UNAVAILABLE"
)

// onlineAllowed reports whether the caller may see Online sources at all.
func onlineAllowed(deps Deps, r *http.Request) bool {
	if deps.Online == nil {
		return false
	}
	id, ok := identityFrom(r.Context())
	return ok && id.User.Role == "admin"
}

type onlineSourceJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// IconURL is the Server's own path to the tile image, or null when the package
	// carried no icon.png (the client draws a generic tile with the name).
	IconURL *string `json:"iconUrl"`
}

// handleOnlineSources serves GET /onlineSources.
func handleOnlineSources(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := []onlineSourceJSON{}
		if onlineAllowed(deps, r) {
			for _, s := range deps.Online.Sources() {
				item := onlineSourceJSON{ID: s.ID, Name: s.Name}
				if s.HasIcon {
					u := APIPrefix + onlineSourcesPrefix + s.ID + "/icon"
					item.IconURL = &u
				}
				out = append(out, item)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": out})
	}
}

type onlineItemJSON struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ThumbnailURL string `json:"thumbnailUrl"`
	DurationMs   int64  `json:"durationMs"`
	Description  string `json:"description,omitempty"`
	PublishedAt  string `json:"publishedAt,omitempty"`
}

type onlineRowJSON struct {
	ID         string           `json:"id"`
	Label      string           `json:"label"`
	Items      []onlineItemJSON `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

// handleOnlineSourceSubtree dispatches /onlineSources/{id}/…, applying auth PER
// LEAF because the thumbnail leaf alone also accepts the media cookie (a browser
// <img> cannot send an Authorization header), as the Title artwork leaf does. The
// source icon leaf is the same kind of media GET.
func handleOnlineSourceSubtree(deps Deps) http.HandlerFunc {
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, onlineSourcesPrefix), "/")
		switch {
		case len(parts) == 2 && parts[1] == "rows":
			requireMethod(http.MethodGet, requireAuth(deps.Auth, handleOnlineRows(deps, parts[0])))(w, r)
		case len(parts) == 3 && parts[1] == "rows":
			requireMethod(http.MethodGet, requireAuth(deps.Auth, handleOnlineRow(deps, parts[0], parts[2])))(w, r)
		case len(parts) == 2 && parts[1] == "icon":
			requireMethod(http.MethodGet, requireAuthAllowCookie(deps.Auth, handleOnlineIcon(deps, parts[0])))(w, r)
		case len(parts) == 4 && parts[1] == "items" && parts[3] == "thumbnail":
			requireMethod(http.MethodGet,
				requireAuthAllowCookie(deps.Auth, handleOnlineThumbnail(deps, parts[0], parts[2])))(w, r)
		case len(parts) == 4 && parts[1] == "items" && parts[3] == "playback":
			requireMethod(http.MethodPost,
				requireAuth(deps.Auth, requireScope(deps.Access, handleOnlinePlayback(deps, parts[0], parts[2]))))(w, r)
		default:
			notFound(w, r)
		}
	}
}

// writeOnlineFailure renders an onlinesource error: an unknown or disabled source
// and an unknown item are one 404, a failing source is a 502.
func writeOnlineFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, onlinesource.ErrNoSource), errors.Is(err, onlinesource.ErrNoItem):
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	case errors.Is(err, onlinesource.ErrUnavailable):
		writeError(w, http.StatusBadGateway, codeSourceUnavailable, "the source is not responding", nil)
	default:
		log.Printf("obelo: api: online source: %v", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "online source request failed", nil)
	}
}

func onlineThumbnailURL(sourceID, itemID string) string {
	return APIPrefix + "/onlineSources/" + sourceID + "/items/" + itemID + "/thumbnail"
}

// handleOnlineRows serves GET /onlineSources/{id}/rows: the only call that runs
// the Plugin's rows().
func handleOnlineRows(deps Deps, sourceID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlineAllowed(deps, r) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		rows, err := deps.Online.Rows(r.Context(), sourceID)
		if err != nil {
			writeOnlineFailure(w, err)
			return
		}
		out := make([]onlineRowJSON, 0, len(rows))
		for _, row := range rows {
			j := onlineRowJSON{ID: row.ID, Label: row.Label, Items: onlineItemsJSON(sourceID, row.Items)}
			if row.NextCursor != "" {
				j.NextCursor = &row.NextCursor
			}
			out = append(out, j)
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": out})
	}
}

func onlineItemsJSON(sourceID string, in []onlinesource.Item) []onlineItemJSON {
	items := make([]onlineItemJSON, 0, len(in))
	for _, it := range in {
		items = append(items, onlineItemJSON{
			ID: it.ID, Title: it.Title, ThumbnailURL: onlineThumbnailURL(sourceID, it.ID),
			DurationMs: it.DurationMs, Description: it.Description, PublishedAt: it.PublishedAt,
		})
	}
	return items
}

// handleOnlineRow serves GET /onlineSources/{id}/rows/{rowId}?cursor=: the next
// page of one row, by the opaque cursor an earlier answer named. nextCursor is
// absent on the last page.
func handleOnlineRow(deps Deps, sourceID, rowID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlineAllowed(deps, r) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		page, err := deps.Online.Row(r.Context(), sourceID, rowID, r.URL.Query().Get("cursor"))
		if err != nil {
			writeOnlineFailure(w, err)
			return
		}
		out := struct {
			Items      []onlineItemJSON `json:"items"`
			NextCursor string           `json:"nextCursor,omitempty"`
		}{Items: onlineItemsJSON(sourceID, page.Items), NextCursor: page.NextCursor}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleOnlineThumbnail serves the Server-proxied thumbnail. The bytes are held in
// memory for this response and never written anywhere.
func handleOnlineThumbnail(deps Deps, sourceID, itemID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlineAllowed(deps, r) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		body, contentType, err := deps.Online.Thumbnail(r.Context(), sourceID, itemID)
		if err != nil {
			writeOnlineFailure(w, err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "private, max-age=300")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}
}

// handleOnlineIcon serves the source's tile image from the Plugin's own package, on
// the same access rule as everything else about a source: a caller who may not see
// the source gets the 404 a missing one does.
func handleOnlineIcon(deps Deps, sourceID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlineAllowed(deps, r) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		body, err := deps.Online.Icon(sourceID)
		if err != nil {
			writeOnlineFailure(w, err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "image/png")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "private, max-age=300")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}
}

// The two ways an Online item reaches a client: the source's bytes relayed
// untouched, one progressive file; or ffmpeg's output, an HLS playlist.
const (
	onlineFormatProgressive = "progressive"
	onlineFormatHLS         = "hls"
	// onlineHLSPlaylist is the one entry point of an encoded session; its segments
	// are named relative to it.
	onlineHLSPlaylist = "index.m3u8"
)

type onlinePlaybackResponse struct {
	SessionID string `json:"sessionId"`
	StreamURL string `json:"streamUrl"`
	// Format is "progressive" (a file a <video> plays directly) or "hls" (a playlist
	// an HLS-capable player plays).
	Format string `json:"format"`
}

// handleOnlinePlayback serves POST /onlineSources/{id}/items/{itemId}/playback:
// resolve() once, choose a variant the client can play untouched under the User's
// Playback ceiling, and open a session whose stream URL carries a stream token.
func handleOnlinePlayback(deps Deps, sourceID, itemID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlineAllowed(deps, r) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		id, _ := identityFrom(r.Context())
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		var req playbackRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		constraints, _ := playback.ClampToCeiling(req.Constraints.toDomain(), scope)
		sess, unsup, err := deps.Online.Play(r.Context(), onlinesource.PlayInput{
			UserID: id.User.ID, SourceID: sourceID, ItemID: itemID,
			Profile: req.DeviceProfile.toDomain(), Constraints: constraints,
		})
		if errors.Is(err, onlinesource.ErrBusy) {
			// 503 SERVER_BUSY, exactly as for a Title at the transcode cap (ADR-0009).
			busy := playback.OnlineServerBusy(constraints)
			writeError(w, http.StatusServiceUnavailable, codeServerBusy,
				"server is at its transcode capacity; retry at a lower bitrate",
				map[string]any{"retryable": true, "suggestedMaxBitrate": busy.SuggestedMaxBitrate})
			return
		}
		if err != nil {
			writeOnlineFailure(w, err)
			return
		}
		if unsup != nil {
			writeError(w, http.StatusNotImplemented, codeTranscodeRequired,
				"direct play not possible for this client; a transcode would be required",
				map[string]any{"reason": string(unsup.Reason), "detail": unsup.Detail})
			return
		}
		grant, err := deps.Auth.MintStreamToken(sess.ID, id.User.ID)
		if err != nil {
			// Without the token the session has no URL a client can use, so it ends
			// here rather than idling until the reaper finds it.
			log.Printf("obelo: api: minting stream token for online session %s: %v", sess.ID, err)
			deps.Online.End(sess.ID)
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to start playback", nil)
			return
		}
		out := onlinePlaybackResponse{
			SessionID: sess.ID,
			StreamURL: APIPrefix + streamRoutePrefix + grant.Token + "/" + streamProgressiveArtifact,
			Format:    onlineFormatProgressive,
		}
		if sess.Transcoded {
			out.StreamURL = APIPrefix + streamRoutePrefix + grant.Token + "/" + streamHLSArtifactPrefix + onlineHLSPlaylist
			out.Format = onlineFormatHLS
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// onlineSessionRoute handles the two /sessions/{id} leaves an Online session has,
// when rest names a live one: POST {id}/progress (the player's keepalive) and
// DELETE {id}. They are answered here rather than by the Title session code on
// purpose: an Online session has no Edition, File or watch state, so the
// keepalive records nothing and cannot, whatever the position says.
func onlineSessionRoute(deps Deps, rest string) (http.HandlerFunc, bool) {
	if deps.Online == nil {
		return nil, false
	}
	if id, ok := strings.CutSuffix(rest, "/progress"); ok && !strings.Contains(id, "/") {
		if _, live := deps.Online.Session(id); live {
			return requireMethod(http.MethodPost, requireAuth(deps.Auth, handleOnlineProgress(deps, id))), true
		}
		return nil, false
	}
	if !strings.Contains(rest, "/") {
		if _, live := deps.Online.Session(rest); live {
			return requireMethod(http.MethodDelete, requireAuth(deps.Auth, handleOnlineEnd(deps, rest))), true
		}
	}
	return nil, false
}

// ownedOnlineSession is the caller's live Online session, or false after answering
// 404 (hide existence).
func ownedOnlineSession(deps Deps, w http.ResponseWriter, r *http.Request, sessionID string) (onlinesource.Session, bool) {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
		return onlinesource.Session{}, false
	}
	sess, live := deps.Online.Session(sessionID)
	if !live || sess.UserID != id.User.ID {
		writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
		return onlinesource.Session{}, false
	}
	return sess, true
}

// handleOnlineProgress is the keepalive: it Touches the session and answers a
// progress body that resumes nothing and marks nothing watched.
func handleOnlineProgress(deps Deps, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ownedOnlineSession(deps, w, r, sessionID); !ok {
			return
		}
		var req progressRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		deps.Online.Touch(sessionID)
		writeJSON(w, http.StatusOK, progressResponse{})
	}
}

// handleOnlineEnd ends the session, which revokes its stream token.
func handleOnlineEnd(deps Deps, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ownedOnlineSession(deps, w, r, sessionID); !ok {
			return
		}
		deps.Online.End(sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// serveOnlineEncoded serves one file of an ffmpeg-encoded session (its playlist or a
// segment) to a stream-token request. The playlist is live while ffmpeg runs, so
// nothing is cached.
func serveOnlineEncoded(deps Deps, w http.ResponseWriter, r *http.Request, sess onlinesource.Session, name string) {
	deps.Online.Touch(sess.ID)
	f, err := deps.Online.OpenEncoded(r.Context(), sess.ID, name)
	if err != nil {
		if errors.Is(err, onlinesource.ErrNoSession) {
			refuseStreamToken(w)
			return
		}
		writeOnlineFailure(w, err)
		return
	}
	defer f.Close()
	h := w.Header()
	if strings.HasSuffix(name, ".m3u8") {
		h.Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		h.Set("Content-Type", "video/mp2t")
	}
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, time.Time{}, f)
}

// serveOnlineStream relays an Online session's bytes to a stream-token request.
// The token already named the session and its User; the upstream address is read
// from the session and goes no further than the fetch.
func serveOnlineStream(deps Deps, w http.ResponseWriter, r *http.Request, sess onlinesource.Session) {
	deps.Online.Touch(sess.ID)
	resp, err := deps.Online.OpenMedia(r.Context(), sess, r.Header.Get("Range"), r.Header.Get("If-Range"))
	if err != nil {
		writeOnlineFailure(w, err)
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}
