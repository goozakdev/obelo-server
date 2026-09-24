package scanner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Local Markers (ADR-0065): a File's own chapters or `.edl` sidecar, read by the
// Scanner with no Plugin involved, through a real store.

// chapterProber answers every path with a 20-minute File carrying chapters, and
// counts its calls so a test can tell a probe from a reuse.
type chapterProber struct {
	chapters []markers.Chapter
	calls    *atomic.Int32
}

func (p chapterProber) Probe(context.Context, string) (MediaInfo, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	return MediaInfo{
		Container:  "mp4",
		DurationMs: 1_200_000,
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
		},
		Chapters: p.chapters,
	}, nil
}

var introChapters = []markers.Chapter{
	{StartMs: 0, EndMs: 30_000, Title: "Cold Open"},
	{StartMs: 30_000, EndMs: 95_000, Title: "Opening Credits"},
	{StartMs: 95_000, EndMs: 1_140_000, Title: "Chapter 2"},
	{StartMs: 1_140_000, EndMs: 1_200_000, Title: "End Credits"},
}

// markerScan scans one Movie folder through a real store and returns the store
// and the Markers stored for its File.
func markerScan(t *testing.T, root string, p Prober, mode Mode) (*store.DB, []store.Marker) {
	t.Helper()
	db := openRealStore(t)
	if _, err := db.CreateLibrary("lib1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}
	return db, rescanMarkers(t, db, p, mode)
}

func rescanMarkers(t *testing.T, db *store.DB, p Prober, mode Mode) []store.Marker {
	t.Helper()
	if _, err := NewService(db, p).ScanMode(context.Background(), "lib1", mode); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var fileID string
	if err := db.QueryRow(`SELECT id FROM files`).Scan(&fileID); err != nil {
		t.Fatalf("scanned file: %v", err)
	}
	ms, err := db.MarkersForFile(fileID)
	if err != nil {
		t.Fatalf("MarkersForFile: %v", err)
	}
	return ms
}

func movieFolder(t *testing.T) (root, video string) {
	t.Helper()
	root = t.TempDir()
	video = filepath.Join(root, "Heat (1995)", "Heat (1995).mkv")
	writeFile(t, video)
	return root, video
}

// TestScannerStoresIntroFromChapters: chapters naming an Intro (and Credits) are
// stored as Local Markers; a chapter naming nothing is not.
func TestScannerStoresIntroFromChapters(t *testing.T) {
	root, _ := movieFolder(t)
	_, got := markerScan(t, root, chapterProber{chapters: introChapters}, ModeIncremental)
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 30_000, EndMs: 95_000},
		{Kind: "credits", Source: "local", StartMs: 1_140_000, EndMs: 1_200_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored markers = %+v, want %+v", got, want)
	}
}

// TestScannerStoresIntroFromEDL: an `.edl` beside the File naming an Intro is
// stored, and it outranks the chapters.
func TestScannerStoresIntroFromEDL(t *testing.T) {
	root, video := movieFolder(t)
	edl := filepath.Join(filepath.Dir(video), "Heat (1995).edl")
	if err := os.WriteFile(edl, []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, got := markerScan(t, root, chapterProber{chapters: introChapters}, ModeIncremental)
	want := []store.Marker{{Kind: "intro", Source: "local", StartMs: 12_500, EndMs: 80_000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored markers = %+v, want only the .edl's %+v", got, want)
	}
}

// TestScannerKeepsMarkersOfAnUnchangedFile: an incremental rescan does not
// re-probe an unchanged File, so its chapter Markers must survive the rebuild of
// its files row — and an `.edl` added since is still read.
func TestScannerKeepsMarkersOfAnUnchangedFile(t *testing.T) {
	root, video := movieFolder(t)
	var calls atomic.Int32
	p := chapterProber{chapters: introChapters, calls: &calls}
	db, first := markerScan(t, root, p, ModeIncremental)

	again := rescanMarkers(t, db, p, ModeIncremental)
	if calls.Load() != 1 {
		t.Fatalf("probes = %d, want 1 (the rescan must reuse the unchanged File)", calls.Load())
	}
	if !reflect.DeepEqual(again, first) {
		t.Errorf("markers after an incremental rescan = %+v, want the stored %+v", again, first)
	}

	edl := filepath.Join(filepath.Dir(video), "Heat (1995).edl")
	if err := os.WriteFile(edl, []byte("1100 1200 0 # Credits\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withEDL := rescanMarkers(t, db, p, ModeIncremental)
	want := []store.Marker{{Kind: "credits", Source: "local", StartMs: 1_100_000, EndMs: 1_200_000}}
	if !reflect.DeepEqual(withEDL, want) {
		t.Errorf("markers after adding an .edl = %+v, want %+v", withEDL, want)
	}
}

func TestParseFFprobeChapters(t *testing.T) {
	info, err := parseFFprobe([]byte(`{
		"format": {"format_name": "matroska,webm", "duration": "1200.0"},
		"chapters": [
			{"start_time": "0.000000", "end_time": "95.500000", "tags": {"title": " Intro "}},
			{"start_time": "95.500000", "end_time": "1200.000000", "tags": {}}
		]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []markers.Chapter{
		{StartMs: 0, EndMs: 95_500, Title: "Intro"},
		{StartMs: 95_500, EndMs: 1_200_000},
	}
	if !reflect.DeepEqual(info.Chapters, want) {
		t.Errorf("chapters = %+v, want %+v", info.Chapters, want)
	}
}
