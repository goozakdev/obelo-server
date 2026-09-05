package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Keeping the mirror fresh (.scratch/linked-servers issue 08, ADR-0056 §4, §6).
//
// The sharer here is a stub rather than a whole Server, for the reason issue 06's
// tests give: what is under test is a set of things a real pair of Servers cannot
// easily be made to do — answer 410 RESYNC, go away mid-sweep, revoke a
// credential, stop granting a Library — and each of them has to be scriptable. The
// end-to-end proof against the real sharing half is in internal/api.
//
// Nothing here waits out a duration. The timer and the backoff are driven through
// the Syncer's injected clock, and every assertion about a background goroutine
// waits on that goroutine reporting its own sweep.

// --- a scriptable sharer -------------------------------------------------------

// stubLib is one Library over there: what it is called, and its feed, which is an
// append-only list. The checkpoint is simply how many rows of it a mirror has
// consumed, which makes "what did the incremental pull ask for, and what did it
// get" readable in the assertions.
type stubLib struct {
	name, kind string
	entities   []ExportEntity
	granted    bool
}

type stubSharer struct {
	mu    sync.Mutex
	libs  map[string]*stubLib
	order []string
	// seen is every path this sharer was asked for, in order — how a test asserts
	// that an incremental pull carried a `since`, or that a revoked Link made no
	// call at all.
	seen []string
	// status, when set, is answered for every call but the handshake: 500 for a
	// sharer having a bad day, 401 for one that has deleted the remote User.
	status int
	// resync answers 410 to any request carrying a `since`, which is the sharer
	// saying the mirror's position is older than its tombstone retention.
	resync bool
	// gate, when set, holds /libraries until the test releases it — how the state
	// at the exact moment after boot is observed without a race.
	gate     chan struct{}
	pageSize int
	subs     map[chan string]struct{}
}

func newStubSharer(t *testing.T) (*stubSharer, *httptest.Server) {
	t.Helper()
	s := &stubSharer{
		libs:     map[string]*stubLib{},
		pageSize: 2,
		subs:     map[chan string]struct{}{},
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *stubSharer) addLibrary(id, name, kind string, entities ...ExportEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.libs[id] = &stubLib{name: name, kind: kind, entities: entities, granted: true}
	s.order = append(s.order, id)
}

func (s *stubSharer) append(id string, entities ...ExportEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.libs[id].entities = append(s.libs[id].entities, entities...)
}

func (s *stubSharer) grant(id string, granted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.libs[id].granted = granted
}

func (s *stubSharer) answer(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *stubSharer) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// nudged pushes one libraryUpdated onto every open /events stream, the way a
// finished scan does on a real sharer.
func (s *stubSharer) nudged(libraryID string) {
	s.mu.Lock()
	subs := make([]chan string, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- libraryID:
		default:
		}
	}
}

func (s *stubSharer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.URL.RequestURI())
	status, gate := s.status, s.gate
	s.mu.Unlock()

	if gate != nil {
		<-gate
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	switch {
	case r.URL.Path == apiPrefix+"/libraries":
		s.serveLibraries(w)
	case r.URL.Path == apiPrefix+"/events":
		s.serveEvents(w, r)
	case strings.HasSuffix(r.URL.Path, "/export"):
		s.serveExport(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *stubSharer) serveLibraries(w http.ResponseWriter) {
	s.mu.Lock()
	out := []map[string]any{}
	for _, id := range s.order {
		if l := s.libs[id]; l.granted {
			out = append(out, map[string]any{"id": id, "name": l.name, "kind": l.kind})
		}
	}
	s.mu.Unlock()
	writeStubJSON(w, map[string]any{"libraries": out})
}

func (s *stubSharer) serveExport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix+"/libraries/"), "/export")
	q := r.URL.Query()

	s.mu.Lock()
	lib, ok := s.libs[id]
	resync := s.resync
	size := s.pageSize
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if resync && q.Get("since") != "" {
		w.WriteHeader(http.StatusGone)
		return
	}

	from := stubPos(q.Get("since"))
	if c := q.Get("cursor"); c != "" {
		from = stubPos(c)
	}
	s.mu.Lock()
	all := append([]ExportEntity(nil), lib.entities...)
	s.mu.Unlock()

	if from > len(all) {
		from = len(all)
	}
	to := from + size
	if to > len(all) {
		to = len(all)
	}
	page := map[string]any{
		"linkProtocolVersion": 1,
		"library":             map[string]any{"id": id, "kind": lib.kind, "name": lib.name},
		"entities":            all[from:to],
		"checkpoint":          strconv.Itoa(to),
	}
	if to < len(all) {
		page["nextCursor"] = strconv.Itoa(to)
	}
	writeStubJSON(w, page)
}

