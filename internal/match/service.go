// Package match is the Match-override domain (ADR-0002, ADR-0014): an Admin
// identity correction that overrules the convention-derived guess and persists
// across rescans. This slice implements fix-match — re-pointing a folder's
// identity, keyed to the folder path — fully. Merge and split (also "Match
// overrides" per CONTEXT.md) are heavier and intentionally NOT implemented here;
// see the TODO at the bottom. The service is transport-agnostic; the api package
// wraps it in thin handlers.
package match

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/marioquake/juicebox/internal/scanner"
	"github.com/marioquake/juicebox/internal/store"
)

// Domain errors, mapped to HTTP by the api layer.
var (
	// ErrValidation covers bad input (no library, no folder path, empty title).
	ErrValidation = errors.New("match: invalid input")
	// ErrNotFound means the target Library does not exist.
	ErrNotFound = errors.New("match: not found")
)

// Store is the persistence the match service needs. *store.DB satisfies it.
type Store interface {
	LibraryByID(id string) (store.Library, error)
	UpsertMatchOverride(o store.MatchOverride) (store.MatchOverride, error)
	MatchOverridesByLibrary(libraryID string) ([]store.MatchOverride, error)
}

// Service implements fix-match and the override attention surface.
type Service struct {
	store Store
}

// NewService builds the match service over the given store.
func NewService(s Store) *Service {
	return &Service{store: s}
}

// FixMatchInput is an Admin identity correction for a folder. Either a corrected
// Title (+ optional Year) or an embedded-style external id (TMDBID/IMDBID) must
// be supplied; the resulting identity key is derived the same way a scan would.
type FixMatchInput struct {
	LibraryID  string
	FolderPath string
	Title      string
	Year       int
	TMDBID     string
	IMDBID     string
}

// FixMatch records (or replaces) the override for a folder. The next scan will
// resolve that folder to the corrected identity instead of its parse, and the
// override persists across rescans (the scan never undoes it). The corrected
// identity is keyed to the FOLDER PATH (ADR-0014): renaming/moving the folder
// later orphans the override, surfaced via Orphaned rather than silently lost.
func (s *Service) FixMatch(in FixMatchInput) (store.MatchOverride, error) {
	if strings.TrimSpace(in.FolderPath) == "" {
		return store.MatchOverride{}, fmt.Errorf("%w: folderPath is required", ErrValidation)
	}
	if !filepath.IsAbs(in.FolderPath) {
		return store.MatchOverride{}, fmt.Errorf("%w: folderPath must be absolute", ErrValidation)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" && in.TMDBID == "" && in.IMDBID == "" {
		return store.MatchOverride{}, fmt.Errorf("%w: a corrected title or embedded id is required", ErrValidation)
	}

	if _, err := s.store.LibraryByID(in.LibraryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MatchOverride{}, ErrNotFound
		}
		return store.MatchOverride{}, err
	}

	key := scanner.IdentityKeyFor(title, in.Year, in.TMDBID, in.IMDBID)
	return s.store.UpsertMatchOverride(store.MatchOverride{
		LibraryID:   in.LibraryID,
		FolderPath:  filepath.Clean(in.FolderPath),
		Title:       title,
		Year:        in.Year,
		TMDBID:      in.TMDBID,
		IMDBID:      in.IMDBID,
		IdentityKey: key,
	})
}

// List returns every Match override for a Library (the Admin attention surface
// reads this to show orphans alongside needs-review / Unmatched). ErrNotFound
// for an unknown Library so the caller answers 404.
func (s *Service) List(libraryID string) ([]store.MatchOverride, error) {
	if _, err := s.store.LibraryByID(libraryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.store.MatchOverridesByLibrary(libraryID)
}

// TODO(issue-06+): merge (collapse two parsed Titles into one identity) and
// split (separate two works that parsed to one identity) are also Match
// overrides per CONTEXT.md but are heavier — they touch multiple Titles and
// their watch state. They are deliberately NOT implemented in this slice; only
// fix-match ships. The api surface exposes no merge/split routes (no pretense
// that they exist), and this is the seam they will extend.
