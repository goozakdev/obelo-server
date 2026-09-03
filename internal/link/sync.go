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

// Keeping the mirror (ADR-0056 §3, §4, §6).
//
// ONE SWEEP is the unit of work in this file: reach the sharer at whichever of
// its addresses answers, reconcile the set of Libraries it grants this Server,
// pull each of them forward from wherever this side had got to, and record what
// the attempt says about the Link. `Sync` is that sweep and everything else
// drives it — the periodic timer, the sharer's `libraryUpdated` nudge and the
// operator's "sync now" are three ways of asking for one, and they are in
// watch.go, which owns the goroutines and none of the decisions.
//
// The sweep is synchronous, idempotent and safe to run twice: every write is
// keyed on the sharer's ids, so the only cost of a redundant sweep is the
// request.

// ErrNoMirror is a Service with no catalog to write into — a narrow unit test of
// the link flow, never a deployment.
var ErrNoMirror = errors.New("link: this server has no mirror to sync into")

// maxSyncPages bounds one Library's walk. The feed is keyset-ordered and the
// cursor is asserted to advance, so this is a guard against a peer that answers
// nonsense rather than an expected limit: 2000 pages at the export's 500-row cap
// is a million rows, which is more than any household has.
const maxSyncPages = 2000

// Sync runs one sweep of a Link and records its outcome (ADR-0056 §4, §6).
//
// A `revoked` Link is NOT swept: the credential is dead until an Admin pastes a
// new invite, and retrying it on a timer only teaches a friend's server to
// rate-limit this one. `SyncNow` is the deliberate exception — an operator
// pressing "sync now" is not a retry.
func (s *Service) Sync(ctx context.Context, linkID string) error {
	return s.sweep(ctx, linkID, false)
}

// SyncNow is POST /links/{id}/sync: force a sweep and answer with the Link as it
// stands afterwards, whether or not the sweep worked.
//
// It goes through the running Syncer when there is one, so the sweep the
// operator asked for is the same sweep the timer would have run — one at a time
// per Link, and the backoff reset by its result — rather than a second one
// racing it. With no Syncer (a narrow unit test) it sweeps inline.
func (s *Service) SyncNow(ctx context.Context, linkID string) (store.Link, error) {
	if _, err := s.store.LinkByID(linkID); err != nil {
		return store.Link{}, err
	}
	var err error
	if sy := s.syncer(); sy != nil {
		err = sy.SweepNow(ctx, linkID)
	} else {
		err = s.sweep(ctx, linkID, true)
	}
	l, readErr := s.store.LinkByID(linkID)
	if readErr != nil {
		return store.Link{}, readErr
	}
	return l, err
}

// sweep is the body of both. forced says an operator asked, which is the one
// thing that gets past the `revoked` stop.
func (s *Service) sweep(ctx context.Context, linkID string, forced bool) error {
	if s.mirror == nil {
		return ErrNoMirror
	}
	l, err := s.store.LinkByID(linkID)
	if err != nil {
		return err
	}
	if l.State == store.LinkStateRevoked && !forced {
		return ErrCredentialDead
	}
	if l.Token == "" {
		return ErrUnreachable
	}

	err = s.pull(ctx, &l)
	s.record(l, err)
	return err
}

