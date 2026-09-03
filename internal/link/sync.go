package link

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Keeping the mirror (ADR-0056 §3, §4).
//
// TODAY THIS FILE DOES ONE THING: the full pull at link time. `Sync` is the seam
// issue 08 grows into — the `libraryUpdated` nudge, the periodic sweep, the
// incremental `since` walk, `410 RESYNC`, the backoff and the three link states
// all hang off this one entry point, and none of them exist yet. What does exist
// is deliberately the shape they need: one call, addressed by Link id, that
// leaves the mirror correct however far behind it was.

// ErrNoMirror is a Service with no catalog to write into — a narrow unit test of
// the link flow, never a deployment.
var ErrNoMirror = errors.New("link: this server has no mirror to sync into")

// maxSyncPages bounds one Library's walk. The feed is keyset-ordered and the
// cursor is asserted to advance, so this is a guard against a peer that answers
// nonsense rather than an expected limit: 2000 pages at the export's 500-row cap
// is a million rows, which is more than any household has.
const maxSyncPages = 2000

// Sync brings this Server's mirror of a Link up to date. Today that is a FULL
// pull of every Library the sharer has granted: ask what they are, make a linked
// Library for each, walk its Export to the end, apply it, and record the
// checkpoint the next pull would resume from (issue 08).
//
// A Library the sharer has stopped granting is left alone here — nothing is
// deleted by a sync, only by an unlink (ADR-0056 §6). Reconciling the granted set
// is issue 08's.
func (s *Service) Sync(ctx context.Context, linkID string) error {
	if s.mirror == nil {
		return ErrNoMirror
	}
	l, err := s.store.LinkByID(linkID)
	if err != nil {
		return err
	}
	if l.ActiveOrigin == "" || l.Token == "" {
		return ErrUnreachable
	}

	client := s.client()
	remotes, err := s.remoteLibraries(ctx, client, l)
	if err != nil {
		return err
	}

	for _, rl := range remotes {
		lib, err := s.mirror.UpsertLinkedLibrary(s.newID(), rl.Name, rl.Kind, l.ID, rl.ID)
		if err != nil {
			return err
		}
		if err := s.pullLibrary(ctx, client, l, lib.ID, rl.ID); err != nil {
			return err
		}
	}
	return nil
}

// pullLibrary walks one Library's Export from the beginning and applies the
// whole thing in one go.
//
// It accumulates every page before applying, rather than applying page by page,
// because the feed is ordered by CHANGE TIME: a Stream can land on page one and
// the Title it hangs under on page nine, and a child whose parent has not arrived
// is skipped. Holding one Library's rows in memory is the price of applying them
// as one consistent set — and it is what lets the apply prune, which is the only
// way a removed Edition or Stream is ever noticed (store.ApplyMirror).
func (s *Service) pullLibrary(ctx context.Context, client *http.Client, l store.Link, libraryID, remoteLibraryID string) error {
	var (
		all        []ExportEntity
		cursor     string
		checkpoint string
	)
	for page := 0; ; page++ {
		if page >= maxSyncPages {
			return fmt.Errorf("link: %s never finished exporting library %s", l.ServerName, remoteLibraryID)
		}
		p, err := s.exportPage(ctx, client, l, remoteLibraryID, cursor)
		if err != nil {
			return err
		}
		all = append(all, p.Entities...)
		checkpoint = p.Checkpoint
		if p.NextCursor == "" {
			break
		}
		if p.NextCursor == cursor {
			return fmt.Errorf("link: %s handed back a cursor that does not advance", l.ServerName)
		}
		cursor = p.NextCursor
	}
	return s.Apply(l.ID, libraryID, all, checkpoint, true)
}

// remoteLibrary is one entry of the sharer's GET /libraries, which under a
// `remote` User's token is exactly the granted set (ADR-0056 §2) — the Export
// carries the same three fields per page, so nothing here learns anything the
// feed would not have told it anyway.
type remoteLibrary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Service) remoteLibraries(ctx context.Context, client *http.Client, l store.Link) ([]remoteLibrary, error) {
	var body struct {
		Libraries []remoteLibrary `json:"libraries"`
	}
	if err := s.getJSON(ctx, client, l, apiPrefix+"/libraries", &body); err != nil {
		return nil, err
	}
	out := make([]remoteLibrary, 0, len(body.Libraries))
	for _, rl := range body.Libraries {
		if rl.ID != "" && rl.Kind != "" {
			out = append(out, rl)
		}
	}
	return out, nil
}

