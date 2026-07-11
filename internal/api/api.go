// Package api is the HTTP/JSON transport for the unified API (ADR-0010).
//
// It serves everything under the /api/v1 path prefix with camelCase JSON
// fields and the standard error envelope (docs/api-contract.md). The package
// is deliberately thin: it routes requests and translates domain values to and
// from JSON, leaving behavior to the domain packages (server metadata here;
// auth/library/catalog in later slices).
package api

import (
	"net/http"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/auth"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/library"
	"github.com/marioquake/juicebox/internal/match"
	"github.com/marioquake/juicebox/internal/organize"
	"github.com/marioquake/juicebox/internal/playback"
	"github.com/marioquake/juicebox/internal/scanner"
	"github.com/marioquake/juicebox/internal/server"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subfetch"
)

// APIPrefix is the version path prefix every route lives under.
const APIPrefix = "/api/v1"

// ScanStatusReader reads the pollable per-Library scan status. *store.DB
// satisfies it; the narrow interface keeps the HTTP layer testable.
type ScanStatusReader interface {
	ScanStatusByLibrary(libraryID string) (store.ScanStatus, error)
}

// LibraryExister reports whether a Library exists, so a status read can answer
// 404 for an unknown Library without loading it.
type LibraryExister interface {
	LibraryExists(id string) (bool, error)
}

// Deps are the dependencies the API surface needs. Later slices extend this
// (auth service, library service, ...). Keeping it a struct of interfaces keeps
// the HTTP layer testable without a live database.
type Deps struct {
	Meta     *server.Metadata
	Auth     *auth.Service
	Access   *access.Service
	Library  *library.Service
	Scanner  *scanner.Service
	Catalog  *catalog.Service
	Match    *match.Service
	Playback *playback.Service
	Enrich   *enrich.Service
	// Organize is the authored-grouping domain (Collections now; Playlists later).
	Organize *organize.Service
	// Events is the realtime SSE Broker (ADR-0016): the /events handler subscribes
	// to it and producers publish enrichProgress. May be nil in narrow unit tests.
	Events *events.Broker
	// EnrichTrigger enqueues a non-blocking background Enrichment pass for a
	// Library after a successful scan (auto-after-scan). The app wires it to the
	// enrich worker queue; it is a no-op when auto-enrich is off (or nil in unit
	// tests), so the scan path never blocks on enrichment.
	EnrichTrigger func(libraryID string)
	ScanStatus    ScanStatusReader
	Libraries     LibraryExister
	// Providers is the DB-backed metadata-provider settings store (Admin-scope
	// /settings/metadata-providers). *store.DB satisfies it. May be nil in narrow
	// unit tests that don't exercise the settings surface.
	Providers ProviderSettingsStore
	// ProviderManager rebuilds + hot-swaps the running Enrichment provider after a
	// settings save (metadata-providers 02). The PUT handler calls Reload; nil
	// leaves persistence working without the runtime swap.
	ProviderManager *enrich.Manager
	// SettingsChanged, when set, is poked by the settings PUT after a successful
	// save + Manager.Reload to wake the scheduled-enrich goroutine so a changed
	// EnrichInterval applies promptly (enrichment-runtime-settings). Nil-safe: a
	// unit test without the app wiring simply skips the wake.
	SettingsChanged func()

	// Per-Library Enrichment policy (ADR-0027, Admin-scope
	// /libraries/{id}/enrichment-policy). EnrichmentPolicy persists the sparse
	// policy; PolicyResolver derives the effective/inherited enablement for display
	// (*enrich.Manager satisfies it); ReEnrichLibrary invalidates the Library's
	// cached effective provider and kicks an immediate re-enrich. Each may be nil in
	// narrow unit tests that don't exercise the policy surface.
	EnrichmentPolicy EnrichmentPolicyStore
	PolicyResolver   EnrichmentPolicyResolver
	ReEnrichLibrary  func(libraryID string)

	// SubFetch is the external subtitle-fetch domain (ADR-0021, subtitles slice 05):
	// the "search online → pick → track appears" flow behind the captions menu. Nil
	// in narrow unit tests that don't exercise fetching.
	SubFetch *subfetch.Service
	// SubtitleProviders is the DB-backed subtitle-provider settings store (Admin-scope
	// /settings/subtitle-providers). *store.DB satisfies it. May be nil in narrow tests.
	SubtitleProviders SubtitleProviderSettingsStore
	// SubtitleProviderManager rebuilds + hot-swaps the running subtitle provider after
	// a settings save. The PUT handler calls Reload; nil leaves persistence working
	// without the runtime swap.
	SubtitleProviderManager *subfetch.Manager
}

