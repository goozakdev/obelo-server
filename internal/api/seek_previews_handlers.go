package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Seek preview delivery (ADR-0070 §5–§7, docs/api-contract.md "Seek previews"). The
// sets are generated ahead of time (internal/seekpreview); this file only serves
// them, three ways:
//
//	decision          seekPreviews in POST /titles/{id}/playback, sheets under the
//	                  session's stream token
//	session sheets    GET /stream/{streamToken}/seekPreviews/{n}.jpg (and the
//	                  session-id twin the relay spends against a sharer)
//	bearer File route GET /files/{id}/seekPreviews[/{n}.jpg], for downloads
//
// The library toggle governs generation only, so nothing here consults it: a set
// that exists is served.

// SeekPreviewReader reads a File's Seek preview record. *store.DB satisfies it.
// Nil in a narrow unit test, where no File has previews.
type SeekPreviewReader interface {
	SeekPreviewFile(fileID string) (store.SeekPreviewFile, error)
}

const (
	// streamSeekPreviewsPrefix is the sheet sub-path under a stream token (and
	// under a session id), "seekPreviews/{n}.jpg".
	streamSeekPreviewsPrefix = "seekPreviews/"
	// fileSeekPreviews is the leaf under /files/{id}.
	fileSeekPreviews = "seekPreviews"
)

// readySeekPreviews is the File's record when it has a usable set: ready, with at
// least one sheet, recorded against the File's current mtime and size. Missing,
// failed, never tried, not-eligible and stale (the File changed since) are all false;
// the janitor or the generator deletes a stale set, never this read path.
func readySeekPreviews(deps Deps, fileID string) (store.SeekPreviewSet, bool) {
	if deps.SeekPreviews == nil || fileID == "" {
		return store.SeekPreviewSet{}, false
	}
	f, err := deps.SeekPreviews.SeekPreviewFile(fileID)
	if err != nil || f.Set == nil || f.Set.Status != store.SeekPreviewReady || f.Set.SheetCount <= 0 ||
		f.Set.Mtime != f.Mtime || f.Set.SizeBytes != f.SizeBytes {
		return store.SeekPreviewSet{}, false
	}
	return *f.Set, true
}

// seekPreviewManifest renders a set with its sheets named under prefix.
func seekPreviewManifest(set store.SeekPreviewSet, prefix string) seekPreviewsJSON {
	return seekPreviewsJSON{
		IntervalMs:  set.IntervalMs,
		FrameWidth:  set.FrameWidth,
		FrameHeight: set.FrameHeight,
		Columns:     set.Columns,
		Rows:        set.Rows,
		FrameCount:  set.FrameCount,
		Sheets:      seekPreviewSheetURLs(prefix, set.SheetCount),
	}
}

// sessionSeekPreviews is the manifest a LOCAL session's decision carries, or nil
// when it carries none: no stream token to build the URLs from, a multi-part
// Edition (a File's frames would not line up with the Edition's timeline), or a
// File without a ready set.
func sessionSeekPreviews(deps Deps, sess playback.Session, streamToken string) *seekPreviewsJSON {
	if streamToken == "" || len(sess.Parts) > 1 {
		return nil
	}
	set, ok := readySeekPreviews(deps, sess.FileID)
	if !ok {
		return nil
	}
	m := seekPreviewManifest(set, streamSeekPreviewsURLPrefix(streamToken))
	return &m
}

func streamSeekPreviewsURLPrefix(streamToken string) string {
	return APIPrefix + streamRoutePrefix + streamToken + "/" + fileSeekPreviews
}

// relaySeekPreviews is the manifest of a RELAYED decision: the sharer's own, read
// into the contract's shape (so it leaves with the fixture's key order and none of
// the keys the contract does not name), with its sheet URLs (which carry the
// sharer's stream token, useless here) replaced by this Server's stream-token route.
// nil when the sharer sent none, or sent something that is not a manifest.
func relaySeekPreviews(sharers map[string]any, streamToken string) *seekPreviewsJSON {
	src, ok := sharers["seekPreviews"].(map[string]any)
	if !ok || streamToken == "" {
		return nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil
	}
	var m seekPreviewsJSON
	if err := json.Unmarshal(raw, &m); err != nil || !plausibleSeekPreviews(m) {
		return nil
	}
	m.Sheets = seekPreviewSheetURLs(streamSeekPreviewsURLPrefix(streamToken), len(m.Sheets))
	return &m
}

// maxSeekPreviewGrid bounds a sheet's columns and rows, so their product cannot
// overflow and no client is asked to lay out an absurd grid.
const maxSeekPreviewGrid = 1000

