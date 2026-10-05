package scanner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A Movie folder whose only main file ffprobe refuses must be listed as
// Unreadable, not abort the whole scan (R04-01): the other folders and the
// post-walk bookkeeping still run.
func TestScanMovieFolderUnreadableMainDoesNotAbort(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "Broken (2001)", "Broken (2001).mkv")
	good := filepath.Join(root, "Fine (2002)", "Fine (2002).mkv")
	writeFile(t, bad)
	writeFile(t, good)

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie", Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, selectiveProber{refuse: "Broken (2001).mkv"})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan aborted on one unreadable file: %v", err)
	}
	if len(cs.trees) != 1 {
		t.Errorf("trees = %d, want 1 (the readable folder)", len(cs.trees))
	}
	if len(cs.unmatched) != 1 || cs.unmatched[0].Path != bad || cs.unmatched[0].Kind != store.UnmatchedUnreadable {
		t.Errorf("unmatched = %+v, want one Unreadable row for %q", cs.unmatched, bad)
	}
}

// selectiveProber refuses one basename and probes everything else.
type selectiveProber struct{ refuse string }

func (p selectiveProber) Probe(ctx context.Context, path string) (MediaInfo, error) {
	if filepath.Base(path) == p.refuse {
		return errProber{}.Probe(ctx, path)
	}
	return fakeProber{height: 1080}.Probe(ctx, path)
}

// An audio file in a Movie folder is not a main video (R04-03).
func TestMovieFolderIgnoresAudioFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Movie (2001)", "Movie (2001).mkv"))
	writeFile(t, filepath.Join(root, "Movie (2001)", "theme.mp3"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie", Roots: []store.LibraryRoot{{Path: root}},
	}}
	if _, err := NewService(cs, fakeProber{height: 1080}).Scan(context.Background(), "lib1"); err != nil {
		t.Fatal(err)
	}
	if len(cs.trees) != 1 {
		t.Fatalf("trees = %d, want 1", len(cs.trees))
	}
	tr := cs.trees[0]
	if tr.Ambiguous {
		t.Errorf("Title flagged ambiguous by a stray audio file")
	}
	if n := countFiles(tr); n != 1 {
		t.Errorf("files = %d, want 1", n)
	}
}

func TestTVShowIgnoresAudioFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Show (2010)", "Season 01", "Show - S01E01.mkv"))
	writeFile(t, filepath.Join(root, "Show (2010)", "theme.mp3"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv", Roots: []store.LibraryRoot{{Path: root}},
	}}
	if _, err := NewService(cs, fakeProber{height: 1080}).Scan(context.Background(), "lib1"); err != nil {
		t.Fatal(err)
	}
	if len(cs.unmatched) != 0 {
		t.Errorf("unmatched = %+v, want none (audio is not TV media)", cs.unmatched)
	}
}

// A Show folder with no parseable identity must not ffprobe its episodes just to
// throw the result away (R04-05).
func TestTVUnidentifiedShowFolderDoesNotProbe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "1080p", "Season 01", "Show - S01E01.mkv"))
	writeFile(t, filepath.Join(root, "1080p", "Season 01", "Show - S01E02.mkv"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv", Roots: []store.LibraryRoot{{Path: root}},
	}}
	p := &countingProber{}
	if _, err := NewService(cs, p).Scan(context.Background(), "lib1"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Errorf("probes = %d, want 0", p.calls)
	}
	if len(cs.unmatched) != 2 {
		t.Errorf("unmatched = %d, want 2", len(cs.unmatched))
	}
}

// A range file (S01E05-E06) becomes two Episodes sharing one path; it must be
// ffprobed once, not once per Episode (R04-11).
func TestTVRangeFileProbedOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Show (2010)", "Season 01", "Show - S01E05-E06.mkv"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv", Roots: []store.LibraryRoot{{Path: root}},
	}}
	p := &countingProber{}
	if _, err := NewService(cs, p).Scan(context.Background(), "lib1"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Errorf("probes = %d, want 1", p.calls)
	}
}

// Path fallback for untagged audio must stop at the Library root (R04-06).
func TestMusicPathFallbackStopsAtRoot(t *testing.T) {
	lib := store.Library{Kind: "music", Roots: []store.LibraryRoot{{Path: "/Volumes/Media/Music"}}}
	rel := libraryRelPath(lib, "/Volumes/Media/Music/01 - Song.mp3")
	artist, album, _, title, _ := parseMusicPath(rel)
	if artist != "" || album != "" || title != "Song" {
		t.Errorf("artist=%q album=%q title=%q, want empty artist/album", artist, album, title)
	}
	rel = libraryRelPath(lib, "/Volumes/Media/Music/Artist/Album (1999)/02 - Two.flac")
	artist, album, year, _, _ := parseMusicPath(rel)
	if artist != "Artist" || album != "Album" || year != 1999 {
		t.Errorf("artist=%q album=%q year=%d", artist, album, year)
	}
}
