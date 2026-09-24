package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/webref"
)

// Web references (the Web reference provider Extension point). One GET leaf on
// the title subtree, open to ANY authenticated User — Members included, because a
// link to where a person can read about a film is part of browsing it:
//
//	GET /titles/{id}/webReferences → { "references": [ { "label", "url" } ] }
//
// The Plugins answer; internal/webref decides what survives. A Title with no ids,
// no provider, or only providers that failed answers an empty list, never an error.

// webReferencesTimeout bounds the whole request. Every provider's call is a pure
// computation over what the request carries, so this is a ceiling for a guest
// that spins rather than a budget anything honest needs.
const webReferencesTimeout = 5 * time.Second

type webReferencesResponse struct {
	References []webref.Reference `json:"references"`
}

func handleTitleWebReferences(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		d, err := deps.Catalog.GetTitle(scope, titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load title", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), webReferencesTimeout)
		defer cancel()
		refs := webref.Collect(ctx, deps.Plugins, d.Kind, heldTitleIDs(d.Title))
		if refs == nil {
			refs = []webref.Reference{}
		}
		writeJSON(w, http.StatusOK, webReferencesResponse{References: refs})
	}
}

// heldTitleIDs is every external id this server holds for the Title, keyed by
// namespace: the ids its folder asserts, with a record row outranking the folder's
// id in the same namespace — the precedence every other read uses (ADR-0045,
// ADR-0060).
func heldTitleIDs(t store.Title) map[string]string {
	held := map[string]string{}
	for ns, id := range t.IdentityIDs {
		if id != "" {
			held[ns] = id
		}
	}
	for ns, id := range t.RecordIDs {
		if id != "" {
			held[ns] = id
		}
	}
	return held
}
