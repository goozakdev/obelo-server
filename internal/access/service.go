// Package access resolves and represents a User's authorization scope over the
// catalog: which Libraries they may browse/play and the maturity ceiling on the
// Titles they may see (CONTEXT.md "Rating ceiling", "Member"). It is the single
// seam the browse/read/play surface consults so a Member sees only what they are
// entitled to — a Title outside a User's access is hidden as 404, never 403
// (api-contract.md "404, not 403").
//
// This slice is a prefactor: Resolve returns an all-access Scope for EVERY User,
// so behavior is unchanged. The library-grant and Rating-ceiling dimensions are
// threaded through the call graph but inert; the enforcing slices fill in what
// Resolve returns (real grants + ceiling) and what the guards/predicate compare,
// without moving where the Scope is threaded.
package access

import (
	"errors"

	"github.com/goozakdev/obelo-server/internal/store"
)

// roleAdmin is the Admin role string (mirrors the value the auth/store layers
// use); an Admin always resolves to an all-access Scope. roleRemote is the role
// a linked Server holds here (ADR-0054 §1): it resolves a Scope exactly like a
// Member, minus any Library that itself arrived over a Link (§4 — sharing does
// not travel).
const (
	roleAdmin  = "admin"
	roleRemote = "remote"
)

// Errors the api layer maps onto HTTP envelopes for the grant-management surface.
var (
	// ErrUserNotFound: the target User of a grant op does not exist (→ 404).
	ErrUserNotFound = errors.New("access: user not found")
	// ErrAdminGrant: library access cannot be granted to an Admin — an Admin is
	// implicitly all-access (→ 422).
	ErrAdminGrant = errors.New("access: cannot grant libraries to an admin")
	// ErrUnknownLibrary: a library id in the grant set does not exist; the whole
	// replace-set is rejected and the prior set is left unchanged (→ 422).
	ErrUnknownLibrary = errors.New("access: unknown library in grant set")
	// ErrLinkedGrant: the target is a `remote` User (a linked Server) and the
	// grant set names a Library that itself arrived over a Link. A mirror is
	// never re-shared — the owner of the files decided who sees them, and one hop
	// later that decision would be made by somebody they never met (ADR-0054 §4,
	// ADR-0056 §7). The whole set is rejected and the prior set is left unchanged,
	// exactly as for an unknown id (→ 422).
	ErrLinkedGrant = errors.New("access: cannot grant a linked library to a remote user")
	// ErrAdminCeiling: a Rating ceiling cannot be set on an Admin — Admins are
	// all-access and the ceiling is never consulted for them (→ 422).
	ErrAdminCeiling = errors.New("access: cannot set a rating ceiling on an admin")
	// ErrUnknownRating: the requested ceiling is not a known rating label (→ 422).
	ErrUnknownRating = errors.New("access: unknown rating ceiling label")
	// ErrUnknownResolution: the requested Playback ceiling names a resolution rung
	// that is not settable (→ 422). The settable set is playbackResolutions.
	ErrUnknownResolution = errors.New("access: unknown playback ceiling resolution")
	// ErrInvalidCeiling: a Playback-ceiling dimension is negative — neither a cap
	// nor the zero value that means uncapped (→ 400).
	ErrInvalidCeiling = errors.New("access: playback ceiling must not be negative")
)

// Store is the persistence the resolver reads. *store.DB satisfies it. It is a
// narrow interface so the resolver stays unit-testable and the seam explicit.
type Store interface {
	UserByID(id string) (store.User, error)
	// LibraryAccessForUser returns the Library ids granted to a User (empty = none).
	LibraryAccessForUser(userID string) ([]string, error)
	// ReplaceLibraryAccess sets a User's grant set to exactly libraryIDs,
	// atomically; an unknown library id yields store.ErrNotFound (prior set kept).
	ReplaceLibraryAccess(userID string, libraryIDs []string) error
	// RatingCeilingForUser returns a User's stored ceiling label ("" = uncapped).
	RatingCeilingForUser(userID string) (string, error)
	// SetRatingCeiling stores a User's ceiling label ("" clears it to uncapped);
	// store.ErrNotFound for an unknown User.
	SetRatingCeiling(userID, label string) error
	// PlaybackCeilingForUser returns a User's Playback ceiling; a zero field means
	// uncapped in that dimension. store.ErrNotFound for an unknown User.
	PlaybackCeilingForUser(userID string) (store.PlaybackCeiling, error)
	// SetPlaybackCeiling stores a User's whole Playback ceiling (a zero field
	// clears that dimension); store.ErrNotFound for an unknown User.
	SetPlaybackCeiling(userID string, c store.PlaybackCeiling) error
	// LinkedLibraryIDs returns the ids of every Library that is a mirror of
	// another household's (ADR-0056 §1); empty on a Server that has never linked.
	LinkedLibraryIDs() ([]string, error)
}

