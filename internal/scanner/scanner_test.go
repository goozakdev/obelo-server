package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// fakeProber returns a fixed MediaInfo for any path, so scanner logic (Edition
// grouping, ambiguity, parts) is testable without ffmpeg/ffprobe.
type fakeProber struct {
	height int
}

func (f fakeProber) Probe(_ context.Context, _ string) (MediaInfo, error) {
	return MediaInfo{
		Container: "mp4",
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: f.height, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2},
		},
	}, nil
}

// captureStore records the upserted trees and unmatched lists.
type captureStore struct {
	lib       store.Library
	trees     []store.TitleTree
	unmatched []store.UnmatchedFile
}

func (c *captureStore) LibraryByID(string) (store.Library, error) { return c.lib, nil }
func (c *captureStore) UpsertTitleTree(t store.TitleTree) error {
	c.trees = append(c.trees, t)
	return nil
}
func (c *captureStore) UpsertShowTree(store.ShowTree) error     { return nil }
func (c *captureStore) RecomputeHiddenShows(string) error       { return nil }
func (c *captureStore) UpsertArtistTree(store.ArtistTree) error { return nil }
func (c *captureStore) RecomputeHiddenArtists(string) error     { return nil }
func (c *captureStore) ReplaceUnmatched(_ string, f []store.UnmatchedFile) error {
	c.unmatched = f
	return nil
}
func (c *captureStore) MarkScanRunning(string) error            { return nil }
func (c *captureStore) MarkScanFinished(string, int, int) error { return nil }
func (c *captureStore) MarkScanError(string, string) error      { return nil }

// Incremental + override seams: the unit tests above scan a fresh temp dir with
// no prior state, so these are inert (empty snapshot, no stored files, no
// overrides). The black-box integration tests exercise the real implementations.
func (c *captureStore) ListFileSnapshots(string) (map[string]store.FileSnapshot, error) {
	return map[string]store.FileSnapshot{}, nil
}
func (c *captureStore) LoadStoredFile(string) (store.File, error) {
	return store.File{}, store.ErrNotFound
}
func (c *captureStore) MarkFilesMissing(string, map[string]bool) (int, error) { return 0, nil }
func (c *captureStore) RecomputeHiddenTitles(string) error                    { return nil }
func (c *captureStore) MatchOverridesByLibrary(string) ([]store.MatchOverride, error) {
	return nil, nil
}
func (c *captureStore) SetMatchOverrideOrphaned(string, bool) error { return nil }

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Above the sample size floor so it isn't treated as junk.
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runScan(t *testing.T, root string, height int) *captureStore {
	t.Helper()
	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, fakeProber{height: height})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return cs
}

// TestScannerAmbiguousCollision: two non-part files that parse to the SAME
// Edition identity flag the Title ambiguous (never silently picked).
func TestScannerAmbiguousCollision(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Collide Movie (2020)")
	// Both have no quality token and the same probed resolution → same Edition.
	writeFile(t, filepath.Join(dir, "Collide Movie (2020).mkv"))
	writeFile(t, filepath.Join(dir, "Collide Movie (2020) copy.mkv"))

	cs := runScan(t, root, 1080)
	if len(cs.trees) != 1 {
		t.Fatalf("trees = %d, want 1", len(cs.trees))
	}
	tree := cs.trees[0]
	if !tree.Ambiguous {
		t.Errorf("collision should flag the Title ambiguous")
	}
	if len(tree.Editions) != 1 {
		t.Errorf("editions = %d, want 1 (both collapsed to one identity)", len(tree.Editions))
	}
}

// TestScannerMultiPartNotAmbiguous: numbered parts join into one Edition and are
// NOT ambiguous (one playthrough, multiple Files).
func TestScannerMultiPartNotAmbiguous(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Split Movie (2012)")
	writeFile(t, filepath.Join(dir, "Split Movie (2012) - part1.mp4"))
	writeFile(t, filepath.Join(dir, "Split Movie (2012) - part2.mp4"))

	cs := runScan(t, root, 1080)
	tree := cs.trees[0]
	if tree.Ambiguous {
		t.Errorf("multi-part must not be ambiguous")
	}
	if len(tree.Editions) != 1 || len(tree.Editions[0].Files) != 2 {
		t.Fatalf("want 1 edition with 2 files, got %d editions", len(tree.Editions))
	}
}

// TestScannerTwoQualityEditions: distinct quality tokens → two Editions, not
// ambiguous.
func TestScannerTwoQualityEditions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Edition Movie (2010)")
	writeFile(t, filepath.Join(dir, "Edition Movie (2010) - 1080p.mkv"))
	writeFile(t, filepath.Join(dir, "Edition Movie (2010) - 2160p.mkv"))

	cs := runScan(t, root, 1080)
	tree := cs.trees[0]
	if tree.Ambiguous {
		t.Errorf("distinct quality editions must not be ambiguous")
	}
	if len(tree.Editions) != 2 {
		t.Fatalf("editions = %d, want 2", len(tree.Editions))
	}
}

// TestScannerUnmatchedBareFile: a bare file with no extractable identity routes
// to Unmatched, never a Title.
func TestScannerUnmatchedBareFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "1080p.mkv"))

	cs := runScan(t, root, 1080)
	if len(cs.trees) != 0 {
		t.Errorf("identityless bare file became a Title (%d trees)", len(cs.trees))
	}
	if len(cs.unmatched) != 1 || filepath.Base(cs.unmatched[0].Path) != "1080p.mkv" {
		t.Errorf("unmatched = %+v, want [1080p.mkv]", cs.unmatched)
	}
}

// TestScannerYearlessNeedsReview: a yearless folder is filed as a Title and
// flagged needs-review.
func TestScannerYearlessNeedsReview(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Yearless Movie")
	writeFile(t, filepath.Join(dir, "Yearless Movie.mp4"))

	cs := runScan(t, root, 1080)
	if len(cs.trees) != 1 {
		t.Fatalf("trees = %d, want 1", len(cs.trees))
	}
	if !cs.trees[0].NeedsReview {
		t.Errorf("yearless Title should be flagged needs-review")
	}
}
