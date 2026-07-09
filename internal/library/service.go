// Package library is the Movie-Library domain (ADR-0006 modular-monolith seam):
// it owns Library creation, listing, lookup, and deletion, plus the rules that
// keep folder ownership unambiguous (ADR-0002). It is transport-agnostic — it
// speaks Libraries and root folders, not HTTP; the api package wraps it in thin
// handlers.
//
// This slice does no scanning: a Library is just its identity (name, kind) and
// its set of root folders. The catalog stays empty until a later slice.
package library

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/google/uuid"
)

// The media kinds a Library can hold (CONTEXT.md). Movie was the v1 slice; TV
// and Music were lit up by the tv-music PRD. A Library holds exactly one kind.
const (
	KindMovie = "movie"
	KindTV    = "tv"
	KindMusic = "music"
)

// validKinds is the set library.Create accepts. Widened from movie-only to the
// full vocabulary (the schema CHECK widened to match in migration 0008).
var validKinds = map[string]bool{KindMovie: true, KindTV: true, KindMusic: true}

// Domain errors, mapped to HTTP envelopes by the api layer. They are coarse and
// descriptive so a client can branch on them.
var (
	// ErrValidation covers bad input: empty name, wrong kind, no roots, or a
	// root path that is not absolute/cleanable. Its message names the problem.
	ErrValidation = errors.New("library: invalid input")
	// ErrFolderOverlap means a requested root folder is, after normalization,
	// equal to / a parent of / a child of a folder already owned by another
	// Library. Folder ownership must be unambiguous (ADR-0002).
	ErrFolderOverlap = errors.New("library: root folder overlaps an existing library")
	// ErrNotFound means no Library has the given id.
	ErrNotFound = errors.New("library: not found")
)

// Store is the persistence the library service needs. *store.DB satisfies it;
// the interface keeps the seam explicit and the service unit-testable.
type Store interface {
	AllLibraryRoots() ([]store.LibraryRoot, error)
	CreateLibrary(id, name, kind string, roots []store.LibraryRootInput) (store.Library, error)
	UpdateLibrary(id string, name *string, addRoots []store.LibraryRootInput) (store.Library, error)
	Libraries() ([]store.Library, error)
	LibraryByID(id string) (store.Library, error)
	DeleteLibrary(id string) error
}

// Service implements the Library operations.
type Service struct {
	store Store
}

// NewService builds the library service over the given store.
func NewService(s Store) *Service {
	return &Service{store: s}
}

// CreateInput is the validated-on-entry request to create a Library.
type CreateInput struct {
	Name        string
	Kind        string
	RootFolders []string
}

// Create validates the input, normalizes the root folders, rejects any overlap
// with folders already owned by another Library (ADR-0002), and persists the
// new Library. It returns the created Library (with its normalized roots).
//
// Folders are NOT required to exist on disk yet: existence is the scanner's
// concern in a later slice, and requiring it here would make Library creation
// depend on volumes being mounted in the exact order an Admin happens to add
// them. We validate only that each path is absolute and cleanable.
func (s *Service) Create(in CreateInput) (store.Library, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return store.Library{}, fmt.Errorf("%w: name must not be empty", ErrValidation)
	}
	if !validKinds[in.Kind] {
		return store.Library{}, fmt.Errorf("%w: kind must be one of movie, tv, music", ErrValidation)
	}
	if len(in.RootFolders) == 0 {
		return store.Library{}, fmt.Errorf("%w: at least one root folder is required", ErrValidation)
	}

	roots, err := s.prepareRoots(in.RootFolders)
	if err != nil {
		return store.Library{}, err
	}
	return s.store.CreateLibrary(uuid.NewString(), name, in.Kind, roots)
}

// UpdateInput is a partial edit of an existing Library. A nil Name leaves the
// name unchanged; an empty AddRootFolders adds no folders. The kind is fixed at
// creation (a Library holds exactly one media kind, CONTEXT.md) and is not
// editable here.
type UpdateInput struct {
	Name           *string
	AddRootFolders []string
}

// Update applies a partial edit to a Library: rename it and/or append root
// folders. It validates a supplied name (non-empty after trimming) and, for
// added folders, normalizes them and rejects any overlap — with each other,
// with the Library's own existing roots, or with another Library's roots
// (ADR-0002). A missing Library is ErrNotFound. Returns the updated Library.
func (s *Service) Update(id string, in UpdateInput) (store.Library, error) {
	var name *string
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			return store.Library{}, fmt.Errorf("%w: name must not be empty", ErrValidation)
		}
		name = &trimmed
	}

	roots, err := s.prepareRoots(in.AddRootFolders)
	if err != nil {
		return store.Library{}, err
	}

	updated, err := s.store.UpdateLibrary(id, name, roots)
	if errors.Is(err, store.ErrNotFound) {
		return store.Library{}, ErrNotFound
	}
	if err != nil {
		return store.Library{}, err
	}
	return updated, nil
}

