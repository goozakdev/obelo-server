package api

import (
	"net/http"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Admin-scope Lyric provider settings: the order the providers are asked in.
// Whether a provider is asked at all is the Plugins screen's switch — a switched
// off Plugin is not registered — so the one thing configured here is the order,
// and the first acceptable Synced answer in it wins (internal/lyricfetch).
//
//	GET /settings/lyric-providers → { "providers": [ { "slug", "name", "description", "docsURL" } ] }
//	PUT /settings/lyric-providers   { "order": ["slug", …] } → the GET shape
//
// The list is always in the order the providers will be asked. A provider the
// PUT leaves out is asked after every one it names, in registration order.

// LyricProviderOrderStore persists the Admin's Lyric provider order. *store.DB
// satisfies it.
type LyricProviderOrderStore interface {
	SetLyricProviderOrder(slugs []string) error
}

type lyricProviderJSON struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	DocsURL     string `json:"docsURL"`
}

type lyricProvidersResponse struct {
	Providers []lyricProviderJSON `json:"providers"`
}

type updateLyricProvidersRequest struct {
	Order []string `json:"order"`
}

func handleLyricProviderSettingsSubtree(deps Deps, rest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rest != "lyric-providers" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if deps.Lyrics == nil || deps.LyricProviderOrder == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"lyric provider settings are not available on this server", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req updateLyricProvidersRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			seen := map[string]bool{}
			for _, slug := range req.Order {
				if _, ok := deps.Plugins.LyricProvider(slug); !ok {
					writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown, "unknown lyric provider: "+slug, nil)
					return
				}
				if seen[slug] {
					writeError(w, http.StatusBadRequest, codeBadRequest, "lyric provider named twice: "+slug, nil)
					return
				}
				seen[slug] = true
			}
			if err := deps.LyricProviderOrder.SetLyricProviderOrder(req.Order); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save lyric provider order", nil)
				return
			}
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			return
		}
		regs, err := deps.Lyrics.Providers()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read lyric provider settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, lyricProvidersResponse{Providers: lyricProvidersJSON(regs)})
	}
}

func lyricProvidersJSON(regs []pluginapi.LyricProviderRegistration) []lyricProviderJSON {
	out := make([]lyricProviderJSON, 0, len(regs))
	for _, r := range regs {
		d := r.Descriptor
		out = append(out, lyricProviderJSON{Slug: d.Slug, Name: d.Name, Description: d.Description, DocsURL: d.DocsURL})
	}
	return out
}
