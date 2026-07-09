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

	"github.com/marioquake/juicebox/internal/store"
)

// roleAdmin is the Admin role string (mirrors the value the auth/store layers
// use); an Admin always resolves to an all-access Scope.
const roleAdmin = "admin"

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
	// ErrAdminCeiling: a Rating ceiling cannot be set on an Admin — Admins are
	// all-access and the ceiling is never consulted for them (→ 422).
	ErrAdminCeiling = errors.New("access: cannot set a rating ceiling on an admin")
	// ErrUnknownRating: the requested ceiling is not a known rating label (→ 422).
	ErrUnknownRating = errors.New("access: unknown rating ceiling label")
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
	ceiling, err := s.store.RatingCeilingForUser(userID)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		IsAdmin:       false,
		AllLibraries:  false,
		LibraryIDs:    libs,
		RatingCeiling: ceilingRank(ceiling),
	}, nil
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

// LibraryAccess returns the Library ids granted to a User (for the Admin
// user-management view). An Admin has no grant rows (they are all-access by
// role), so this is empty for an Admin — the caller reads the role to know that
// means "all Libraries", not "none".
func (s *Service) LibraryAccess(userID string) ([]string, error) {
	return s.store.LibraryAccessForUser(userID)
}

// SetLibraryAccess replaces a Member's grant set with exactly libraryIDs. It
// rejects an unknown User (ErrUserNotFound), an Admin target (ErrAdminGrant —
// Admins are implicitly all-access), and an unknown library id in the set
// (ErrUnknownLibrary, leaving the prior set unchanged).
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
	if err := s.store.ReplaceLibraryAccess(userID, libraryIDs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnknownLibrary
		}
		return err
	}
	return nil
}
