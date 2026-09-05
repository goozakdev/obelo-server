package link

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The flow around the two things a real pair of Servers cannot easily be made to
// do: DISAGREE about the protocol version, and be reachable at one address out of
// several. Both need a peer this test can misconfigure, so the peer here is a
// stub rather than a whole Server; the end-to-end proof that this flow works
// against the real sharing half lives in internal/api/link_test.go.

// stubPeer is a sharing Server as far as linking can see it: a handshake and a
// redemption. Everything it answers is settable, which is the point.
type stubPeer struct {
	id            string
	name          string
	serverLinking bool
	version       int
	// redeemStatus/redeemBody override the redemption when set, so a test can put
	// the version disagreement at the REDEEM rather than at the probe.
	redeemStatus int
	redeemBody   string

	mu       sync.Mutex
	probes   int
	redeems  int
	spent    bool
	lastBody map[string]any
}

func newStubPeer(t *testing.T, p *stubPeer) *httptest.Server {
	t.Helper()
	if p.version == 0 {
		p.version = 1
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/server":
			p.mu.Lock()
			p.probes++
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  p.id,
				"name":                p.name,
				"version":             "0.1.0",
				"supportedVersions":   []int{1},
				"features":            map[string]bool{"serverLinking": p.serverLinking},
				"setupRequired":       false,
				"linkProtocolVersion": p.version,
			})
		case "/api/v1/auth/link/redeem":
			p.mu.Lock()
			p.redeems++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.lastBody = body
			spent := p.spent
			p.spent = true
			p.mu.Unlock()

			if p.redeemStatus != 0 {
				w.WriteHeader(p.redeemStatus)
				_, _ = w.Write([]byte(p.redeemBody))
				return
			}
			if spent {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"INVALID_INVITE","message":"gone"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":               "obelo_deadbeefdeadbeefdeadbeefdeadbeef",
				"user":                map[string]any{"id": "u1", "username": "home", "role": "remote"},
				"device":              map[string]any{"id": "d1", "name": "Home", "platform": "server"},
				"linkProtocolVersion": p.version,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *stubPeer) counts() (probes, redeems int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes, p.redeems
}

// memStore is an in-memory Store, so the flow can be exercised without a
// database. It enforces the one invariant the real table's UNIQUE constraint
// does: at most one Link per peer.
type memStore struct {
	mu    sync.Mutex
	links []store.Link
}

func (m *memStore) Links() ([]store.Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Link(nil), m.links...), nil
}

func (m *memStore) LinkByID(id string) (store.Link, error) {
	return m.find(func(l store.Link) bool { return l.ID == id })
}

func (m *memStore) LinkByServerID(serverID string) (store.Link, error) {
	return m.find(func(l store.Link) bool { return l.ServerID == serverID })
}

func (m *memStore) find(match func(store.Link) bool) (store.Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		if match(l) {
			return l, nil
		}
	}
	return store.Link{}, store.ErrNotFound
}

func (m *memStore) InsertLink(l store.Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.links {
		if e.ServerID == l.ServerID {
			return errors.New("memstore: UNIQUE constraint failed: links.server_id")
		}
	}
	m.links = append(m.links, l)
	return nil
}

func (m *memStore) UpdateLinkCredential(l store.Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.links {
		if e.ID == l.ID {
			l.CreatedAt, l.ServerID, l.LastSyncedAt = e.CreatedAt, e.ServerID, e.LastSyncedAt
			l.State, l.LastError = store.LinkStateConnected, ""
			m.links[i] = l
			return nil
		}
	}
	return store.ErrNotFound
}

// The three state-machine writers (issue 08). They are the real table's
// semantics and not a simplification of them: a success clears the last error
// and stamps last_synced_at, a failure records the reason, and the active origin
// moves on its own.
func (m *memStore) SetLinkState(id, state, lastError string) error {
	return m.update(id, func(l *store.Link) { l.State, l.LastError = state, lastError })
}

func (m *memStore) SetLinkSynced(id, syncedAt string) error {
	return m.update(id, func(l *store.Link) {
		l.State, l.LastError, l.LastSyncedAt = store.LinkStateConnected, "", syncedAt
	})
}

