// Package scanner walks a Library's root folders and derives the Movie catalog
// — Title → Edition → File → Stream, plus Extras, local Artwork, and the
// Unmatched bucket — from local on-disk information only (ADR-0002): the
// folder/file name carries identity (title + year, optional embedded id), and
// ffprobe extracts the technical attributes and elementary Streams. It is
// deterministic and fully offline.
//
// This slice (issue 05) implements the full Movie naming convention from
// docs/naming-convention.md: multiple Editions within one folder (quality
// tokens + explicit {edition-…}), multi-part joining, extras (subfolders +
// suffixes), sample/junk filtering + an extension allowlist, embedded
// {tmdb/imdb-id}, local poster/fanart artwork, and the needs-review / Unmatched
// attention surface. Nothing recognized is silently dropped.
//
// The package is a modular-monolith seam (ADR-0006): it depends on a Store
// interface and a Prober interface, never on HTTP. The app layer wires the real
// store and ffprobe; tests fake them.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/juicebox/internal/store"
)

// ErrRootsUnavailable is returned by a scan when one or more of a Library's
// configured roots cannot be reached (they stat as ENOENT — the classic
// unmounted network share). The scan deliberately skips the soft-delete prune
// pass in this case so a transient outage cannot empty the catalog: a walk that
// sees nothing because the share is offline is NOT evidence the media was
// deleted (ADR-0008). The library is left untouched and the scan is recorded as
// errored, prompting a retry once the share returns.
var ErrRootsUnavailable = errors.New("scanner: one or more library roots are unavailable")

// Store is the persistence the scanner needs: it reads the Library being scanned
// and writes the resolved catalog + Unmatched list + scan status. *store.DB
// satisfies it; the interface keeps the seam explicit and the scanner testable.
//
// The incremental-scan methods (ListFileSnapshots / LoadStoredFile /
// MarkFilesMissing / RecomputeHiddenTitles) and the Match-override methods let a
// rescan process only changed files (skipping ffprobe on unchanged ones),
// soft-delete absent files, and honor/orphan folder-anchored overrides.
type Store interface {
	LibraryByID(id string) (store.Library, error)
	UpsertTitleTree(tree store.TitleTree) error
	// UpsertShowTree persists one resolved TV Show folder (Show → Seasons →
	// Episode Titles). Used only for TV libraries; the Movie path uses
	// UpsertTitleTree unchanged.
	UpsertShowTree(tree store.ShowTree) error
	// RecomputeHiddenShows derives Season/Show hidden state from Episode
	// visibility after the soft-delete pass (TV analogue of RecomputeHiddenTitles).
	RecomputeHiddenShows(libraryID string) error
	// UpsertArtistTree persists one resolved Music Artist grouping (Artist →
	// Albums → Track Titles). Used only for Music libraries; Movie/TV use their
	// own upsert paths unchanged.
	UpsertArtistTree(tree store.ArtistTree) error
	// RecomputeHiddenArtists derives Album/Artist hidden state from Track
	// visibility after the soft-delete pass (Music analogue of
	// RecomputeHiddenShows).
	RecomputeHiddenArtists(libraryID string) error
	ReplaceUnmatched(libraryID string, files []store.UnmatchedFile) error
	MarkScanRunning(libraryID string) error
	// MarkScanRunningScope marks a Targeted scan running, tagging the row with the
	// entity's label (ADR-0030/0031) so the admin surface shows "Scanning <scope>…".
	MarkScanRunningScope(libraryID, scope string) error
	MarkScanFinished(libraryID string, titlesFound, filesFound int) error
	MarkScanError(libraryID, message string) error

	// Incremental change-detection + soft-delete.
	ListFileSnapshots(libraryID string) (map[string]store.FileSnapshot, error)
	LoadStoredFile(path string) (store.File, error)
	// MarkFilesMissing soft-deletes files absent from the latest walk. unresolvedDirs
	// are subtrees that could not be read this walk (a transient network-FS failure):
	// a file beneath one is left present, never marked Missing, since an unreadable
	// subtree is not evidence of deletion (ADR-0008).
	MarkFilesMissing(libraryID string, seenPaths map[string]bool, unresolvedDirs []string) (int, error)
	// MarkFilesMissingUnder is MarkFilesMissing scoped to a Targeted scan: it only
	// soft-deletes present Files under one of scopeDirs (the folders it walked),
	// leaving everything else untouched (ADR-0031). Returns the count marked Missing.
	MarkFilesMissingUnder(libraryID string, scopeDirs []string, seenPaths map[string]bool, unresolvedDirs []string) (int, error)
	RecomputeHiddenTitles(libraryID string) error

	// Match overrides (fix-match), keyed to the folder path.
	MatchOverridesByLibrary(libraryID string) ([]store.MatchOverride, error)
	SetMatchOverrideOrphaned(id string, orphaned bool) error
}