// serveEvents is the sharer's /events: the ": connected" comment, then one
// libraryUpdated per nudge, until the caller goes away.
func (s *stubSharer) serveEvents(w http.ResponseWriter, r *http.Request) {
	ch := make(chan string, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case id := <-ch:
			fmt.Fprintf(w, "event: libraryUpdated\ndata: {\"libraryId\":%q}\n\n", id)
			flusher.Flush()
		}
	}
}

func stubPos(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func writeStubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- a mirror that remembers what it was told -----------------------------------

type applyCall struct {
	libraryID string
	entities  []store.MirrorEntity
	full      bool
}

type memMirror struct {
	mu         sync.Mutex
	libs       []store.Library
	applies    []applyCall
	tombstoned []string
}

func (m *memMirror) UpsertLinkedLibrary(id, name, kind, linkID, remoteLibraryID string) (store.Library, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.libs {
		if l.LinkID == linkID && l.RemoteLibraryID == remoteLibraryID {
			return l, nil
		}
	}
	l := store.Library{
		ID: id, Name: name, Kind: kind, Source: store.LibrarySourceLinked,
		LinkID: linkID, RemoteLibraryID: remoteLibraryID,
	}
	m.libs = append(m.libs, l)
	return l, nil
}

func (m *memMirror) LibrariesForLink(linkID string) ([]store.Library, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Library
	for _, l := range m.libs {
		if l.LinkID == linkID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m *memMirror) DeleteLibrariesForLink(linkID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var kept []store.Library
	n := 0
	for _, l := range m.libs {
		if l.LinkID == linkID {
			n++
			continue
		}
		kept = append(kept, l)
	}
	m.libs = kept
	return n, nil
}

func (m *memMirror) SetLibraryCheckpoint(id, checkpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.libs {
		if m.libs[i].ID == id {
			m.libs[i].RemoteCheckpoint = checkpoint
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memMirror) ApplyMirror(libraryID string, entities []store.MirrorEntity, full bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applies = append(m.applies, applyCall{libraryID: libraryID, entities: entities, full: full})
	return nil
}

// TombstoneMirror mirrors the real one's two effects: nothing is deleted, and the
// checkpoint is cleared so a re-grant is a full pull.
func (m *memMirror) TombstoneMirror(libraryID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tombstoned = append(m.tombstoned, libraryID)
	for i := range m.libs {
		if m.libs[i].ID == libraryID {
			m.libs[i].RemoteCheckpoint = ""
		}
	}
	return nil
}

func (m *memMirror) calls() []applyCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]applyCall(nil), m.applies...)
}

func (m *memMirror) last(t *testing.T) applyCall {
	t.Helper()
	calls := m.calls()
	if len(calls) == 0 {
		t.Fatal("nothing was applied to the mirror")
	}
	return calls[len(calls)-1]
}

// memPublisher records the two nudges a sweep can send.
type memPublisher struct {
	mu        sync.Mutex
	libraries []string
	states    []string
}

func (p *memPublisher) PublishLibraryUpdated(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.libraries = append(p.libraries, id)
}

func (p *memPublisher) PublishLinkState(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = append(p.states, id)
}

func (p *memPublisher) counts() (libraries, states int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.libraries), len(p.states)
}

// --- the fixture ---------------------------------------------------------------