// record moves the Link's state to what the sweep just proved (ADR-0056 §6) and
// nudges the admin surface when it MOVED.
//
// Only a transition is announced and logged. A friend's server that is off for a
// week is one line and one event, not one an hour: the state is on GET /links
// for anyone who asks, and a log that repeats itself is a log nobody reads.
func (s *Service) record(l store.Link, err error) {
	if errors.Is(err, context.Canceled) {
		// The sweep was abandoned — this Server is shutting down, or the Link was
		// unlinked under it. That says nothing about the other household, and
		// writing `unreachable` here would leave a shut-down Server telling the next
		// boot a lie about a friend it never finished asking. (A DEADLINE, by
		// contrast, is a peer that would not answer, and does fall through.)
		return
	}
	state, lastError := store.LinkStateConnected, ""
	switch {
	case err == nil:
	case errors.Is(err, ErrCredentialDead):
		state, lastError = store.LinkStateRevoked, "the sharing server no longer accepts this server's credential"
	default:
		state, lastError = store.LinkStateUnreachable, err.Error()
	}

	if state == store.LinkStateConnected {
		if serr := s.store.SetLinkSynced(l.ID, s.now().UTC().Format(time.RFC3339)); serr != nil {
			log.Printf("obelo: link: recording the sync of %q: %v", l.ServerName, serr)
			return
		}
	} else if serr := s.store.SetLinkState(l.ID, state, lastError); serr != nil {
		log.Printf("obelo: link: recording the state of %q: %v", l.ServerName, serr)
		return
	}

	if state == l.State {
		return
	}
	switch state {
	case store.LinkStateConnected:
		log.Printf("obelo: link: %q is reachable again at %s; its libraries are up to date",
			l.ServerName, l.ActiveOrigin)
	case store.LinkStateUnreachable:
		log.Printf("obelo: link: %q is not answering (%v); its libraries stay, marked unavailable, "+
			"and this server will keep trying", l.ServerName, err)
	case store.LinkStateRevoked:
		// The one state no amount of retrying fixes, so the line says what does.
		log.Printf("obelo: link: %q has revoked this server's credential — the remote user or its "+
			"device was deleted over there. Nothing here is lost and nothing will be retried: ask them "+
			"for a fresh invite and paste it into Linked servers (\"paste a new invite\") to re-key this "+
			"link in place.", l.ServerName)
	}
	s.publishLinkState(l.ID)
}

// pull is one sweep's work against a reachable sharer: the granted set, then
// each granted Library brought forward.
func (s *Service) pull(ctx context.Context, l *store.Link) error {
	client := s.client()
	remotes, origin, err := s.reachLibraries(ctx, client, *l)
	if err != nil {
		return err
	}
	if origin != l.ActiveOrigin {
		if err := s.store.SetLinkActiveOrigin(l.ID, origin); err != nil {
			return err
		}
		log.Printf("obelo: link: %q is now being reached at %s", l.ServerName, origin)
		l.ActiveOrigin = origin
	}

	if err := s.reconcile(*l, remotes); err != nil {
		return err
	}

	for _, rl := range remotes {
		lib, err := s.mirror.UpsertLinkedLibrary(s.newID(), rl.Name, rl.Kind, l.ID, rl.ID)
		if err != nil {
			return err
		}
		changed, err := s.pullLibrary(ctx, client, *l, lib)
		if err != nil {
			return err
		}
		if changed {
			// The mirror moved, so every client looking at this shelf should look
			// again — the same nudge a scan of a local Library publishes, which is
			// what makes a friend's new film appear in the app without a refresh.
			s.publishLibraryUpdated(lib.ID)
		}
	}
	return nil
}

// reconcile handles the Libraries the sharer has STOPPED granting (ADR-0056 §6).
//
// A revoked grant is tombstoned in place — hidden, kept — and never deleted.
// Deleting would take this household's Watch state with it and would give a
// Library that comes back a second identity; hiding leaves browse, Home rows and
// search excluding exactly what the sharer no longer offers, and a re-grant
// restores the shelf with the same local ids on the next (full) pull. See
// store.DB.TombstoneMirror.
//
// The other direction needs no code: a Library newly granted is one the upsert
// has never seen, so it becomes a linked Library and is pulled in full.
func (s *Service) reconcile(l store.Link, remotes []remoteLibrary) error {
	existing, err := s.mirror.LibrariesForLink(l.ID)
	if err != nil {
		return err
	}
	granted := make(map[string]bool, len(remotes))
	for _, rl := range remotes {
		granted[rl.ID] = true
	}
	for _, lib := range existing {
		if granted[lib.RemoteLibraryID] {
			continue
		}
		if err := s.mirror.TombstoneMirror(lib.ID); err != nil {
			return err
		}
		log.Printf("obelo: link: %q no longer shares %q; it is hidden here and nothing was deleted "+
			"— it comes back as it was if they share it again", l.ServerName, lib.Name)
		s.publishLibraryUpdated(lib.ID)
	}
	return nil
}

