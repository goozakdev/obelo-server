package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/library"
	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/store"
)

// GET /libraries/{id}/export — the Library Export (ADR-0056 §4).
//
// One flat, cursor-paginated feed of every entity beneath a Library, for the
// mirror on a linked Server. It is deliberately NOT the browse API: the browse
// endpoints are shaped for screens (nested, capped per row, carrying the calling
// User's Watch state) and have no "since", and a full walk of a TV library
// through them is one request per Show and then one per Season.
//
// Who may call it: a `remote` User (this is the one route its token exists for)
// and an Admin, who can call anything. Every other role — a Member included —
// gets 404, the same answer an ungranted Library gives, because a Member has no
// business knowing this route is here (api-contract.md, 404-not-403).

const (
	// exportDefaultLimit / exportMaxLimit bound one page of the feed. The cap is
	// the issue's ≤500; the default is smaller because a page carries every
	// Stream of every File and a TV library's rows are small but numerous.
	exportDefaultLimit = 200
	exportMaxLimit     = 500

	// exportTombstoneRetention is how far back a `since` may reach and still be
	// answerable incrementally. Beyond it the sharer cannot promise the feed
	// still holds every tombstone the mirror missed, so it says RESYNC and the
	// home Server does a full pull (ADR-0056 §4).
	//
	// Today nothing purges a soft-deleted row (ADR-0008's "purge missing" is
	// still a future action), so the honest retention is unbounded and this
	// window is a deliberate ceiling on how stale a mirror may be before it is
	// made to start over. It has to exist now rather than later: a home Server
	// that never learns to handle 410 will hand a year-old cursor to a Server
	// that HAS started purging, and quietly keep films that no longer exist.
	exportTombstoneRetention = 30 * 24 * time.Hour
)

// exportEntityJSON is one row of the feed. `data` is the entity's public field
// set — see store/export.go, which is the single place deciding what a remote
// peer may learn about a Title.
type exportEntityJSON struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	ParentID  string         `json:"parentId,omitempty"`
	UpdatedAt string         `json:"updatedAt"`
	DeletedAt string         `json:"deletedAt,omitempty"`
	Data      map[string]any `json:"data"`
}

type exportLibraryJSON struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type exportResponse struct {
	// LinkProtocolVersion stamps the feed the way the invite, the redeem and the
	// relay do (ADR-0055 §3): the mirror branches on features, never on this, but
	// a version it does not know is a clear refusal rather than a mis-parse.
	LinkProtocolVersion int                `json:"linkProtocolVersion"`
	Library             exportLibraryJSON  `json:"library"`
	Entities            []exportEntityJSON `json:"entities"`
	NextCursor          string             `json:"nextCursor,omitempty"`
	// Checkpoint is the position after the last entity on this page — the value
	// to pass as `since` on the next pull. It is present on every page (unlike
	// nextCursor, which is absent on the last one) so a mirror that stops early
	// still resumes exactly where it stopped.
	Checkpoint string `json:"checkpoint"`
}

// exportCursorPayload is the wire form of an export position, base64url'd like
// the browse cursor (catalog/cursor.go). Opaque to the caller; the three fields
// are the keyset tuple.
type exportCursorPayload struct {
	U string `json:"u"` // updated_at
	T string `json:"t"` // entity type
	I string `json:"i"` // entity id
}