// Mode selects how much a scan re-derives.
type Mode int

const (
	// ModeIncremental is the default: only new/changed/absent files are
	// processed; an unchanged file reuses its stored ffprobe attributes and is
	// never re-probed (ADR-0008). Absent files are soft-deleted (marked Missing).
	ModeIncremental Mode = iota
	// ModeFull re-derives every file from scratch — it ignores the stored
	// snapshot and re-ffprobes everything. The explicit, rare operator action
	// (POST /libraries/{id}/scan with {"mode":"full"} or ?mode=full).
	ModeFull
)

// Service runs scans. It owns a Store and a Prober (the ffprobe seam), plus an
// in-process set of the libraries currently being scanned so a second scan of the
// same library is rejected while one is in flight (see beginScan).
type Service struct {
	store  Store
	prober Prober

	mu      sync.Mutex
	running map[string]bool // library IDs with a scan in flight this process
}

// NewService builds a scanner over the given store and prober. Pass FFprobe{}
// in production; a fake Prober in unit tests.
func NewService(s Store, p Prober) *Service {
	return &Service{store: s, prober: p, running: map[string]bool{}}
}

// ErrScanInProgress is returned when a scan is requested for a Library that
// already has one running in this process. Manual (StartScan) and scheduled
// (ScanModeProgress) scans share one in-flight set, so a scheduled tick skips a
// Library a manual scan is already walking and two manual scans can't overlap —
// halving the concurrent directory-read pressure a flaky network mount sees and
// avoiding duplicate work. The set is process-local (authoritative for the live
// process and empty after a restart), so a crashed scan never leaves a stuck lock.
var ErrScanInProgress = errors.New("scanner: a scan is already running for this library")

// beginScan claims the in-flight slot for a Library, returning false if a scan is
// already running for it. endScan releases it.
func (s *Service) beginScan(libraryID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[libraryID] {
		return false
	}
	s.running[libraryID] = true
	return true
}

func (s *Service) endScan(libraryID string) {
	s.mu.Lock()
	delete(s.running, libraryID)
	s.mu.Unlock()
}

// Result summarizes a completed scan.
type Result struct {
	TitlesFound int
	FilesFound  int
	// UnresolvedDirs are directories skipped because their contents could not be
	// read after retries (a transient network-FS blip). The scan still succeeds —
	// everything else was catalogued and nothing beneath these was pruned — but the
	// caller can surface them so the operator knows a subtree went unscanned.
	UnresolvedDirs []string
}

// Progress is a snapshot a scan reports through its onProgress callback as it
// walks a Library, so a caller (the HTTP scan handler / the scheduled-scan loop)
// can fan it out as a realtime event. It mirrors enrich.Progress: the counts are
// "found so far" — the Scanner does not pre-count, so there is no total mid-walk.
// Complete is true on the terminal callback, which carries the authoritative
// final TitlesFound / FilesFound (the same numbers MarkScanFinished records).
type Progress struct {
	LibraryID   string
	TitlesFound int
	FilesFound  int
	Complete    bool
	// Added / Removed carry a Targeted scan's "what changed" delta (ADR-0030) on
	// its terminal Complete event: Files newly present in the walked scope and
	// Files soft-deleted from it. Both 0 for a full-Library scan (which reports via
	// TitlesFound/FilesFound), so a client shows the delta only for a Targeted scan.
	Added   int
	Removed int
}

// scanCtx carries the per-scan state threaded through folder/file resolution:
// the mode, the prior on-disk snapshot used to skip re-probing unchanged files,
// the set of paths seen on this walk (for soft-deleting the rest), and the
// folder-anchored Match overrides to apply.
type scanCtx struct {
	mode      Mode
	snapshots map[string]store.FileSnapshot  // path → prior state (empty in ModeFull)
	seen      map[string]bool                // every present media path seen this walk
	overrides map[string]store.MatchOverride // folder path → override
	probes    int                            // ffprobe invocations (instrumentation/tests)
	// unresolved are directories whose contents could not be read this walk (a
	// transient network-FS read failure that outlasted retries). The soft-delete
	// pass skips anything beneath them: an unreadable subtree is not evidence of
	// deletion, so its Files must stay present (ADR-0008, the subtree analogue of
	// the unreachable-root guard).
	unresolved []string
}

// Scan performs an incremental synchronous scan of the Library's roots
// (ADR-0008): it walks each root, resolves every movie folder / bare file to a
// Title tree, re-ffprobing only files that are new or changed (path/mtime/size)
// and reusing stored attributes otherwise, then soft-deletes files absent from
// disk (marks them Missing) and hides Titles whose Files are all Missing. Match
// overrides keyed to the folder path are applied and orphaned overrides surfaced.
// Scan status is recorded throughout. Pass ModeFull to re-derive everything.
//
// Synchronous by design: the HTTP scan handler runs it to completion before
// responding, and the scheduled scan goroutine calls it directly.
func (s *Service) Scan(ctx context.Context, libraryID string) (Result, error) {
	return s.ScanMode(ctx, libraryID, ModeIncremental)
}

