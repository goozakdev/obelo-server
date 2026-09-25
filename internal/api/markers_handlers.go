package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/markerfetch"
	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/playback"
)

// Markers (ADR-0065). One GET leaf on the session subtree, owner-only like
// progress, so a player can offer Skip for the File it is actually playing:
//
//	GET /sessions/{id}/markers → { "markers": [ { "kind", "source", "startMs", "endMs", "autoSkip", "watchedPoint" } ] }
//
// Times are on the session File's own timeline. autoSkip is the viewer's own
// auto-skip setting for the Marker's kind (/me/marker-auto-skip): a player skips
// such a Marker by itself instead of offering the Skip button. watchedPoint marks
// the Credits Marker whose crossing marks the Title watched (the server's
// CreditsFloor rule); only that one may advance to a next episode. A File with no
// Markers answers an empty list; a reaped, ended or foreign session is 404.
//
// This GET is what a player does when playback starts, so it is also where a
// File's Marker providers are first asked (internal/markerfetch). The asking is
// not the viewer's: it runs apart from the request, so a viewer giving up on the
// read ends nothing but the read. The read waits for it only markersReadWait and
// then serves what is already known — the File's own and Detected Markers never
// wait on a provider — and what a slow provider finds is served from the next
// read on.

// markersReadWait is how long a read waits for a File's Marker providers before
// it answers without them.
const markersReadWait = 3 * time.Second

// markersFetchTimeout bounds one asking of every Marker provider, the wait for
// each Plugin's call slot included. Each call has its own budget inside it; this
// is the ceiling on the whole asking.
const markersFetchTimeout = 30 * time.Second

type markerJSON struct {
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	StartMs      int64  `json:"startMs"`
	EndMs        int64  `json:"endMs"`
	AutoSkip     bool   `json:"autoSkip"`
	WatchedPoint bool   `json:"watchedPoint"`
}

type sessionMarkersResponse struct {
	Markers []markerJSON `json:"markers"`
}

func handleSessionMarkers(svc *playback.Service, fetch *markerfetch.Service, autoSkip MarkerAutoSkipStore, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if sess, ok := svc.Sessions().Get(sessionID); ok && fetch != nil && sess.UserID == id.User.ID && sess.FileID != "" {
			fetched := make(chan struct{})
			go func() {
				defer close(fetched)
				ctx, cancel := context.WithTimeout(context.Background(), markersFetchTimeout)
				defer cancel()
				if err := fetch.FetchFile(ctx, sess.TitleID, sess.FileID); err != nil {
					log.Printf("obelo: fetching markers of session %s: %v", sessionID, err)
				}
			}()
			wait := time.NewTimer(markersReadWait)
			select {
			case <-fetched:
			case <-wait.C:
			case <-r.Context().Done():
			}
			wait.Stop()
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
		// Best-effort: a failure to read the setting offers every Marker's Skip
		// button rather than failing the read over a preference.
		skip := map[string]bool{}
		if autoSkip != nil {
			if kinds, err := autoSkip.MarkerAutoSkipKinds(id.User.ID); err == nil {
				for _, kind := range kinds {
					skip[kind] = true
				}
			}
		}
		watchedPoint := svc.SessionWatchedPointMs(sessionID)
		out := sessionMarkersResponse{Markers: make([]markerJSON, 0, len(ms))}
		for _, m := range ms {
			out.Markers = append(out.Markers, markerJSON{
				Kind: m.Kind, Source: m.Source, StartMs: m.StartMs, EndMs: m.EndMs,
				AutoSkip:     skip[m.Kind],
				WatchedPoint: m.Kind == markers.KindCredits && m.StartMs == watchedPoint,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}