func encodeExportCursor(c store.ExportCursor) string {
	if c.IsZero() {
		return ""
	}
	b, _ := json.Marshal(exportCursorPayload{U: c.UpdatedAt, T: c.Type, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeExportCursor(s string) (store.ExportCursor, error) {
	if s == "" {
		return store.ExportCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.ExportCursor{}, err
	}
	var p exportCursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return store.ExportCursor{}, err
	}
	if p.U == "" {
		return store.ExportCursor{}, errors.New("api: export cursor names no instant")
	}
	return store.ExportCursor{UpdatedAt: p.U, Type: p.T, ID: p.I}, nil
}

// exportCursorAge reports how long ago a cursor's instant was, and whether it
// could be read at all. Stored stamps are RFC3339 with milliseconds (migration
// 0061); a value that predates it may still be in SQLite's datetime('now')
// shape, which formatTimestamp already knows how to read, so both are accepted
// — an unreadable one is treated as ancient rather than as "just now", which is
// the safe direction (a RESYNC costs a full pull; a wrong "fresh" loses rows).
func exportCursorAge(c store.ExportCursor, now time.Time) (time.Duration, bool) {
	if c.IsZero() {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339, c.UpdatedAt); err == nil {
		return now.Sub(t), true
	}
	if t, err := time.Parse(sqliteDateTime, c.UpdatedAt); err == nil {
		return now.Sub(t), true
	}
	return exportTombstoneRetention * 2, true
}

// handleLibraryExport serves one page of the feed. It runs behind requireScope
// (the Library grant is the `remote` User's only entitlement) and does its own
// role check, because this is the one read route a Member must not reach.
func handleLibraryExport(deps Deps, libraryID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if id.User.Role != "admin" && id.User.Role != auth.RoleRemote {
			notFound(w)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		if deps.Export == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"export unavailable", nil)
			return
		}

		lib, err := deps.Library.Get(scope, libraryID)
		if errors.Is(err, library.ErrNotFound) {
			notFound(w)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read library", nil)
			return
		}
		// A Library this Server itself mirrored from somebody else is never
		// re-shared: the owner of the files decided who sees them, and a hop later
		// that decision would be made by somebody they never met (ADR-0054 §4,
		// ADR-0056 §7). `libraries.source` does not exist yet — issue 07 adds it —
		// so this is the hook issue 07/11 fills in; nil means nothing is linked,
		// which is true of every Server until then.
		if deps.LinkedLibrary != nil && deps.LinkedLibrary(libraryID) {
			notFound(w)
			return
		}

		q := r.URL.Query()
		since, err := decodeExportCursor(q.Get("since"))
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, "invalid cursor", nil)
			return
		}
		cur, err := decodeExportCursor(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, "invalid cursor", nil)
			return
		}
		if age, ok := exportCursorAge(since, time.Now()); ok && age > exportTombstoneRetention {
			// 410, not 400: the request was well formed and the answer is "that
			// position is gone" — pull everything again (ADR-0056 §4).
			writeError(w, http.StatusGone, codeResync,
				"cursor is older than the tombstone retention; pull the library in full", nil)
			return
		}

		// `cursor` continues the current walk and `since` starts it, so the walk
		// position always wins when both are present — it is by construction at or
		// past the since.
		after := since
		if !cur.IsZero() {
			after = cur
		}

		page, err := deps.Export.ExportLibrary(libraryID, after, parseExportLimit(q.Get("limit")))
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to export library", nil)
			return
		}

		out := exportResponse{
			LinkProtocolVersion: server.LinkProtocolVersion,
			Library:             exportLibraryJSON{ID: lib.ID, Kind: lib.Kind, Name: lib.Name},
			Entities:            make([]exportEntityJSON, 0, len(page.Entities)),
		}
		last := after
		for _, e := range page.Entities {
			out.Entities = append(out.Entities, exportEntityJSON{
				Type:      e.Type,
				ID:        e.ID,
				ParentID:  e.ParentID,
				UpdatedAt: exportTimestamp(e.UpdatedAt),
				DeletedAt: exportTimestamp(e.DeletedAt),
				Data:      e.Data,
			})
			last = store.ExportCursor{UpdatedAt: e.UpdatedAt, Type: e.Type, ID: e.ID}
		}
		out.Checkpoint = encodeExportCursor(last)
		if page.HasMore {
			out.NextCursor = out.Checkpoint
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// exportTimestamp normalizes a stored stamp to RFC3339 like formatTimestamp,
// but keeps the sub-second part when the value already IS RFC3339. Migration
// 0061 writes updated_at with milliseconds and the feed is ordered on it, so
// truncating to whole seconds here would publish a coarser instant than the one
// the cursor seeks against — two rows a millisecond apart would read as
// simultaneous to anybody comparing the field instead of the cursor.
func exportTimestamp(stored string) string {
	if stored == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, stored); err == nil {
		return stored
	}
	return formatTimestamp(stored)
}

// parseExportLimit clamps the limit= param into [1, exportMaxLimit], defaulting
// on absence or garbage. An over-large ask is clamped rather than refused: the
// caller is another Server, and a page smaller than it asked for costs it one
// extra round trip, where a 400 costs it the sync.
func parseExportLimit(s string) int {
	if s == "" {
		return exportDefaultLimit
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return exportDefaultLimit
	}
	if n > exportMaxLimit {
		return exportMaxLimit
	}
	return n
}

// notFound writes the hide-existence 404 this route uses for every refusal a
// caller must not be able to tell apart.
func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
}

// exportLibraryID extracts the {id} from /libraries/{id}/export.
func exportLibraryID(rest string) string {
	id := strings.TrimSuffix(rest, "/export")
	if id == rest || id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}
