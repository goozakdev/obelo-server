package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/link"
	"github.com/goozakdev/obelo-server/internal/playback"
)

// The one-hop relay's transport (ADR-0056 §5).
//
//	GET /api/v1/relay/{sessionId}/{the sharer's own path tail}
//
// A relay Session names a session on another household's Server, and every media
// URL its Decision carried was rewritten onto this route with THE REMOTE PATH
// TAIL PRESERVED:
//
//	sharer   /api/v1/sessions/{remoteId}/hls/index.m3u8
//	here     /api/v1/relay/{localId}/sessions/{remoteId}/hls/index.m3u8
//
// Keeping the tail is not decoration. Every playlist Obelo emits uses BARE
// RELATIVE URIs (ADR-0039), so a player resolves "000.ts" against the playlist's
// own URL — and because the tail's shape is preserved, that resolution lands on
// the matching relay path by construction, with not one playlist byte needing to
// change. It is the same reason the stream token rides the path, applied one hop
// further out.
//
// The tail is nonetheless VALIDATED against the session rather than forwarded as
// given: a client may name this session's own progressive stream, its own HLS
// artifacts, and the subtitle tracks of the Title it is playing, and nothing else.
// Without that, the route would be an open proxy into a friend's server holding
// this household's credential.
//
// Auth is the media routes' own — bearer or the ms_media cookie, bound to the
// local Session's User — through requireAuthAllowCookie, the same middleware
// /sessions/{id}/stream uses. The third media credential, the stream token
// (ADR-0039), reaches these bytes on its own existing route: /stream/{token}/…
// resolves to a session, and a RELAY session is served from here rather than from
// a scratch dir that does not exist (see handleStreamTokenSubtree).

// relayRoutePrefix is the subtree, relative to APIPrefix.
const relayRoutePrefix = "/relay/"

// handleRelaySubtree dispatches every /relay/{sessionId}/… media request.
func handleRelaySubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			return
		}
		sessionID, tail, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, relayRoutePrefix), "/")
		if !ok || sessionID == "" || tail == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
			return
		}
		requireAuthAllowCookie(deps.Auth, func(w http.ResponseWriter, r *http.Request) {
			id, ok := identityFrom(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
				return
			}
			sess, ok := deps.Playback.Sessions().Get(sessionID)
			if !ok || sess.UserID != id.User.ID || !sess.IsRelay() {
				// Unknown, ended, not-yours, or not a relay at all: one 404, the same
				// existence-hiding answer /sessions/{id}/stream gives.
				writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
				return
			}
			serveRelayMedia(deps, w, r, sess, tail, relayURIMapper(sess.ID))
		})(w, r)
	}
}

// relayTailAllowed reports whether a requested path tail is one this session may
// reach on the sharer.
//
// Three shapes, and they are exactly the URLs a Decision for this session can
// carry: its progressive stream, its HLS artifacts (playlists, segments, init
// segments, the demuxed audio and in-band subtitle renditions — all single path
// elements under the session's own /hls/), and the out-of-band subtitle tracks of
// the Title it is playing. Anything else — another session's media, another
// Title's, a browse endpoint, a path with a traversal in it — is refused here,
// because the credential this request would be spent under is the household's.
func relayTailAllowed(sess playback.Session, tail string) bool {
	if tail == "" || strings.Contains(tail, "..") {
		return false
	}
	sessionRoot := "sessions/" + sess.RemoteSessionID
	if tail == sessionRoot+"/stream" {
		return true
	}
	if file, ok := strings.CutPrefix(tail, sessionRoot+"/hls/"); ok {
		return file != "" && !strings.Contains(file, "/")
	}
	if sess.RemoteTitleID != "" {
		if file, ok := strings.CutPrefix(tail, "titles/"+sess.RemoteTitleID+"/subtitles/"); ok {
			return file != "" && !strings.Contains(file, "/")
		}
	}
	return false
}

