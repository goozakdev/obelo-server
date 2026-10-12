package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/seekpreview"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Seek preview administration (ADR-0070): the per-Library toggle, coverage and
// failed list, retry, delete, and the per-Title / per-Show "generate now". All
// Admin-only, and mirroring Marker detection's routes.
//
//	GET    /libraries/{id}/seek-previews        → status
//	PUT    /libraries/{id}/seek-previews          { "enabled": bool } → status
//	DELETE /libraries/{id}/seek-previews        → status (toggle off, every set and record gone)
//	POST   /libraries/{id}/seek-previews/retry    { "fileId"?: string } → 202 {}
//	POST   /titles/{id}/generate-seek-previews  → 202 {}
//	POST   /shows/{id}/generate-seek-previews   → 202 {}
//
// A Seek preview failure is not an attention kind: it shows only here.

// SeekPreviewGenerator queues and deletes Seek preview generation.
// *seekpreview.Generator satisfies it.
type SeekPreviewGenerator interface {
	AfterScan(libraryID string)
	Retry(fileID string) error
	GenerateTitle(titleID string) error
	GenerateShow(showID string) error
	DeleteLibrary(libraryID string) error
}

// SeekPreviewAdminStore is what the admin routes read and write. *store.DB
// satisfies it.
type SeekPreviewAdminStore interface {
	SeekPreviewsEnabled(libraryID string) (bool, error)
	SetSeekPreviewsEnabled(libraryID string, on bool) error
	SeekPreviewCoverage(libraryID string) (store.SeekPreviewCoverage, error)
	SeekPreviewFile(fileID string) (store.SeekPreviewFile, error)
	SeekPreviewFilesOfTitle(titleID string) ([]store.SeekPreviewFile, error)
	SeekPreviewFilesOfShow(showID string) ([]store.SeekPreviewFile, error)
	SeekPreviewFileIDsOfLibrary(libraryID string) ([]string, error)
	DeleteSeekPreviewRecord(fileID string) error
}

