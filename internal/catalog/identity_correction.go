package catalog

import "github.com/marioquake/juicebox/internal/store"

// Wrong-item identity correction (item-editing/04, ADR-0019). The catalog side of
// the destructive Match-override apply: derive the fix-match folder anchor for a
// Movie / Show (so the api handler can key the override exactly as the scanner
// does), then — once the override is recorded — re-key the live row and wipe the
// now-irrelevant watch state + Locked fields in one step (a genuinely different
// work is a clean slate). The api handler orchestrates match.FixMatch + the
// Enrichment-override re-enrich around these.

// FolderAnchorForTitle returns a leaf Title's kind, its Library id, and the
// on-disk folder anchor its fix-match Match override must be keyed to (the same
// key the scanner uses: a Movie's folder, or the file itself when dropped loose at
// a root). The caller checks kind == "movie" before offering Wrong-item.
// ErrNotFound for an unknown Title.
func (s *Service) FolderAnchorForTitle(titleID string) (kind, libraryID, anchor string, err error) {
	d, err := s.getTitleDetail(titleID)
	if err != nil {
		return "", "", "", err // ErrNotFound flows through
	}
	lib, err := s.store.LibraryByID(d.LibraryID)
	if err != nil {
		return "", "", "", err
	}
	return d.Kind, d.LibraryID, store.NeedsReviewAnchor(d.Kind, firstFilePath(d), rootsOf(lib)), nil
}

// FolderAnchorForShow returns a Show's Library id and the top-level folder anchor
// its fix-match Match override must be keyed to (the scanner keys a Show override
// by its top-level folder, since Episodes nest under it). ErrNotFound for an
// unknown Show (or one with no on-disk files).
func (s *Service) FolderAnchorForShow(showID string) (libraryID, anchor string, err error) {
	sh, err := s.store.ShowByID(showID)
	if err != nil {
		return "", "", err // ErrNotFound flows through
	}
	lib, err := s.store.LibraryByID(sh.LibraryID)
	if err != nil {
		return "", "", err
	}
	path, err := s.store.AnyFilePathForShow(showID)
	if err != nil {
		return "", "", err // ErrNotFound when the Show has no files
	}
	return sh.LibraryID, store.NeedsReviewAnchor("show", path, rootsOf(lib)), nil
}

// CorrectTitleIdentity re-keys a Movie Title to the picked work and, because
// identity actually changed, resets its watch state and clears its Locked fields
// (ADR-0014 clean slate). The caller has already recorded the folder-keyed Match
// override and passes the identity_key it was keyed with, so the live row and the
// override agree. ErrNotFound for an unknown Title.
func (s *Service) CorrectTitleIdentity(titleID, title string, year int, tmdbID, identityKey string) error {
	if err := s.store.RekeyTitleIdentity(titleID, title, year, tmdbID, identityKey); err != nil {
		return err
	}
	if err := s.store.ResetWatchStateForTitle(titleID); err != nil {
		return err
	}
	return s.store.ClearTitleFieldLocks(titleID)
}

// CorrectShowIdentity is CorrectTitleIdentity for a Show: re-key the Show, reset
// every Episode's watch state, and clear the Show's Locked fields. ErrNotFound for
// an unknown Show.
func (s *Service) CorrectShowIdentity(showID, title string, year int, tmdbID, identityKey string) error {
	if err := s.store.RekeyShowIdentity(showID, title, year, tmdbID, identityKey); err != nil {
		return err
	}
	if err := s.store.ResetWatchStateForShow(showID); err != nil {
		return err
	}
	return s.store.ClearEntityFieldLocks(store.EntityShow, showID)
}

// firstFilePath returns any on-disk file path from a Title detail (the first
// File of the first Edition), used to derive the fix-match folder anchor. "" when
// the Title has no files.
func firstFilePath(d store.TitleDetail) string {
	for _, e := range d.Editions {
		for _, f := range e.Files {
			if f.Path != "" {
				return f.Path
			}
		}
	}
	return ""
}

// rootsOf projects a Library's root folder paths, for the anchor derivation.
func rootsOf(lib store.Library) []string {
	roots := make([]string, 0, len(lib.Roots))
	for _, r := range lib.Roots {
		roots = append(roots, r.Path)
	}
	return roots
}