func (m *memStore) SetLinkActiveOrigin(id, origin string) error {
	return m.update(id, func(l *store.Link) { l.ActiveOrigin = origin })
}

func (m *memStore) update(id string, apply func(*store.Link)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.links {
		if m.links[i].ID == id {
			apply(&m.links[i])
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memStore) DeleteLink(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.links {
		if e.ID == id {
			m.links = append(m.links[:i], m.links[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

// newService wires a Service over the in-memory store with a fixed identity.
func newService(t *testing.T, st Store, opts Options) *Service {
	t.Helper()
	if opts.NewID == nil {
		var n int
		opts.NewID = func() string { n++; return "link-" + string(rune('a'+n-1)) }
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	return New(st, func() Identity {
		return Identity{ID: "home-server-id", Name: "Brandon's server"}
	}, opts)
}

// inviteFor builds an invite naming the given origins, IN ORDER.
func inviteFor(t *testing.T, serverID string, version int, exp time.Time, origins ...string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"v": version, "id": serverID, "name": "Amy's server",
		"origins": origins, "code": "the-code", "exp": exp.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	return InviteScheme + base64.RawURLEncoding.EncodeToString(raw)
}

// TestOriginsAreTriedInOrderAndTheOneThatAnsweredIsRemembered is ADR-0055 §2's
// "the home Server tries them in order and remembers the one that answered".
// The dead address is FIRST, so a walk that reordered or parallelized would pass
// by accident.
func TestOriginsAreTriedInOrderAndTheOneThatAnsweredIsRemembered(t *testing.T) {
	peer := &stubPeer{id: "amy-server-id", name: "Amy's server", serverLinking: true}
	live := newStubPeer(t, peer)

	// A listener that is closed: connecting to it is refused at once, which is
	// what a friend's port-forward looks like when their router is off.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	st := &memStore{}
	svc := newService(t, st, Options{})
	l, rekeyed, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), deadURL, live.URL))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rekeyed {
		t.Error("a first link reported itself a re-key")
	}
	if l.ActiveOrigin != live.URL {
		t.Errorf("activeOrigin = %q, want the one that answered (%q)", l.ActiveOrigin, live.URL)
	}
	// Both addresses are kept, in the order the invite carried them: the dead one
	// is the friend's router coming back tomorrow, not a mistake to discard.
	if len(l.Origins) != 2 || l.Origins[0] != deadURL || l.Origins[1] != live.URL {
		t.Errorf("origins = %v, want both in the invite's order", l.Origins)
	}
	if l.State != store.LinkStateConnected {
		t.Errorf("state = %q, want connected", l.State)
	}
	if l.Token == "" || l.DeviceID != "d1" {
		t.Errorf("link did not record the credential: token empty=%t deviceID=%q", l.Token == "", l.DeviceID)
	}
}

// TestAnOriginThatIsADifferentServerIsSkipped: an invite's addresses are
// addresses to TRY, not to believe. A stale one now pointing at somebody else's
// machine must not become the Link, and must not stop the other one working.
func TestAnOriginThatIsADifferentServerIsSkipped(t *testing.T) {
	stranger := newStubPeer(t, &stubPeer{id: "somebody-else", serverLinking: true})
	right := newStubPeer(t, &stubPeer{id: "amy-server-id", name: "Amy's server", serverLinking: true})

	svc := newService(t, &memStore{}, Options{})
	l, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), stranger.URL, right.URL))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if l.ActiveOrigin != right.URL {
		t.Errorf("activeOrigin = %q, want the server the invite names", l.ActiveOrigin)
	}
}

// TestNoOriginAnswersIsUnreachable: nothing about the request was wrong, so this
// is not a 4xx-shaped failure — it is ADR-0056 §6's `unreachable` arriving at
// link time.
func TestNoOriginAnswersIsUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	svc := newService(t, &memStore{}, Options{})
	_, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), deadURL))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Create error = %v, want ErrUnreachable", err)
	}
	// The message has to carry what the last address actually said; "could not
	// link" leaves an operator with nothing to take to the other household.
	if !strings.Contains(err.Error(), deadURL) {
		t.Errorf("error %q names no address", err)
	}
}