// prepareRoots normalizes a set of requested root-folder paths and rejects
// overlap: duplicates/ancestry within the request, and overlap with any folder
// already owned by a Library (ADR-0002). It returns a persistable
// LibraryRootInput per unique folder (with a fresh id). An empty input is a
// valid no-op (used by Update when only the name changes).
func (s *Service) prepareRoots(rawFolders []string) ([]store.LibraryRootInput, error) {
	// Normalize and de-duplicate within the request itself.
	normalized := make([]string, 0, len(rawFolders))
	seen := make(map[string]bool)
	for _, raw := range rawFolders {
		p, err := normalizePath(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		// A root that overlaps another root in the SAME request is also
		// ambiguous (e.g. /movies and /movies/4k), so check intra-request too.
		for _, other := range normalized {
			if pathsOverlap(p, other) {
				return nil, fmt.Errorf("%w: root folders %q and %q overlap within the request",
					ErrFolderOverlap, p, other)
			}
		}
		if !seen[p] {
			seen[p] = true
			normalized = append(normalized, p)
		}
	}
	if len(normalized) == 0 {
		return nil, nil
	}

	// Reject overlap with any folder already owned by a Library (including, on an
	// Update, the target Library's own roots — a nested/duplicate add is equally
	// ambiguous).
	existing, err := s.store.AllLibraryRoots()
	if err != nil {
		return nil, err
	}
	for _, want := range normalized {
		for _, have := range existing {
			if pathsOverlap(want, have.Path) {
				return nil, fmt.Errorf("%w: %q overlaps existing root %q",
					ErrFolderOverlap, want, have.Path)
			}
		}
	}

	roots := make([]store.LibraryRootInput, 0, len(normalized))
	for _, p := range normalized {
		roots = append(roots, store.LibraryRootInput{ID: uuid.NewString(), Path: p})
	}
	return roots, nil
}

// List returns the Libraries the caller may see (with their root folders),
// filtered to the caller's access Scope: an Admin sees every Library, a Member
// only those granted to them. The set is small and unpaginated, so the filter
// is applied in memory. A Member with no grants gets an empty list (not an error).
func (s *Service) List(scope access.Scope) ([]store.Library, error) {
	libs, err := s.store.Libraries()
	if err != nil {
		return nil, err
	}
	out := make([]store.Library, 0, len(libs))
	for _, l := range libs {
		if scope.AllowsLibrary(l.ID) {
			out = append(out, l)
		}
	}
	return out, nil
}

// Get returns one Library by id, or ErrNotFound — including when the Library is
// outside the caller's access Scope (hide existence: a Member cannot tell an
// ungranted Library apart from a missing one).
func (s *Service) Get(scope access.Scope, id string) (store.Library, error) {
	if !scope.AllowsLibrary(id) {
		return store.Library{}, ErrNotFound
	}
	lib, err := s.store.LibraryByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Library{}, ErrNotFound
	}
	if err != nil {
		return store.Library{}, err
	}
	return lib, nil
}

// Delete removes a Library (and its empty catalog), or ErrNotFound.
func (s *Service) Delete(id string) error {
	err := s.store.DeleteLibrary(id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// normalizePath cleans a root folder path to a canonical, absolute form so the
// overlap check and the UNIQUE(path) constraint compare like with like. It
// rejects empty and relative paths (we have no working directory to anchor a
// relative server-side path to, and an ambiguous root undermines identity).
func normalizePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("root folder must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("root folder %q must be an absolute path", raw)
	}
	// filepath.Clean collapses ".", "..", and duplicate separators and strips a
	// trailing separator, so "/movies/", "/movies", and "/movies/." all canonicalize
	// to the same string and compare equal in the overlap check.
	return filepath.Clean(p), nil
}

// pathsOverlap reports whether two normalized absolute paths conflict for
// folder-ownership purposes: they are equal, or one is an ancestor of the other.
// Two roots where one nests inside the other would make a File reachable from
// two Libraries, so both directions are a conflict (ADR-0002). Sibling paths
// that merely share a prefix string (e.g. /movies and /movies-4k) do NOT
// overlap, which is why we compare on path boundaries, not raw prefixes.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return isAncestor(a, b) || isAncestor(b, a)
}

// isAncestor reports whether dir is a strict ancestor directory of child, using
// path-segment boundaries so "/movies" is an ancestor of "/movies/4k" but not of
// "/movies-extra".
func isAncestor(dir, child string) bool {
	if dir == child {
		return false
	}
	sep := string(filepath.Separator)
	prefix := dir
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(child, prefix)
}