// pullLibrary brings one Library forward and reports whether anything landed.
//
// It is INCREMENTAL when this side has a checkpoint: `since` is the position the
// last pull reached (libraries.remote_checkpoint), and the feed answers only what
// changed after it. Two things send it back to a full pull:
//
//   - `410 RESYNC` — the sharer says that position is older than its tombstone
//     retention and it can no longer promise the feed still holds every removal
//     this mirror missed (ADR-0056 §4).
//   - THE EDITION/STREAM GAP (issue 07's parting note). Editions and Streams have
//     no soft-delete and the sharer's scanner rebuilds them with fresh ids, so a
//     removal there can carry no tombstone and is visible only as an ABSENCE —
//     which is meaningful in a full pull and meaningless in an incremental one,
//     where an absence means "did not change". So store.ApplyMirror prunes on a
//     full pull only, and an incremental pull that carries ANY Edition, File or
//     Stream row is re-walked in full so that prune can run.
//
// That escalation is the simplest thing that is correct, and it is deliberately
// blunt: it triggers on an addition as readily as on a removal, because the two
// are the same event over there (a rebuilt subtree) and this side cannot tell
// them apart without asking for the whole truth anyway. A metadata-only change —
// an enrichment pass, a rename, a tombstone — stays incremental, which is the
// common case and the one that has to be cheap.
func (s *Service) pullLibrary(ctx context.Context, client *http.Client, l store.Link, lib store.Library) (bool, error) {
	since := lib.RemoteCheckpoint
	full := since == ""

	entities, checkpoint, err := s.walk(ctx, client, l, lib.RemoteLibraryID, since)
	if errors.Is(err, ErrResync) {
		log.Printf("obelo: link: %q asked for a full pull of %q; its cursor here was too old",
			l.ServerName, lib.Name)
		since, full = "", true
		entities, checkpoint, err = s.walk(ctx, client, l, lib.RemoteLibraryID, "")
	}
	if err != nil {
		return false, err
	}
	if !full && carriesMedia(entities) {
		full = true
		entities, checkpoint, err = s.walk(ctx, client, l, lib.RemoteLibraryID, "")
		if err != nil {
			return false, err
		}
	}
	if !full && len(entities) == 0 {
		// Nothing changed over there. Not a no-op for the Link — the call that
		// proved it is what moves the state to `connected` — but nothing to apply
		// and nothing for a client to refetch.
		return false, nil
	}
	if err := s.Apply(l.ID, lib.ID, entities, checkpoint, full); err != nil {
		return false, err
	}
	return len(entities) > 0, nil
}

// mediaTypes are the three wire types with no tombstone of their own. See
// pullLibrary.
var mediaTypes = map[string]bool{
	store.ExportEdition: true,
	store.ExportFile:    true,
	store.ExportStream:  true,
}

func carriesMedia(entities []ExportEntity) bool {
	for _, e := range entities {
		if mediaTypes[e.Type] {
			return true
		}
	}
	return false
}

