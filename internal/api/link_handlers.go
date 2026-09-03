package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/link"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The receiving half of linking, over HTTP (ADR-0055, ADR-0056,
// .scratch/linked-servers issue 06): an Admin pastes the one string a friend
// sent, and this Server holds the credential from then on.
//
// Every route here is Admin scope. Linking is not a per-User act — a Link is the
// household's relationship with another household, and the Libraries it brings
// are then granted to Users like any other (ADR-0056 §2).
//
// Since issue 07 a Link also brings Libraries: POST /links redeems, creates a
// linked Library per granted Library on the sharer and does the first full pull,
// and DELETE /links takes all of it away again. `libraries` on GET /links is
// those shelves.

// linkJSON is one Link on the wire.
//
// THERE IS NO TOKEN FIELD AND THERE MUST NEVER BE ONE. What this row holds is an
// outbound credential against somebody else's Server (ADR-0055 §1) — the same
// posture as a provider API key, which this API reports only as a hasKey
// boolean. There is not even a hasToken here, because a Link without a token
// cannot exist: `state` is what an operator actually wants to know.
type linkJSON struct {
	ID         string `json:"id"`
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	// State is connected | unreachable | revoked (ADR-0056 §6). The admin page
	// branches on it, so it is a closed set and never a free-text status.
	State string `json:"state"`
	// ActiveOrigin is the address that answered, and Origins every address the
	// invite carried, in the order it carried them. Both are shown so an operator
	// can see WHICH path is carrying their films — the tailnet one or the public
	// one — which is otherwise invisible and is the first thing to look at when a
	// Link is slow.
	ActiveOrigin string   `json:"activeOrigin"`
	Origins      []string `json:"origins"`
	// LastSyncedAt is null until the mirror has pulled once (issue 08). A pointer
	// rather than an empty string, for KeyExpiry's reason: null is this Server
	// saying "never", which is a statement, where "" is an absence.
	LastSyncedAt *string `json:"lastSyncedAt"`
	LastError    string  `json:"lastError"`
	// Libraries are the linked Libraries this Link brought (ADR-0056 §1) — empty
	// until the first pull finishes, and empty forever for a sharer who granted
	// this Server nothing.
	Libraries []linkLibraryJSON `json:"libraries"`
}

// linkLibraryJSON is a linked Library as the Linked servers page lists it.
type linkLibraryJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func toLinkJSON(l store.Link, libs []store.Library) linkJSON {
	origins := l.Origins
	if origins == nil {
		origins = []string{}
	}
	var synced *string
	if l.LastSyncedAt != "" {
		synced = &l.LastSyncedAt
	}
	return linkJSON{
		ID:           l.ID,
		ServerID:     l.ServerID,
		ServerName:   l.ServerName,
		State:        l.State,
		ActiveOrigin: l.ActiveOrigin,
		Origins:      origins,
		LastSyncedAt: synced,
		LastError:    l.LastError,
		Libraries:    toLinkLibraries(libs),
	}
}

func toLinkLibraries(libs []store.Library) []linkLibraryJSON {
	out := make([]linkLibraryJSON, 0, len(libs))
	for _, l := range libs {
		out = append(out, linkLibraryJSON{ID: l.ID, Name: l.Name, Kind: l.Kind})
	}
	return out
}

// linkWithLibraries reads a Link's mirrored shelves for the wire. A read failure
// is reported as "none": the Link itself is the answer this endpoint owes, and
// losing the whole list because one join failed helps nobody.
func linkWithLibraries(deps Deps, l store.Link) linkJSON {
	libs, err := deps.Links.Libraries(l.ID)
	if err != nil {
		libs = nil
	}
	return toLinkJSON(l, libs)
}

// linkRequest is the body of both POST /links and POST /links/{id}/rekey: the
// whole `obelo-link:` string, one field. Not "a hostname and a code" — that is
// two fields, two mistakes and no room for a second address (ADR-0055 §2).
type linkRequest struct {
	Invite string `json:"invite"`
}

// handleLinksCollection serves GET /links and POST /links.
func handleLinksCollection(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleListLinks(deps)(w, r)
		case http.MethodPost:
			handleCreateLink(deps)(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// handleLinkSubtree serves DELETE /links/{id} and POST /links/{id}/rekey.
func handleLinkSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/links/")

		if id, ok := strings.CutSuffix(rest, "/rekey"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost, handleRekeyLink(deps, id))(w, r)
			return
		}
		if rest == "" || strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		requireMethod(http.MethodDelete, handleDeleteLink(deps, rest))(w, r)
	}
}

