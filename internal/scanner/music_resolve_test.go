package scanner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverMusicRootSkipsUnreadableSubtree is the regression test for the SMB
// transient-ENOENT scan abort: a Music library on a network mount would fail its
// entire scan with "open <artist dir>: no such file or directory" when smbfs
// briefly returned ENOENT for a directory that exists, because filepath.WalkDir's
// walkFn returned the error and aborted. The resilient walk must instead SKIP an
// unreadable subtree, record it in unresolved, and still return every audio file
// under the readable siblings. An unreadable directory here stands in for the
// transient failure (persistent EACCES exercises the same skip-and-record path).
func TestDiscoverMusicRootSkipsUnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions; cannot simulate an unreadable subtree")
	}
	// Keep the test fast: a mode-000 directory is a *persistent* failure, so the
	// retry budget would otherwise sleep ~1s. Drop the backoffs for this test.
	orig := readDirBackoffs
	readDirBackoffs = nil
	t.Cleanup(func() { readDirBackoffs = orig })

	root := t.TempDir()
	// Readable artist: root/Good Artist/Album/01 - Track.flac
	good := filepath.Join(root, "Good Artist", "Album (2020)")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	goodFile := filepath.Join(good, "01 - Track.flac")
	if err := os.WriteFile(goodFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unreadable artist: root/Blocked Artist has a track, but the directory can't
	// be read (mode 000) — the transient-ENOENT stand-in.
	blocked := filepath.Join(root, "Blocked Artist")
	if err := os.MkdirAll(filepath.Join(blocked, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "Album", "02 - Track.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore perms BEFORE t.TempDir's RemoveAll cleanup (cleanups run LIFO, and
	// TempDir registered its removal first), or the tree can't be deleted.
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	audio, _, unresolved, err := discoverMusicRoot(root)
	if err != nil {
		t.Fatalf("discoverMusicRoot returned a hard error instead of skipping: %v", err)
	}

	// The readable subtree's file must still be discovered — the whole scan is not
	// thrown away over one bad directory.
	found := false
	for _, p := range audio {
		if p == goodFile {
			found = true
		}
	}
	if !found {
		t.Errorf("readable file %q not discovered; audio=%v", goodFile, audio)
	}

	// The unreadable directory must be recorded so the caller suppresses pruning
	// beneath it.
	if len(unresolved) != 1 || unresolved[0] != blocked {
		t.Errorf("unresolved = %v, want exactly [%q]", unresolved, blocked)
	}
}