// walk reads one Library's feed from `since` to the end and returns every row it
// carried plus the position to resume from.
//
// It accumulates before applying, rather than applying page by page, because the
// feed is ordered by CHANGE TIME: a Stream can land on page one and the Title it
// hangs under on page nine, and a child whose parent has not arrived is skipped.
// Holding one Library's rows in memory is the price of applying them as one
// consistent set — and it is what lets the apply prune, which is the only way a
// removed Edition or Stream is ever noticed (store.ApplyMirror).
func (s *Service) walk(ctx context.Context, client *http.Client, l store.Link, remoteLibraryID, since string) ([]ExportEntity, string, error) {
	var (
		all        []ExportEntity
		cursor     string
		checkpoint string
	)
	for page := 0; ; page++ {
		if page >= maxSyncPages {
			return nil, "", fmt.Errorf("link: %s never finished exporting library %s", l.ServerName, remoteLibraryID)
		}
		p, err := s.exportPage(ctx, client, l, remoteLibraryID, since, cursor)
		if err != nil {
			return nil, "", err
		}
		all = append(all, p.Entities...)
		checkpoint = p.Checkpoint
		if p.NextCursor == "" {
			break
		}
		if p.NextCursor == cursor {
			return nil, "", fmt.Errorf("link: %s handed back a cursor that does not advance", l.ServerName)
		}
		cursor = p.NextCursor
	}
	return all, checkpoint, nil
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

// reachLibraries asks the sharer what it grants, walking the invite's addresses
// until one answers (ADR-0056 §6: "retried with backoff across every origin").
//
// Reachability is decided ONCE per sweep, by this first call, and the origin that
// answered carries the rest of it. A 401 stops the walk immediately: every origin
// leads to the same machine, so a dead credential is dead at all of them and
// trying the others only spends requests to be told so again.
func (s *Service) reachLibraries(ctx context.Context, client *http.Client, l store.Link) ([]remoteLibrary, string, error) {
	var lastErr error
	for _, origin := range originsFor(l) {
		libs, err := s.remoteLibraries(ctx, client, l, origin)
		switch {
		case err == nil:
			return libs, origin, nil
		case errors.Is(err, ErrCredentialDead):
			return nil, "", err
		default:
			lastErr = fmt.Errorf("%s: %w", origin, err)
		}
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", ErrUnreachable
}

// originsFor is the order a sweep tries addresses in: the one that worked last
// time first, then the rest as the invite listed them. A Link whose row somehow
// carries no addresses at all still tries its active origin, which is the only
// thing it knows.
func originsFor(l store.Link) []string {
	out := make([]string, 0, len(l.Origins)+1)
	if l.ActiveOrigin != "" {
		out = append(out, l.ActiveOrigin)
	}
	for _, o := range l.Origins {
		if o != l.ActiveOrigin {
			out = append(out, o)
		}
	}
	return out
}

func (s *Service) remoteLibraries(ctx context.Context, client *http.Client, l store.Link, origin string) ([]remoteLibrary, error) {
	var body struct {
		Libraries []remoteLibrary `json:"libraries"`
	}
	if err := s.getJSON(ctx, client, l, origin, apiPrefix+"/libraries", &body); err != nil {
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

func (s *Service) exportPage(ctx context.Context, client *http.Client, l store.Link, remoteLibraryID, since, cursor string) (ExportPage, error) {
	path := apiPrefix + "/libraries/" + url.PathEscape(remoteLibraryID) + "/export"
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var p ExportPage
	if err := s.getJSON(ctx, client, l, l.ActiveOrigin, path, &p); err != nil {
		return ExportPage{}, err
	}
	return p, nil
}

// getJSON is the read half of talking to a peer: one authenticated GET, bounded,
// with the two statuses that mean something to this side kept apart from the rest.
//
// A 401 is the sharer saying the credential is dead — ADR-0056 §6's `revoked`,
// which record() acts on and which no amount of retrying fixes. A 410 is the
// export's RESYNC, which pullLibrary answers with a full pull.
func (s *Service) getJSON(ctx context.Context, client *http.Client, l store.Link, origin, path string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
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
// is gone. ADR-0056 §6's `revoked`.
var ErrCredentialDead = errors.New("link: the sharing server no longer accepts this server's credential")

// ErrResync is the sharer answering 410 RESYNC: the cursor is older than their
// tombstone retention and the mirror must start over (ADR-0056 §4).
var ErrResync = errors.New("link: the sharing server asked for a full pull")

// syncAfterLink is the full pull that follows a successful link or re-key.
//
// It runs INSIDE the request that established the Link — the Admin waits for it —
// and that is a deliberate choice recorded here: the first pull is the difference
// between "linked" and "there are two new libraries in every app". Its failure is
// logged and NOT returned: the Link is real, the credential is real, the state it
// left behind is honest, and the sweep after it will catch up.
func (s *Service) syncAfterLink(ctx context.Context, l store.Link) {
	if s.mirror == nil {
		return
	}
	// forced: a Link being re-keyed may still be sitting at `revoked`, and the
	// paste the operator just made is exactly the fix for it.
	if err := s.sweep(ctx, l.ID, true); err != nil {
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

// syncTimeout is the deadline one whole sweep gets. It is generous next to the
// per-call one because a first pull of a thousand-episode library is many pages.
const syncTimeout = 5 * time.Minute
