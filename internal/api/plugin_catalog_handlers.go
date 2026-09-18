package api

import (
	"net/http"

	"github.com/goozakdev/obelo-server/internal/plugins"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The optional catalog and the pinned publisher keys (.scratch/plugin-system
// issue 15), Admin-scope, on the /settings/plugins subtree beside the install
// routes.
//
//	GET    /settings/plugins/catalog     → { url, entries, error }
//	PUT    /settings/plugins/catalog     → { url } — "" clears it
//	GET    /settings/plugins/publishers  → { publishers }
//	PUT    /settings/plugins/publishers  → { publisher, publicKey }
//	DELETE /settings/plugins/publishers/{publisher}
//
// # Two features, one posture: both OFF, and both the operator's own decision
//
// This project runs no catalog and vouches for no publisher (ADR-0001), so there
// is no default index address, no bundled key and no fallback. A server that has
// never used these routes browses nothing and verifies nothing, and behaves
// exactly as it did before they existed. Everything they turn on, an Admin typed.
//
// # The catalog GET answers 200 when the catalog is broken, and that is the point
//
// The Plugins screen's other two install paths — upload a file, paste a URL —
// have nothing to do with the catalog. An index that is down, moved, slow or not
// an index at all must not take them away, so a failed fetch comes back as
// `entries: []` with a sentence in `error`, under a 200. The screen shows the
// note and the rest of it keeps working. Answering 5xx would make somebody else's
// outage look like this server failing, and would cost an operator the two paths
// that were fine.
//
// A non-empty `error` beside a 200 is therefore NORMAL and a client must render
// it as a note rather than as a request that failed. It is the same shape the
// Tailnet status uses for a node that will not come up (ADR-0043): the world
// being unavailable is not a request that failed.
//
// # Installing from an entry adds nothing
//
// There is no "install this entry" route, deliberately. A catalog entry is a
// manifest URL, so installing one is POST /settings/plugins/from-url with that
// URL — the same endpoint, the same safe-fetch policy, the same first-hop address
// check, the same refusals. An entry pointing into this server's own network is
// refused with the sentence a pasted address gets, because it IS a pasted address
// as far as the install path is concerned. A second route would have been a second
// place for that policy to be got wrong.

// --- Wire shapes ------------------------------------------------------------

// pluginCatalogResponse is the GET/PUT body.
//
// Error is not omitempty and neither is Entries: a client has to be able to tell
// "there was no problem" from "the server did not send the field", and a screen
// that renders an absent array as a spinner forever is the failure mode this
// prevents.
type pluginCatalogResponse struct {
	// URL is the configured index, "" when none is. THE BROWSE TAB KEYS OFF THIS,
	// not off Entries: a catalog that is configured and unreachable still has a
	// tab, and the tab is where the note belongs.
	URL string `json:"url"`
	// Entries is what the index offered, in its own order, [] when it could not be
	// read or genuinely holds nothing.
	Entries []pluginapi.CatalogEntry `json:"entries"`
	// Error is the quiet note for the operator, "" when the catalog was read.
	Error string `json:"error"`
}

// setPluginCatalogRequest is the PUT body. An empty URL CLEARS the catalog, which
// is why the field is a plain string and not a pointer: there is exactly one
// setting here and no partial update to express.
type setPluginCatalogRequest struct {
	URL string `json:"url"`
}

// pluginPublishersResponse is the pinned keys.
//
// The list being EMPTY is the default policy — nothing is verified — and not an
// absence of data. A client says so in words, because a bare empty table invites
// an operator to conclude the opposite.
type pluginPublishersResponse struct {
	Publishers []plugins.Publisher `json:"publishers"`
}

// pinPluginPublisherRequest is the PUT body: a name and a base64 ed25519 public
// key. The name is the LOOKUP — a signature naming this publisher is checked
// under this key and no other — so it is not a label and a typo in it is a
// publisher nobody pinned.
type pinPluginPublisherRequest struct {
	Publisher string `json:"publisher"`
	PublicKey string `json:"publicKey"`
}

// --- Catalog ----------------------------------------------------------------

func handleGetPluginCatalog(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writePluginCatalog(w, r, deps)
	}
}

func handleSetPluginCatalog(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setPluginCatalogRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := deps.PluginManager.SetCatalogURL(req.URL); err != nil {
			writePluginError(w, err, "failed to save the catalog address")
			return
		}
		// Answered with the freshly fetched index rather than with an echo of the
		// URL, so a screen that has just been pointed somewhere new renders from one
		// response — and learns at once if the address does not answer.
		writePluginCatalog(w, r, deps)
	}
}

func writePluginCatalog(w http.ResponseWriter, r *http.Request, deps Deps) {
	result, err := deps.PluginManager.Catalog(r.Context())
	if err != nil {
		// Only this server's own database can get here; everything the catalog did
		// is a note inside a 200.
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read the catalog settings", nil)
		return
	}
	entries := result.Entries
	if entries == nil {
		entries = []pluginapi.CatalogEntry{}
	}
	writeJSON(w, http.StatusOK, pluginCatalogResponse{
		URL:     result.URL,
		Entries: entries,
		Error:   result.Note,
	})
}

// --- Pinned publishers -------------------------------------------------------

func handleGetPluginPublishers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writePluginPublishers(w, deps)
	}
}

func handlePinPluginPublisher(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req pinPluginPublisherRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := deps.PluginManager.PinPublisher(req.Publisher, req.PublicKey); err != nil {
			writePluginError(w, err, "failed to pin the publisher key")
			return
		}
		writePluginPublishers(w, deps)
	}
}

func handleUnpinPluginPublisher(deps Deps, publisher string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.PluginManager.UnpinPublisher(publisher); err != nil {
			writePluginError(w, err, "failed to unpin the publisher key")
			return
		}
		writePluginPublishers(w, deps)
	}
}

func writePluginPublishers(w http.ResponseWriter, deps Deps) {
	list, err := deps.PluginManager.Publishers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read the pinned publishers", nil)
		return
	}
	if list == nil {
		list = []plugins.Publisher{}
	}
	writeJSON(w, http.StatusOK, pluginPublishersResponse{Publishers: list})
}