// Scope is a User's resolved access over the catalog (the PRD's "AccessScope").
// It is resolved once per request from the authenticated identity and threaded
// into the catalog/playback domain calls. The zero value grants nothing
// (fail-closed): callers always pass a Scope produced by Resolve.
type Scope struct {
	// IsAdmin is true for an Admin — carried so callers can branch on role.
	IsAdmin bool
	// AllLibraries is true when the User may see every Library (an Admin, or the
	// inert prefactor default for everyone). When true, LibraryIDs is ignored.
	AllLibraries bool
	// LibraryIDs is the visible Library set when !AllLibraries (empty = none).
	LibraryIDs []string
	// RatingCeiling is the maximum allowed maturity rank; 0 means uncapped. The
	// rating dimension is applied by a later slice; it is carried here so the seam
	// is complete.
	RatingCeiling int
	// MaxResolution, MaxBitrate and MaxStreams are the User's Playback ceiling
	// (CONTEXT.md, ADR-0054 §2): how a Title may play for this User, as opposed to
	// the Rating ceiling's what they may see. They ride on the Scope for the same
	// reason the grants and the Rating ceiling do — requireScope resolves them once
	// per request and threads them into playback, which clamps the first two into
	// the session's Constraints and enforces the third at session creation.
	//
	// A ceiling NEVER hides a Title: nothing in the browse/read surface reads these
	// fields, and negotiation answers with a cheaper tier rather than a 404.
	//
	// MaxResolution is a resolution token ("1080p"), "" = uncapped; MaxBitrate is
	// bits/sec, 0 = uncapped; MaxStreams is concurrent Playback sessions,
	// 0 = uncapped. An Admin resolves to AllAccess and carries none of them.
	MaxResolution string
	MaxBitrate    int64
	MaxStreams    int
}

// AllowsLibrary reports whether the Scope may see the given Library. An
// all-access Scope allows every Library (the prefactor default and any Admin).
func (s Scope) AllowsLibrary(libraryID string) bool {
	if s.AllLibraries {
		return true
	}
	for _, id := range s.LibraryIDs {
		if id == libraryID {
			return true
		}
	}
	return false
}

// StoreFilter projects the Scope onto the persistence-side predicate the
// cross-library aggregate reads (the Home rows, search) apply in SQL. The store
// package owns AccessFilter so it never imports this package (avoiding a cycle);
// this is the one conversion point.
func (s Scope) StoreFilter() store.AccessFilter {
	return store.AccessFilter{
		AllLibraries:   s.AllLibraries,
		LibraryIDs:     s.LibraryIDs,
		BlockedRatings: s.blockedRatings(),
	}
}

// AllAccess returns an unrestricted Scope (every Library, uncapped). It is the
// Scope any Admin resolves to, and what Admin-only handlers pass when they read a
// Title to return its detail — they act regardless of browse visibility, having
// already passed the Admin gate.
func AllAccess() Scope { return Scope{IsAdmin: true, AllLibraries: true} }

// Service resolves a User's Scope. Constructed once and shared.
type Service struct {
	store Store
}

// NewService builds the access resolver over the given store.
func NewService(s Store) *Service { return &Service{store: s} }

// Resolve returns the access Scope for the User with the given id. An Admin
// resolves to all Libraries (by role — no grant rows needed, so a Library added
// later is implicitly theirs). A Member resolves to exactly their granted
// Library set: an empty set means they see no catalog. The Rating-ceiling
// dimension is still uncapped here (a later slice fills it in); this method is
// the single place per-User access is computed.
func (s *Service) Resolve(userID string) (Scope, error) {
	u, err := s.store.UserByID(userID)
	if err != nil {
		return Scope{}, err
	}
	if u.Role == roleAdmin {
		return Scope{IsAdmin: true, AllLibraries: true}, nil
	}
	libs, err := s.store.LibraryAccessForUser(userID)
	if err != nil {
		return Scope{}, err
	}
	// The one-hop invariant, enforced where the Scope is COMPUTED and not only
	// where a grant is written (ADR-0054 §4, ADR-0056 §7). A grant row naming a
	// linked Library can only reach a `remote` User behind the API — a restore of
	// an older database, a hand-edit, a Library that became a mirror after it was
	// granted — and this is the last place before the answer is used, so a row
	// that should not exist buys nothing. A failed read fails the whole Resolve
	// (fail closed): the alternative is silently re-sharing somebody else's files.
	if u.Role == roleRemote && len(libs) > 0 {
		libs, err = s.localLibrariesOnly(libs)
		if err != nil {
			return Scope{}, err
		}
	}
	ceiling, err := s.store.RatingCeilingForUser(userID)
	if err != nil {
		return Scope{}, err
	}
	play, err := s.store.PlaybackCeilingForUser(userID)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		IsAdmin:       false,
		AllLibraries:  false,
		LibraryIDs:    libs,
		RatingCeiling: ceilingRank(ceiling),
		MaxResolution: play.MaxResolution,
		MaxBitrate:    play.MaxBitrate,
		MaxStreams:    play.MaxStreams,
	}, nil
}

