package scanner

import (
	"context"
	"os"

	"github.com/marioquake/juicebox/internal/store"
)

// targeted.go implements the Targeted scan (ADR-0030, ADR-0031): a scan restricted
// to the folders of a single browsable entity (a Movie/Show/Album/Artist), rather
// than a Library's whole root set. It reuses the exact per-folder resolvers a full
// scan uses — so identity derivation and Match-override preservation are identical
// — differing only in what it walks (a caller-supplied folder set) and how it
// prunes (per reached folder, never assuming an unreachable folder is empty).
//
// The scope's folders are resolved by the API layer from the entity's present File
// paths (store.*ScanScope + NeedsReviewAnchor); the scanner just walks them. Which
// resolver runs is driven by lib.Kind, exactly as in scanRoots.

// TargetedScope is one Targeted scan's input: the anchor folders to re-walk
// (directories, or a bare-file path for a loose Movie) and the entity's display
// Label for the scan_status scope tag.
type TargetedScope struct {
	Folders []string
	Label   string
}

// TargetedResult summarizes a completed Targeted scan. Added / Removed are the
// "what changed" delta (Files newly present in scope / soft-deleted from it);
// Partial names scope folders that were unreachable this run and left untouched.
type TargetedResult struct {
	TitlesFound int
	FilesFound  int
	Added       int
	Removed     int
	Partial     []string
}

// TargetedScan runs a Targeted scan SYNCHRONOUSLY, returning its result — the
// blocking sibling of StartTargetedScan (the way ScanMode is to StartScan). It
// claims the shared per-Library lock, marks the row running with the scope tag,
// walks, and records the outcome. Used by tests and any caller that wants to block.
func (s *Service) TargetedScan(ctx context.Context, libraryID string, scope TargetedScope) (TargetedResult, error) {
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return TargetedResult{}, err
	}
	if !s.beginScan(libraryID) {
		return TargetedResult{}, ErrScanInProgress
	}
	defer s.endScan(libraryID)
	if err := s.store.MarkScanRunningScope(libraryID, scope.Label); err != nil {
		return TargetedResult{}, err
	}
	res, scanErr := s.scanScope(ctx, lib, scope, nil)
	if scanErr != nil {
		_ = s.store.MarkScanError(lib.ID, scanErr.Error())
		return TargetedResult{}, scanErr
	}
	if err := s.store.MarkScanFinished(lib.ID, res.TitlesFound, res.FilesFound); err != nil {
		return TargetedResult{}, err
	}
	return res, nil
}

// StartTargetedScan begins a Targeted scan in the BACKGROUND, mirroring StartScan:
// it validates the Library, claims the SHARED per-Library scan lock (so a targeted
// and a full scan can't overlap — ADR-0031), marks the row running with the scope
// tag, and then walks on the supplied (request-detached) ctx. done fires with the
// terminal error when the scan settles, letting the API layer run its post-scan
// side-effects (auto-enrich the touched Titles, libraryUpdated) off the scanner's
// events-free core (ADR-0006). Returns ErrScanInProgress when a scan of this
// Library is already running, ErrNotFound for an unknown Library.
func (s *Service) StartTargetedScan(ctx context.Context, libraryID string, scope TargetedScope, onProgress func(Progress), done func(error)) error {
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return err // ErrNotFound flows back to the handler → 404
	}
	if !s.beginScan(libraryID) {
		return ErrScanInProgress
	}
	if err := s.store.MarkScanRunningScope(libraryID, scope.Label); err != nil {
		s.endScan(libraryID)
		return err
	}
	go func() {
		defer s.endScan(libraryID)
		scanErr := s.runTargetedScan(ctx, lib, scope, onProgress)
		if done != nil {
			done(scanErr)
		}
	}()
	return nil
}

// runTargetedScan walks the scope and records the outcome, assuming running is
// already marked. Mirrors runScan; the terminal Complete event carries the delta.
func (s *Service) runTargetedScan(ctx context.Context, lib store.Library, scope TargetedScope, onProgress func(Progress)) error {
	res, scanErr := s.scanScope(ctx, lib, scope, onProgress)
	if scanErr != nil {
		_ = s.store.MarkScanError(lib.ID, scanErr.Error())
		return scanErr
	}
	if err := s.store.MarkScanFinished(lib.ID, res.TitlesFound, res.FilesFound); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(Progress{
			LibraryID:   lib.ID,
			TitlesFound: res.TitlesFound,
			FilesFound:  res.FilesFound,
			Added:       res.Added,
			Removed:     res.Removed,
			Complete:    true,
		})
	}
	return nil
}

