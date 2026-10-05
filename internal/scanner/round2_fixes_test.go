package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// countingDB counts the per-file reads and the marker writes a scan makes.
type countingDB struct {
	*store.DB
	loads, edlWrites, edlReads int
}

func (c *countingDB) LoadStoredFile(p string) (store.File, error) {
	c.loads++
	return c.DB.LoadStoredFile(p)
}
func (c *countingDB) ReplaceEDLMarkers(p string, ms []store.Marker) error {
	c.edlWrites++
	return c.DB.ReplaceEDLMarkers(p, ms)
}
func (c *countingDB) LocalMarkersFromEDL(p string) (bool, error) {
	c.edlReads++
	return c.DB.LocalMarkersFromEDL(p)
}

// addingStore is a captureStore that also takes AddUnmatched, as *store.DB does.
type addingStore struct {
	captureStore
	added []store.UnmatchedFile
}

func (a *addingStore) AddUnmatched(_ string, f []store.UnmatchedFile) error {
	a.added = append(a.added, f...)
	return nil
}

// A Targeted scan of a Movie folder whose main ffprobe refuses keeps the new
// Unreadable row instead of throwing it away (refuter-09).
func TestTargetedMovieScanKeepsUnreadableRows(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "Broken (2001)", "Broken (2001).mkv")
	writeFile(t, bad)
	as := &addingStore{captureStore: captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie", Roots: []store.LibraryRoot{{Path: root}},
	}}}
	svc := NewService(as, selectiveProber{refuse: "Broken (2001).mkv"})
	if _, err := svc.TargetedScan(context.Background(), "lib1", TargetedScope{
		Folders: []string{filepath.Dir(bad)}, Label: "Broken",
	}); err != nil {
		t.Fatalf("targeted scan: %v", err)
	}
	if len(as.added) != 1 || as.added[0].Path != bad || as.added[0].Kind != store.UnmatchedUnreadable {
		t.Errorf("added = %+v, want one Unreadable row for %q", as.added, bad)
	}
}

// R04-08: an incremental no-op scan reads the stored Files in bulk, and writes an
// `.edl`'s Markers only when they changed.
func TestIncrementalScanBulkReadsAndSkipsUnchangedEDLWrites(t *testing.T) {
	root := t.TempDir()
	video := filepath.Join(root, "Heat (1995)", "Heat (1995).mkv")
	writeFile(t, video)
	edl := filepath.Join(root, "Heat (1995)", "Heat (1995).edl")
	if err := os.WriteFile(edl, []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := &countingDB{DB: openRealStore(t)}
	if _, err := db.CreateLibrary("lib1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, chapterProber{chapters: introChapters})
	scan := func() {
		t.Helper()
		if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
			t.Fatal(err)
		}
	}
	scan() // first scan: probes, writes the .edl's Markers
	db.loads, db.edlWrites, db.edlReads = 0, 0, 0
	scan()
	if db.loads != 0 || db.edlWrites != 0 || db.edlReads != 0 {
		t.Errorf("no-op scan: LoadStoredFile=%d ReplaceEDLMarkers=%d LocalMarkersFromEDL=%d, want all 0",
			db.loads, db.edlWrites, db.edlReads)
	}

	// A changed .edl is still written.
	if err := os.WriteFile(edl, []byte("20 90 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	if db.edlWrites != 1 {
		t.Errorf("changed .edl: ReplaceEDLMarkers = %d, want 1", db.edlWrites)
	}
	var start int64
	if err := db.QueryRow(`SELECT start_ms FROM markers WHERE file_path = ? AND source = 'local'`, video).Scan(&start); err != nil || start != 20_000 {
		t.Errorf("stored start = %d, %v; want 20000", start, err)
	}

	// An .edl that goes away sends the File back to its chapters.
	if err := os.Remove(edl); err != nil {
		t.Fatal(err)
	}
	scan()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM markers WHERE file_path = ? AND from_edl = 1`, video).Scan(&n); err != nil || n != 0 {
		t.Errorf("edl-sourced markers after removing the .edl = %d, %v; want 0", n, err)
	}
}

// R02-15: the scanner sort key IS the store one, so a scanned Title and a
// re-keyed one cannot order differently.
func TestSortTitleIsTheStoresRule(t *testing.T) {
	for _, in := range []string{"The Matrix", "  An Apple ", "A Bugs Life", "Theater", "Anthem", "the"} {
		if got, want := sortTitle(in), store.SortTitle(in); got != want {
			t.Errorf("sortTitle(%q) = %q, store.SortTitle = %q", in, got, want)
		}
	}
	if got := sortTitle("The Matrix"); got != "matrix" {
		t.Errorf("sortTitle(The Matrix) = %q, want matrix", got)
	}
}

// A single folder level above an untagged file is the Artist, not the Album
// (refuter-09): "Artist/song.mp3" has no album folder to read.
func TestMusicPathFallbackOneLevelIsTheArtist(t *testing.T) {
	artist, album, _, title, _ := parseMusicPath("Artist/01 - Song.mp3")
	if artist != "Artist" || album != "" || title != "Song" {
		t.Errorf("artist=%q album=%q title=%q, want Artist / no album / Song", artist, album, title)
	}
	artist, album, _, _, _ = parseMusicPath("Artist/Album (1999)/01 - Song.mp3")
	if artist != "Artist" || album != "Album" {
		t.Errorf("two-level: artist=%q album=%q", artist, album)
	}
}
