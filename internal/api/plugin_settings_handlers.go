package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins"
)

// Admin-scope Installed-plugin management (ADR-0058, .scratch/plugin-system issue
// 10): the surface an Admin installs a Plugin through, without a restart and
// without touching the filesystem.
//
// It is NOT the Extension points' settings surface. What a Plugin DOES — which
// events a sink hears, which URL it posts to, the key it signs with — is
// configured on the screen for its Extension point, through the same endpoint as
// the Built-in beside it (/settings/event-sinks, /settings/metadata-providers,
// /settings/subtitle-providers). That sameness is the design of ADR-0057 and this
// surface must not duplicate it. What lives here is only what a Built-in cannot
// have: getting the code onto the server, switching it on and off, forgiving a
// failure, and taking it away again.
//
//	GET    /settings/plugins            → what is installed
//	POST   /settings/plugins            → install from an upload (multipart)
//	POST   /settings/plugins/from-url   → install from a pasted URL
//	POST   /settings/plugins/{id}/enable
//	POST   /settings/plugins/{id}/disable
//	POST   /settings/plugins/{id}/reenable
//	DELETE /settings/plugins/{id}       → uninstall
//
// Every route is Admin-only, behind the same requireAuth + requireAdmin the whole
// /settings/ subtree is behind. A Member gets the same 403 every other settings
// endpoint gives them, which is the point: this is not a special surface with a
// special rule.

// PluginManager is the Installed-plugin lifecycle the handlers drive.
// *plugins.Manager satisfies it; the narrow interface is what keeps the HTTP layer
// free of a wasm runtime.
type PluginManager interface {
	List(ctx context.Context) ([]plugins.Installed, error)
	Install(ctx context.Context, manifest, module []byte, source string) (plugins.Installed, error)
	InstallFromURL(ctx context.Context, url string) (plugins.Installed, error)
	SetEnabled(ctx context.Context, id string, enabled bool) (plugins.Installed, error)
	Reenable(ctx context.Context, id string) (plugins.Installed, error)
	Uninstall(ctx context.Context, id string) error
}

// --- Wire shapes ------------------------------------------------------------

// pluginsResponse is the GET body and what every verb answers with, so a client
// that just enabled something already holds the whole new truth and needs no
// second request.
type pluginsResponse struct {
	Plugins []plugins.Installed `json:"plugins"`
}

// installFromURLRequest is the POST /settings/plugins/from-url body: the URL of a
// manifest.json, with the module beside it.
type installFromURLRequest struct {
	URL string `json:"url"`
}

// maxUploadBytes bounds the whole multipart body. It is the module cap plus room
// for the manifest and the MIME framing, so the refusal an Admin gets for an
// oversized module is the module's own sentence rather than a bare 413 from the
// parser.
const maxUploadBytes = plugins.MaxModuleBytes + (1 << 20)

// --- Routing ----------------------------------------------------------------

// handlePluginSettingsSubtree dispatches the /settings/plugins routes off the
// /settings/ subtree (already behind requireAuth + requireAdmin).
func handlePluginSettingsSubtree(deps Deps, rest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.PluginManager == nil {
			// A build wired without the loader answers honestly rather than with a
			// 404 that reads exactly like a typo'd path.
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"installed plugins are not available on this server", nil)
			return
		}
		tail := strings.TrimPrefix(rest, "plugins")
		tail = strings.TrimPrefix(tail, "/")

		switch {
		case tail == "":
			switch r.Method {
			case http.MethodGet:
				handleGetPlugins(deps)(w, r)
			case http.MethodPost:
				handleInstallPlugin(deps)(w, r)
			default:
				w.Header().Set("Allow", "GET, POST")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			}
		case tail == "from-url":
			requireMethod(http.MethodPost, handleInstallPluginFromURL(deps))(w, r)
		default:
			id, verb, hasVerb := strings.Cut(tail, "/")
			if id == "" || strings.Contains(verb, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			if !hasVerb {
				requireMethod(http.MethodDelete, handleUninstallPlugin(deps, id))(w, r)
				return
			}
			switch verb {
			case "enable":
				requireMethod(http.MethodPost, handleSetPluginEnabled(deps, id, true))(w, r)
			case "disable":
				requireMethod(http.MethodPost, handleSetPluginEnabled(deps, id, false))(w, r)
			case "reenable":
				requireMethod(http.MethodPost, handleReenablePlugin(deps, id))(w, r)
			default:
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			}
		}
	}
}

// --- GET --------------------------------------------------------------------

func handleGetPlugins(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writePluginList(w, r, deps)
	}
}

// --- Install ----------------------------------------------------------------