// ScanMode is Scan with an explicit Mode (full vs incremental).
func (s *Service) ScanMode(ctx context.Context, libraryID string, mode Mode) (Result, error) {
	return s.ScanModeProgress(ctx, libraryID, mode, nil)
}

// ScanModeProgress is ScanMode with an optional progress callback, invoked as
// folders/files are resolved during the walk (counts found-so-far) and once more
// at the end with Complete=true carrying the final authoritative counts — the
// same numbers MarkScanFinished records. onProgress may be nil (a no-op,
// preserving the pure Scan path and the scanner's unit tests). It mirrors
// enrich.EnrichLibraryProgress; the callback is wired by the API/app layer so
// scanner stays free of any events import (ADR-0006).
func (s *Service) ScanModeProgress(ctx context.Context, libraryID string, mode Mode, onProgress func(Progress)) (Result, error) {
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return Result{}, err // ErrNotFound flows through to the handler
	}
	// Reject a concurrent scan of the same Library (a scheduled tick landing on a
	// Library a manual scan is already walking) so both don't hammer the mount.
	if !s.beginScan(libraryID) {
		return Result{}, ErrScanInProgress
	}
	defer s.endScan(libraryID)
	if err := s.store.MarkScanRunning(libraryID); err != nil {
		return Result{}, err
	}
	return s.runScan(ctx, lib, mode, onProgress)
}

// StartScan begins a scan in the BACKGROUND and returns as soon as the Library
// is validated and durably marked running — so the caller can answer 404 for an
// unknown Library (ErrNotFound flows back synchronously) and report a "running"
// status immediately, without blocking on the walk. The scan itself then runs on
// the supplied ctx (which the caller should detach from any request, so a client
// disconnect can't cancel it); onProgress fires during the walk and once at
// completion. When the scan settles, done (if non-nil) is invoked with the
// terminal error (nil on success), letting the API layer fire its post-scan
// side-effects — auto-enrich, libraryUpdated, terminal-on-error event — off the
// scanner's events-free core (ADR-0006). It mirrors ScanModeProgress, which the
// scheduled safety-net path still uses synchronously.
func (s *Service) StartScan(ctx context.Context, libraryID string, mode Mode, onProgress func(Progress), done func(error)) error {
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return err // ErrNotFound flows back to the handler → 404
	}
	// Reject a second concurrent scan of the same Library (double-click, or a
	// manual scan while a scheduled tick is mid-walk) before marking running, so
	// the counters aren't reset out from under the in-flight scan.
	if !s.beginScan(libraryID) {
		return ErrScanInProgress
	}
	if err := s.store.MarkScanRunning(libraryID); err != nil {
		s.endScan(libraryID) // release: we never launched the goroutine
		return err
	}
	go func() {
		defer s.endScan(libraryID)
		_, scanErr := s.runScan(ctx, lib, mode, onProgress)
		if done != nil {
			done(scanErr)
		}
	}()
	return nil
}

// runScan walks the Library and records the outcome, assuming the running state
// is already marked. Shared by the synchronous ScanModeProgress and the
// background StartScan.
func (s *Service) runScan(ctx context.Context, lib store.Library, mode Mode, onProgress func(Progress)) (Result, error) {
	res, scanErr := s.scanRoots(ctx, lib, mode, onProgress)
	if scanErr != nil {
		_ = s.store.MarkScanError(lib.ID, scanErr.Error())
		return Result{}, scanErr
	}
	if err := s.store.MarkScanFinished(lib.ID, res.TitlesFound, res.FilesFound); err != nil {
		return Result{}, err
	}
	// Terminal progress: the authoritative final counts (same as MarkScanFinished),
	// so a client hides its "scanning…" indicator and does one final refetch.
	if onProgress != nil {
		onProgress(Progress{
			LibraryID:   lib.ID,
			TitlesFound: res.TitlesFound,
			FilesFound:  res.FilesFound,
			Complete:    true,
		})
	}
	return res, nil
}

