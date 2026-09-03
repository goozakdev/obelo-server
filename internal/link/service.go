package link

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Store is the persistence a Link needs. *store.DB satisfies it; the narrow
// interface keeps this package testable without a live database and keeps the
// set of tables linking may touch visible in one place.
type Store interface {
	Links() ([]store.Link, error)
	LinkByID(id string) (store.Link, error)
	LinkByServerID(serverID string) (store.Link, error)
	InsertLink(l store.Link) error
	UpdateLinkCredential(l store.Link) error
	DeleteLink(id string) error
}

// Identity is this Server's own id and name (ADR-0034), presented to the sharer
// as the Device (ADR-0055 §4). It is a function rather than a value because the
// name is freely changeable and a re-key months later should present the current
// one.
type Identity struct {
	ID   string
	Name string
}

// ErrUnreachable is every origin in the invite failing to answer. It is the
// state ADR-0056 §6 calls `unreachable` arriving at link time, and it is a 503
// on the wire rather than a 4xx: nothing about the request was wrong.
var ErrUnreachable = errors.New("link: none of the addresses in this invite answered")

// ErrServerMismatch is a re-key whose invite names a different Server than the
// Link being re-keyed. A Link is bound to one peer for its whole life — that is
// what makes the mirror's ids meaningful — so the answer is "this invite belongs
// to a different server", not a silent repoint.
var ErrServerMismatch = errors.New("link: this invite is for a different server")

// Service is the home side of linking. One per Server.
type Service struct {
	store  Store
	self   func() Identity
	dialer *Dialer

	// version is the link-protocol version this build speaks. Injected rather than
	// read from the server package at each use so a test can put the two sides on
	// different versions without rebuilding either.
	version int
	now     func() time.Time
	newID   func() string
	timeout time.Duration

	// OnLinked and OnUnlinked are the SEAM FOR ISSUES 07 AND 08, and they are
	// no-ops today.
	//
	// ADR-0056 says a Link's granted libraries become linked Library rows here and
	// that the first sync follows immediately. Neither the `linked` Library source
	// nor the mirror exists yet, so rather than guess at their shape this package
	// announces the two moments they will need — a Link came into being, a Link
	// went away — and builds nothing behind them. A Link with no Libraries is the
	// correct state of this server until issue 07 lands, not a half-built one, and
	// GET /links honestly reports an empty `libraries` list for it.
	//
	// Both run INSIDE the request that caused them and must not block on a network:
	// issue 08 owns the timer and the backoff, and the first sync belongs on its
	// queue, not on the Admin's paste.
	OnLinked   func(l store.Link)
	OnUnlinked func(l store.Link)
}

// Options are Service's injectable seams. Every field is optional; the zero
// value is what production uses.
type Options struct {
	// Version overrides the link-protocol version this Server claims. Zero uses
	// server.LinkProtocolVersion, which is what every deployment does — this exists
	// so a test can stand up two Servers that disagree.
	Version int
	// Now, NewID and Timeout are the clock, the id source and the per-call
	// deadline. Zero values use the real ones.
	Now     func() time.Time
	NewID   func() string
	Timeout time.Duration
	// Tailnet is the node the dialer may reach a peer over (ADR-0055 §5). Nil is a
	// deployment with no Tailnet, where every origin goes to the operating system.
	Tailnet TailnetNode
}

// New wires the Service.
func New(s Store, self func() Identity, opts Options) *Service {
	svc := &Service{
		store:   s,
		self:    self,
		dialer:  &Dialer{Node: opts.Tailnet},
		version: opts.Version,
		now:     opts.Now,
		newID:   opts.NewID,
		timeout: opts.Timeout,
	}
	if svc.version == 0 {
		svc.version = server.LinkProtocolVersion
	}
	if svc.now == nil {
		svc.now = time.Now
	}
	if svc.newID == nil {
		svc.newID = func() string { return uuid.NewString() }
	}
	return svc
}

// Version is the link-protocol version this Server speaks. The API layer reads
// it to fill in the "ours" side of a mismatch.
func (s *Service) Version() int { return s.version }

// List returns every Link, oldest first.
func (s *Service) List() ([]store.Link, error) { return s.store.Links() }

// Get returns one Link, store.ErrNotFound when there is none.
func (s *Service) Get(id string) (store.Link, error) { return s.store.LinkByID(id) }

// Create links this Server to another from a pasted invite string (POST /links).
//
// Same server id already on file is a RE-KEY (ADR-0055 §2) rather than an error
// or a second Link: the id in the invite is precisely what makes "this is the
// same friend, at a new address, with a fresh code" expressible. rekeyed reports
// which happened, so the transport can answer 200 against 201.
func (s *Service) Create(ctx context.Context, invite string) (l store.Link, rekeyed bool, err error) {
	inv, err := s.parse(invite)
	if err != nil {
		return store.Link{}, false, err
	}

	existing, err := s.store.LinkByServerID(inv.ServerID)
	switch {
	case err == nil:
		l, err := s.establish(ctx, existing.ID, inv)
		return l, true, err
	case errors.Is(err, store.ErrNotFound):
		l, err := s.establish(ctx, "", inv)
		return l, false, err
	default:
		return store.Link{}, false, err
	}
}

// Rekey replaces the credential on an existing Link (POST /links/{id}/rekey) —
// the move the Linked servers page offers when a Link has gone `revoked`.
//
// It differs from Create in one way and it is the point of the endpoint: the
// invite MUST name the Link being re-keyed. Create is addressed by the invite
// and finds its own row; this is addressed by a row and refuses an invite that
// does not match it, so an operator with two friends' invites in a clipboard
// cannot repoint one Link at the other household.
func (s *Service) Rekey(ctx context.Context, id, invite string) (store.Link, error) {
	existing, err := s.store.LinkByID(id)
	if err != nil {
		return store.Link{}, err
	}
	inv, err := s.parse(invite)
	if err != nil {
		return store.Link{}, err
	}
	if inv.ServerID != existing.ServerID {
		return store.Link{}, ErrServerMismatch
	}
	return s.establish(ctx, existing.ID, inv)
}