type syncFixture struct {
	sharer *stubSharer
	peer   *httptest.Server
	store  *memStore
	mirror *memMirror
	pub    *memPublisher
	svc    *Service
	linkID string
}

// newSyncFixture is a home Server already linked to a sharer. The Link row is
// written directly rather than redeemed: the redemption is issue 06's and every
// test here starts after it.
func newSyncFixture(t *testing.T, origins ...string) *syncFixture {
	t.Helper()
	sharer, peer := newStubSharer(t)
	if len(origins) == 0 {
		origins = []string{peer.URL}
	}
	st := &memStore{}
	f := &syncFixture{
		sharer: sharer, peer: peer, store: st,
		mirror: &memMirror{}, pub: &memPublisher{}, linkID: "link-1",
	}
	if err := st.InsertLink(store.Link{
		ID: f.linkID, ServerID: "sharer-id", ServerName: "Amy's server",
		Origins: origins, ActiveOrigin: origins[0], Token: "tok", DeviceID: "d1",
		LinkProtocolVersion: 1, State: store.LinkStateConnected,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	f.svc = newService(t, st, Options{Mirror: f.mirror, Events: f.pub})
	return f
}

func (f *syncFixture) link(t *testing.T) store.Link {
	t.Helper()
	l, err := f.store.LinkByID(f.linkID)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *syncFixture) sync(t *testing.T) error {
	t.Helper()
	return f.svc.Sync(context.Background(), f.linkID)
}

func (f *syncFixture) mustSync(t *testing.T) {
	t.Helper()
	if err := f.sync(t); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

func entity(typ, id, parent string) ExportEntity {
	return ExportEntity{
		Type: typ, ID: id, ParentID: parent,
		UpdatedAt: "2026-01-01T00:00:00.000Z",
		Data:      map[string]any{"title": id},
	}
}

// waitFor polls a condition. It exists so no test sleeps for a fixed time: the
// wait ends the moment the goroutine has done the thing, and the deadline is only
// there so a broken build fails instead of hanging.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the pulls -----------------------------------------------------------------

// TestIncrementalPullAppliesOnlyWhatChanged is the point of the checkpoint: the
// second sweep asks the sharer what happened AFTER the first one and applies that,
// rather than walking a thousand-episode library again to find one rename.
func TestIncrementalPullAppliesOnlyWhatChanged(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie",
		entity(store.ExportTitle, "t1", ""), entity(store.ExportTitle, "t2", ""))

	f.mustSync(t)
	first := f.mirror.last(t)
	if !first.full || len(first.entities) != 2 {
		t.Fatalf("first pull applied %d entities (full=%v), want 2 as a full pull",
			len(first.entities), first.full)
	}

	f.sharer.append("rl1", entity(store.ExportTitle, "t3", ""))
	f.mustSync(t)

	second := f.mirror.last(t)
	if second.full {
		t.Error("the second pull was full; the checkpoint from the first should have made it incremental")
	}
	if len(second.entities) != 1 || second.entities[0].RemoteID != "t3" {
		t.Fatalf("the second pull applied %+v, want only the new title", second.entities)
	}
	if !containsPath(f.sharer.paths(), "since=2") {
		t.Errorf("no request carried the checkpoint as `since`: %v", f.sharer.paths())
	}

	// A third sweep with nothing new applies nothing at all — and still leaves the
	// Link connected, because the call that proved it is what moves the state.
	before := len(f.mirror.calls())
	f.mustSync(t)
	if got := len(f.mirror.calls()); got != before {
		t.Errorf("a sweep with nothing to do applied %d times, want %d", got-before, 0)
	}
	if st := f.link(t).State; st != store.LinkStateConnected {
		t.Errorf("state = %q, want connected", st)
	}
}

// TestResyncRestartsTheWholePull: the sharer's 410 means "that position is older
// than my tombstone retention" — the mirror cannot trust an incremental pull from
// it, so it starts over.
func TestResyncRestartsTheWholePull(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	f.mustSync(t)

	f.sharer.mu.Lock()
	f.sharer.resync = true
	f.sharer.mu.Unlock()
	f.sharer.append("rl1", entity(store.ExportTitle, "t2", ""))

	f.mustSync(t)
	last := f.mirror.last(t)
	if !last.full {
		t.Error("the pull after a 410 was incremental; RESYNC means start over")
	}
	if len(last.entities) != 2 {
		t.Fatalf("the restarted pull carried %d entities, want the whole library (2)", len(last.entities))
	}
	if st := f.link(t).State; st != store.LinkStateConnected {
		t.Errorf("state = %q after a handled RESYNC, want connected", st)
	}
}

// TestAnIncrementalPullCarryingMediaIsEscalated is the Edition/Stream gap issue 07
// left open, closed the simplest way that is correct.
//
// Those three types have no tombstone: a removal over there is visible only as an
// absence, which is meaningful in a full pull (where store.ApplyMirror prunes) and
// meaningless in an incremental one. So an incremental pull that carries any of
// them is re-walked in full, and the prune runs.
func TestAnIncrementalPullCarryingMediaIsEscalated(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	f.mustSync(t)

	// A metadata-only change stays incremental: this is the common case and the one
	// that has to stay cheap.
	f.sharer.append("rl1", entity(store.ExportTitle, "t2", ""))
	f.mustSync(t)
	if f.mirror.last(t).full {
		t.Fatal("a title-only change was escalated to a full pull; only Edition/File/Stream should be")
	}

	// A rebuilt subtree is not: an Edition arriving means the sharer's scanner
	// rewrote that Title's media, and the only way to notice what it REMOVED is to
	// ask for the whole truth.
	f.sharer.append("rl1", entity(store.ExportEdition, "e1", "t1"))
	f.mustSync(t)

	last := f.mirror.last(t)
	if !last.full {
		t.Error("an incremental pull carrying an Edition was applied without a prune")
	}
	if len(last.entities) != 3 {
		t.Errorf("the escalated pull carried %d entities, want the whole library (3)", len(last.entities))
	}
}

// --- the state machine ----------------------------------------------------------

// TestSweepRecordsEveryTransition walks ADR-0056 §6 end to end on one Link:
// connected → unreachable → connected → revoked, with the nudge fired once per
// MOVE and the last error kept honest.
func TestSweepRecordsEveryTransition(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))

	f.mustSync(t)
	if l := f.link(t); l.State != store.LinkStateConnected || l.LastSyncedAt == "" || l.LastError != "" {
		t.Fatalf("after a good sweep = %+v, want connected, stamped and with no error", l)
	}
	_, states := f.pub.counts()
	if states != 0 {
		t.Errorf("a sweep that changed nothing published %d linkState nudges, want 0", states)
	}

	// Their server is having a bad day.
	f.sharer.answer(http.StatusInternalServerError)
	if err := f.sync(t); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a 500 gave %v, want an unreachable", err)
	}
	l := f.link(t)
	if l.State != store.LinkStateUnreachable || l.LastError == "" {
		t.Fatalf("after a 500 = %+v, want unreachable with a reason", l)
	}
	if _, states := f.pub.counts(); states != 1 {
		t.Errorf("linkState nudges = %d, want 1 (the move to unreachable)", states)
	}

	// A repeat says the same thing and must not re-announce it.
	_ = f.sync(t)
	if _, states := f.pub.counts(); states != 1 {
		t.Errorf("linkState nudges = %d after a second failure, want still 1", states)
	}

	// They come back.
	f.sharer.answer(0)
	f.mustSync(t)
	if l := f.link(t); l.State != store.LinkStateConnected || l.LastError != "" {
		t.Fatalf("after recovery = %+v, want connected with the reason cleared", l)
	}
	if _, states := f.pub.counts(); states != 2 {
		t.Errorf("linkState nudges = %d, want 2 (unreachable, then connected)", states)
	}

	// They delete the remote User.
	f.sharer.answer(http.StatusUnauthorized)
	if err := f.sync(t); !errors.Is(err, ErrCredentialDead) {
		t.Fatalf("a 401 gave %v, want the credential to be reported dead", err)
	}
	if st := f.link(t).State; st != store.LinkStateRevoked {
		t.Fatalf("state = %q after a 401, want revoked", st)
	}
}

