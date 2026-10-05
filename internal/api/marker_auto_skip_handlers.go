package api

import (
	"net/http"

	"github.com/goozakdev/obelo-server/internal/markers"
)

// Auto-skip (ADR-0065 §6): which Marker kinds the caller's player skips by
// itself instead of offering a Skip button. It is the caller's own setting —
// there is no route naming another User — and it is kept on the Server, so it
// follows the User from client to client.
//
//	GET /me/marker-auto-skip → { "intro", "recap", "credits", "preview": bool }
//	PUT /me/marker-auto-skip   the same object → 200 the saved object
//
// A PUT is the whole choice, like the Playback ceiling's: an omitted kind is off.

// MarkerAutoSkipStore persists each User's auto-skip kinds. *store.DB satisfies it.
type MarkerAutoSkipStore interface {
	MarkerAutoSkipKinds(userID string) ([]string, error)
	SetMarkerAutoSkipKinds(userID string, kinds []string) error
}

type markerAutoSkipJSON struct {
	Intro   bool `json:"intro"`
	Recap   bool `json:"recap"`
	Credits bool `json:"credits"`
	Preview bool `json:"preview"`
}

func (a markerAutoSkipJSON) kinds() []string {
	// A fixed order, so the same toggles are persisted identically on every save.
	var out []string
	for _, k := range []struct {
		kind string
		on   bool
	}{
		{markers.KindIntro, a.Intro},
		{markers.KindRecap, a.Recap},
		{markers.KindCredits, a.Credits},
		{markers.KindPreview, a.Preview},
	} {
		if k.on {
			out = append(out, k.kind)
		}
	}
	return out
}

func toMarkerAutoSkipJSON(kinds []string) markerAutoSkipJSON {
	var out markerAutoSkipJSON
	for _, kind := range kinds {
		switch kind {
		case markers.KindIntro:
			out.Intro = true
		case markers.KindRecap:
			out.Recap = true
		case markers.KindCredits:
			out.Credits = true
		case markers.KindPreview:
			out.Preview = true
		}
	}
	return out
}

func handleMarkerAutoSkip(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			return
		}
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if deps.MarkerAutoSkip == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "auto-skip is not available", nil)
			return
		}
		if r.Method == http.MethodPut {
			var req markerAutoSkipJSON
			if !decodeJSON(w, r, &req) {
				return
			}
			if err := deps.MarkerAutoSkip.SetMarkerAutoSkipKinds(id.User.ID, req.kinds()); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save auto-skip", nil)
				return
			}
		}
		kinds, err := deps.MarkerAutoSkip.MarkerAutoSkipKinds(id.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load auto-skip", nil)
			return
		}
		writeJSON(w, http.StatusOK, toMarkerAutoSkipJSON(kinds))
	}
}