// serveRelayMedia carries one artifact from the sharer to this client.
//
// Playlists are read whole and rewritten (rewriteRelayPlaylist); everything else
// — segments, init segments, progressive bytes — is STREAMED, with the status,
// the content type, the length or the 206's Content-Range, and Accept-Ranges
// passed through so a seek behaves exactly as it does on a local file. Nothing is
// buffered to disk: a relay is a pipe, and a household watching a friend's 4K
// film must not first write it here.
func serveRelayMedia(deps Deps, w http.ResponseWriter, r *http.Request, sess playback.Session, tail string, mapURI func(string) string) {
	if deps.Links == nil {
		writeError(w, http.StatusServiceUnavailable, codeLinkUnreachable,
			"this server cannot relay playback", nil)
		return
	}
	if !relayTailAllowed(sess, tail) {
		writeError(w, http.StatusNotFound, codeNotFound, "session media unavailable", nil)
		return
	}
	resp, err := deps.Links.RelayFetch(r.Context(), sess.RelayLinkID, r.Method, tail, r.Header)
	if err != nil {
		writeRelayFetchError(w, err)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	// The fetch itself is this session's keepalive, exactly as a local manifest or
	// segment request is (Service.HLSPlaylist Touches). Without it a relayed direct
	// play — which reports progress but fetches bytes for minutes between reports —
	// would be reaped out from under a viewer who is watching it.
	deps.Playback.Sessions().Touch(sess.ID)

	if isRelayPlaylist(tail) && resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayPlaylistBytes))
		if err != nil {
			writeError(w, http.StatusBadGateway, codeLinkUnreachable, "failed to read the shared playlist", nil)
			return
		}
		w.Header().Set("Content-Type", hlsContentType(relayArtifactName(tail)))
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewriteRelayPlaylist(body, mapURI))
		return
	}

	for _, h := range relayResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", hlsContentType(relayArtifactName(tail)))
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	// Straight through. A copy error is a client that went away or a friend's
	// server that dropped mid-segment; the status is already written, so there is
	// nothing to report but the truncation the player will see anyway.
	_, _ = io.Copy(w, resp.Body)
}

// maxRelayPlaylistBytes bounds the one artifact kind that is read into memory.
const maxRelayPlaylistBytes = 4 << 20

// relayResponseHeaders are what a relayed media response carries back. An
// allowlist rather than a copy, for the request side's reason in reverse: the
// sharer's cookies, its Server header and anything else it chooses to say stop
// here. These five are what a player needs to seek.
var relayResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control",
}

// writeRelayFetchError renders a failed media fetch. The two Link states get the
// codes the play itself would have got (ADR-0056 §6), so a client that lost a
// friend's server mid-film reads the same story it would have on a fresh play.
func writeRelayFetchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, link.ErrCredentialDead):
		writeError(w, http.StatusServiceUnavailable, codeLinkRevoked,
			"the sharing server no longer accepts this server's credential", nil)
	default:
		writeError(w, http.StatusServiceUnavailable, codeLinkUnreachable,
			"the sharing server could not be reached", nil)
	}
}

// relayArtifactName is the last path element of a tail — the filename the content
// type is decided from.
func relayArtifactName(tail string) string {
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		return tail[i+1:]
	}
	return tail
}

func isRelayPlaylist(tail string) bool {
	return strings.HasSuffix(tail, ".m3u8")
}

// --- playlist rewriting -------------------------------------------------------