// Unlink deletes a Link, handing the credential back first (ADR-0055,
// Consequences) so the sharer is left with no Device for this household.
//
// The surrender is BEST EFFORT and its failure is logged, never returned. A friend
// whose server is off, or who has already deleted the `remote` User, must not be
// able to keep this household linked to them: the local delete is the operation,
// and the courtesy call is a courtesy.
func (s *Service) Unlink(ctx context.Context, id string) error {
	l, err := s.store.LinkByID(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	if err := s.surrender(ctx, s.client(), l); err != nil {
		log.Printf("obelo: link: unlinking %q: the sharing server was not told (%v); "+
			"its device row for this server may linger until it is revoked there", l.ServerName, err)
	}

	if err := s.store.DeleteLink(l.ID); err != nil {
		return err
	}
	// Issue 07/08's seam: the linked Libraries, their mirrored rows and the Watch
	// state on them are deleted here once they exist. Nothing does today.
	if s.OnUnlinked != nil {
		s.OnUnlinked(l)
	}
	return nil
}

// parse decodes and time-checks an invite. The version stamped INSIDE the string
// is checked here too, before a single packet leaves: a mismatch discovered from
// the string alone is a mismatch that could never have spent the code, which is
// the strongest form of ADR-0055 §3's promise.
func (s *Service) parse(invite string) (Invite, error) {
	inv, err := ParseInvite(invite)
	if err != nil {
		return Invite{}, err
	}
	if inv.Expired(s.now()) {
		return Invite{}, ErrInviteExpired
	}
	if inv.Version != s.version {
		return Invite{}, &ProtocolMismatch{Theirs: inv.Version, Ours: s.version}
	}
	return inv, nil
}

// establish is the shared body of Create, Rekey and the re-key branch of Create:
// find an origin that answers as the right Server, spend the code there, and
// write the row. id is empty for a new Link and the existing row's id for a
// re-key.
func (s *Service) establish(ctx context.Context, id string, inv Invite) (store.Link, error) {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout()*time.Duration(len(inv.Origins)+1))
	defer cancel()

	client := s.client()
	origin, peer, err := s.reach(ctx, client, inv)
	if err != nil {
		return store.Link{}, err
	}

	res, err := s.redeem(ctx, client, origin, inv)
	if err != nil {
		return store.Link{}, err
	}

	name := peer.Name
	if name == "" {
		name = inv.ServerName
	}
	l := store.Link{
		ID:                  id,
		ServerID:            inv.ServerID,
		ServerName:          name,
		Origins:             inv.Origins,
		ActiveOrigin:        origin,
		Token:               res.Token,
		DeviceID:            res.DeviceID,
		LinkProtocolVersion: peer.LinkProtocolVersion,
		State:               store.LinkStateConnected,
		CreatedAt:           s.now().UTC().Format(time.RFC3339),
	}
	if id == "" {
		l.ID = s.newID()
		if err := s.store.InsertLink(l); err != nil {
			return store.Link{}, err
		}
	} else if err := s.store.UpdateLinkCredential(l); err != nil {
		return store.Link{}, err
	}

	// Issue 07/08's seam: the linked Libraries are created and the first sync is
	// kicked here. Nothing does today, and GET /links reports no libraries.
	if s.OnLinked != nil {
		s.OnLinked(l)
	}
	return l, nil
}

// reach walks the origins IN ORDER and stops at the first that answers as the
// Server the invite names (ADR-0055 §2).
//
// The two kinds of failure are kept apart deliberately. An origin that does not
// answer, or answers as somebody else, is that ORIGIN's problem and the walk
// continues — an invite carrying a tailnet name and a public one exists exactly
// so one of them can be wrong. A version mismatch is the SERVER's problem and
// stops the walk at once: every origin leads to the same machine, so trying the
// next one can only produce the same refusal a second time.
func (s *Service) reach(ctx context.Context, client *http.Client, inv Invite) (string, Peer, error) {
	var lastErr error
	for _, origin := range inv.Origins {
		peer, err := s.probe(ctx, client, origin)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", origin, err)
			continue
		}
		if peer.ID != inv.ServerID {
			lastErr = fmt.Errorf("%s: %w", origin, ErrWrongServer)
			continue
		}
		// A Server with no serverLinking flag speaks no version of this protocol at
		// all, which is version 0 for the purposes of the one sentence the operator
		// needs: their server needs an upgrade.
		if !peer.ServerLinking {
			return "", Peer{}, &ProtocolMismatch{Theirs: 0, Ours: s.version}
		}
		if peer.LinkProtocolVersion != s.version {
			return "", Peer{}, &ProtocolMismatch{Theirs: peer.LinkProtocolVersion, Ours: s.version}
		}
		return origin, peer, nil
	}
	if lastErr != nil {
		return "", Peer{}, fmt.Errorf("%w (%v)", ErrUnreachable, lastErr)
	}
	return "", Peer{}, ErrUnreachable
}

func (s *Service) client() *http.Client { return s.dialer.HTTPClient(s.callTimeout()) }

func (s *Service) callTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return defaultRequestTimeout
}

// OverTailnet reports which of the two dialers an origin would use. It exists
// for the admin surface and for tests; nothing in the link flow branches on it,
// because the dialer already did.
func (s *Service) OverTailnet(origin string) bool {
	return s.dialer.OverTailnet(originHost(origin))
}