// scanScope is the Targeted analogue of scanRoots: it walks the scope's folders
// (per lib.Kind), soft-deletes within scope, and recomputes hidden state. It
// deliberately does NOT touch the Library's Unmatched list or run the override
// orphan-surfacing pass — both are whole-Library operations a narrow scan has no
// business rewriting (ADR-0031); a full scan owns them.
func (s *Service) scanScope(ctx context.Context, lib store.Library, scope TargetedScope, onProgress func(Progress)) (TargetedResult, error) {
	sc := &scanCtx{
		mode:      ModeIncremental, // Targeted scan is always incremental (Q9: one "Scan" action)
		seen:      map[string]bool{},
		snapshots: map[string]store.FileSnapshot{},
		overrides: map[string]store.MatchOverride{},
	}
	snaps, err := s.store.ListFileSnapshots(lib.ID)
	if err != nil {
		return TargetedResult{}, err
	}
	sc.snapshots = snaps
	overrides, err := s.store.MatchOverridesByLibrary(lib.ID)
	if err != nil {
		return TargetedResult{}, err
	}
	for _, o := range overrides {
		sc.overrides[o.FolderPath] = o
	}

	// Reachability pre-check (ADR-0031): a scope folder we cannot stat — an
	// unmounted share (ENOENT) or a transient network-FS blip — is skipped
	// untouched and reported partial, never assumed empty. Recording it in
	// sc.unresolved makes the scoped soft-delete spare its Files. A folder that
	// stats OK is classified dir-vs-file so the Movie path can pick the right
	// resolver (a loose file at a root resolves as a bare file).
	type reachable struct {
		path  string
		isDir bool
	}
	var walkable []reachable
	var partial []string
	for _, folder := range scope.Folders {
		info, statErr := os.Stat(folder)
		if statErr != nil {
			sc.unresolved = append(sc.unresolved, folder)
			partial = append(partial, folder)
			continue
		}
		walkable = append(walkable, reachable{path: folder, isDir: info.IsDir()})
	}
	// Nothing reachable: the entire scope is offline. Treat it like the full scan's
	// unreachable-root guard — error out rather than committing a scan that saw
	// nothing (which would otherwise soft-delete everything in scope).
	if len(walkable) == 0 && len(partial) > 0 {
		return TargetedResult{}, ErrRootsUnavailable
	}

	titlesFound, filesFound := 0, 0
	emit := func() {
		if onProgress != nil {
			onProgress(Progress{LibraryID: lib.ID, TitlesFound: titlesFound, FilesFound: filesFound})
		}
	}

	switch {
	case lib.Kind == "music":
		dirs := make([]string, 0, len(walkable))
		for _, r := range walkable {
			dirs = append(dirs, r.path)
		}
		t, f, _, err := s.scanMusicDirs(ctx, sc, lib, dirs)
		if err != nil {
			return TargetedResult{}, err
		}
		titlesFound += t
		filesFound += f
		emit()
	case lib.Kind == "tv":
		for _, r := range walkable {
			if err := ctx.Err(); err != nil {
				return TargetedResult{}, err
			}
			tree, _, ok, err := s.resolveShowFolder(ctx, sc, lib, r.path)
			if err != nil {
				return TargetedResult{}, err
			}
			if !ok {
				continue
			}
			if err := s.store.UpsertShowTree(tree); err != nil {
				return TargetedResult{}, err
			}
			t, f := countShowTree(tree)
			titlesFound += t
			filesFound += f
			emit()
		}
	default: // movie
		for _, r := range walkable {
			if err := ctx.Err(); err != nil {
				return TargetedResult{}, err
			}
			var (
				tree store.TitleTree
				ok   bool
				err  error
			)
			if r.isDir {
				tree, _, ok, err = s.resolveFolder(ctx, sc, lib, r.path)
			} else {
				tree, _, ok, err = s.resolveBareFile(ctx, sc, lib, r.path)
			}
			if err != nil {
				return TargetedResult{}, err
			}
			if !ok {
				continue
			}
			if err := s.store.UpsertTitleTree(tree); err != nil {
				return TargetedResult{}, err
			}
			titlesFound++
			filesFound += countFiles(tree)
			emit()
		}
	}

	// Scoped soft-delete: a present File under the walked scope but not seen this
	// pass is Missing — but never one under an unreachable folder (spared above).
	removed, err := s.store.MarkFilesMissingUnder(lib.ID, scope.Folders, sc.seen, sc.unresolved)
	if err != nil {
		return TargetedResult{}, err
	}
	// Hidden-state recompute is library-wide but idempotent — only Titles whose
	// File presence actually changed (all within scope) flip.
	if err := s.store.RecomputeHiddenTitles(lib.ID); err != nil {
		return TargetedResult{}, err
	}
	if lib.Kind == "tv" {
		if err := s.store.RecomputeHiddenShows(lib.ID); err != nil {
			return TargetedResult{}, err
		}
	}
	if lib.Kind == "music" {
		if err := s.store.RecomputeHiddenArtists(lib.ID); err != nil {
			return TargetedResult{}, err
		}
	}

	// Added = Files newly present in scope this walk (new to the catalog, or
	// returned from Missing). Computed from the pre-walk snapshot, no extra stat.
	added := 0
	for p := range sc.seen {
		if snap, ok := sc.snapshots[p]; !ok || !snap.Present {
			added++
		}
	}

	return TargetedResult{
		TitlesFound: titlesFound,
		FilesFound:  filesFound,
		Added:       added,
		Removed:     removed,
		Partial:     partial,
	}, nil
}