func (s *Service) exportPage(ctx context.Context, client *http.Client, l store.Link, remoteLibraryID, cursor string) (ExportPage, error) {
	path := apiPrefix + "/libraries/" + url.PathEscape(remoteLibraryID) + "/export"
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	var p ExportPage
	if err := s.getJSON(ctx, client, l, path, &p); err != nil {
		return ExportPage{}, err
	}
	return p, nil
}

// getJSON is the read half of talking to a peer: one authenticated GET, bounded,
// with the two statuses that mean something to this side kept apart from the rest.
//
// A 401 is the sharer saying the credential is dead — the `revoked` state
// ADR-0056 §6 names, which issue 08 turns into a state transition and a "paste a
// new invite" on the admin page. It is surfaced as its own error now so that
// issue has something to switch on rather than a string to match.
func (s *Service) getJSON(ctx context.Context, client *http.Client, l store.Link, path string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.ActiveOrigin+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer closeBody(resp)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrCredentialDead
	case resp.StatusCode == http.StatusGone:
		return ErrResync
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: %s answered %d for %s", ErrUnreachable, l.ServerName, resp.StatusCode, path)
	}
	if err := decodeBody(resp, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrNotObelo, err)
	}
	return nil
}

// ErrCredentialDead is the sharer answering 401: the `remote` User or its Device
// is gone. ADR-0056 §6's `revoked`, which issue 08 acts on.
var ErrCredentialDead = errors.New("link: the sharing server no longer accepts this server's credential")

// ErrResync is the sharer answering 410 RESYNC: the cursor is older than their
// tombstone retention and the mirror must start over (ADR-0056 §4). Today every
// pull is already a full one, so nothing can raise it; it is named here because
// the code that CAN raise it is issue 08's and the meaning is settled now.
var ErrResync = errors.New("link: the sharing server asked for a full pull")

// syncAfterLink is the full pull that follows a successful link or re-key.
//
// It runs INSIDE the request that established the Link — the Admin waits for it —
// and that is a deliberate, temporary choice recorded here: the first pull is the
// difference between "linked" and "there are two new libraries in every app", and
// a background job would need the queue, the retry and the progress reporting that
// issue 08 owns. Its failure is logged and NOT returned: the Link is real, the
// credential is real, and the next sync will catch up.
func (s *Service) syncAfterLink(ctx context.Context, l store.Link) {
	if s.mirror == nil {
		return
	}
	if err := s.Sync(ctx, l.ID); err != nil {
		log.Printf("obelo: link: the first pull from %q did not finish (%v); "+
			"the linked libraries will fill in on the next sync", l.ServerName, err)
	}
}

// dropMirror is the other half of unlinking: the linked Libraries this Link
// brought, their mirrored rows and the Watch state on them all go (ADR-0056 §6).
// One DELETE, and the schema's cascades carry it down.
func (s *Service) dropMirror(l store.Link) {
	if s.mirror == nil {
		return
	}
	n, err := s.mirror.DeleteLibrariesForLink(l.ID)
	if err != nil {
		log.Printf("obelo: link: unlinking %q: its mirrored libraries could not be removed (%v)",
			l.ServerName, err)
		return
	}
	if n > 0 {
		log.Printf("obelo: link: unlinked %q and removed %d mirrored librar%s",
			l.ServerName, n, plural(n))
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// Libraries lists the linked Libraries one Link brought, for GET /links.
func (s *Service) Libraries(linkID string) ([]store.Library, error) {
	if s.mirror == nil {
		return nil, nil
	}
	return s.mirror.LibrariesForLink(linkID)
}

// syncTimeout is the deadline one whole pull gets. It is generous next to the
// per-call one because a first pull of a thousand-episode library is many pages,
// and cheap to raise when issue 08 moves it off the request.
const syncTimeout = 5 * time.Minute
