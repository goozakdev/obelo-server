package api

import (
	"errors"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/playback"
)

// Markers (ADR-0065). One GET leaf on the session subtree, owner-only like
// progress, so a player can offer Skip for the File it is actually playing:
//
//	GET /sessions/{id}/markers → { "markers": [ { "kind", "source", "startMs", "endMs" } ] }
//
// Times are on the session File's own timeline. A File with no Markers answers an
// empty list; a reaped, ended or foreign session is 404.

type markerJSON struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
}

type sessionMarkersResponse struct {
	Markers []markerJSON `json:"markers"`
}

func handleSessionMarkers(svc *playback.Service, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		ms, err := svc.SessionMarkers(id.User.ID, sessionID)
		switch {
		case errors.Is(err, playback.ErrSessionNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "session not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load markers", nil)
			return
		}
		out := sessionMarkersResponse{Markers: make([]markerJSON, 0, len(ms))}
		for _, m := range ms {
			out.Markers = append(out.Markers, markerJSON{Kind: m.Kind, Source: m.Source, StartMs: m.StartMs, EndMs: m.EndMs})
		}
		writeJSON(w, http.StatusOK, out)
	}
}