func (s *Service) scanRoots(ctx context.Context, lib store.Library, mode Mode, onProgress func(Progress)) (Result, error) {
	sc := &scanCtx{
		mode:      mode,
		seen:      map[string]bool{},
		snapshots: map[string]store.FileSnapshot{},
		overrides: map[string]store.MatchOverride{},
	}
	// Incremental: load the prior snapshot so unchanged files skip ffprobe. Full:
	// leave snapshots empty so every file is treated as changed and re-probed.
	if mode == ModeIncremental {
		snaps, err := s.store.ListFileSnapshots(lib.ID)
		if err != nil {
			return Result{}, err
		}
		sc.snapshots = snaps
	}
	overrides, err := s.store.MatchOverridesByLibrary(lib.ID)
	if err != nil {
		return Result{}, err
	}
	for _, o := range overrides {
		sc.overrides[o.FolderPath] = o
	}

	var folders []string // movie folders (subdirectories of a root)
	var bareFiles []string

	var unreachableRoots []string
	for _, root := range lib.Roots {
		f, b, reachable, err := s.discoverRoot(root.Path)
		if err != nil {
			return Result{}, err
		}
		if !reachable {
			unreachableRoots = append(unreachableRoots, root.Path)
		}
		folders = append(folders, f...)
		bareFiles = append(bareFiles, b...)
	}

	// Stable order so a scan is deterministic regardless of walk order.
	sort.Strings(folders)
	sort.Strings(bareFiles)

	titlesFound := 0
	filesFound := 0
	var unmatched []store.UnmatchedFile

	// emit fires a mid-walk progress snapshot (Complete=false) with the counts
	// found so far, so a client's indicator advances during a long scan. A nil
	// callback is a no-op; the terminal Complete event is fired by ScanModeProgress.
	emit := func() {
		if onProgress != nil {
			onProgress(Progress{LibraryID: lib.ID, TitlesFound: titlesFound, FilesFound: filesFound})
		}
	}

	switch {
	case lib.Kind == "music":
		// Music: embedded tags are identity authority (amended ADR-0002). The walker
		// recurses every root for audio files, probes each for tags, derives the
		// Artist → Album → Track identity (Album-Artist grouping; path fallback when
		// tags are absent), and upserts one Artist subtree per grouping. folders +
		// bareFiles are not used here — music nests arbitrarily, so a dedicated
		// recursive walk gathers every audio file regardless of folder depth.
		t, f, unm, err := s.scanMusicLibrary(ctx, sc, lib)
		if err != nil {
			return Result{}, err
		}
		titlesFound += t
		filesFound += f
		unmatched = append(unmatched, unm...)
		emit()
	case lib.Kind == "tv":
		// TV: each top-level folder is a Show folder; a bare media file at the root
		// has no Show context, so it is Unmatched (never auto-guessed into a Show).
		for _, folder := range folders {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			tree, unm, ok, err := s.resolveShowFolder(ctx, sc, lib, folder)
			if err != nil {
				return Result{}, err
			}
			unmatched = append(unmatched, unm...)
			if !ok {
				continue
			}
			if err := s.store.UpsertShowTree(tree); err != nil {
				return Result{}, err
			}
			t, f := countShowTree(tree)
			titlesFound += t
			filesFound += f
			emit()
		}
		for _, path := range bareFiles {
			unmatched = append(unmatched, unmatchedFile(path, "media file outside any Show folder"))
		}
	default:
		for _, folder := range folders {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			tree, unm, ok, err := s.resolveFolder(ctx, sc, lib, folder)
			if err != nil {
				return Result{}, err
			}
			unmatched = append(unmatched, unm...)
			if !ok {
				continue
			}
			if err := s.store.UpsertTitleTree(tree); err != nil {
				return Result{}, err
			}
			titlesFound++
			filesFound += countFiles(tree)
			emit()
		}

		for _, path := range bareFiles {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			tree, unm, ok, err := s.resolveBareFile(ctx, sc, lib, path)
			if err != nil {
				return Result{}, err
			}
			unmatched = append(unmatched, unm...)
			if !ok {
				continue
			}
			if err := s.store.UpsertTitleTree(tree); err != nil {
				return Result{}, err
			}
			titlesFound++
			filesFound += countFiles(tree)
			emit()
		}
	}

	if err := s.store.ReplaceUnmatched(lib.ID, unmatched); err != nil {
		return Result{}, err
	}

	// Prune guard: if any configured root was unreachable this walk (an unmounted
	// network share stats as ENOENT), skip the soft-delete/hide/orphan passes
	// entirely and error out. Those passes infer "gone from disk ⇒ Missing" from
	// absence in sc.seen, but an offline share yields an empty (or partial) walk
	// that is NOT evidence of deletion — running them would mark the catalog
	// Missing and hide it from browse, emptying the library until the next manual
	// rescan. Upserts for roots that WERE reachable already committed above, so
	// healthy roots still update; only the destructive step is withheld (ADR-0008).
	if len(unreachableRoots) > 0 {
		return Result{}, fmt.Errorf("%w: %v", ErrRootsUnavailable, unreachableRoots)
	}

	// Soft-delete: files not seen this walk become Missing (not deleted), then
	// Titles whose every File is Missing are hidden from browse (ADR-0008).
	if _, err := s.store.MarkFilesMissing(lib.ID, sc.seen, sc.unresolved); err != nil {
		return Result{}, err
	}
	if err := s.store.RecomputeHiddenTitles(lib.ID); err != nil {
		return Result{}, err
	}
	// TV: after Episode (Title) visibility is final, derive Season/Show hidden
	// state from it so a Show whose every Episode went Missing drops out of the
	// grid but stays fetchable (ADR-0008). No-op for Movie libraries.
	if lib.Kind == "tv" {
		if err := s.store.RecomputeHiddenShows(lib.ID); err != nil {
			return Result{}, err
		}
	}
	// Music: after Track (Title) visibility is final, derive Album/Artist hidden
	// state from it so an Artist whose every Track went Missing drops out of the
	// list but stays fetchable (ADR-0008). No-op for Movie/TV libraries.
	if lib.Kind == "music" {
		if err := s.store.RecomputeHiddenArtists(lib.ID); err != nil {
			return Result{}, err
		}
	}

	// Orphan surfacing: an override whose anchor folder no longer exists on disk
	// is flagged orphaned (surfaced in the Admin attention list), never lost.
	for _, o := range overrides {
		_, statErr := os.Stat(o.FolderPath)
		orphaned := os.IsNotExist(statErr)
		if orphaned != o.Orphaned {
			if err := s.store.SetMatchOverrideOrphaned(o.ID, orphaned); err != nil {
				return Result{}, err
			}
		}
	}

	return Result{TitlesFound: titlesFound, FilesFound: filesFound, UnresolvedDirs: sc.unresolved}, nil
}

