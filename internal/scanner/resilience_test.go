package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestMovieScanSkipsUnreadableFolder is the Movie/TV analogue of the music
// resilience fix: a folder that can't be read (the transient smbfs ENOENT
// stand-in) must be skipped and recorded, not abort the whole scan — the readable
// siblings still resolve.
func TestMovieScanSkipsUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions; cannot simulate an unreadable folder")
	}
	orig := readDirBackoffs
	readDirBackoffs = nil // mode-000 is persistent; skip the retry sleeps
	t.Cleanup(func() { readDirBackoffs = orig })

	root := t.TempDir()
	goodDir := filepath.Join(root, "Good Movie (2020)")
	writeFile(t, filepath.Join(goodDir, "Good Movie (2020).mkv"))

	blocked := filepath.Join(root, "Blocked Movie (2019)")
	writeFile(t, filepath.Join(blocked, "Blocked Movie (2019).mkv"))
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) }) // restore before TempDir removal

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, fakeProber{height: 1080})

	res, err := svc.Scan(context.Background(), "lib1")
	if err != nil {
		t.Fatalf("scan aborted instead of skipping the unreadable folder: %v", err)
	}

	// The readable movie still resolved.
	if len(cs.trees) != 1 || cs.trees[0].Title.Title != "Good Movie" {
		t.Errorf("readable movie not resolved; trees=%d", len(cs.trees))
	}
	// The unreadable folder was recorded so the prune spares it.
	if len(res.UnresolvedDirs) != 1 || res.UnresolvedDirs[0] != blocked {
		t.Errorf("UnresolvedDirs = %v, want [%q]", res.UnresolvedDirs, blocked)
	}
}

// blockingProber blocks the first Probe until release is closed, so a test can
// hold a scan mid-flight and observe the concurrency guard.
type blockingProber struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProber) Probe(_ context.Context, _ string) (MediaInfo, error) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return MediaInfo{
		Container: "mp4",
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
		},
	}, nil
}

// TestScanRejectsConcurrentScanOfSameLibrary verifies the in-flight guard: while
// one scan of a Library is running, a second is rejected with ErrScanInProgress
// (so a scheduled tick + a manual scan can't hammer the mount at once), and the
// slot frees once the first completes.
func TestScanRejectsConcurrentScanOfSameLibrary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Movie (2020)")
	writeFile(t, filepath.Join(dir, "Movie (2020).mkv"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	prober := &blockingProber{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewService(cs, prober)

	done := make(chan error, 1)
	go func() {
		_, err := svc.Scan(context.Background(), "lib1")
		done <- err
	}()

	<-prober.entered // scan A is now in-flight (the guard slot is claimed)

	// A concurrent scan of the same Library is rejected, not started.
	if _, err := svc.ScanMode(context.Background(), "lib1", ModeIncremental); !errors.Is(err, ErrScanInProgress) {
		t.Fatalf("concurrent scan err = %v, want ErrScanInProgress", err)
	}

	close(prober.release) // let scan A finish
	if err := <-done; err != nil {
		t.Fatalf("scan A failed: %v", err)
	}

	// The slot is freed on completion: a fresh scan is allowed again.
	if _, err := svc.ScanMode(context.Background(), "lib1", ModeIncremental); err != nil {
		t.Fatalf("scan after completion err = %v, want nil", err)
	}
}