func handleListLinks(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Links == nil {
			writeLinkUnwired(w)
			return
		}
		links, err := deps.Links.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not list linked servers", nil)
			return
		}
		out := make([]linkJSON, 0, len(links))
		for _, l := range links {
			out = append(out, linkWithLibraries(deps, l))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleCreateLink redeems a pasted invite and stores the Link.
//
// A 201 is a new Link and a 200 is a re-key of one that already existed
// (ADR-0055 §2: the same server id updates the Link in place). The distinction
// is on the status rather than in the body because it is exactly the
// created/updated distinction those two statuses mean, and a client that ignores
// it still gets the right Link back either way.
func handleCreateLink(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Links == nil {
			writeLinkUnwired(w)
			return
		}
		var req linkRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		l, rekeyed, err := deps.Links.Create(r.Context(), req.Invite)
		if err != nil {
			writeLinkError(w, err)
			return
		}
		status := http.StatusCreated
		if rekeyed {
			status = http.StatusOK
		}
		writeJSON(w, status, linkWithLibraries(deps, l))
	}
}

func handleRekeyLink(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Links == nil {
			writeLinkUnwired(w)
			return
		}
		var req linkRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		l, err := deps.Links.Rekey(r.Context(), id, req.Invite)
		if err != nil {
			writeLinkError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, linkWithLibraries(deps, l))
	}
}

// handleDeleteLink unlinks: the sharer is told, then the Link goes.
//
// It answers 204 whether or not the sharer could be reached. That is the
// contract and not a shortcut — see link.Service.Unlink for why a friend's
// server being off must not be able to keep this household linked to them.
func handleDeleteLink(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Links == nil {
			writeLinkUnwired(w)
			return
		}
		if err := deps.Links.Unlink(r.Context(), id); err != nil {
			writeLinkError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeLinkError maps the link domain's refusals onto the wire.
//
// Each one is a different sentence and a different next move for the operator,
// which is the whole reason they are separate errors rather than one "could not
// link": "that string is not an invite", "ask for a fresh one", "one of you
// needs an upgrade", "their server is not answering".
func writeLinkError(w http.ResponseWriter, err error) {
	var mismatch *link.ProtocolMismatch
	switch {
	case errors.Is(err, link.ErrBadInvite):
		writeError(w, http.StatusBadRequest, codeBadInvite,
			"that is not a valid invite; ask for the string again", nil)
	case errors.Is(err, link.ErrInviteExpired):
		// 410 and not 400: the string is perfectly well formed and it is the INVITE
		// that is gone, which is the distinction Gone exists to make and the one
		// that tells the operator to ask for a fresh one rather than re-copy this.
		writeError(w, http.StatusGone, codeInviteExpired,
			"this invite has expired; ask for a fresh one", nil)
	case errors.Is(err, link.ErrInviteRefused):
		writeError(w, http.StatusBadRequest, codeBadInvite,
			"the other server would not accept this invite; ask for a fresh one", nil)
	case errors.As(err, &mismatch):
		// details is named from the ASKING side here — theirs and ours — where the
		// sharing side's identical code names them supported/requested from its own
		// (issue 03). Each is the only frame that reads correctly where it is
		// emitted, and `upgrade` says outright which of ADR-0055 §3's two sentences
		// to show, so no client has to compare the numbers itself.
		writeError(w, http.StatusConflict, codeLinkProtocol,
			"the two servers speak different link protocol versions", map[string]any{
				"theirs":  mismatch.Theirs,
				"ours":    mismatch.Ours,
				"upgrade": mismatch.Upgrade(),
			})
	case errors.Is(err, link.ErrServerMismatch):
		writeError(w, http.StatusConflict, codeLinkServerMismatch,
			"this invite is for a different server than the link you are re-keying", nil)
	case errors.Is(err, link.ErrUnreachable):
		// 503 rather than a 4xx: nothing about the request was wrong, the other
		// household's machine did not answer. The message carries the reason the
		// last origin gave, which is the only thing an operator can act on.
		writeError(w, http.StatusServiceUnavailable, codeLinkUnreachable,
			"could not reach that server at any of the addresses in the invite: "+err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	default:
		writeError(w, http.StatusInternalServerError, codeInternal,
			"could not complete the link", nil)
	}
}

// writeLinkUnwired answers the narrow unit-test wiring where no link service
// exists — 503 with a message, never a 404 that reads like a typo'd path.
func writeLinkUnwired(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
		"linked servers are not available on this server", nil)
}