// readDirTolerant reads a directory with retry, tolerating a transient failure
// mid-walk. On persistent failure it records dir in sc.unresolved — so the
// soft-delete pass spares anything beneath it (ADR-0008) — and returns nil
// entries with no error, so the caller resolves whatever it could read and the
// scan is never aborted over one unreadable folder on a flaky network mount. A
// folder that genuinely vanished is likewise spared here and only pruned once its
// parent listing stops naming it. This is the Movie/TV analogue of the resilient
// music walk (readDirResilient + discoverMusicRoot).
func (sc *scanCtx) readDirTolerant(dir string) []os.DirEntry {
	entries, err := readDirResilient(dir)
	if err != nil {
		sc.unresolved = append(sc.unresolved, dir)
		return nil
	}
	return entries
}

// discoverRoot lists one root, returning its movie subfolders and its bare
// recognized-media files (the single-file fallback). reachable reports whether
// the root resolved on disk: a non-existent root (ENOENT — e.g. an unmounted
// volume) returns reachable=false with no error, so the caller can distinguish
// "the share is offline" from "the share is present but empty" and skip the
// destructive prune in the former case (ADR-0008). Any other stat error
// propagates. A root that stats OK but whose listing fails even after retries is
// treated as unreachable (reachable=false, no error) rather than aborting: a
// transient network-FS blip on the root listing must not be read as "the whole
// library was deleted", and the prune guard already spares an unreachable root.
func (s *Service) discoverRoot(root string) (folders, bareFiles []string, reachable bool, err error) {
	info, statErr := os.Stat(root)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("scanner: stat root %q: %w", root, statErr)
	}
	if !info.IsDir() {
		return nil, nil, true, nil
	}

	entries, readErr := readDirResilient(root)
	if readErr != nil {
		return nil, nil, false, nil
	}
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			folders = append(folders, full)
		} else if isMedia(e.Name()) {
			bareFiles = append(bareFiles, full)
		}
	}
	return folders, bareFiles, true, nil
}

// classifiedFile is one recognized media file in a movie folder, tagged with the
// detail parsed from its name.
type classifiedFile struct {
	path      string
	name      string
	extraType string // non-empty ⇒ this is an Extra
	part      int    // >0 ⇒ multi-part member
}