// plausibleSeekPreviews is whether a sharer's manifest can be laid out: every
// number positive, and exactly as many sheets as the frames fill (DECISIONS 23).
func plausibleSeekPreviews(m seekPreviewsJSON) bool {
	if m.IntervalMs <= 0 || m.FrameWidth <= 0 || m.FrameHeight <= 0 || m.FrameCount <= 0 ||
		m.Columns <= 0 || m.Columns > maxSeekPreviewGrid || m.Rows <= 0 || m.Rows > maxSeekPreviewGrid {
		return false
	}
	perSheet := int64(m.Columns) * int64(m.Rows)
	return int64(len(m.Sheets)) == (int64(m.FrameCount)+perSheet-1)/perSheet
}

// parseSeekPreviewSheet reads "{n}.jpg" into n: canonical decimal digits only, so
// "-1", "+1", "01", "-0" and "1.jpg.jpg" are all refused.
func parseSeekPreviewSheet(name string) (int, bool) {
	digits, ok := strings.CutSuffix(name, ".jpg")
	if !ok || digits == "" || len(digits) > 6 || digits[0] < '0' || digits[0] > '9' {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || strconv.Itoa(n) != digits {
		return 0, false
	}
	return n, true
}

// openSeekPreviewSheet opens sheet n of a File's set, whose index must be within the
// record's sheet count.
func openSeekPreviewSheet(deps Deps, fileID string, set store.SeekPreviewSet, name string) (*os.File, bool) {
	n, ok := parseSeekPreviewSheet(name)
	if !ok || n >= set.SheetCount {
		return nil, false
	}
	f, err := os.Open(filepath.Join(deps.SeekPreviewsDir, fileID, strconv.Itoa(n)+".jpg"))
	if err != nil {
		return nil, false
	}
	return f, true
}

func serveSeekPreviewSheet(w http.ResponseWriter, r *http.Request, f *os.File) {
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "file unavailable", nil)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// serveSessionSeekPreview answers GET/HEAD for sheet `name` of the session's
// played File. Every refusal is the stream-token subtree's one envelope — bad
// name, index out of range, no ready set, sheet gone from disk, a session that is
// not the caller's, a relay session — so the route says nothing a dead token
// would not.
func serveSessionSeekPreview(deps Deps, w http.ResponseWriter, r *http.Request, userID, sessionID, name string) {
	sess, ok := deps.Playback.Sessions().Get(sessionID)
	if !ok || sess.UserID != userID || sess.IsRelay() || len(sess.Parts) > 1 {
		refuseStreamToken(w)
		return
	}
	set, ok := readySeekPreviews(deps, sess.FileID)
	if !ok {
		refuseStreamToken(w)
		return
	}
	f, ok := openSeekPreviewSheet(deps, sess.FileID, set, name)
	if !ok {
		refuseStreamToken(w)
		return
	}
	serveSeekPreviewSheet(w, r, f)
}

// handleSessionSeekPreview is GET /sessions/{id}/seekPreviews/{n}.jpg: the
// session-id twin of the stream-token route, which is what a Linked Library's
// relay spends against the sharer (it holds a bearer credential, not the sharer's
// stream token). Bearer or media cookie, like /sessions/{id}/stream.
func handleSessionSeekPreview(deps Deps, sessionID, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		serveSessionSeekPreview(deps, w, r, id.User.ID, sessionID, name)
	}
}

// handleFileSeekPreviews serves GET /files/{id}/seekPreviews (sheet == "") and
// GET /files/{id}/seekPreviews/{n}.jpg. Scoped and collapsed exactly like
// handleFileDownload: unknown, out-of-scope, orphaned and Missing Files, and a File
// with no ready set, all get the one `404 "file not found"`; only a sheet that is
// recorded but unreadable on disk is `404 "file unavailable"`.
func handleFileSeekPreviews(deps Deps, cat *catalog.Service, fileID, sheet string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identityFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		f, err := cat.FileByID(scope, fileID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load file", nil)
			return
		}
		set, ok := readySeekPreviews(deps, fileID)
		if !f.Present || !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		}
		if sheet == "" {
			writeJSON(w, http.StatusOK, seekPreviewManifest(set, APIPrefix+"/files/"+fileID+"/"+fileSeekPreviews))
			return
		}
		n, ok := parseSeekPreviewSheet(sheet)
		if !ok || n >= set.SheetCount {
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		}
		sf, ok := openSeekPreviewSheet(deps, fileID, set, sheet)
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "file unavailable", nil)
			return
		}
		serveSeekPreviewSheet(w, r, sf)
	}
}