// TestRevokedStaysRevokedAndIsNeverRetried: `revoked` is the one state no retry
// fixes, so the automatic sweep does not even dial. The proof is that the sharer
// sees no further requests — a Link that kept calling would teach a friend's
// server to rate-limit this one, and would flip the state back to `unreachable`
// the moment their machine rebooted, hiding the thing an operator has to act on.
func TestRevokedStaysRevokedAndIsNeverRetried(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	f.mustSync(t)

	f.sharer.answer(http.StatusUnauthorized)
	_ = f.sync(t)
	if st := f.link(t).State; st != store.LinkStateRevoked {
		t.Fatalf("state = %q, want revoked", st)
	}

	// Their server comes back, credential still gone — and even if it had not, this
	// side does not ask.
	f.sharer.answer(0)
	before := len(f.sharer.paths())
	if err := f.sync(t); !errors.Is(err, ErrCredentialDead) {
		t.Fatalf("sweeping a revoked link gave %v, want the credential-dead refusal", err)
	}
	if after := len(f.sharer.paths()); after != before {
		t.Errorf("a revoked link made %d calls, want none", after-before)
	}
	if st := f.link(t).State; st != store.LinkStateRevoked {
		t.Errorf("state = %q, want it to stay revoked", st)
	}
}

// TestSweepWalksTheOriginsAndRemembersTheWinner: the invite's addresses are
// addresses to TRY, and which of them works changes over a Link's life — a
// friend's tailnet comes and goes. The one that answered is remembered, so the
// next sweep starts there.
func TestSweepWalksTheOriginsAndRemembersTheWinner(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	f := newSyncFixture(t)
	live := f.peer.URL
	if err := f.store.update(f.linkID, func(l *store.Link) {
		l.Origins, l.ActiveOrigin = []string{dead.URL, live}, dead.URL
	}); err != nil {
		t.Fatal(err)
	}
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))

	f.mustSync(t)
	if got := f.link(t).ActiveOrigin; got != live {
		t.Fatalf("activeOrigin = %q, want the address that answered (%q)", got, live)
	}
}

