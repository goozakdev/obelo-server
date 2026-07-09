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

	"github.com/marioquake/juicebox/internal/api"
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
const placeholderMarker = "juicebox-spa-placeholder"

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
	return mux
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
