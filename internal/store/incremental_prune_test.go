package store_test

import (
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestMarkFilesMissingSparesUnresolvedSubtree is the store-side half of the SMB
// transient-ENOENT fix: when a scan skips an unreadable subtree, the files
// beneath it must be left present (not soft-deleted), while genuinely absent
// files elsewhere are still marked Missing. Without this, a transient read blip
// would wrongly hide real Tracks (ADR-0008).
func TestMarkFilesMissingSparesUnresolvedSubtree(t *testing.T) {
	db := openTemp(t)

	lib, err := db.CreateLibrary("lib1", "Music", "music", []store.LibraryRootInput{{Path: "/lib"}})
	if err != nil {
		t.Fatalf("create library: %v", err)
	}

	blockedPath := filepath.Join("/lib", "Blocked Artist", "Album", "01.flac")
	goodPath := filepath.Join("/lib", "Good Artist", "Album", "02.flac")
	gonePath := filepath.Join("/lib", "Deleted Artist", "Album", "03.flac")

	// Seed three present files, each as its own Title/Edition.
	for i, p := range []string{blockedPath, goodPath, gonePath} {
		id := "t" + string(rune('0'+i))
		tree := store.TitleTree{
			Title: store.Title{
				ID:          id,
				LibraryID:   lib.ID,
				Kind:        "track",
				Title:       p,
				IdentityKey: p,
			},
			Editions: []store.Edition{{
				ID:    id + "-ed",
				Files: []store.File{{ID: id + "-f", Path: p, Present: true}},
			}},
		}
		if err := db.UpsertTitleTree(tree); err != nil {
			t.Fatalf("upsert %q: %v", p, err)
		}
	}

	// This walk "saw" only goodPath. blockedPath's subtree was unreadable;
	// gonePath is genuinely absent.
	seen := map[string]bool{goodPath: true}
	unresolved := []string{filepath.Join("/lib", "Blocked Artist")}

	n, err := db.MarkFilesMissing(lib.ID, seen, unresolved)
	if err != nil {
		t.Fatalf("MarkFilesMissing: %v", err)
	}
	// Only gonePath should be marked Missing — blockedPath is spared by the guard.
	if n != 1 {
		t.Errorf("marked %d files Missing, want 1 (only the genuinely-absent one)", n)
	}
	assertPresent(t, db, blockedPath, true) // spared: under an unresolved subtree
	assertPresent(t, db, goodPath, true)    // seen this walk
	assertPresent(t, db, gonePath, false)   // genuinely gone
}

func assertPresent(t *testing.T, db *store.DB, path string, want bool) {
	t.Helper()
	var present bool
	if err := db.QueryRow(`SELECT present FROM files WHERE path = ?`, path).Scan(&present); err != nil {
		t.Fatalf("query present for %q: %v", path, err)
	}
	if present != want {
		t.Errorf("file %q present = %v, want %v", path, present, want)
	}
}