// --- the granted set ------------------------------------------------------------

// TestReconcilingTheGrantedSetBothWays: a Library newly granted becomes a linked
// Library, and one revoked is TOMBSTONED IN PLACE — hidden, kept — never deleted.
// A delete would take this household's Watch state with it (ADR-0056 §6), and a
// re-grant would come back as a stranger. This asserts it comes back as itself.
func TestReconcilingTheGrantedSetBothWays(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	f.mustSync(t)

	libs, _ := f.mirror.LibrariesForLink(f.linkID)
	if len(libs) != 1 {
		t.Fatalf("the link brought %d libraries, want 1", len(libs))
	}
	films := libs[0].ID

	// She shares a second one.
	f.sharer.addLibrary("rl2", "Amy's music", "music", entity(store.ExportArtist, "a1", ""))
	f.mustSync(t)
	libs, _ = f.mirror.LibrariesForLink(f.linkID)
	if len(libs) != 2 {
		t.Fatalf("after a new grant the link has %d libraries, want 2", len(libs))
	}

	// And stops sharing the first.
	f.sharer.grant("rl1", false)
	f.mustSync(t)
	libs, _ = f.mirror.LibrariesForLink(f.linkID)
	if len(libs) != 2 {
		t.Fatalf("a revoked grant removed a shelf (%d left, want 2); it must be hidden, not deleted", len(libs))
	}
	if len(f.mirror.tombstoned) != 1 || f.mirror.tombstoned[0] != films {
		t.Fatalf("tombstoned %v, want exactly the library she stopped sharing (%s)", f.mirror.tombstoned, films)
	}

	// She shares it again: the same shelf, and a FULL pull, because the tombstone
	// cleared the checkpoint and every unchanged Title has to be un-hidden.
	f.sharer.grant("rl1", true)
	f.mustSync(t)
	libs, _ = f.mirror.LibrariesForLink(f.linkID)
	if len(libs) != 2 {
		t.Fatalf("a re-grant made a second shelf (%d), want the original 2", len(libs))
	}
	calls := f.mirror.calls()
	var restored *applyCall
	for i := range calls {
		if calls[i].libraryID == films {
			restored = &calls[i]
		}
	}
	if restored == nil || !restored.full {
		t.Errorf("the pull after a re-grant was %+v, want a full one", restored)
	}
}