// resolveFolder resolves one movie folder into a TitleTree. It walks the folder
// (one level, plus the recognized extras subfolders), classifies every file, and
// assembles Editions/Extras/Artwork. A folder whose name yields no identity but
// that contains recognized media routes those files to Unmatched (ok=false).
func (s *Service) resolveFolder(ctx context.Context, sc *scanCtx, lib store.Library, folder string) (store.TitleTree, []store.UnmatchedFile, bool, error) {
	id, idOK := ParseIdentity(filepath.Base(folder))

	// A Match override anchored to this folder overrules the parsed identity and
	// persists across rescans (ADR-0002/0014). It also rescues a folder the
	// convention couldn't parse (idOK=false) by supplying the corrected identity.
	if ov, ok := sc.overrides[folder]; ok {
		id = Identity{
			Title:  ov.Title,
			Year:   ov.Year,
			Key:    ov.IdentityKey,
			TMDBID: ov.TMDBID,
			IMDBID: ov.IMDBID,
		}
		idOK = true
	}

	// A folder that can't be read after retries is skipped (recorded in
	// sc.unresolved so the prune spares it) rather than aborting the whole scan.
	entries := sc.readDirTolerant(folder)

	var mains []classifiedFile
	var extras []classifiedFile
	var sidecars []string          // subtitle filenames within this folder (sorted order)
	artwork := map[string]string{} // role → path (first wins, stable order)
	var artworkOrder []string

	addArtwork := func(role, path string) {
		if _, seen := artwork[role]; !seen {
			artwork[role] = path
			artworkOrder = append(artworkOrder, role)
		}
	}

	// Top-level files in the folder.
	var topFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() {
			topFiles = append(topFiles, e)
		}
	}
	sort.Slice(topFiles, func(i, j int) bool { return topFiles[i].Name() < topFiles[j].Name() })

	for _, e := range topFiles {
		full := filepath.Join(folder, e.Name())
		if role := artworkRole(e.Name()); role != "" {
			addArtwork(role, full)
			continue
		}
		if isSubtitle(e.Name()) {
			// A Sidecar subtitle: recorded as a selectable Subtitle track (parsed
			// for language/forced + text-vs-image) once the Title is assembled
			// below. topFiles is sorted, so sidecars stays deterministic.
			sidecars = append(sidecars, e.Name())
			continue
		}
		if !isMedia(e.Name()) {
			continue
		}
		size := fileSize(full)
		if isJunk(e.Name(), size) {
			continue // sample/junk ignored entirely
		}
		if et := extraTypeFromSuffix(e.Name()); et != "" {
			extras = append(extras, classifiedFile{path: full, name: e.Name(), extraType: et})
			continue
		}
		mains = append(mains, classifiedFile{
			path: full, name: e.Name(),
			part: partNumber(e.Name()),
		})
	}

	// Recognized extras subfolders.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		et := extraTypeFromFolder(e.Name())
		if et == "" {
			continue
		}
		sub := filepath.Join(folder, e.Name())
		// An unreadable extras subfolder is skipped (recorded, spared from prune);
		// the main movie in this folder still resolves from the top-level entries.
		subEntries := sc.readDirTolerant(sub)
		sort.Slice(subEntries, func(i, j int) bool { return subEntries[i].Name() < subEntries[j].Name() })
		for _, se := range subEntries {
			if se.IsDir() || !isMedia(se.Name()) {
				continue
			}
			full := filepath.Join(sub, se.Name())
			if isJunk(se.Name(), fileSize(full)) {
				continue
			}
			extras = append(extras, classifiedFile{path: full, name: se.Name(), extraType: et})
		}
	}

	// A folder that has no parseable identity but holds recognized media: those
	// files are Unmatched (never auto-guessed into a Title). Extras with no
	// parent identity have nothing to attach to, so they go to Unmatched too.
	if !idOK {
		var unm []store.UnmatchedFile
		for _, cf := range mains {
			unm = append(unm, unmatched(cf.path, "no parseable identity from folder name"))
		}
		for _, cf := range extras {
			unm = append(unm, unmatched(cf.path, "extra with no identifiable parent title"))
		}
		return store.TitleTree{}, unm, false, nil
	}

	// A folder with a valid identity but zero main videos (only extras/artwork/
	// junk) is not a browsable Title: route any extras to Unmatched (no Title to
	// attach to) and skip.
	if len(mains) == 0 {
		var unm []store.UnmatchedFile
		for _, cf := range extras {
			unm = append(unm, unmatched(cf.path, "extra with no main video in folder"))
		}
		return store.TitleTree{}, unm, false, nil
	}

	tree, err := s.assembleTitle(ctx, sc, lib, id, mains, extras, artwork, artworkOrder)
	if err != nil {
		return store.TitleTree{}, nil, false, err
	}
	tree.Subtitles = buildSidecarSubtitles(folder, sidecars)
	return tree, nil, true, nil
}

// resolveBareFile resolves a recognized media file sitting directly at a root
// (the single-file fallback). Junk is ignored; an unparseable name → Unmatched;
// otherwise a one-Edition one-File Title.
//
// Sidecar subtitles are only persisted for the folder-based Movie layout
// (resolveFolder) in this slice: a loose file's root-level sidecars aren't
// collected here, and the TV/episode path (tv_resolve.go) likewise doesn't
// gather them yet. Embedded subtitle Streams still surface everywhere (they ride
// the shared buildFile path). Widening sidecar persistence to those layouts is a
// tracked follow-up (.scratch/subtitles/issues/01).
func (s *Service) resolveBareFile(ctx context.Context, sc *scanCtx, lib store.Library, path string) (store.TitleTree, []store.UnmatchedFile, bool, error) {
	name := filepath.Base(path)
	if isJunk(name, fileSize(path)) {
		return store.TitleTree{}, nil, false, nil
	}
	id, ok := ParseIdentity(stripExt(name))
	// A bare-file Title anchors its override to the file path itself.
	if ov, has := sc.overrides[path]; has {
		id = Identity{Title: ov.Title, Year: ov.Year, Key: ov.IdentityKey, TMDBID: ov.TMDBID, IMDBID: ov.IMDBID}
		ok = true
	}
	if !ok {
		return store.TitleTree{}, []store.UnmatchedFile{unmatched(path, "no parseable identity from file name")}, false, nil
	}
	cf := classifiedFile{path: path, name: name, part: partNumber(name)}
	tree, err := s.assembleTitle(ctx, sc, lib, id, []classifiedFile{cf}, nil, nil, nil)
	if err != nil {
		// A bare file we can't probe goes to Unmatched rather than vanishing.
		return store.TitleTree{}, []store.UnmatchedFile{unmatched(path, "could not probe file: "+err.Error())}, false, nil
	}
	return tree, nil, true, nil
}

