package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// countingProber counts Probe calls so a test can assert that an unchanged file
// is NOT re-ffprobed on a no-op rescan (the whole point of incremental scanning,
// ADR-0008). It returns a fixed MediaInfo like fakeProber.
type countingProber struct {
	calls int
}

func (p *countingProber) Probe(_ context.Context, _ string) (MediaInfo, error) {
	p.calls++
	return MediaInfo{
		Container: "mp4",
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2},
		},
	}, nil
}

// statefulStore is an in-memory Store that persists enough state across scans
// for the incremental path to work: files keyed by path (with mtime/size/present
// and their streams) and the owning Title identity. It implements the snapshot,
// load, mark-missing, and override seams the incremental scan exercises.
type statefulStore struct {
	lib       store.Library
	files     map[string]store.File // path → stored File (with streams)
	keyByPath map[string]string     // path → owning Title identity_key
	overrides []store.MatchOverride
}

func newStatefulStore(root string) *statefulStore {
	return &statefulStore{
		lib: store.Library{ID: "lib1", Kind: "movie",
			Roots: []store.LibraryRoot{{Path: root}}},
		files:     map[string]store.File{},
		keyByPath: map[string]string{},
	}
}

func (s *statefulStore) LibraryByID(string) (store.Library, error) { return s.lib, nil }

func (s *statefulStore) UpsertTitleTree(t store.TitleTree) error {
	for _, e := range t.Editions {
		for _, f := range e.Files {
			s.files[f.Path] = f
			s.keyByPath[f.Path] = t.IdentityKey
		}
	}
	return nil
}
func (s *statefulStore) ReplaceUnmatched(string, []store.UnmatchedFile) error { return nil }
func (s *statefulStore) MarkScanRunning(string) error                         { return nil }
func (s *statefulStore) MarkScanRunningScope(string, string) error            { return nil }
func (s *statefulStore) MarkScanFinished(string, int, int) error              { return nil }
func (s *statefulStore) MarkScanError(string, string) error                   { return nil }

func (s *statefulStore) ListFileSnapshots(string) (map[string]store.FileSnapshot, error) {
	out := map[string]store.FileSnapshot{}
	for path, f := range s.files {
		out[path] = store.FileSnapshot{
			Path: path, Mtime: f.Mtime, SizeBytes: f.SizeBytes,
			Present: f.Present, IdentityKey: s.keyByPath[path],
		}
	}
	return out, nil
}

func (s *statefulStore) LoadStoredFile(path string) (store.File, error) {
	f, ok := s.files[path]
	if !ok {
		return store.File{}, store.ErrNotFound
	}
	return f, nil
}

