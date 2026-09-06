package api

import (
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The writer guard, and the two wire fields that tell a client what it is looking
// at (ADR-0056 §1).
//
// A linked Library is a Library in every read: granted per User, in Home rows, in
// search, in Collections and Playlists, under the Rating ceiling. It differs in
// exactly one direction — nothing here may WRITE to it, because the Server that
// owns the files is the identity authority for them (ADR-0002, ADR-0019). So
// every mutating route that can name one answers 409 LINKED_LIBRARY, and the
// guard lives at the DISPATCHER rather than inside each handler: the dispatchers
// are the one place where "which entity, by which id" is already parsed, and a
// guard added there cannot be forgotten by the next handler somebody writes
// under an existing leaf.

// LinkedLibraryReader is the mirror's read side for the API layer. *store.DB
// satisfies it. Nil in a narrow unit test, and nothing is then linked — which is
// true of every Server with no Links.
type LinkedLibraryReader interface {
	// IsLinkedLibrary reports whether a Library is a mirror. An unknown Library is
	// false: "there is no such Library" is the handler's refusal to make, not the
	// guard's.
	IsLinkedLibrary(id string) (bool, error)
	// LinkedLibraryStates maps each linked Library id to whether its sharing Server
	// is reachable (`available`) and the name it was linked under (`linkedServer`).
	// Nil when nothing is linked.
	LinkedLibraryStates() (map[string]store.LinkedLibraryState, error)
	// LibraryOfTitle and LibraryOfEntity resolve the Library behind an entity id,
	// which is what lets the guard sit on /titles/{id}/… and /shows/{id}/… without
	// each handler learning to ask.
	LibraryOfTitle(titleID string) (string, error)
	LibraryOfEntity(entityType, entityID string) (string, error)
}

// linkedLibrary reports whether a Library is a mirror, failing OPEN: a Server
// with no mirror wiring, or a read that errors, must not start refusing writes to
// ordinary local Libraries.
func linkedLibrary(deps Deps, libraryID string) bool {
	if deps.Mirror == nil || libraryID == "" {
		return false
	}
	linked, err := deps.Mirror.IsLinkedLibrary(libraryID)
	return err == nil && linked
}

// writeLinkedLibraryRefusal is the one sentence every guarded route gives.
func writeLinkedLibraryRefusal(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, codeLinkedLibrary,
		"this library is a mirror of another server's; it can only be changed there", nil)
}

// libraryIDOf pulls the {id} out of a "/libraries/{id}/…" tail, which is what
// the subtree dispatcher has in hand.
func libraryIDOf(rest string) string {
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// requireLocalLibrary refuses a write aimed at a mirrored Library, by Library id.
func requireLocalLibrary(deps Deps, libraryID string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if linkedLibrary(deps, libraryID) {
			writeLinkedLibraryRefusal(w)
			return
		}
		next(w, r)
	}
}

// requireLocalTitle refuses a write aimed at a mirrored Movie / Episode / Track.
func requireLocalTitle(deps Deps, titleID string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Mirror != nil {
			if libID, err := deps.Mirror.LibraryOfTitle(titleID); err == nil && linkedLibrary(deps, libID) {
				writeLinkedLibraryRefusal(w)
				return
			}
		}
		next(w, r)
	}
}

// requireLocalEntity refuses a write aimed at a mirrored Show / Season / Artist /
// Album (store.EntityShow and friends).
func requireLocalEntity(deps Deps, entityType, entityID string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Mirror != nil {
			if libID, err := deps.Mirror.LibraryOfEntity(entityType, entityID); err == nil && linkedLibrary(deps, libID) {
				writeLinkedLibraryRefusal(w)
				return
			}
		}
		next(w, r)
	}
}

// --- the two wire fields ------------------------------------------------------

// linkedState is the per-request answer to "which Libraries here are mirrors, is
// the Server behind each one reachable, and what is it called". It is read once
// per request that needs it and is nil on the overwhelmingly common Server that
// has no Links, so the decoration costs a map lookup that always misses.
type linkedState map[string]store.LinkedLibraryState

func loadLinkedState(deps Deps) linkedState {
	if deps.Mirror == nil {
		return nil
	}
	states, err := deps.Mirror.LinkedLibraryStates()
	if err != nil {
		return nil
	}
	return states
}

// decorate answers the `linked` / `available` / `linkedServer` triple for one
// Library id. available is a pointer so it is emitted ONLY for a linked Library:
// a local Library is not "available", it simply is, and a client that sees the
// field at all knows it is looking at somebody else's shelf. server is the
// sharing Server's display name — the provenance a member-facing surface names —
// and is empty (so `omitempty`-dropped) for a local Library.
//
// Today available is "the Link is connected". Issue 08 owns the state machine
// that moves it, and nothing else about this shape changes when it lands.
func (l linkedState) decorate(libraryID string) (bool, *bool, string) {
	if l == nil {
		return false, nil, ""
	}
	st, ok := l[libraryID]
	if !ok {
		return false, nil, ""
	}
	available := st.Available
	return true, &available, st.ServerName
}

// linkedFromLibrary is the same answer for a Library row already in hand, which
// knows its own source without a second read.
func linkedFromLibrary(lib store.Library, l linkedState) (bool, *bool, string) {
	if !lib.Linked() {
		return false, nil, ""
	}
	if st, ok := l[lib.ID]; ok {
		available := st.Available
		return true, &available, st.ServerName
	}
	no := false
	return true, &no, ""
}
