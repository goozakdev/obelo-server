package scanner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// TestScannerFindsAnEDLWhateverItsCase: `Heat (1995).EDL` is the File's `.edl` on
// a case-sensitive disk too.
func TestScannerFindsAnEDLWhateverItsCase(t *testing.T) {
	root, video := movieFolder(t)
	edl := filepath.Join(filepath.Dir(video), "Heat (1995).EDL")
	if err := os.WriteFile(edl, []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, got := markerScan(t, root, chapterProber{chapters: introChapters}, ModeIncremental)
	want := []store.Marker{{Kind: "intro", Source: "local", StartMs: 12_500, EndMs: 80_000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored markers = %+v, want the .EDL's %+v", got, want)
	}
}

// TestEDLNameMatchesAnyCase: the lookup itself, over a directory listing, so it
// is exercised whatever the case-sensitivity of the disk the test runs on. An
// exact name wins over one differing only in case.
func TestEDLNameMatchesAnyCase(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"Heat (1995).mkv", "Heat (1995).EDL"}, "Heat (1995).EDL"},
		{[]string{"heat (1995).Edl", "Heat (1995).mkv"}, "heat (1995).Edl"},
		{[]string{"Heat (1995).EDL", "Heat (1995).edl"}, "Heat (1995).edl"},
		{[]string{"Heat (1995).mkv", "Heat (1995).edl.bak", "Heat.edl"}, ""},
	}
	for _, c := range cases {
		if got := edlName("Heat (1995).mkv", c.names); got != c.want {
			t.Errorf("edlName(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

// TestScannerDropsTheMarkersOfADeletedEDL: the `.edl` of an unchanged File is
// deleted, or rewritten to name nothing. The next incremental scan must not keep
// serving its spans: the File's chapters are the answer again, which takes one
// probe of the File, since an unchanged File's chapters are not kept apart.
func TestScannerDropsTheMarkersOfADeletedEDL(t *testing.T) {
	for name, change := range map[string]func(edl string) error{
		"deleted":       os.Remove,
		"names nothing": func(edl string) error { return os.WriteFile(edl, []byte("300 360 3\n"), 0o644) },
		"renamed away":  func(edl string) error { return os.Rename(edl, edl+".bak") },
	} {
		t.Run(name, func(t *testing.T) {
			root, video := movieFolder(t)
			edl := filepath.Join(filepath.Dir(video), "Heat (1995).edl")
			if err := os.WriteFile(edl, []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			p := chapterProber{chapters: introChapters, calls: &calls}
			db, _ := markerScan(t, root, p, ModeIncremental)
			if err := change(edl); err != nil {
				t.Fatal(err)
			}
			got := rescanMarkers(t, db, p, ModeIncremental)
			want := []store.Marker{
				{Kind: "intro", Source: "local", StartMs: 30_000, EndMs: 95_000},
				{Kind: "credits", Source: "local", StartMs: 1_140_000, EndMs: 1_200_000},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("markers once the .edl is gone = %+v, want the chapters' %+v", got, want)
			}
			// Once back on its chapters the File is unchanged again: not re-probed.
			before := calls.Load()
			rescanMarkers(t, db, p, ModeIncremental)
			if calls.Load() != before {
				t.Errorf("probes on the next rescan = %d, want none", calls.Load()-before)
			}
		})
	}
}

// TestScannerKeepsChapterMarkersWithoutAnEDL: a File whose Markers came from its
// chapters, with no `.edl` at all, is not re-probed by every scan for want of one.
func TestScannerKeepsChapterMarkersWithoutAnEDL(t *testing.T) {
	root, _ := movieFolder(t)
	var calls atomic.Int32
	p := chapterProber{chapters: introChapters, calls: &calls}
	db, first := markerScan(t, root, p, ModeIncremental)
	again := rescanMarkers(t, db, p, ModeIncremental)
	if calls.Load() != 1 {
		t.Errorf("probes = %d, want 1", calls.Load())
	}
	if !reflect.DeepEqual(again, first) {
		t.Errorf("markers = %+v, want %+v", again, first)
	}
}

// TestScannerRemovesWhatItKeptAboutAGonePath: a File renamed to a new name loses
// its old files row, and everything kept by the old path — its Markers of every
// source, the question its Marker providers answered, what detection heard —
// goes with it. The renamed File's own Markers are kept.
func TestScannerRemovesWhatItKeptAboutAGonePath(t *testing.T) {
	root, video := movieFolder(t)
	edl := filepath.Join(filepath.Dir(video), "Heat (1995).edl")
	if err := os.WriteFile(edl, []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := chapterProber{chapters: introChapters}
	db, _ := markerScan(t, root, p, ModeIncremental)
	if err := db.SaveFetchedMarkers(video, "q", []store.Marker{{Kind: "credits", StartMs: 1_150_000, EndMs: 1_200_000}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDetectedMarkers(video, []store.Marker{{Kind: "recap", StartMs: 0, EndMs: 5_000}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDecode(video, false); err != nil {
		t.Fatal(err)
	}
	tables := []string{"markers", "marker_fetches", "marker_detection_files", "marker_detection_failures"}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE file_path = ?`, video).Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s holds %d rows for the File before the rename (%v), want some", table, n, err)
		}
	}

	renamed := filepath.Join(filepath.Dir(video), "Heat (1995) - 1080p.mkv")
	if err := os.Rename(video, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(edl, strings.TrimSuffix(renamed, ".mkv")+".edl"); err != nil {
		t.Fatal(err)
	}
	got := rescanMarkers(t, db, p, ModeIncremental)
	if want := []store.Marker{{Kind: "intro", Source: "local", StartMs: 12_500, EndMs: 80_000}}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers of the renamed File = %+v, want %+v", got, want)
	}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE file_path = ?`, video).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s keeps %d rows for the old path", table, n)
		}
	}
}

// TestScannerKeepsMarkersOfAMissingFile: a File gone from disk is Missing, not
// gone (ADR-0008) — its row stays, and so do its Markers, for when it returns.
func TestScannerKeepsMarkersOfAMissingFile(t *testing.T) {
	root, video := movieFolder(t)
	p := chapterProber{chapters: introChapters}
	db, first := markerScan(t, root, p, ModeIncremental)
	if err := os.Remove(video); err != nil {
		t.Fatal(err)
	}
	got := rescanMarkers(t, db, p, ModeIncremental)
	if !reflect.DeepEqual(got, first) {
		t.Errorf("markers of a Missing File = %+v, want the stored %+v", got, first)
	}
}

// TestScannerRereadsAMissingPartOfATitle: a part of a multi-file Title that goes
// missing loses its files row with the rest of the Title rebuilt around it, so
// what was kept about its path — its Markers, the question its Marker providers
// answered — is removed by that scan. None of it is lost for good: when the File
// comes back it is new to the scan, so it is probed and its chapters read again.
func TestScannerRereadsAMissingPartOfATitle(t *testing.T) {
	root := t.TempDir()
	pt1 := filepath.Join(root, "Split (2012)", "Split (2012) - pt1.mkv")
	pt2 := filepath.Join(root, "Split (2012)", "Split (2012) - pt2.mkv")
	writeFile(t, pt1)
	writeFile(t, pt2)
	var calls atomic.Int32
	p := chapterProber{chapters: introChapters, calls: &calls}
	db := openRealStore(t)
	if _, err := db.CreateLibrary("lib1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}
	scan := func() {
		t.Helper()
		if _, err := NewService(db, p).ScanMode(context.Background(), "lib1", ModeIncremental); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	rowsOf := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE file_path = ?`, pt2).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	scan()
	if err := db.SaveFetchedMarkers(pt2, "q", []store.Marker{{Kind: "credits", StartMs: 1_150_000, EndMs: 1_200_000}}); err != nil {
		t.Fatal(err)
	}
	if rowsOf("markers") == 0 || rowsOf("marker_fetches") == 0 {
		t.Fatal("the part has no Markers or fetch before it goes missing")
	}

	away := filepath.Join(t.TempDir(), "pt2.mkv")
	if err := os.Rename(pt2, away); err != nil {
		t.Fatal(err)
	}
	scan()
	if n := rowsOf("markers"); n != 0 {
		t.Errorf("markers keeps %d rows for the missing part, want them removed", n)
	}
	if n := rowsOf("marker_fetches"); n != 0 {
		t.Errorf("marker_fetches keeps %d rows for the missing part, want it removed so the providers are asked again", n)
	}

	if err := os.Rename(away, pt2); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	scan()
	if calls.Load() == before {
		t.Error("the returned part was not probed")
	}
	var fileID string
	if err := db.QueryRow(`SELECT id FROM files WHERE path = ?`, pt2).Scan(&fileID); err != nil {
		t.Fatalf("the returned part has no files row: %v", err)
	}
	got, err := db.MarkersForFile(fileID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 30_000, EndMs: 95_000},
		{Kind: "credits", Source: "local", StartMs: 1_140_000, EndMs: 1_200_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("markers of the returned part = %+v, want its chapters' %+v", got, want)
	}
}

// TestScannerKeepsMarkersOfAPathStillOnDisk: a path no files row holds is not
// gone while it is still on disk — say a File another Library's scan has read
// but not yet written. Its Markers are kept: an unchanged File is not re-probed,
// so they would not be read again.
func TestScannerKeepsMarkersOfAPathStillOnDisk(t *testing.T) {
	root, _ := movieFolder(t)
	elsewhere := filepath.Join(t.TempDir(), "Ronin (1998).mkv")
	writeFile(t, elsewhere)
	p := chapterProber{chapters: introChapters}
	db, _ := markerScan(t, root, p, ModeIncremental)
	if err := db.ReplaceLocalMarkers(elsewhere, []store.Marker{{Kind: "intro", Source: "local", StartMs: 0, EndMs: 60_000}}); err != nil {
		t.Fatal(err)
	}
	rescanMarkers(t, db, p, ModeIncremental)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM markers WHERE file_path = ?`, elsewhere).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("markers of a path still on disk = %d rows, want the 1 kept", n)
	}
}

// TestScannerListsAFolderOnceAScan: finding each File's `.edl` must not list its
// folder once per File — a flat folder of N Files would cost N listings of N
// names, every scan, which is slow on a network share.
func TestScannerListsAFolderOnceAScan(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Heat (1995).mkv", "Ronin (1998).mkv", "Alien (1979).mkv", "Jaws (1975).mkv", "Big (1988).mkv"} {
		writeFile(t, filepath.Join(root, name))
	}
	if err := os.WriteFile(filepath.Join(root, "Heat (1995).edl"), []byte("12.5 80 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	listings := map[string]int{}
	old := listDir
	listDir = func(dir string) ([]os.DirEntry, error) {
		listings[dir]++
		return old(dir)
	}
	t.Cleanup(func() { listDir = old })

	var calls atomic.Int32
	p := chapterProber{chapters: introChapters, calls: &calls}
	db := openRealStore(t)
	if _, err := db.CreateLibrary("lib1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}
	for scan, mode := range []Mode{ModeIncremental, ModeIncremental, ModeFull} {
		clear(listings)
		if _, err := NewService(db, p).ScanMode(context.Background(), "lib1", mode); err != nil {
			t.Fatalf("scan %d: %v", scan, err)
		}
		if listings[root] > 1 {
			t.Errorf("scan %d listed the folder %d times for .edl files, want at most once", scan, listings[root])
		}
	}
	if calls.Load() != 10 {
		t.Fatalf("probes = %d, want 10 (5 Files, probed by the first and the full scan)", calls.Load())
	}
	var heat string
	if err := db.QueryRow(`SELECT id FROM files WHERE path = ?`, filepath.Join(root, "Heat (1995).mkv")).Scan(&heat); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.MarkersForFile(heat); !reflect.DeepEqual(got, []store.Marker{{Kind: "intro", Source: "local", StartMs: 12_500, EndMs: 80_000}}) {
		t.Errorf("markers of the File with an .edl = %+v, want the .edl's", got)
	}
}

// TestScannerKeepsOneFolderListingAtATime: a scan's `.edl` lookup holds the `.edl`
// names of the folder it is in and nothing more — not every name of every folder
// the scan has listed, which grows with the whole library. Its Files are looked
// up together, so the one folder is still listed once for all of them.
func TestScannerKeepsOneFolderListingAtATime(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, name := range []string{"Heat (1995).mkv", "Heat (1995).EDL", "poster.jpg", "Heat (1995).en.srt"} {
		writeFile(t, filepath.Join(a, name))
	}
	writeFile(t, filepath.Join(b, "Big (1988).mkv"))
	listings := map[string]int{}
	old := listDir
	listDir = func(dir string) ([]os.DirEntry, error) {
		listings[dir]++
		return old(dir)
	}
	t.Cleanup(func() { listDir = old })

	sc := &scanCtx{}
	if got := sc.namesIn(a); !reflect.DeepEqual(got, []string{"Heat (1995).EDL"}) {
		t.Errorf("names kept for %s = %q, want only its .edl", a, got)
	}
	sc.namesIn(a)
	if listings[a] != 1 {
		t.Errorf("one folder looked up twice in a row listed %d times, want once", listings[a])
	}
	sc.namesIn(b)
	sc.namesIn(a)
	if listings[a] != 2 {
		t.Errorf("a folder looked up again after another was listed %d times, want twice: only the folder in hand is kept", listings[a])
	}
}
