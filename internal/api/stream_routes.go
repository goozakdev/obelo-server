package api

import (
	"net/http"
	"strings"

	"github.com/marioquake/obelo-server/internal/store"
)

// Path-carried session media (.scratch/session-stream-tokens, issue 02).
//
//	GET /api/v1/stream/{streamToken}/stream       progressive direct play
//	GET /api/v1/stream/{streamToken}/hls/{file}   master.m3u8 | index.m3u8 | NNN.ts
//	                                              | .m4s | init.mp4 | audio_{id}.*
//	                                              | subs_{id}.*
//
// This is a SECOND way to reach the SAME bytes the /sessions/{id}/… routes
// serve, for the class of player that can present neither credential those
// routes accept: one that hands the URL to somebody else and lets THEM fetch.
// AirPlay video is the motivating case — AVFoundation gives the receiver the
// URL, and that receiver is a television running software we do not own, so it
// will neither set an Authorization header nor reliably carry the ms_media
// cookie. Every existing route is untouched; this ADDS a path rather than
// altering one.
//
// # Why the token is in the PATH and not the query — do not "simplify" this
//
// Every playlist this server emits uses BARE RELATIVE URIs: "000.ts" from
// nameFmt (internal/playback/remux.go), `#EXT-X-MAP:URI="init.mp4"`, and
// audio_<id>.m3u8 / subs_<id>.m3u8 as rendition URI= values
// (internal/audio/hls.go, subtitle.RenditionLines). A player resolves those
// against the playlist's URL WITH THE QUERY STRING DISCARDED, so a "?token=" on
// a playlist never reaches its segments — the manifest would load and every
// segment would 401.
//
// A path prefix resolves correctly by construction: "000.ts" relative to
// /api/v1/stream/{tok}/hls/index.m3u8 is /api/v1/stream/{tok}/hls/000.ts. ZERO
// playlist bytes change, which is the whole point of the shape. The alternative
// is post-processing three playlist producers — the master builder, the remux
// media playlist, and FFMPEG'S OWN PLAYLIST, which this server deliberately
// serves verbatim for the runtimes that do not synthesize their own (remux,
// video-copy, audio-only; ADR-0024) precisely so the segment/playlist
// correspondence stays exact.
//
// So: a query parameter here would break on a real receiver and pass every test
// that only fetched a manifest. It is written down because the tidy-up is
// obvious and the breakage is not.
//
// # Posture
//
//   - GET only. No method on these paths mutates anything; there is no
//     token-authenticated progress report and no token-authenticated session
//     delete, and there must never be one.
//   - A bad, expired, or revoked token is a 404 with the SAME envelope a
//     wrong-User session fetch produces. Never 401/403, and never a body that
//     tells the three apart.
//   - The token must not be logged. It is in r.URL.Path, so the request that
//     reaches the handlers below carries a REDACTED path (see
//     redactStreamTokenRequest), and any outer access log redacts through
//     RedactPath.

const (
	// streamRoutePrefix is the subtree, relative to APIPrefix (the api mux sees
	// paths with the version prefix already stripped).
	streamRoutePrefix = "/stream/"
	// streamProgressiveArtifact is the direct-play leaf; everything else under a
	// token is an /hls/ artifact.
	streamProgressiveArtifact = "stream"
	// streamHLSArtifactPrefix is the HLS sub-path under a token. It mirrors the
	// "/hls/" element of the session-id routes so BOTH shapes of URL name the same
	// artifacts the same way — a client (or a human reading a log) does not have to
	// learn two spellings.
	streamHLSArtifactPrefix = "hls/"
	// redactedToken is what stands in for the secret anywhere a stream-token URL is
	// written down. A fixed marker rather than a truncation: a prefix of a secret is
	// still a search key, and there is nothing here worth debugging with.
	redactedToken = "[redacted]"
)