// rewriteRelayPlaylist maps every URI in an HLS playlist through mapURI.
//
// For a playlist Obelo itself wrote this changes NOTHING: every URI it emits is
// bare and relative ("000.ts", `#EXT-X-MAP:URI="init.mp4"`, "audio_<id>.m3u8"),
// and a relative URI resolves against the relay path correctly on its own — which
// is the whole reason the path tail is preserved. The rewrite exists for the
// absolute form: a sharer on a later version that emits "/api/v1/sessions/…"
// would otherwise send this household's player at its own server, where the
// session does not exist and the credential is not held. Handling it costs a
// pass over a few hundred lines and removes a silent, version-skewed failure that
// would appear only on a real player.
//
// Every place a URI can hide is covered: a bare line, and the URI= attribute of
// EXT-X-MAP (the fMP4 init segment), EXT-X-MEDIA (the demuxed audio and in-band
// subtitle renditions) and EXT-X-KEY (which this server never emits — it encrypts
// nothing — and which is rewritten anyway so a peer that does is not broken by
// the omission).
func rewriteRelayPlaylist(data []byte, mapURI func(string) string) []byte {
	if mapURI == nil {
		return data
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			lines[i] = rewriteURIAttribute(line, mapURI)
		default:
			lines[i] = mapURI(trimmed)
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// rewriteURIAttribute rewrites every URI="…" value in a playlist tag line.
func rewriteURIAttribute(line string, mapURI func(string) string) string {
	const attr = `URI="`
	var b strings.Builder
	rest := line
	for {
		i := strings.Index(rest, attr)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i+len(attr)])
		rest = rest[i+len(attr):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(mapURI(rest[:j]))
		b.WriteString(`"`)
		rest = rest[j+1:]
	}
}

// relayURIMapper maps an ABSOLUTE api URI from the sharer onto this session's
// relay path, preserving the tail. A relative URI is left exactly as it is — it
// already resolves correctly — and so is anything that is not one of this API's
// paths.
func relayURIMapper(localSessionID string) func(string) string {
	prefix := APIPrefix + "/relay/" + localSessionID + "/"
	return func(uri string) string {
		tail, ok := strings.CutPrefix(uri, APIPrefix+"/")
		if !ok || tail == "" {
			return uri
		}
		return prefix + tail
	}
}

// relayTokenURIMapper is the same for the stream-token entry point, where the
// credential — not a session id — is the path element, and this Server must not
// write it into a playlist it did not have to.
//
// It maps an absolute artifact URI of THIS session back to its bare filename,
// which resolves against the token playlist's own URL to the right token path.
// Everything else is left alone: a URI naming something outside this session's
// HLS directory has no expressible form under a token and must not be invented.
func relayTokenURIMapper(sess playback.Session) func(string) string {
	hlsRoot := APIPrefix + "/sessions/" + sess.RemoteSessionID + "/hls/"
	return func(uri string) string {
		if file, ok := strings.CutPrefix(uri, hlsRoot); ok && file != "" && !strings.Contains(file, "/") {
			return file
		}
		return uri
	}
}

// relayTailForArtifact maps the stream-token route's artifact name onto the
// sharer's path tail: "stream" is the progressive leaf and "hls/{file}" is an HLS
// artifact, the same two shapes that route names for a local session.
func relayTailForArtifact(sess playback.Session, artifact string) (string, bool) {
	if artifact == streamProgressiveArtifact {
		return "sessions/" + sess.RemoteSessionID + "/stream", true
	}
	file, ok := strings.CutPrefix(artifact, streamHLSArtifactPrefix)
	if !ok || file == "" || strings.Contains(file, "/") {
		return "", false
	}
	return "sessions/" + sess.RemoteSessionID + "/hls/" + file, true
}

// --- the relayed Decision -----------------------------------------------------

// relayDecisionResponse is the sharer's Decision, re-served.
//
// It is a COPY of what came back with four changes and no others, which is what
// "the sharer's answers pass through verbatim" (ADR-0056 §5) means in practice —
// a tier, a bitrate estimate, a stream list, a field a later version added: all
// of it reaches the client exactly as the machine holding the file wrote it.
//
//   - sessionId becomes the LOCAL session's. The remote one is this Server's
//     bookkeeping and means nothing to a client here.
//   - streamUrl and every subtitle url are rewritten onto the relay route.
//   - the sharer's streamToken is DROPPED, and this Server mints its own. Theirs
//     authorises their session on their routes; handing it to a client here would
//     be handing out a credential that this household's URLs cannot spend and that
//     the client cannot be told is useless.
func relayDecisionResponse(dec playback.Decision, localSessionID string) map[string]any {
	src := dec.Relay.Decision
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	out["sessionId"] = localSessionID
	delete(out, "streamToken")
	delete(out, "streamTokenExpiresAt")

	mapURL := relayURIMapper(localSessionID)
	if u, ok := out["streamUrl"].(string); ok {
		out["streamUrl"] = mapURL(u)
	}
	if subs, ok := out["subtitles"].([]any); ok {
		rewritten := make([]any, 0, len(subs))
		for _, entry := range subs {
			track, ok := entry.(map[string]any)
			if !ok {
				rewritten = append(rewritten, entry)
				continue
			}
			copied := make(map[string]any, len(track))
			for k, v := range track {
				copied[k] = v
			}
			if u, ok := copied["url"].(string); ok {
				copied["url"] = mapURL(u)
			}
			rewritten = append(rewritten, copied)
		}
		out["subtitles"] = rewritten
	}
	return out
}

// --- relayed artwork ----------------------------------------------------------

// serveRelayArtwork answers an artwork request for a MIRRORED entity, fetching
// the image from the sharer on the first request and serving it from the artwork
// cache afterwards (ADR-0056 §5). It reports whether it answered: false means
// "this is not a mirrored entity", and the caller writes its own 404.
//
// The scope guard is the caller's — every artwork handler asked the catalog
// first, and the catalog answers ErrNotFound for an out-of-scope entity exactly
// as it does for a missing one — with ONE addition: an entity whose Library is
// outside the Scope must not become fetchable by having no local artwork row, so
// the Library is re-checked here before a single byte is asked for.
func serveRelayArtwork(deps Deps, w http.ResponseWriter, r *http.Request, scope scopeAllower, kind, entityID, role string) bool {
	if deps.Links == nil || deps.Mirror == nil {
		return false
	}
	libraryID, err := relayLibraryOf(deps, kind, entityID)
	if err != nil || !linkedLibrary(deps, libraryID) || !scope.AllowsLibrary(libraryID) {
		return false
	}
	path, err := deps.Links.RelayArtwork(r.Context(), kind, entityID, role)
	switch {
	case err == nil:
		http.ServeFile(w, r, path)
	case errors.Is(err, link.ErrArtworkAbsent), errors.Is(err, link.ErrNotRelayed):
		// The sharer has no image for this entity, which is the same answer a local
		// Title with no poster gives.
		writeError(w, http.StatusNotFound, codeNotFound, "artwork not found", nil)
	case errors.Is(err, link.ErrCredentialDead):
		writeError(w, http.StatusServiceUnavailable, codeLinkRevoked,
			"the sharing server no longer accepts this server's credential", nil)
	default:
		writeError(w, http.StatusServiceUnavailable, codeLinkUnreachable,
			"the sharing server could not be reached", nil)
	}
	return true
}

// scopeAllower is the sliver of access.Scope the artwork guard needs.
type scopeAllower interface{ AllowsLibrary(id string) bool }

// relayLibraryOf resolves the Library behind an entity id, through the same
// mirror reader the writer guards use.
func relayLibraryOf(deps Deps, kind, entityID string) (string, error) {
	if kind == relayKindTitle {
		return deps.Mirror.LibraryOfTitle(entityID)
	}
	return deps.Mirror.LibraryOfEntity(kind, entityID)
}

// relayKindTitle is the entity kind a Movie / Episode / Track is addressed by,
// mirroring store.ExportTitle without importing the export vocabulary for one
// word.
const relayKindTitle = "title"

// --- passing the sharer's refusal through -------------------------------------

// relayRefusalOf unwraps a negotiation error into the sharer's own refusal, or
// nil when it is not one.
func relayRefusalOf(err error) *link.RemoteRefusal {
	var refusal *link.RemoteRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	return nil
}

// relayRefusalStatus is the status to answer with. The sharer's own is used
// as-is — that is the point — unless it is not a refusal status at all, which is
// a peer misbehaving and reads here as a bad gateway.
func relayRefusalStatus(ref *link.RemoteRefusal) int {
	if ref.Status < 400 || ref.Status > 599 {
		return http.StatusBadGateway
	}
	return ref.Status
}

// relayRefusalCode keeps the sharer's error code, falling back to this API's own
// vocabulary for a peer that answered a status with no envelope.
func relayRefusalCode(ref *link.RemoteRefusal) string {
	if ref.Code != "" {
		return ref.Code
	}
	if ref.Status == http.StatusNotFound {
		return codeNotFound
	}
	return codeLinkUnreachable
}