// probedFile is one main video paired with its probed attributes and the
// Edition discriminator derived from its name/resolution. When reused holds a
// stored File (incremental scan of an unchanged file: not re-ffprobed), stored
// carries that prior row's attributes/streams instead of a fresh probe.
type probedFile struct {
	cf     classifiedFile
	media  MediaInfo
	ed     string
	mtime  string
	reused bool
	stored store.File
}

// fileMtime returns a file's modification time as RFC3339 UTC, "" on error.
func fileMtime(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano)
}

// unchanged reports whether the file at path matches its prior snapshot on
// mtime AND size (the cheap change signal). In ModeFull no snapshot exists, so
// every file is treated as changed and re-probed.
func (sc *scanCtx) unchanged(path string, size int64, mtime string) bool {
	if sc.mode == ModeFull {
		return false
	}
	snap, ok := sc.snapshots[path]
	if !ok {
		return false // new file
	}
	return snap.Present && snap.Mtime == mtime && snap.SizeBytes == size
}

// editionGroup gathers the probed files that share one Edition identity.
type editionGroup struct {
	name   string
	probes []probedFile
}

// allParts reports whether every file in the group is a numbered multi-part
// member (so they join into one Edition rather than colliding).
func (g *editionGroup) allParts() bool {
	for _, p := range g.probes {
		if p.cf.part == 0 {
			return false
		}
	}
	return true
}

// assembleTitle builds the full TitleTree from classified main videos, extras,
// and artwork. It probes each file, groups mains into Editions (by edition name,
// joining parts), flags ambiguity (two non-part files in the same Edition), and
// flags needs-review (a yearless identity).
func (s *Service) assembleTitle(
	ctx context.Context, sc *scanCtx, lib store.Library, id Identity,
	mains, extras []classifiedFile, artwork map[string]string, artworkOrder []string,
) (store.TitleTree, error) {
	var ps []probedFile
	for _, cf := range mains {
		sc.seen[cf.path] = true // present on disk this walk (drives soft-delete)
		size := fileSize(cf.path)
		mtime := fileMtime(cf.path)

		// Incremental skip: an unchanged file reuses its stored row+streams and is
		// NOT re-ffprobed (the expensive step — skipping it is the whole point).
		if sc.unchanged(cf.path, size, mtime) {
			if stored, err := s.store.LoadStoredFile(cf.path); err == nil {
				ps = append(ps, probedFile{
					cf: cf, mtime: mtime, reused: true, stored: stored,
					ed: editionName(cf.name, stored.Height),
				})
				continue
			}
			// Fall through to a fresh probe if the stored row vanished.
		}

		sc.probes++
		media, err := s.prober.Probe(ctx, cf.path)
		if err != nil {
			// One unprobeable main in a multi-file folder is dropped here; the
			// bare-file caller turns a probe failure on the only file into
			// Unmatched (nothing recognized silently vanishes there).
			continue
		}
		video, _ := media.PrimaryVideo()
		ps = append(ps, probedFile{cf: cf, media: media, mtime: mtime, ed: editionName(cf.name, video.Height)})
	}
	if len(ps) == 0 {
		return store.TitleTree{}, fmt.Errorf("scanner: no probeable main video for %q", id.Title)
	}

	// Group by Edition name. Parts of the same Edition join (multiple Files);
	// two non-part files in the same Edition flag the Title ambiguous.
	var order []string
	groups := map[string]*editionGroup{}
	for _, p := range ps {
		g, ok := groups[p.ed]
		if !ok {
			g = &editionGroup{name: p.ed}
			groups[p.ed] = g
			order = append(order, p.ed)
		}
		g.probes = append(g.probes, p)
	}

	ambiguous := false
	var editions []store.Edition
	for _, name := range order {
		g := groups[name]
		sort.Slice(g.probes, func(i, j int) bool {
			pi, pj := g.probes[i].cf.part, g.probes[j].cf.part
			if pi != pj {
				return pi < pj
			}
			return g.probes[i].cf.path < g.probes[j].cf.path
		})
		// Collision: >1 file in one Edition that are NOT all parts → ambiguous.
		if len(g.probes) > 1 && !g.allParts() {
			ambiguous = true
		}

		editionID := uuid.NewString()
		var files []store.File
		for _, p := range g.probes {
			var f store.File
			if p.reused {
				// Reuse the stored attributes/streams verbatim; only re-key the
				// edition membership (UpsertTitleTree preserves the row id by path).
				f = p.stored
				f.EditionID = editionID
				f.Streams = rekeyStreams(f.ID, f.Streams)
			} else {
				f = buildFile(editionID, p.cf.path, p.media)
			}
			f.Mtime = p.mtime
			f.Present = true
			// SizeBytes is the change-detection key alongside mtime, so it must be
			// the on-disk stat size (authoritative and matching the snapshot),
			// not ffprobe's reported format size which can be absent/approximate.
			if sz := fileSize(p.cf.path); sz > 0 {
				f.SizeBytes = sz
			}
			files = append(files, f)
		}
		editions = append(editions, store.Edition{ID: editionID, Name: name, Files: files})
	}

	var storeExtras []store.Extra
	for _, cf := range extras {
		sc.seen[cf.path] = true // extras count as present media too
		ex := store.Extra{
			ID:        uuid.NewString(),
			Type:      cf.extraType,
			Path:      cf.path,
			SizeBytes: fileSize(cf.path),
		}
		if media, err := s.prober.Probe(ctx, cf.path); err == nil {
			ex.Container = media.Container
			ex.DurationMs = media.DurationMs
			if media.SizeBytes > 0 {
				ex.SizeBytes = media.SizeBytes
			}
		}
		storeExtras = append(storeExtras, ex)
	}

	var storeArt []store.Artwork
	for _, role := range artworkOrder {
		storeArt = append(storeArt, store.Artwork{
			ID:   uuid.NewString(),
			Role: role,
			Path: artwork[role],
		})
	}

	return store.TitleTree{
		Title: store.Title{
			ID:          uuid.NewString(),
			LibraryID:   lib.ID,
			Kind:        lib.Kind,
			Title:       id.Title,
			Year:        id.Year,
			IdentityKey: id.Key,
			SortTitle:   sortTitle(id.Title),
			TMDBID:      id.TMDBID,
			IMDBID:      id.IMDBID,
			NeedsReview: !id.HasYear(),
			Ambiguous:   ambiguous,
		},
		Editions: editions,
		Extras:   storeExtras,
		Artwork:  storeArt,
	}, nil
}