// --- the goroutine, its timer and its nudge --------------------------------------

// fakeClock is the Syncer's whole sense of time. Every wait it asks for is
// recorded and hands back a channel this test fires, so the timer and the backoff
// are exercised without a millisecond of real waiting.
type fakeClock struct {
	mu    sync.Mutex
	asked []time.Duration
	waits []chan time.Time
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, d)
	c.waits = append(c.waits, ch)
	return ch
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

func (c *fakeClock) durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.asked...)
}

// fire releases the nth wait (0-based), which is how a test says "the timer came
// round".
func (c *fakeClock) fire(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < len(c.waits) {
		select {
		case c.waits[n] <- time.Now():
		default:
		}
	}
}

// sweepLog records every sweep a Syncer runs, so a test waits on the work rather
// than on the clock.
type sweepLog struct {
	mu   sync.Mutex
	errs []error
}

func (s *sweepLog) record(_ string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}

func (s *sweepLog) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.errs)
}

// blockingSubscribe is a subscription that never yields anything and returns when
// the worker is cancelled — the shape of a healthy stream, for the tests that are
// not about the stream.
func blockingSubscribe(ctx context.Context, _ store.Link, _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestTheTimerSweepsAgain: with no nudge and nobody pressing anything, the mirror
// is still pulled every interval. This is the poll fallback the whole SSE
// optimisation is allowed to be an optimisation OVER.
func TestTheTimerSweepsAgain(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))

	clock := &fakeClock{}
	log := &sweepLog{}
	sy := NewSyncer(f.svc, SyncerOptions{
		Interval:  17 * time.Minute,
		After:     clock.After,
		Subscribe: blockingSubscribe,
		OnSweep:   log.record,
	})
	if err := sy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Stop)

	waitFor(t, "the first sweep", func() bool { return log.count() >= 1 })
	waitFor(t, "the interval to be waited on", func() bool { return clock.pending() >= 1 })
	if got := clock.durations()[0]; got != 17*time.Minute {
		t.Fatalf("waited %v between sweeps, want the configured interval", got)
	}

	f.sharer.append("rl1", entity(store.ExportTitle, "t2", ""))
	clock.fire(0)

	waitFor(t, "the timer's sweep", func() bool { return log.count() >= 2 })
	waitFor(t, "the new title to land", func() bool {
		calls := f.mirror.calls()
		last := calls[len(calls)-1]
		return len(last.entities) == 1 && last.entities[0].RemoteID == "t2"
	})
}

// TestAnUnreachableLinkBacksOff: a friend's server that is off must not be
// hammered, and one that reboots must be picked up without an operator. The
// schedule is ADR-0056 §6's — a minute, doubling, capped.
func TestAnUnreachableLinkBacksOff(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.answer(http.StatusInternalServerError)

	clock := &fakeClock{}
	log := &sweepLog{}
	sy := NewSyncer(f.svc, SyncerOptions{
		Interval:  time.Hour,
		After:     clock.After,
		Subscribe: blockingSubscribe,
		OnSweep:   log.record,
	})
	if err := sy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Stop)

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, d := range want {
		waitFor(t, "sweep "+strconv.Itoa(i+1), func() bool { return log.count() >= i+1 })
		waitFor(t, "backoff "+strconv.Itoa(i+1), func() bool { return clock.pending() >= i+1 })
		if got := clock.durations()[i]; got != d {
			t.Fatalf("backoff %d was %v, want %v (the whole schedule: %v)", i+1, got, d, clock.durations())
		}
		clock.fire(i)
	}
	if st := f.link(t).State; st != store.LinkStateUnreachable {
		t.Errorf("state = %q while the sharer is down, want unreachable", st)
	}
}