// Handler builds the root http.Handler for the whole API, mounted at /api/v1.
// Unknown paths under the prefix return the standard NOT_FOUND envelope rather
// than Go's plain-text 404, so every response a client sees is well-formed.
func Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// Register /server for all methods and gate the method inside the handler:
	// a catch-all "/" route (below) would otherwise shadow ServeMux's built-in
	// 405 handling, so requireMethod gives us an enveloped 405 with an Allow
	// header for non-GET requests to a known path.
	mux.HandleFunc("/server", requireMethod(http.MethodGet, handleServerInfo(deps.Meta)))

	// Authentication spine (ADR-0013, ADR-0015). Setup and login are public;
	// logout and the devices endpoints sit behind the bearer-auth middleware,
	// which attaches the authenticated identity to the request context.
	mux.HandleFunc("/setup", requireMethod(http.MethodPost, handleSetup(deps.Auth)))
	mux.HandleFunc("/auth/login", requireMethod(http.MethodPost, handleLogin(deps.Auth)))
	mux.HandleFunc("/auth/logout",
		requireMethod(http.MethodPost, requireAuth(deps.Auth, handleLogout(deps.Auth))))

	// GET /devices lists the caller's Devices.
	mux.HandleFunc("/devices",
		requireMethod(http.MethodGet, requireAuth(deps.Auth, handleListDevices(deps.Auth))))
	// DELETE /devices/{id} revokes a Device (self or Admin).
	mux.HandleFunc("/devices/",
		requireMethod(http.MethodDelete, requireAuth(deps.Auth, handleDeleteDevice(deps.Auth))))

	// User management (ADR-0010 admin scope): create/list/get/delete Users and
	// reset passwords. Every route is Admin-only — requireAdmin layers on
	// requireAuth, which attaches the identity. Method dispatch happens inside the
	// handlers because both subtrees serve more than one method (POST/GET on the
	// collection; GET/DELETE on a single User, PUT on its /password sub-resource).
	mux.HandleFunc("/users",
		requireAuth(deps.Auth, requireAdmin(handleUsersCollection(deps.Auth))))
	mux.HandleFunc("/users/",
		requireAuth(deps.Auth, requireAdmin(handleUserSubtree(deps))))

	// Library management (ADR-0010 admin scope). Every route is Admin-only:
	// requireAdmin layers on requireAuth, which attaches the identity. Method
	// dispatch happens inside the handlers because both subtrees serve more than
	// one method. POST/GET on the collection; GET/DELETE on a single Library.
	mux.HandleFunc("/libraries",
		requireAuth(deps.Auth, handleLibrariesCollection(deps)))
	// The /libraries/ subtree carries both Admin single-Library ops
	// (GET/DELETE /libraries/{id}) and the authenticated browse/scan
	// sub-resources (/libraries/{id}/titles, /libraries/{id}/scan). A single
	// dispatcher routes by sub-resource so each leaf applies its own auth scope
	// (scan POST is Admin; browse + status are any authenticated User).
	mux.HandleFunc("/libraries/",
		requireAuth(deps.Auth, handleLibrarySubtree(deps)))

	// The /titles/ subtree serves these leaves (all authenticated):
	//   GET  /titles/{id}            → one Title with nested Editions/Files/Streams
	//   GET  /titles/{id}/artwork/.. → local artwork bytes (bearer OR media cookie)
	//   POST /titles/{id}/playback   → direct-play negotiation (ADR-0003 tier 1)
	//   PUT  /titles/{id}/watchState → manual watched/unwatched toggle
	// The dispatcher applies auth PER LEAF (not an outer requireAuth) because the
	// artwork GET must also accept the media cookie (browser <img>), while every
	// other leaf stays bearer-only.
	mux.HandleFunc("/titles/", handleTitleSubtree(deps))

	// TV browse hierarchy (issue tv-music/01), both authenticated GETs:
	//   GET /shows/{id}/seasons   → a Show's Seasons (ordered, hidden excluded)
	//   GET /seasons/{id}/episodes → a Season's Episodes (Titles)
	// Access control is 404-not-403 (a Show/Season in no accessible Library is
	// hidden), exactly like Movies. An unknown id is also 404.
	//   GET /shows/{id}/artwork/{role}    → fetched Show poster/backdrop (cookie ok)
	//   GET /seasons/{id}/artwork/{role}  → fetched Season poster (cookie ok)
	// The subtree dispatchers apply auth per leaf so the artwork GET accepts the
	// media cookie (browser <img>) while the listings stay bearer-only.
	mux.HandleFunc("/shows/", handleShowSubtree(deps))
	mux.HandleFunc("/seasons/", handleSeasonSubtree(deps))

	// Music browse hierarchy (issue tv-music/03), all authenticated:
	//   GET /artists/{id}/albums  → an Artist's Albums (ordered, hidden excluded)
	//   GET /albums/{id}/tracks   → an Album's Tracks (Titles) in disc/track order
	//   GET /albums/{id}/artwork  → the Album's local cover image (cookie-capable
	//                               so a browser <img> can load it)
	// Access control is 404-not-403 (an Artist/Album in no accessible Library is
	// hidden), exactly like Movies/TV. An unknown id is also 404.
	//   GET /artists/{id}/artwork/{role} → fetched Artist image (cookie ok)
	mux.HandleFunc("/artists/", handleArtistSubtree(deps))
	mux.HandleFunc("/albums/", handleAlbumSubtree(deps))

	// Cast headshots (cast-photos/01), cookie-capable so a browser <img> loads a
	// face with only the media cookie:
	//   GET /people/{personRef}/artwork/{role}  → fetched person headshot bytes
	// A person is not a catalog entity with a Library; access follows the Titles
	// that credit the ref (a person only reachable through an inaccessible Library
	// is hidden as 404), and an unknown/photoless ref is also 404. Auth is applied
	// per leaf so the GET accepts the media cookie, mirroring the artwork subtrees.
	mux.HandleFunc("/people/", handlePersonSubtree(deps))

	// Playback session lifecycle (ADR-0004 progressive byte-range). The dispatcher
	// applies auth PER LEAF because GET {id}/stream must also accept the media
	// cookie (browser <video>), while POST {id}/progress and DELETE {id} stay
	// bearer-only. Ownership is enforced inside the handlers (another User's
	// session is hidden as 404). GET {id}/stream serves the File bytes with Range
	// support; DELETE {id} is the clean stop.
	mux.HandleFunc("/sessions/", handleSessionSubtree(deps))

	// Collections (collections-playlists 01): Admin-curated, shared groupings of
	// Titles. Writes (POST/PUT/DELETE on the Collection and its items) are Admin
	// scope; reads (GET list/detail) are any authenticated User. A `/collections`
	// + `/collections/` pair mirrors the `/libraries` dispatcher, routing by
	// sub-resource and gating each leaf's scope inside the handler. Reads return
	// FULL membership in this slice (the per-viewer access filter is issue 02).
	mux.HandleFunc("/collections",
		requireAuth(deps.Auth, handleCollectionsCollection(deps)))
	mux.HandleFunc("/collections/",
		requireAuth(deps.Auth, handleCollectionSubtree(deps)))

	// Playlists (collections-playlists 03): User-owned, ordered, single-media-kind
	// queues. Every route is public scope with the caller as owner — no Admin
	// override. A `/playlists` + `/playlists/` pair mirrors the `/collections`
	// dispatcher; ownership is enforced inside each handler and a foreign Playlist
	// is hidden as 404 (like another User's playback session). The GET detail leaf
	// is wrapped with requireScope inside the subtree so member resolution uses the
	// caller's (== owner's) access Scope. Ordering ops (reorder, remove-by-item-id)
	// land in issue 04.
	mux.HandleFunc("/playlists",
		requireAuth(deps.Auth, handlePlaylistsCollection(deps)))
	mux.HandleFunc("/playlists/",
		requireAuth(deps.Auth, handlePlaylistSubtree(deps)))

	// Watchlist (watchlist 01): the per-User system Playlist, addressed by NAME
	// rather than id — the server resolves (and lazily seeds) "the caller's
	// Watchlist" on every touch. Owner == caller like /playlists; the GET detail
	// resolves the caller's access Scope inside the handler.
	mux.HandleFunc("/watchlist",
		requireAuth(deps.Auth, handleWatchlist(deps)))
	mux.HandleFunc("/watchlist/",
		requireAuth(deps.Auth, handleWatchlistSubtree(deps)))

	// Metadata-provider settings (ADR-0010 admin scope, metadata-providers 02).
	// Every route is Admin-only — requireAdmin layers on requireAuth. Method +
	// sub-resource dispatch happen inside the handler (like /users): GET/PUT on the
	// collection, POST on the {slug}/test leaf.
	mux.HandleFunc("/settings/",
		requireAuth(deps.Auth, requireAdmin(handleSettingsSubtree(deps))))

	// GET /files/{id}/download: the sessionless direct-file stream behind the
	// "Open in VLC" affordance. Auth is bearer OR ?token= (an external player on a
	// downloaded .xspf cannot send a header or the media cookie). Original bytes,
	// byte-range capable; no Playback session is involved.
	mux.HandleFunc("/files/", handleFileSubtree(deps))

	// GET /home: the per-User computed Home surface — Continue Watching +
	// Recently Added rows (issue 08). Authenticated; computed, never stored.
	mux.HandleFunc("/home",
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleHome(deps.Catalog)))))

	// GET /search?q=: cross-kind search (issue tv-music/04) — Movies, Shows,
	// Artists/Albums, and (drilling in) Episodes/Tracks in one grouped response,
	// access-filtered (hidden excluded) exactly like browse. Authenticated.
	mux.HandleFunc("/search",
		requireMethod(http.MethodGet, requireAuth(deps.Auth, requireScope(deps.Access, handleSearch(deps.Catalog)))))

	// GET /events: the single server→client SSE stream (ADR-0016). Cookie-capable
	// auth because a browser EventSource cannot set an Authorization header (same
	// rationale as the media GETs); native clients may use the bearer header.
	mux.HandleFunc("/events",
		requireMethod(http.MethodGet, requireAuthAllowCookie(deps.Auth, requireScope(deps.Access, handleEvents(deps.Events)))))

	// Catch-all: anything not matched returns the standard NOT_FOUND envelope.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// ServeMux routed here because no pattern matched. If the path exists
		// but the method is wrong, net/http's mux would have returned 405 with
		// an Allow header; this handler covers genuinely unknown paths.
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	})

	// Mount under the version prefix and strip it so handlers register clean
	// paths ("/server"). The trailing-slash redirect for the bare prefix is
	// handled by registering both forms.
	root := http.NewServeMux()
	root.Handle(APIPrefix+"/", http.StripPrefix(APIPrefix, mux))
	root.HandleFunc(APIPrefix, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	})

	// Anything outside /api/v1 entirely.
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	})

	return root
}
