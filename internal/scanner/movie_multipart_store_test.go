package scanner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// partDurationProber answers with a distinguishable duration per part, keyed on
// the filename, so TotalDurationMs' sum is checked against real per-part numbers
// rather than two coincidentally-equal ones.
type partDurationProber struct{}

func (partDurationProber) Probe(_ context.Context, path string) (MediaInfo, error) {
	dur := int64(1_500_000)
	if strings.Contains(path, "part2") {
		dur = 1_200_000
	}
	return MediaInfo{
		Container:  "mp4",
		DurationMs: dur,
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
		},
	}, nil
}

// openRealStore opens a real, migrated store.DB in a temp file — the seam
// TestScannedMoviePartsSurviveIntoPlayback needs and the fake captureStore above
// cannot give it: whether a real scan's stored rows are what the playback layer's
// Edition methods actually read.
func openRealStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestScannedMoviePartsSurviveIntoPlayback: a real scan of a real part-named
// MOVIE, through a real store, comes back as one Edition whose PartOrdinal on
// each File is what the SCANNER parsed from the filename (not hand-set the way
// playback/multipart_test.go's fixtures are), and that stored order is what the
// playback layer (store.Edition.IsMultiPart / TotalDurationMs) actually reads.
// TV (catalog.TestScanMultiPartEpisodeKeepsBothPartsAcrossScans) and Placement
// (catalog.TestApplyMergeFoldsOntoJointTimeline) already prove a real scan or
// Apply survives into the stored Edition for their own writers; nothing proved a
// Movie scanned straight off disk carries its part order all the way to the
// Edition methods playback reads.
func TestScannedMoviePartsSurviveIntoPlayback(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Split Movie (2012)")
	part1 := filepath.Join(dir, "Split Movie (2012) - part1.mkv")
	part2 := filepath.Join(dir, "Split Movie (2012) - part2.mkv")
	writeFile(t, part1)
	writeFile(t, part2)

	db := openRealStore(t)
	if _, err := db.CreateLibrary("lib1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}

	svc := NewService(db, partDurationProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	var titleID string
	if err := db.QueryRow(`SELECT id FROM titles WHERE library_id = 'lib1'`).Scan(&titleID); err != nil {
		t.Fatalf("scanned title: %v", err)
	}
	detail, err := db.TitleByID(titleID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(detail.Editions) != 1 || len(detail.Editions[0].Files) != 2 {
		t.Fatalf("editions/files = %+v, want one Edition of two Files", detail.Editions)
	}
	ed := detail.Editions[0]
	if ed.Files[0].Path != part1 || ed.Files[0].PartOrdinal != 1 ||
		ed.Files[1].Path != part2 || ed.Files[1].PartOrdinal != 2 {
		t.Fatalf("stored files = %+v, want part1 then part2 with ordinals 1, 2", ed.Files)
	}
	if !ed.IsMultiPart() {
		t.Fatal("a real scan of a part-named Movie did not come back multi-part")
	}
	if got := ed.TotalDurationMs(); got != 2_700_000 {
		t.Fatalf("total duration = %d, want the sum of both real-scanned parts, 2700000", got)
	}
}