// TestTheSharersNudgeSchedulesAPull is the whole reason a mirror feels live: a
// Title added on the sharer is here within seconds, over the /events subscription
// this Server holds open under the Link's token, an hour before the timer would
// have noticed.
func TestTheSharersNudgeSchedulesAPull(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))

	log := &sweepLog{}
	sy := NewSyncer(f.svc, SyncerOptions{
		// An interval long enough that nothing but the nudge can cause the second
		// sweep this test waits for.
		Interval: time.Hour,
		Debounce: time.Millisecond,
		OnSweep:  log.record,
	})
	if err := sy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Stop)

	waitFor(t, "the first sweep", func() bool { return log.count() >= 1 })
	waitFor(t, "the event subscription", func() bool { return containsPath(f.sharer.paths(), "/events") })

	f.sharer.append("rl1", entity(store.ExportTitle, "t2", ""))
	f.sharer.nudged("rl1")

	waitFor(t, "the nudged sweep", func() bool { return log.count() >= 2 })
	waitFor(t, "the new title to land", func() bool {
		calls := f.mirror.calls()
		last := calls[len(calls)-1]
		return len(last.entities) == 1 && last.entities[0].RemoteID == "t2"
	})
}

// TestEveryLinkStartsUnreachable: a Server that boots without a network says so.
// The alternative is showing a `connected` inherited from last week and only
// discovering the truth when somebody presses play.
//
// The sharer is held at the door so the state right after boot is observable
// without racing the first sweep.
func TestEveryLinkStartsUnreachable(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	gate := make(chan struct{})
	f.sharer.mu.Lock()
	f.sharer.gate = gate
	f.sharer.mu.Unlock()

	if st := f.link(t).State; st != store.LinkStateConnected {
		t.Fatalf("the fixture starts at %q, want connected — otherwise this proves nothing", st)
	}

	log := &sweepLog{}
	sy := NewSyncer(f.svc, SyncerOptions{Interval: time.Hour, Subscribe: blockingSubscribe, OnSweep: log.record})
	if err := sy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Stop)

	if st := f.link(t).State; st != store.LinkStateUnreachable {
		t.Fatalf("state at boot = %q, want unreachable until a call succeeds", st)
	}

	close(gate)
	waitFor(t, "the first sweep", func() bool { return log.count() >= 1 })
	waitFor(t, "the link to come up", func() bool { return f.link(t).State == store.LinkStateConnected })
}

// TestSyncNowForcesASweepThroughTheWorker is the "try again now" the admin page
// offers. It runs one sweep — the worker's, not a second one racing it — and it is
// the ONE thing that dials a revoked Link, because a human asking is not a retry.
func TestSyncNowForcesASweepThroughTheWorker(t *testing.T) {
	f := newSyncFixture(t)
	f.sharer.addLibrary("rl1", "Amy's films", "movie", entity(store.ExportTitle, "t1", ""))
	f.sharer.answer(http.StatusUnauthorized)

	log := &sweepLog{}
	sy := NewSyncer(f.svc, SyncerOptions{Interval: time.Hour, Subscribe: blockingSubscribe, OnSweep: log.record})
	if err := sy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Stop)

	waitFor(t, "the first sweep", func() bool { return log.count() >= 1 })
	waitFor(t, "the link to go revoked", func() bool { return f.link(t).State == store.LinkStateRevoked })

	// They restore the remote User and the operator presses "sync now".
	f.sharer.answer(0)
	l, err := f.svc.SyncNow(context.Background(), f.linkID)
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if l.State != store.LinkStateConnected {
		t.Fatalf("after a forced sweep state = %q, want connected", l.State)
	}
	if len(f.mirror.calls()) == 0 {
		t.Error("the forced sweep applied nothing; it should have pulled the library")
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}
