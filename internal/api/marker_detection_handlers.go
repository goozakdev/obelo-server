package api

import (
	"errors"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Marker detection (ADR-0065 §4): the per-Library toggle and the per-Show
// "detect markers now". Both Admin-only.
//
//	GET  /libraries/{id}/marker-detection → { "enabled": bool, "available": bool }
//	PUT  /libraries/{id}/marker-detection   { "enabled": bool } → { "enabled": bool, "available": bool }
//	POST /shows/{id}/detect-markers        → 202 {}
//
// Only a TV Library has the toggle — on by default. Any other Library has none,
// and its toggle route is 404 exactly as an unknown Library's is: absent, not off.
// On a host with no usable ffmpeg detection does not run at all; the toggle
// keeps its setting but says it is unavailable, so it is never shown as on.

// MarkerDetector queues detection work. *markerdetect.Detector satisfies it.
type MarkerDetector interface {
	AfterScan(libraryID string)
	DetectShow(showID string) error
}

// MarkerDetectionToggleStore persists the toggle. *store.DB satisfies it.
type MarkerDetectionToggleStore interface {
	MarkerDetectionEnabled(libraryID string) (bool, error)
	SetMarkerDetectionEnabled(libraryID string, on bool) error
}

type markerDetectionJSON struct {
	Enabled   bool `json:"enabled"`
	Available bool `json:"available"`
}

type markerDetectionRequest struct {
	Enabled *bool `json:"enabled"`
}

// markersAfterScan queues detection for a Library whose scan just completed.
func markersAfterScan(deps Deps) func(string) {
	return func(libraryID string) {
		if deps.MarkerDetection != nil {
			deps.MarkerDetection.AfterScan(libraryID)
		}
	}
}

func handleMarkerDetectionToggle(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/marker-detection")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if deps.MarkerDetectionToggle == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "marker detection is not available", nil)
			return
		}
		if r.Method == http.MethodPut {
			var req markerDetectionRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			if req.Enabled == nil {
				writeError(w, http.StatusBadRequest, codeBadRequest, "enabled is required", nil)
				return
			}
			if err := deps.MarkerDetectionToggle.SetMarkerDetectionEnabled(id, *req.Enabled); err != nil {
				writeMarkerDetectionError(w, err)
				return
			}
		}
		on, err := deps.MarkerDetectionToggle.MarkerDetectionEnabled(id)
		if err != nil {
			writeMarkerDetectionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, markerDetectionJSON{Enabled: on, Available: deps.MarkerDetection != nil})
	}
}

func writeMarkerDetectionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
	case errors.Is(err, store.ErrNoMarkerDetection):
		writeError(w, http.StatusNotFound, codeNotFound, "this library has no marker detection", nil)
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read marker detection", nil)
	}
}

// handleDetectShowMarkers queues one Show for detection now, ahead of anything
// waiting from a scan. It still never starts while a Transcode is running.
func handleDetectShowMarkers(deps Deps, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MarkerDetection == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "marker detection is not available", nil)
			return
		}
		switch err := deps.MarkerDetection.DetectShow(showID); {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "show not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to start marker detection", nil)
			return
		}
		writeJSON(w, http.StatusAccepted, struct{}{})
	}
}