type seekPreviewFailureJSON struct {
	FileID  string `json:"fileId"`
	TitleID string `json:"titleId"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Reason  string `json:"reason"`
}

type seekPreviewStatusJSON struct {
	Enabled   bool                     `json:"enabled"`
	Available bool                     `json:"available"`
	Eligible  int                      `json:"eligible"`
	Ready     int                      `json:"ready"`
	Failed    []seekPreviewFailureJSON `json:"failed"`
}

type seekPreviewToggleRequest struct {
	Enabled *bool `json:"enabled"`
}

type seekPreviewRetryRequest struct {
	FileID string `json:"fileId"`
}

func writeSeekPreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNoSeekPreviews):
		writeError(w, http.StatusNotFound, codeNotFound, "this library has no seek previews", nil)
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read seek previews", nil)
	}
}

func seekPreviewStatus(deps Deps, libraryID string, enabled bool) (seekPreviewStatusJSON, error) {
	cov, err := deps.SeekPreviewAdmin.SeekPreviewCoverage(libraryID)
	if err != nil {
		return seekPreviewStatusJSON{}, err
	}
	out := seekPreviewStatusJSON{
		Enabled:   enabled,
		Available: deps.SeekPreviewGenerator != nil,
		Eligible:  cov.Eligible,
		Ready:     cov.Ready,
		Failed:    make([]seekPreviewFailureJSON, 0, len(cov.Failed)),
	}
	for _, f := range cov.Failed {
		out.Failed = append(out.Failed, seekPreviewFailureJSON(f))
	}
	return out, nil
}

// handleLibrarySeekPreviews serves GET, PUT and DELETE on a Library's Seek
// previews. A linked mirror is refused by the caller.
func handleLibrarySeekPreviews(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SeekPreviewAdmin == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "seek previews are not available", nil)
			return
		}
		// Only a local Movie or TV Library has the toggle; this is the 404 for the rest.
		enabled, err := deps.SeekPreviewAdmin.SeekPreviewsEnabled(id)
		if err != nil {
			writeSeekPreviewError(w, err)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var req seekPreviewToggleRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			if req.Enabled == nil {
				writeError(w, http.StatusBadRequest, codeBadRequest, "enabled is required", nil)
				return
			}
			if err := deps.SeekPreviewAdmin.SetSeekPreviewsEnabled(id, *req.Enabled); err != nil {
				writeSeekPreviewError(w, err)
				return
			}
			enabled = *req.Enabled
			if enabled && deps.SeekPreviewGenerator != nil {
				deps.SeekPreviewGenerator.AfterScan(id)
			}
		case http.MethodDelete:
			// The toggle goes off first, so a failed delete still leaves it off and a
			// repeat finishes the job. With no generator (no usable ffmpeg) there is
			// nothing to stop, and the sets are removed directly.
			if err := deps.SeekPreviewAdmin.SetSeekPreviewsEnabled(id, false); err != nil {
				writeSeekPreviewError(w, err)
				return
			}
			var derr error
			if deps.SeekPreviewGenerator != nil {
				derr = deps.SeekPreviewGenerator.DeleteLibrary(id)
			} else {
				derr = seekpreview.RemoveLibrary(deps.SeekPreviewAdmin, deps.SeekPreviewsDir, id)
			}
			if derr != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to delete seek previews", nil)
				return
			}
			enabled = false
		}
		st, err := seekPreviewStatus(deps, id, enabled)
		if err != nil {
			writeSeekPreviewError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// handleRetrySeekPreviews queues one failed File, or every failed File of the
// Library, ahead of the queue.
func handleRetrySeekPreviews(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SeekPreviewGenerator == nil || deps.SeekPreviewAdmin == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "seek previews are not available", nil)
			return
		}
		var req seekPreviewRetryRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) { // no body at all is {}
			writeError(w, http.StatusBadRequest, codeBadRequest, "invalid JSON body", nil)
			return
		}
		if _, err := deps.SeekPreviewAdmin.SeekPreviewsEnabled(id); err != nil {
			writeSeekPreviewError(w, err)
			return
		}
		if req.FileID != "" {
			f, err := deps.SeekPreviewAdmin.SeekPreviewFile(req.FileID)
			if errors.Is(err, store.ErrNotFound) || err == nil && f.LibraryID != id {
				writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
				return
			}
			if err == nil {
				err = deps.SeekPreviewGenerator.Retry(req.FileID)
			}
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to retry seek previews", nil)
				return
			}
			writeJSON(w, http.StatusAccepted, struct{}{})
			return
		}
		cov, err := deps.SeekPreviewAdmin.SeekPreviewCoverage(id)
		if err != nil {
			writeSeekPreviewError(w, err)
			return
		}
		for _, f := range cov.Failed {
			if err := deps.SeekPreviewGenerator.Retry(f.FileID); err != nil && !errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to retry seek previews", nil)
				return
			}
		}
		writeJSON(w, http.StatusAccepted, struct{}{})
	}
}

// handleGenerateSeekPreviews is "generate now" for one Movie / Episode (show
// false) or one Show: forced, ahead of the queue, whatever the toggle says. An
// unknown entity, or one with no eligible File, is 404.
func handleGenerateSeekPreviews(deps Deps, id string, show bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SeekPreviewGenerator == nil || deps.SeekPreviewAdmin == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable, "seek previews are not available", nil)
			return
		}
		list := deps.SeekPreviewAdmin.SeekPreviewFilesOfTitle
		generate := deps.SeekPreviewGenerator.GenerateTitle
		what := "title"
		if show {
			list, generate, what = deps.SeekPreviewAdmin.SeekPreviewFilesOfShow, deps.SeekPreviewGenerator.GenerateShow, "show"
		}
		files, err := list(id)
		if err == nil && len(files) == 0 {
			err = store.ErrNotFound
		}
		if err == nil {
			err = generate(id)
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, what+" not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to start seek previews", nil)
		default:
			writeJSON(w, http.StatusAccepted, struct{}{})
		}
	}
}