func (s *statefulStore) MarkFilesMissing(_ string, seen map[string]bool, unresolved []string) (int, error) {
	under := func(path string) bool {
		for _, p := range unresolved {
			if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	n := 0
	for path, f := range s.files {
		if f.Present && !seen[path] && !under(path) {
			f.Present = false
			s.files[path] = f
			n++
		}
	}
	return n, nil
}
func (s *statefulStore) MarkFilesMissingUnder(_ string, scopeDirs []string, seen map[string]bool, unresolved []string) (int, error) {
	under := func(path string, prefixes []string) bool {
		for _, p := range prefixes {
			if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	n := 0
	for path, f := range s.files {
		if f.Present && under(path, scopeDirs) && !seen[path] && !under(path, unresolved) {
			f.Present = false
			s.files[path] = f
			n++
		}
	}
	return n, nil
}
func (s *statefulStore) RecomputeHiddenTitles(string) error      { return nil }
func (s *statefulStore) UpsertShowTree(store.ShowTree) error     { return nil }
func (s *statefulStore) RecomputeHiddenShows(string) error       { return nil }
func (s *statefulStore) UpsertArtistTree(store.ArtistTree) error { return nil }
func (s *statefulStore) RecomputeHiddenArtists(string) error     { return nil }

func (s *statefulStore) MatchOverridesByLibrary(string) ([]store.MatchOverride, error) {
	return s.overrides, nil
}
func (s *statefulStore) SetMatchOverrideOrphaned(id string, orphaned bool) error {
	for i := range s.overrides {
		if s.overrides[i].ID == id {
			s.overrides[i].Orphaned = orphaned
		}
	}
	return nil
}

// TestIncrementalSkipsUnchangedFiles: after an initial scan probes a file, a
// second incremental scan with NO on-disk change re-probes nothing.
func TestIncrementalSkipsUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Quiet Movie (2020)", "Quiet Movie (2020).mp4"))

	ss := newStatefulStore(root)
	prober := &countingProber{}
	svc := NewService(ss, prober)

	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	if prober.calls != 1 {
		t.Fatalf("initial scan probes = %d, want 1", prober.calls)
	}

	// No-op rescan: nothing changed on disk → no new probes.
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if prober.calls != 1 {
		t.Errorf("no-op rescan re-probed (calls = %d, want still 1)", prober.calls)
	}
}

// TestIncrementalProbesOnlyChangedFile: adding one file processes only that
// file; the unchanged one is not re-probed.
func TestIncrementalProbesOnlyChangedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A Movie (2001)", "A Movie (2001).mp4"))

	ss := newStatefulStore(root)
	prober := &countingProber{}
	svc := NewService(ss, prober)

	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	first := prober.calls // 1

	// Add a brand-new movie folder; rescan.
	writeFile(t, filepath.Join(root, "B Movie (2002)", "B Movie (2002).mp4"))
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if prober.calls != first+1 {
		t.Errorf("after adding one file, probes = %d, want %d (only the new file)", prober.calls, first+1)
	}
}

// TestScanPreservesLibraryWhenRootUnavailable: when a library root is unreachable
// (an unmounted network share presents as ENOENT), the scan must NOT run the
// soft-delete prune. A transient outage leaves an empty walk, but every File stays
// present so the library remains browsable instead of going empty until the next
// manual rescan (ADR-0008: absence via an unmounted volume is not deletion).
func TestScanPreservesLibraryWhenRootUnavailable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Steady Movie (2005)", "Steady Movie (2005).mp4")
	writeFile(t, path)

	ss := newStatefulStore(root)
	svc := NewService(ss, &countingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	if !ss.files[path].Present {
		t.Fatalf("file should be present after initial scan")
	}

	// Simulate the share going offline: the whole root vanishes (stat → ENOENT),
	// exactly as an unmounted network share presents.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Scan(context.Background(), "lib1")
	if !errors.Is(err, ErrRootsUnavailable) {
		t.Fatalf("scan over an unavailable root: err = %v, want ErrRootsUnavailable", err)
	}
	if !ss.files[path].Present {
		t.Errorf("file was pruned during a root outage, want still present (library preserved)")
	}
}

// TestIncrementalFullModeReprobes: ModeFull re-probes everything even when
// nothing changed on disk.
func TestIncrementalFullModeReprobes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Full Movie (2003)", "Full Movie (2003).mp4"))

	ss := newStatefulStore(root)
	prober := &countingProber{}
	svc := NewService(ss, prober)

	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	first := prober.calls

	if _, err := svc.ScanMode(context.Background(), "lib1", ModeFull); err != nil {
		t.Fatalf("full rescan: %v", err)
	}
	if prober.calls != first*2 {
		t.Errorf("full rescan probes = %d, want %d (re-derives everything)", prober.calls, first*2)
	}
}

// TestIncrementalMarksMissing: removing a file marks it Missing (present=0), not
// deleted; re-adding restores it to present.
func TestIncrementalMarksMissing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Vanish Movie (2004)", "Vanish Movie (2004).mp4")
	writeFile(t, path)

	ss := newStatefulStore(root)
	svc := NewService(ss, &countingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	if !ss.files[path].Present {
		t.Fatalf("file should be present after initial scan")
	}

	// Remove the file and rescan: it is soft-deleted (Missing), still in the store.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("rescan after removal: %v", err)
	}
	f, ok := ss.files[path]
	if !ok {
		t.Fatalf("file row was deleted, want soft-delete (Missing)")
	}
	if f.Present {
		t.Errorf("removed file still present, want Missing")
	}

	// Re-add and rescan: restored to present.
	writeFile(t, path)
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("rescan after re-add: %v", err)
	}
	if !ss.files[path].Present {
		t.Errorf("re-added file not restored to present")
	}
}