// TestAVersionMismatchNeverSpendsTheCode is ADR-0055 §3's promise, in the three
// places the disagreement can be found. In every one of them the invite must
// still be redeemable once the two sides agree, which is what "refuses at link
// time" is worth.
func TestAVersionMismatchNeverSpendsTheCode(t *testing.T) {
	t.Run("the version stamped in the invite string", func(t *testing.T) {
		peer := &stubPeer{id: "amy-server-id", serverLinking: true, version: 1}
		live := newStubPeer(t, peer)
		svc := newService(t, &memStore{}, Options{Version: 1})

		_, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 7, time.Now().Add(time.Hour), live.URL))
		var mismatch *ProtocolMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("Create error = %v, want a ProtocolMismatch", err)
		}
		if mismatch.Theirs != 7 || mismatch.Ours != 1 || mismatch.Upgrade() != "ours" {
			t.Errorf("mismatch = %+v upgrade %q, want theirs 7 / ours 1 / ours", mismatch, mismatch.Upgrade())
		}
		// Not one packet left this Server: the string alone settled it.
		if probes, redeems := peer.counts(); probes != 0 || redeems != 0 {
			t.Errorf("the peer saw %d probes and %d redemptions; it should have seen none", probes, redeems)
		}
	})

	t.Run("the version the handshake reports", func(t *testing.T) {
		peer := &stubPeer{id: "amy-server-id", serverLinking: true, version: 2}
		live := newStubPeer(t, peer)
		svc := newService(t, &memStore{}, Options{Version: 1})

		_, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), live.URL))
		var mismatch *ProtocolMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("Create error = %v, want a ProtocolMismatch", err)
		}
		if mismatch.Theirs != 2 || mismatch.Ours != 1 || mismatch.Upgrade() != "ours" {
			t.Errorf("mismatch = %+v upgrade %q", mismatch, mismatch.Upgrade())
		}
		// THE POINT: the probe happened, the redemption did not. The invite is live.
		if probes, redeems := peer.counts(); probes != 1 || redeems != 0 {
			t.Errorf("the peer saw %d probes and %d redemptions; want 1 and 0 — the code must not be spent", probes, redeems)
		}
	})

	t.Run("a server that does not speak this protocol at all", func(t *testing.T) {
		// No serverLinking flag: it speaks version 0 of nothing, and the sentence
		// the operator needs is the same one — their server needs an upgrade.
		peer := &stubPeer{id: "amy-server-id", serverLinking: false, version: 1}
		live := newStubPeer(t, peer)
		svc := newService(t, &memStore{}, Options{Version: 1})

		_, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), live.URL))
		var mismatch *ProtocolMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("Create error = %v, want a ProtocolMismatch", err)
		}
		if mismatch.Theirs != 0 || mismatch.Upgrade() != "theirs" {
			t.Errorf("mismatch = %+v upgrade %q, want theirs 0 / theirs", mismatch, mismatch.Upgrade())
		}
		if _, redeems := peer.counts(); redeems != 0 {
			t.Errorf("the peer saw %d redemptions; it speaks no version of this protocol", redeems)
		}
	})

	t.Run("the sharer's own 409, if it gets that far", func(t *testing.T) {
		// Belt and braces: the two sides agreed at the handshake and disagreed at the
		// redemption (the sharer was upgraded in between). Its details are named from
		// ITS side — supported/requested — and must be read that way round.
		peer := &stubPeer{
			id: "amy-server-id", serverLinking: true, version: 1,
			redeemStatus: http.StatusConflict,
			redeemBody:   `{"error":{"code":"LINK_PROTOCOL","message":"no","details":{"supported":4,"requested":1}}}`,
		}
		live := newStubPeer(t, peer)
		svc := newService(t, &memStore{}, Options{Version: 1})

		_, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), live.URL))
		var mismatch *ProtocolMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("Create error = %v, want a ProtocolMismatch", err)
		}
		if mismatch.Theirs != 4 || mismatch.Ours != 1 {
			t.Errorf("mismatch = %+v, want theirs 4 / ours 1 (supported is THEIRS)", mismatch)
		}
	})
}

