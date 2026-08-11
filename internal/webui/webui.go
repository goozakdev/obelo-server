// Package webui serves the embedded React SPA (ADR-0012) and composes the
// top-level HTTP routing: requests under /api/v1 go to the API handler
// unchanged; everything else is served from the embedded Vite build with an
// index.html fallback so client-side routes deep-link and survive a refresh
// (ADR-0006, one process/port).
//
// # Build order
//
// The SPA's production bundle is embedded from ./dist via go:embed (below).
// The Vite build (in ../../web) must run BEFORE `go build`, copying its output
// into this package's dist/ directory — see the repo Makefile (`make build`
// runs `make web` first). A committed placeholder dist/index.html keeps
// `go build ./...` and `go test ./...` working standalone (without a Node
// toolchain); a real build overwrites it with the hashed asset bundle.
//
// If dist/ were ever missing, go:embed would be a COMPILE error — the desired
// loud failure. The placeholder avoids that for plain `go build`, while
// IsPlaceholder lets a deployment/CI check fail loudly on a stale/placeholder
// bundle.
package webui

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/marioquake/obelo-server/internal/api"
)

// dist holds the built SPA assets. The Vite build writes here before `go build`
// embeds them. A committed placeholder index.html guarantees the directory
// exists so this remains compilable standalone.
//
//go:embed all:dist
var dist embed.FS

// placeholderMarker appears in the committed placeholder index.html and nowhere
// in a real Vite build. IsPlaceholder uses it so build tooling can detect that
// the real frontend bundle was never built in.
const placeholderMarker = "obelo-spa-placeholder"

// Handler wraps the API handler with SPA serving. The returned handler routes:
//
//   - /api/v1, /api/v1/...  → apiHandler (unchanged; unknown API paths still get
//     the NOT_FOUND envelope, never index.html)
//   - everything else       → an embedded static asset if one matches the path,
//     otherwise index.html (the SPA fallback for client-side routes)
//
// Distinguishing the two is purely by prefix: anything under the API prefix is
// always the API's concern, so a typo like /api/v1/nope returns the API's JSON
// 404 rather than the SPA shell. Only non-API paths fall back to index.html.
//
// Everything the routed handler produces then passes through securityHeaders —
// this is the one place that sees every response the process emits, which is why
// the baseline security headers live here rather than in the API's own
// middleware chain.
func Handler(apiHandler http.Handler) http.Handler {
	static := newStaticHandler()

	mux := http.NewServeMux()
	// The API owns its whole subtree. Registering both forms covers the bare
	// prefix and the trailing-slash subtree; the API handler itself enveloped
	// 404s for misses under it.
	mux.Handle(api.APIPrefix, apiHandler)
	mux.Handle(api.APIPrefix+"/", apiHandler)
	// Everything else is the SPA.
	mux.Handle("/", static)
	return securityHeaders(mux)
}