// handleInstallPlugin takes the two files a Plugin is, as multipart/form-data:
//
//	manifest — the manifest.json, byte for byte as the author wrote it
//	module   — the .wasm
//
// Two parts rather than one archive. An archive would make this server pick a
// container format, unpack paths it did not choose, and defend against a member
// called "../../obelo.db"; two named parts need none of that, and they are the
// same two files under the same two names they take on disk — so what an Admin
// uploads, what a URL install fetches and what the loader reads are one layout
// described once.
func handleInstallPlugin(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"send the plugin as multipart/form-data with a `manifest` part and a `module` part", nil)
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()

		manifest, ok := readUploadPart(w, r, "manifest", plugins.MaxManifestBytes)
		if !ok {
			return
		}
		module, ok := readUploadPart(w, r, "module", plugins.MaxModuleBytes)
		if !ok {
			return
		}
		installed, err := deps.PluginManager.Install(r.Context(), manifest, module, plugins.SourceUpload)
		if err != nil {
			writePluginError(w, err, "failed to install the plugin")
			return
		}
		writePluginInstalled(w, r, deps, installed)
	}
}

// handleInstallPluginFromURL installs from a pasted URL: the manifest at that URL,
// and the module beside it. The fetch goes through the safe fetcher, and — unlike
// every other outbound fetch in this server — the first hop is address-checked
// too, because what is being fetched is code this server will execute. See
// plugins.Manager.checkSourceURL.
func handleInstallPluginFromURL(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req installFromURLRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		installed, err := deps.PluginManager.InstallFromURL(r.Context(), req.URL)
		if err != nil {
			writePluginError(w, err, "failed to install the plugin")
			return
		}
		writePluginInstalled(w, r, deps, installed)
	}
}

// --- Lifecycle --------------------------------------------------------------

func handleSetPluginEnabled(deps Deps, id string, enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := deps.PluginManager.SetEnabled(r.Context(), id, enabled); err != nil {
			writePluginError(w, err, "failed to apply the plugin setting")
			return
		}
		writePluginList(w, r, deps)
	}
}

func handleReenablePlugin(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := deps.PluginManager.Reenable(r.Context(), id); err != nil {
			writePluginError(w, err, "failed to re-enable the plugin")
			return
		}
		writePluginList(w, r, deps)
	}
}

func handleUninstallPlugin(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.PluginManager.Uninstall(r.Context(), id); err != nil {
			writePluginError(w, err, "failed to uninstall the plugin")
			return
		}
		writePluginList(w, r, deps)
	}
}

// --- helpers ----------------------------------------------------------------

// readUploadPart reads one named file part, refusing a missing one by name so an
// Admin whose form sent only half of a Plugin is told which half.
func readUploadPart(w http.ResponseWriter, r *http.Request, name string, limit int64) ([]byte, bool) {
	file, _, err := r.FormFile(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"the upload has no `"+name+"` part; a plugin is a manifest and a module", nil)
		return nil, false
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the `"+name+"` part could not be read", nil)
		return nil, false
	}
	if int64(len(body)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, codeBadRequest,
			"the `"+name+"` part is larger than this server will accept", nil)
		return nil, false
	}
	return body, true
}

// writePluginInstalled answers a successful install with 201 and the whole list,
// so the screen re-renders from one response.
func writePluginInstalled(w http.ResponseWriter, r *http.Request, deps Deps, installed plugins.Installed) {
	list, err := deps.PluginManager.List(r.Context())
	if err != nil {
		// The Plugin IS installed at this point, so the honest answer is the one
		// thing that is certainly true rather than an error that would read as
		// "nothing happened".
		writeJSON(w, http.StatusCreated, pluginsResponse{Plugins: []plugins.Installed{installed}})
		return
	}
	writeJSON(w, http.StatusCreated, pluginsResponse{Plugins: list})
}

func writePluginList(w http.ResponseWriter, r *http.Request, deps Deps) {
	list, err := deps.PluginManager.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read the installed plugins", nil)
		return
	}
	if list == nil {
		list = []plugins.Installed{}
	}
	writeJSON(w, http.StatusOK, pluginsResponse{Plugins: list})
}

// writePluginError turns a loader refusal into the code and the sentence that go
// with it, and anything else into a 500.
//
// The MESSAGE IS THE LOADER'S, verbatim. It is the sentence that names which side
// to upgrade, or which id is taken, or what the module did when this server tried
// to instantiate it — and re-wording it here would leave the useful half in a log
// nobody reads.
func writePluginError(w http.ResponseWriter, err error, fallback string) {
	var refusal *plugins.Refusal
	if !errors.As(err, &refusal) {
		writeError(w, http.StatusInternalServerError, codeInternal, fallback, nil)
		return
	}
	status, code := http.StatusUnprocessableEntity, codePluginInvalidManifest
	switch refusal.Reason {
	case plugins.ReasonAPIVersion:
		code = codePluginAPIVersion
	case plugins.ReasonDuplicate:
		// 409, not 422: the request is well-formed and the server's state is what
		// refuses it, which is exactly what a conflict is.
		status, code = http.StatusConflict, codePluginDuplicate
	case plugins.ReasonModule:
		code = codePluginInvalidModule
	case plugins.ReasonSource:
		code = codePluginSourceRefused
	case plugins.ReasonUnknown:
		status, code = http.StatusNotFound, codePluginUnknown
	}
	writeError(w, status, code, refusal.Message, nil)
}