// TestASpentInviteIsRefusedAsSuchIsNotAMalformedString: the sharer's
// INVALID_INVITE means the string was fine and the CODE is gone, which is a
// different next move from "ask for the string again".
func TestASpentInviteIsRefused(t *testing.T) {
	peer := &stubPeer{id: "amy-server-id", serverLinking: true}
	live := newStubPeer(t, peer)
	svc := newService(t, &memStore{}, Options{})
	invite := inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), live.URL)

	if _, _, err := svc.Create(context.Background(), invite); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// The stub spends the code exactly once, like the real compare-and-swap does.
	_, err := svc.Rekey(context.Background(), "link-a", invite)
	if !errors.Is(err, ErrInviteRefused) {
		t.Fatalf("second use error = %v, want ErrInviteRefused", err)
	}
	if errors.Is(err, ErrBadInvite) {
		t.Error("a spent code must not read as a malformed string")
	}
}

// TestASecondInviteFromTheSameServerRekeysInPlace is ADR-0055 §2's reason for
// putting the server id in the string: a friend at a new address with a fresh
// code is the SAME Link, and the mirror behind it survives.
func TestASecondInviteFromTheSameServerRekeysInPlace(t *testing.T) {
	first := newStubPeer(t, &stubPeer{id: "amy-server-id", name: "Amy's server", serverLinking: true})
	moved := newStubPeer(t, &stubPeer{id: "amy-server-id", name: "Amy's new server", serverLinking: true})

	st := &memStore{}
	var linked []store.Link
	svc := newService(t, st, Options{})
	svc.OnLinked = func(l store.Link) { linked = append(linked, l) }

	before, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), first.URL))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	after, rekeyed, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), moved.URL))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if !rekeyed {
		t.Error("a second invite from the same server did not report itself a re-key")
	}
	if after.ID != before.ID {
		t.Errorf("link id changed on re-key: %q → %q; the mirror behind it would be orphaned", before.ID, after.ID)
	}
	if after.CreatedAt != before.CreatedAt {
		t.Errorf("createdAt moved on re-key: %q → %q; it is the same relationship", before.CreatedAt, after.CreatedAt)
	}
	if after.ActiveOrigin != moved.URL {
		t.Errorf("activeOrigin = %q, want the new address", after.ActiveOrigin)
	}
	// The peer's CURRENT name, read from its handshake rather than from the
	// invite's copy of it: renaming a Server is cosmetic and must reach here.
	if after.ServerName != "Amy's new server" {
		t.Errorf("serverName = %q, want the name the handshake reports", after.ServerName)
	}

	links, _ := st.Links()
	if len(links) != 1 {
		t.Fatalf("%d links on file, want exactly 1 — a re-key must not fork the relationship", len(links))
	}
	// Issue 07/08's seam fires on both: a re-key is a moment the mirror has to
	// hear about too (its addresses moved).
	if len(linked) != 2 {
		t.Errorf("OnLinked fired %d times, want 2 (the link and the re-key)", len(linked))
	}
}

// TestRekeyRefusesAnInviteForADifferentServer: a Link is bound to one peer for
// its whole life, because the mirror behind it is keyed by that peer's ids.
func TestRekeyRefusesAnInviteForADifferentServer(t *testing.T) {
	amy := newStubPeer(t, &stubPeer{id: "amy-server-id", serverLinking: true})
	bob := newStubPeer(t, &stubPeer{id: "bob-server-id", serverLinking: true})

	st := &memStore{}
	svc := newService(t, st, Options{})
	l, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), amy.URL))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Rekey(context.Background(), l.ID,
		inviteFor(t, "bob-server-id", 1, time.Now().Add(time.Hour), bob.URL))
	if !errors.Is(err, ErrServerMismatch) {
		t.Fatalf("Rekey error = %v, want ErrServerMismatch", err)
	}
	current, _ := st.LinkByID(l.ID)
	if current.ServerID != "amy-server-id" || current.ActiveOrigin != amy.URL {
		t.Errorf("the link was repointed anyway: %+v", current)
	}
}