func buildFile(editionID, path string, media MediaInfo) store.File {
	fileID := uuid.NewString()
	video, _ := media.PrimaryVideo()
	audio, _ := media.PrimaryAudio()

	streams := make([]store.Stream, 0, len(media.Streams))
	for _, st := range media.Streams {
		streams = append(streams, store.Stream{
			ID:              uuid.NewString(),
			FileID:          fileID,
			Index:           st.Index,
			Kind:            st.Kind,
			Codec:           st.Codec,
			Language:        st.Language,
			Width:           st.Width,
			Height:          st.Height,
			Channels:        st.Channels,
			IsDefault:       st.IsDefault,
			Forced:          st.Forced,
			Title:           st.Title,
			Commentary:      st.Commentary,
			HearingImpaired: st.HearingImpaired,
		})
	}

	return store.File{
		ID:         fileID,
		EditionID:  editionID,
		Path:       path,
		Container:  media.Container,
		VideoCodec: video.Codec,
		AudioCodec: audio.Codec,
		Width:      video.Width,
		Height:     video.Height,
		Bitrate:    media.Bitrate,
		DurationMs: media.DurationMs,
		SizeBytes:  media.SizeBytes,
		Streams:    streams,
	}
}

// rekeyStreams returns the streams re-pointed at fileID with FRESH stream ids.
// A reused File's stored streams carry their prior ids; a fresh id is assigned
// here so two Title subtrees that reuse the SAME on-disk file (a multi-episode
// S01E05-E06 file maps to two Episode Titles) never insert two streams with the
// same id — the streams table keys on id, and stream ids are not stable identity
// (watch state is per-Title, not per-Stream), so regenerating them is safe.
func rekeyStreams(fileID string, streams []store.Stream) []store.Stream {
	out := make([]store.Stream, len(streams))
	for i, st := range streams {
		st.ID = uuid.NewString()
		st.FileID = fileID
		out[i] = st
	}
	return out
}

func unmatched(path, reason string) store.UnmatchedFile {
	return store.UnmatchedFile{ID: uuid.NewString(), Path: path, Reason: reason}
}

// countShowTree returns (#Episode Titles, #Files) in a resolved Show subtree, so
// the scan result counts an Episode like a Movie Title (a multi-episode file
// counts as two Titles, sharing the one File).
func countShowTree(tree store.ShowTree) (titles, files int) {
	for _, st := range tree.Seasons {
		for _, ep := range st.Episodes {
			titles++
			files += countFiles(ep.TitleTree)
		}
	}
	return titles, files
}

func countFiles(tree store.TitleTree) int {
	n := 0
	for _, e := range tree.Editions {
		n += len(e.Files)
	}
	return n
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func stripExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