// contentSecurityPolicy is the policy served with EVERY response. It is the
// defense-in-depth behind React's escaping: the SPA keeps the bearer token in
// localStorage when "Remember me" is on (web/src/auth/session.tsx), so one
// successful script injection is a full account takeover, not a defacement.
// Nothing in the app uses dangerouslySetInnerHTML today, which is exactly the
// property a CSP is here to survive losing.
//
// EVERY source expression below is load-bearing and was validated against the
// real Vite bundle driven in a browser, including live HLS playback. Read the
// per-directive notes before tightening anything — the two blob: allowances in
// particular look like leftovers and are not (see media-src/worker-src). A CSP
// that silently breaks video playback is worse than no CSP at all: the failure
// lands mid-movie in a household member's browser, with nothing in the server
// log to explain it.
//
// Deliberately ABSENT:
//
//   - Strict-Transport-Security (see securityHeaders).
//   - upgrade-insecure-requests, for the same reason: this server speaks plain
//     HTTP by design (ADR-0005), so upgrading its own same-origin subresources
//     to https:// would break every LAN install instantly.
//   - report-uri/report-to: there is no collector to send violations to, and a
//     self-hosted box should not be phoning anywhere.
const contentSecurityPolicy = "default-src 'self'; " +
	// Backstop for anything not named below (fetch destinations, manifest,
	// fonts). 'self' is correct because the whole app — SPA shell, hashed
	// assets, API, artwork, media — is served from this one origin/port
	// (ADR-0006). There are no fonts and no web manifest today; if either is
	// added under /assets it is already covered.
	"base-uri 'none'; " +
	// The app never emits a <base> tag. Blocking it stops an injected
	// <base href="//evil"> from re-pointing every relative asset/API URL,
	// which is the classic way to escape a 'self'-only script-src.
	"object-src 'none'; " +
	// No <object>/<embed>/plugin content anywhere; legacy plugin embedding is
	// a script-execution path with no upside here.
	"frame-ancestors 'none'; " +
	// Clickjacking: nothing may frame the management UI. Paired with
	// X-Frame-Options: DENY for clients that predate frame-ancestors.
	"frame-src 'none'; " +
	// The app embeds no iframes. Stated explicitly rather than left to fall
	// back, because child-src (below, for old WebKit's worker handling) would
	// otherwise become the fallback and quietly permit blob: frames.
	"form-action 'self'; " +
	// Every <form> in the app is a React onSubmit with no action attribute, so
	// a native submit would target the current URL — same origin. 'self' rather
	// than 'none' so that a form which does fall through to a native submit
	// still works; 'none' would turn a missing preventDefault into a blocked
	// navigation that looks like a dead button.
	"script-src 'self'; " +
	// Verified against a real `make web` build: index.html carries NO inline
	// script — Vite emits a single <script type="module" crossorigin
	// src="/assets/index-<hash>.js"> plus same-origin dynamic chunks (hls.js is
	// lazy-imported). So no 'unsafe-inline', no nonce plumbing, and no
	// 'unsafe-eval': neither the app bundle nor hls.js contains a `new
	// Function(` or eval() call.
	"style-src 'self'; " +
	// Also verified, and the answer that surprises people: 'unsafe-inline' is
	// NOT needed. Vite extracts all CSS to /assets/index-<hash>.css, and React's
	// style={{…}} prop writes through CSSOM (element.style.color = …), which CSP
	// does not police — only literal style attributes and injected <style>
	// elements are, and the app produces neither. If a dependency ever starts
	// injecting a <style> tag at runtime, the fix is a hash or a nonce, not
	// 'unsafe-inline': with 'unsafe-inline' in style-src an injected style can
	// still be used to reskin the page for phishing and to exfiltrate DOM
	// contents through attribute selectors. One known, accepted casualty: the
	// committed dist/index.html PLACEHOLDER carries an inline <style>, so the
	// "web UI was not built" page renders unstyled. It is a build-mistake page,
	// its text is the whole point, and styling it is not worth a hash that would
	// have to be kept in lockstep with that file.
	"img-src 'self'; " +
	// EVERY image this app renders is same-origin: library artwork, cast
	// headshots, and — since the metadata-provider image proxy landed — the admin
	// Edit-item pickers' candidate thumbnails too. Those last ones used to come
	// straight off image.tmdb.org and the Cover Art Archive, which is why this
	// directive carried `https:`; the server now fetches them itself and serves
	// them from GET /api/v1/providerImage (internal/api/provider_image.go), so
	// nothing is left for `https:` to permit except an outbound signalling
	// channel for injected script. Verified by grep AND in a browser with this
	// exact policy: the record-search picker, the Fix-label image grid, and the
	// browse/detail screens load with zero CSP violations and zero third-party
	// requests.
	//
	// If a screen ever needs a genuinely third-party image, proxy it the same way
	// rather than restoring `https:` — a self-hosted media server that phones a
	// stranger's host from the household's browsers is the thing ADR-0001 exists
	// to prevent, and the CSP is what keeps that decision from being made by
	// accident.
	"media-src 'self' blob:; " +
	// 'self' is direct play and the HLS segments. blob: is NOT optional and is
	// NOT theoretical: the hls.js MSE path attaches the stream by setting
	// video.src to a blob: object URL wrapping the MediaSource, so this fires on
	// every transcode/directStream playback in Chrome/Firefox/Edge — every
	// non-Safari browser in the house (Safari takes the native-HLS path, which is
	// plain 'self'). Measured with this exact policy minus the blob:, driving a
	// real transcode session: Chrome blocks the attach and <video> fails with
	// MEDIA_ELEMENT_ERROR "Media load rejected by URL safety check" — a dead
	// player with nothing in the server log. Do not remove it.
	"connect-src 'self'; " +
	// The API, hls.js's own playlist/segment fetches, and the SSE stream at
	// GET /api/v1/events via EventSource (ADR-0016) — all same-origin, because
	// the SPA is served from the same origin/port it calls (ADR-0006).
	"worker-src 'self' blob:; " +
	// blob: is hls.js, and it is the one allowance here that is deliberately
	// AHEAD of what the current bundle does. hls.js's demuxer worker is built as
	// `new Worker(URL.createObjectURL(blob))` — there is no workerPath, so there
	// is no same-origin URL that could be allowed instead. It does not fire
	// today only because of a packaging accident: the worker body is inlined as
	// the global __HLS_WORKER_BUNDLE__ in hls.js's UMD dist, NOT in dist/hls.mjs,
	// and Vite resolves the "import" condition, so hls.js's own
	// `typeof __HLS_WORKER_BUNDLE__ === "function"` guard is false and it demuxes
	// on the main thread (enableWorker stays true; web/src/player/hls.ts does not
	// turn it off). Anything that flips that guard — hls.js changing its ESM
	// packaging, a bundler-resolution change, someone setting workerPath —
	// re-arms the blob: worker. Measured, not assumed: serving this same policy
	// with `worker-src 'self'` makes Chrome refuse the construction outright
	// ("Creating a worker from 'blob:…' violates … worker-src 'self'"). Keeping
	// blob: costs nothing (a blob: worker can only run script the page already
	// managed to create, which script-src 'self' is what actually guards) and
	// saves a future upgrade from turning into a mystery playback regression.
	"child-src blob:"
	// The pre-worker-src fallback: WebKit before 15.4 honors child-src for
	// workers and ignores worker-src entirely. Cheap insurance for an old Safari
	// that ends up on the MSE path (a current Safari takes native HLS and spawns
	// no worker at all). frame-src above is explicit so this line cannot
	// accidentally become the frame policy.