// localLibrariesOnly drops any Library that arrived over a Link from ids,
// preserving order. It is the one place the "a mirror is never re-shared" filter
// is spelled, shared by Resolve and SetLibraryAccess.
func (s *Service) localLibrariesOnly(ids []string) ([]string, error) {
	linked, err := s.linkedSet()
	if err != nil {
		return nil, err
	}
	if len(linked) == 0 {
		return ids, nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !linked[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// linkedSet reads the mirrored Library ids as a set (nil when nothing here came
// over a Link, which is every Server until one is linked).
func (s *Service) linkedSet() (map[string]bool, error) {
	ids, err := s.store.LinkedLibraryIDs()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// RatingCeiling returns a User's stored ceiling label ("" = uncapped), for the
// Admin user-management view.
func (s *Service) RatingCeiling(userID string) (string, error) {
	return s.store.RatingCeilingForUser(userID)
}

// SetRatingCeiling sets (or, with label "", clears) a Member's Rating ceiling. It
// rejects an unknown User (ErrUserNotFound), an Admin target (ErrAdminCeiling —
// Admins are all-access), and a label that is not on the maturity ladder
// (ErrUnknownRating).
func (s *Service) SetRatingCeiling(userID, label string) error {
	u, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if u.Role == roleAdmin {
		return ErrAdminCeiling
	}
	if label != "" && !isLadderLabel(label) {
		return ErrUnknownRating
	}
	if err := s.store.SetRatingCeiling(userID, label); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

// PlaybackCeiling returns a User's stored Playback ceiling (zero fields =
// uncapped), for the Admin user-management view.
func (s *Service) PlaybackCeiling(userID string) (store.PlaybackCeiling, error) {
	return s.store.PlaybackCeilingForUser(userID)
}

// SetPlaybackCeiling sets a non-Admin User's whole Playback ceiling (ADR-0054
// §2). It is a REPLACE of all three dimensions, not a patch: a dimension left at
// its zero value is cleared to uncapped, so the caller always sends the ceiling
// it means (the grant set works the same way).
//
// It rejects an unknown User (ErrUserNotFound), an Admin target (ErrAdminCeiling
// — an Admin is all-access and uncapped, exactly as for the Rating ceiling), a
// resolution rung that is not settable (ErrUnknownResolution), and a negative
// bitrate/stream count (ErrInvalidCeiling — 0 already means uncapped, so a
// negative says nothing). A settable rung is stored canonically ("1080P" →
// "1080p") so the negotiator's clamp never has to fold case.
func (s *Service) SetPlaybackCeiling(userID string, c store.PlaybackCeiling) error {
	u, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if u.Role == roleAdmin {
		return ErrAdminCeiling
	}
	if c.MaxResolution != "" {
		canon, ok := canonicalResolution(c.MaxResolution)
		if !ok {
			return ErrUnknownResolution
		}
		c.MaxResolution = canon
	}
	if c.MaxBitrate < 0 || c.MaxStreams < 0 {
		return ErrInvalidCeiling
	}
	if err := s.store.SetPlaybackCeiling(userID, c); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

// LibraryAccess returns the Library ids granted to a User (for the Admin
// user-management view). An Admin has no grant rows (they are all-access by
// role), so this is empty for an Admin — the caller reads the role to know that
// means "all Libraries", not "none".
func (s *Service) LibraryAccess(userID string) ([]string, error) {
	return s.store.LibraryAccessForUser(userID)
}

// SetLibraryAccess replaces a Member's grant set with exactly libraryIDs. It
// rejects an unknown User (ErrUserNotFound), an Admin target (ErrAdminGrant —
// Admins are implicitly all-access), a linked Library aimed at a `remote` User
// (ErrLinkedGrant — sharing does not travel), and an unknown library id in the
// set (ErrUnknownLibrary). Every refusal leaves the prior set unchanged.
//
// A Member is unaffected: a linked Library is granted to the household's own
// people exactly like any other (ADR-0056 §2). It is the second hop that is
// refused, not the first.
func (s *Service) SetLibraryAccess(userID string, libraryIDs []string) error {
	u, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if u.Role == roleAdmin {
		return ErrAdminGrant
	}
	if u.Role == roleRemote && len(libraryIDs) > 0 {
		// Checked before the write, so the whole set is refused with the prior set
		// intact — the same shape the unknown-id rejection has. A set that names
		// both a linked Library and an unknown one is LINKED_GRANT: the two mean
		// the same thing to the caller (nothing was applied, fix the set).
		linked, err := s.linkedSet()
		if err != nil {
			return err
		}
		for _, id := range libraryIDs {
			if linked[id] {
				return ErrLinkedGrant
			}
		}
	}
	if err := s.store.ReplaceLibraryAccess(userID, libraryIDs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnknownLibrary
		}
		return err
	}
	return nil
}