// TestAnExpiredInviteNeverDials: the clock is checked before the network, so a
// friend's day-old string costs no round trip and produces the sentence about
// the expiry rather than a generic refusal from over there.
func TestAnExpiredInviteNeverDials(t *testing.T) {
	peer := &stubPeer{id: "amy-server-id", serverLinking: true}
	live := newStubPeer(t, peer)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	svc := newService(t, &memStore{}, Options{Now: func() time.Time { return now }})

	_, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, now.Add(-time.Minute), live.URL))
	if !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("Create error = %v, want ErrInviteExpired", err)
	}
	if probes, _ := peer.counts(); probes != 0 {
		t.Errorf("the peer saw %d probes; an expired invite must not dial", probes)
	}
}

// TestUnlinkSucceedsWhetherOrNotTheSharerIsThere. Both halves matter and the
// second is the one that would be got wrong: a friend whose server is off must
// not be able to keep this household linked to them.
func TestUnlinkSucceedsWhetherOrNotTheSharerIsThere(t *testing.T) {
	t.Run("reachable: the device is deleted over there", func(t *testing.T) {
		var deleted []string
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/server":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "amy-server-id", "features": map[string]bool{"serverLinking": true},
					"linkProtocolVersion": 1,
				})
			case r.URL.Path == "/api/v1/auth/link/redeem":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":  "obelo_tok",
					"device": map[string]any{"id": "device-42"},
				})
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/devices/"):
				if r.Header.Get("Authorization") != "Bearer obelo_tok" {
					t.Errorf("device delete carried %q", r.Header.Get("Authorization"))
				}
				deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/devices/"))
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}))
		defer peer.Close()

		st := &memStore{}
		var unlinked []store.Link
		svc := newService(t, st, Options{})
		svc.OnUnlinked = func(l store.Link) { unlinked = append(unlinked, l) }

		l, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), peer.URL))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := svc.Unlink(context.Background(), l.ID); err != nil {
			t.Fatalf("Unlink: %v", err)
		}
		// The DEVICE, not just the token: a logout would leave the row behind, still
		// listed on the sharer's Users page with the date we last called.
		if len(deleted) != 1 || deleted[0] != "device-42" {
			t.Errorf("devices deleted on the sharer = %v, want [device-42]", deleted)
		}
		if links, _ := st.Links(); len(links) != 0 {
			t.Errorf("%d links remain after unlinking", len(links))
		}
		if len(unlinked) != 1 {
			t.Errorf("OnUnlinked fired %d times, want 1", len(unlinked))
		}
	})

	t.Run("unreachable: it still succeeds locally", func(t *testing.T) {
		peer := newStubPeer(t, &stubPeer{id: "amy-server-id", serverLinking: true})
		st := &memStore{}
		svc := newService(t, st, Options{Timeout: 2 * time.Second})
		l, _, err := svc.Create(context.Background(),
			inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), peer.URL))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// The friend's server goes away between linking and unlinking, which is the
		// common case: the reason to unlink is often that it is not coming back.
		peer.Close()

		if err := svc.Unlink(context.Background(), l.ID); err != nil {
			t.Fatalf("Unlink against an unreachable sharer = %v, want success", err)
		}
		if links, _ := st.Links(); len(links) != 0 {
			t.Errorf("%d links remain; an unreachable friend must not be able to prevent unlinking", len(links))
		}
	})
}

// TestTheRedemptionPresentsThisServersIdentity is ADR-0055 §4: the sharer's
// Users page shows "Brandon's server" with a real last-seen because the redeem
// carried this Server's id as the Device clientId.
func TestTheRedemptionPresentsThisServersIdentity(t *testing.T) {
	peer := &stubPeer{id: "amy-server-id", serverLinking: true}
	live := newStubPeer(t, peer)
	svc := newService(t, &memStore{}, Options{})
	if _, _, err := svc.Create(context.Background(),
		inviteFor(t, "amy-server-id", 1, time.Now().Add(time.Hour), live.URL)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	peer.mu.Lock()
	body := peer.lastBody
	peer.mu.Unlock()

	self, _ := body["server"].(map[string]any)
	if self["id"] != "home-server-id" || self["name"] != "Brandon's server" {
		t.Errorf("redeem presented %v, want this Server's own identity", self)
	}
	if body["code"] != "the-code" {
		t.Errorf("redeem sent code %v", body["code"])
	}
	if v, _ := body["linkProtocolVersion"].(float64); int(v) != 1 {
		t.Errorf("redeem stamped version %v, want 1", body["linkProtocolVersion"])
	}
}