// securityHeaders wraps h so every response — SPA shell, hashed asset, API
// envelope, artwork, and media byte-range alike — carries the same baseline
// security headers.
//
// ONE uniform set, deliberately, rather than a tighter policy for /api/v1 and a
// looser one for the SPA. The usual API hardening (default-src 'none') would be
// actively wrong here: the API subtree is not only JSON — it serves artwork,
// progressive media, and HLS segments, and a top-level navigation to one of
// those URLs (an operator debugging a stream, a media document opened in a tab)
// creates a real document that then needs 'self'/blob: to render itself. And
// nosniff already means no browser treats an application/json response as
// markup, so the CSP on an API response is close to inert anyway. Two policies
// would buy nothing and would guarantee that one of them eventually drifts.
//
// Headers are set on the way IN, before h writes anything, so they land on every
// status code including the 304s http.ServeContent produces for hashed assets.
//
// NO Strict-Transport-Security, and this is a decision rather than an omission.
// This server speaks plain HTTP and expects the operator's TLS-terminating proxy
// to own transport security (ADR-0005), so HSTS is that proxy's call to make
// with knowledge of its own certificate and hostname. It is also uniquely
// unforgiving to get wrong from here: HSTS is sticky per hostname, so a browser
// that sees it once for a LAN name refuses plain HTTP to that name for the whole
// max-age — no warning to click through, and undoing it means a trip to
// chrome://net-internals on every device in the household. Emitting it from a
// server that is usually reached over http:// on a LAN hostname could brick the
// only path a family has to their media. Do not "complete the set" by adding it.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		// Never let a browser second-guess our Content-Type. Matters most for
		// the API's JSON envelopes and for user-supplied bytes (uploaded
		// artwork, media files): sniffing is how a file that we call
		// application/json or image/jpeg gets executed as HTML.
		head.Set("X-Content-Type-Options", "nosniff")
		// Belt-and-braces with CSP frame-ancestors above: frame-ancestors is
		// the modern, more expressive directive, but X-Frame-Options is what
		// pre-CSP2 clients understand, and they are exactly the clients least
		// able to defend themselves.
		head.Set("X-Frame-Options", "DENY")
		// no-referrer, not the browser default (strict-origin-when-cross-
		// origin), because this app's URLs are themselves sensitive: the
		// direct-file download carries ?token=<session token> in the URL
		// (api.requireAuthAllowQueryToken), and library/session/title IDs sit in
		// the path. The default would still hand the full URL to every
		// same-origin request. It also used to be the only thing standing between
		// the metadata providers and this server's (often home DDNS) hostname,
		// back when the admin pickers loaded thumbnails from TMDB and the Cover
		// Art Archive directly; that hole is now closed at the source (the images
		// are proxied, and img-src is 'self'), which makes this header defense in
		// depth rather than the defense. Keep it: nothing server-side reads
		// Referer, so there is nothing to lose by sending none.
		head.Set("Referrer-Policy", "no-referrer")
		head.Set("Content-Security-Policy", contentSecurityPolicy)
		h.ServeHTTP(w, r)
	})
}

