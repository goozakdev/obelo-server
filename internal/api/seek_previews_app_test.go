package api_test

import (
	"context"
	"image"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/seekpreview"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// gatedExtractor decodes with the real ffmpeg, but only once the gate opens, and
// notes which File each frame was for, in order.
type gatedExtractor struct {
	inner seekpreview.Extractor
	gate  chan struct{}
	mu    sync.Mutex
	order []string
}

func (g *gatedExtractor) Frame(ctx context.Context, path string, atMs int64) (image.Image, error) {
	g.mu.Lock()
	base := filepath.Base(path)
	if len(g.order) == 0 || g.order[len(g.order)-1] != base {
		g.order = append(g.order, base)
	}
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.Frame(ctx, path, atMs)
}

func (g *gatedExtractor) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.order...)
}

func sheetStamps(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "seekpreviews", "*", "0.jpg"))
	out := map[string]time.Time{}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			out[p] = fi.ModTime()
		}
	}
	return out
}

func pollFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSeekPreviewsEndToEndThroughTheApp runs the production wiring over real
// ffmpeg on short synthetic movies: a boot backfills Files that have no set (a
// File being played jumps the queue), and touching a File then scanning
// regenerates its set.
func TestSeekPreviewsEndToEndThroughTheApp(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe on PATH")
	}
	root := t.TempDir()
	names := []string{"Alpha (2001)", "Bravo (2002)", "Charlie (2003)"}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(ffmpeg, "-y", "-v", "error",
			"-f", "lavfi", "-i", "testsrc=duration=25:size=320x180:rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=25",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
			filepath.Join(root, n, n+".mp4")).CombinedOutput()
		if err != nil {
			t.Fatalf("making %s: %v: %s", n, err, out)
		}
	}
	dir := t.TempDir()

	// First boot: generation off for the Library, so the scan leaves Files with no set.
	srv1 := testharness.New(t, testharness.WithDataDir(dir))
	token := adminToken(t, srv1)
	libID := createMovieLibrary(t, srv1, token, root)
	srv1.Exec(`INSERT INTO library_seek_previews (library_id, enabled) VALUES (?, 0)`, libID)
	scanLib(t, srv1, token, libID, "")
	srv1.Close()
	if n := len(sheetStamps(t, dir)); n != 0 {
		t.Fatalf("%d sets made with generation off", n)
	}
	db, err := store.Open(filepath.Join(dir, "obelo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`DELETE FROM library_seek_previews`); err != nil {
		t.Fatal(err)
	}
	for i, n := range names { // Charlie is the newest
		if _, err := db.Exec(`UPDATE files SET added_at = ? WHERE path LIKE ?`,
			time.Date(2024, 1, i+1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05"), "%/"+n+".mp4"); err != nil {
			t.Fatal(err)
		}
	}

	// Second boot, generation on and held at the first frame: the backfill queues all
	// three, Charlie (newest) is taken first, and playing Alpha moves it ahead of Bravo.
	gate := &gatedExtractor{inner: &seekpreview.FFmpeg{}, gate: make(chan struct{})}
	srv2 := testharness.New(t, testharness.WithDataDir(dir), testharness.WithSeekPreviewExtractor(gate))
	token = login(t, srv2, "brandon", "hunter2hunter2", "Laptop", "macos", "admin-client").Token
	pollFor(t, "the backfill to start on a File", func() bool { return len(gate.seen()) == 1 })
	alpha := findTitle(t, listAllTitles(t, srv2, token, libID), "Alpha")
	if status, body := srv2.JSON(http.MethodPost, "/api/v1/titles/"+alpha+"/playback", token, mp4Profile(), nil); status != http.StatusOK {
		t.Fatalf("playback = %d; body: %s", status, body)
	}
	close(gate.gate)
	pollFor(t, "three sets from the boot backfill", func() bool { return len(sheetStamps(t, dir)) == 3 })
	want := []string{"Charlie (2003).mp4", "Alpha (2001).mp4", "Bravo (2002).mp4"}
	if got := gate.seen(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("generation order = %v, want %v (newest in flight, then the played File, then the rest)", got, want)
	}

	// Touch Bravo and scan: its set, and only its set, is made again.
	var bravoID string
	if err := db.QueryRow(`SELECT id FROM files WHERE path LIKE '%/Bravo (2002).mp4'`).Scan(&bravoID); err != nil {
		t.Fatal(err)
	}
	before := sheetStamps(t, dir)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, names[1], names[1]+".mp4"), later, later); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv2, token, libID, "")
	bravoSheet := filepath.Join(dir, "seekpreviews", bravoID, "0.jpg")
	pollFor(t, "Bravo's set to be made again", func() bool {
		fi, err := os.Stat(bravoSheet)
		return err == nil && fi.ModTime().After(before[bravoSheet])
	})
	for p, ts := range sheetStamps(t, dir) {
		if p != bravoSheet && !ts.Equal(before[p]) {
			t.Errorf("%s was made again though its File did not change", p)
		}
	}
}