// handleStreamTokenSubtree dispatches every route under /stream/{streamToken}/…
// (see the file comment for why the token rides the path). It resolves the token
// to a session and a User and then calls the IDENTICAL handlers the session-id
// routes call — handleSessionStream and hlsArtifactHandler — so the two entry
// points differ in nothing but how the session id and User are established:
//
//	session-id path: requireAuthAllowCookie → User from bearer/cookie; session id
//	                 from the URL; ownership checked inside the handler.
//	token path:      ResolveStreamToken → session id AND User from the token;
//	                 ownership is what the token IS, so there is nothing else to
//	                 check.
func handleStreamTokenSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The METHOD gate runs FIRST, before the token is even looked at, and that
		// order is load-bearing: if verification ran first, a POST bearing a live
		// token would 405 while a POST bearing a dead one 404, and the difference
		// between the two answers is a token-validity oracle that needs no media at
		// all. Method-then-credential means every non-GET gets one answer.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
			return
		}
		raw, artifact, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, streamRoutePrefix), "/")
		if !ok || raw == "" || artifact == "" {
			refuseStreamToken(w)
			return
		}
		sessionID, userID, ok := deps.Auth.ResolveStreamToken(raw)
		if !ok {
			// Unknown, expired, revoked, and "an auth_tokens bearer pasted into the
			// path" are one indistinguishable refusal — the resolver deliberately
			// returns no error, so there is nothing here that COULD be rendered.
			refuseStreamToken(w)
			return
		}
		user, err := deps.Auth.User(userID)
		if err != nil {
			// The token names a User who is gone. The stream_tokens row cascades on
			// user delete so this should be unreachable; if it happens anyway it fails
			// closed, as the same 404.
			refuseStreamToken(w)
			return
		}

		// From here down the raw token exists ONLY in this function's locals: the
		// request handed on carries a redacted path, so nothing downstream — a
		// handler, a future middleware, a panic dump — can write the secret out.
		r = redactStreamTokenRequest(r)
		r = r.WithContext(withIdentity(r.Context(), streamTokenIdentity(user)))

		if artifact == streamProgressiveArtifact {
			// Progressive direct play. http.ServeContent still owns Range/206/If-Range
			// exactly as on the bearer path — an AirPlay receiver range-seeks, and this
			// route changes nothing about how that is answered.
			handleSessionStream(deps.Playback, sessionID)(w, r)
			return
		}
		file, ok := strings.CutPrefix(artifact, streamHLSArtifactPrefix)
		if !ok || file == "" || strings.Contains(file, "/") {
			refuseStreamToken(w)
			return
		}
		hlsArtifactHandler(deps, sessionID, file)(w, r)
	}
}

// refuseStreamToken is the ONE answer this subtree gives to every credential
// failure: unknown token, expired token, token whose session was revoked, an
// account bearer pasted into the path, and a malformed path all get it.
//
// It is byte-for-byte the envelope a wrong-User session fetch already produces
// (handleSessionStream's "session not found"), because a refusal that looked
// different from that one would say "this token was real, its session was not
// yours" — which is exactly the existence-hiding the posture forbids. 404, never
// 401 or 403: a 401 would invite a client to retry with a bearer, and a 403
// would confirm the resource exists.
func refuseStreamToken(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
}

// streamTokenIdentity is the identity a stream-token request runs as. It carries
// the token's User and NOTHING else:
//
//   - no Device, because a stream token is not a sign-in — the party fetching is
//     a television that never authenticated and has no Device row;
//   - no Token, because that field is the raw ACCOUNT credential (logout revokes
//     exactly it), and a stream token is not one. Putting the stream secret there
//     would both be a lie and hand it to every handler downstream.
//
// The handlers this reaches key on User.ID alone (session ownership). That is
// the invariant: anything reachable from the token route must be satisfiable
// with an id, and anything needing more than an id — a role check, a Device, the
// account token — must not be reachable from here at all.
func streamTokenIdentity(user store.User) identity {
	return identity{User: user}
}

// redactStreamTokenRequest returns a copy of r whose URL (and RequestURI) carry
// the redaction marker in place of the token.
//
// This is the cheap structural answer to "the secret is in the URL, and a URL is
// the thing servers write down". Rather than auditing every present and future
// consumer of r.URL, the token is removed from the request at the one point that
// has finished with it, so a logger, an error report, or a stack dump taken
// anywhere below cannot leak what it cannot see. RedactPath covers the other
// direction — an access log wrapped OUTSIDE the API handler, which sees the path
// before this function runs.
func redactStreamTokenRequest(r *http.Request) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = RedactPath(r.URL.Path)
	// RawPath holds the pre-decoding form and would still carry the token; the
	// redacted path needs no escaping, so dropping it is both correct and safe.
	r2.URL.RawPath = ""
	r2.RequestURI = RedactPath(r.RequestURI)
	return r2
}

// RedactPath replaces the stream token in a media URL path with a fixed marker,
// leaving every other path untouched. It is exported because the caller that
// most needs it is OUTSIDE this package: any access log, proxy log, or error
// reporter wrapping the server sees the raw path and will write it down by
// default. Route it through here first.
//
// It accepts the path with or without the /api/v1 prefix, because both forms
// exist: an outer middleware sees /api/v1/stream/{tok}/…, while the api mux sees
// /stream/{tok}/… after StripPrefix.
func RedactPath(p string) string {
	if rest, ok := strings.CutPrefix(p, APIPrefix+streamRoutePrefix); ok {
		return APIPrefix + streamRoutePrefix + redactFirstSegment(rest)
	}
	if rest, ok := strings.CutPrefix(p, streamRoutePrefix); ok {
		return streamRoutePrefix + redactFirstSegment(rest)
	}
	return p
}

// redactFirstSegment swaps the leading path segment (the token) for the marker
// and keeps the rest — the artifact name is not a secret and is the only part of
// the URL worth reading in a log.
func redactFirstSegment(rest string) string {
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return redactedToken + rest[i:]
	}
	return redactedToken
}