// staticHandler serves files from the embedded dist FS with an index.html
// fallback for unknown paths (client routes).
type staticHandler struct {
	root      fs.FS
	indexHTML []byte
}

func newStaticHandler() *staticHandler {
	// Strip the "dist" prefix so request paths map cleanly onto asset names.
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Impossible unless the embed directive is broken; fail loudly at boot.
		panic("webui: embedded dist subtree missing: " + err.Error())
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("webui: embedded dist/index.html missing: " + err.Error())
	}
	return &staticHandler{root: sub, indexHTML: index}
}

// IsPlaceholder reports whether the embedded bundle is the committed
// placeholder rather than a real Vite build. A deployment/CI gate can call this
// to fail loudly before shipping a binary with no real UI.
func IsPlaceholder() bool {
	index, err := dist.ReadFile("dist/index.html")
	if err != nil {
		return true
	}
	return isPlaceholderContent(index)
}

// isPlaceholderContent is the pure detection rule: index.html that still carries
// the placeholder marker is not a real build. Kept separate so it can be tested
// against literal content without depending on whatever bundle currently sits in
// the embedded dist/ (which flips between placeholder and real across builds).
func isPlaceholderContent(index []byte) bool {
	return bytes.Contains(index, []byte(placeholderMarker))
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// The SPA is static; only GET/HEAD make sense. Anything else is a 405.
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		h.serveIndex(w, r)
		return
	}

	f, err := h.root.Open(name)
	if err != nil {
		// No such asset → this is a client-side route; serve the SPA shell so
		// deep links and refreshes load the app (PRD user story 37).
		h.serveIndex(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		// A directory path (e.g. "/assets") is not a servable asset; fall back.
		h.serveIndex(w, r)
		return
	}

	// Serve the real asset. http.ServeContent sets Content-Type from the
	// extension and handles Range/conditional requests, which matters for the
	// hashed JS/CSS bundles.
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// embed.FS files implement io.ReadSeeker; this is defensive.
		h.serveIndex(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
}

// serveIndex writes index.html with a 200 (not 404): the SPA owns routing, so a
// path the server doesn't recognize as an asset is a client route, and the app
// resolves it (or shows its own not-found). index.html is never cached so a new
// deploy is picked up immediately; hashed assets carry their own immutable URLs.
func (h *staticHandler) serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.indexHTML)
}
