package store_test

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestMultiEpisodeIncrementalReuseCollision reproduces the scheduled-scan crash:
//   store: inserting file "...S04E19-20...": UNIQUE constraint failed: files.id
//
// A combined-episode file (S04E19-20) resolves to TWO Episode Titles that share
// one on-disk path. On an incremental rescan the scanner REUSES the stored File
// for that path (LoadStoredFile), which returns ONE row — so BOTH Episode Titles
// carry the SAME File.ID. When one of those Titles is newly appearing (e.g. the
// range-parsing feature just shipped, so E20 shows up for the first time while
// the file itself is unchanged and thus reused), writeTitleSubtree's keep-both
// branch inserts the duplicated stored id and collides.
func TestMultiEpisodeIncrementalReuseCollision(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)

	const path = "/Volumes/brandonj/Videos/TV/BSG (2003)/Season 4/BSG (2003) - S04E19-20 - Daybreak.mkv"

	show := store.Show{
		ID: "show1", LibraryID: "libtv", Title: "BSG", Year: 2003,
		IdentityKey: "bsg|2003", SortTitle: "bsg",
	}

	// Pre-range binary: the file resolved to a SINGLE episode E19 with file id "fa".
	first := store.ShowTree{
		Show: show,
		Seasons: []store.SeasonTree{{
			SeasonNumber: 4, IdentityKey: "bsg|2003|s04",
			Episodes: []store.EpisodeTree{
				episodeTree("t19", "bsg|2003|s04e19", 4, 19, "fa", path),
			},
		}},
	}
	if err := db.UpsertShowTree(first); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Range-aware binary, incremental rescan: the file is unchanged so the scanner
	// REUSES the stored row for `path` for BOTH episodes — LoadStoredFile returns
	// the same single row, so E19 and E20 both carry file id "fa". E20 is new.
	second := store.ShowTree{
		Show: show,
		Seasons: []store.SeasonTree{{
			SeasonNumber: 4, IdentityKey: "bsg|2003|s04",
			Episodes: []store.EpisodeTree{
				episodeTree("t19", "bsg|2003|s04e19", 4, 19, "fa", path),
				episodeTree("t20", "bsg|2003|s04e20", 4, 20, "fa", path),
			},
		}},
	}
	if err := db.UpsertShowTree(second); err != nil {
		t.Fatalf("incremental rescan of combined episode failed: %v", err)
	}

	// Both Episode Titles must now own a distinct, present File row for the shared
	// path (the combined episode plays under either Title).
	assertDistinctPresentFiles(t, db, path, 2)

	// A further steady-state rescan (both Titles now exist) stays clean and keeps
	// exactly two distinct rows — no drift, no collision.
	if err := db.UpsertShowTree(second); err != nil {
		t.Fatalf("steady-state rescan failed: %v", err)
	}
	assertDistinctPresentFiles(t, db, path, 2)
}

func assertDistinctPresentFiles(t *testing.T, db *store.DB, path string, want int) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM files WHERE path = ? AND present = 1`, path)
	if err != nil {
		t.Fatalf("query files: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids[id] = true
	}
	if len(ids) != want {
		t.Fatalf("present file rows for %q = %d (distinct ids %v), want %d", path, len(ids), ids, want)
	}
}

func episodeTree(titleID, identityKey string, season, episode int, fileID, path string) store.EpisodeTree {
	return store.EpisodeTree{
		TitleTree: store.TitleTree{
			Title: store.Title{
				ID: titleID, LibraryID: "libtv", Kind: "episode",
				Title: "Daybreak", IdentityKey: identityKey, SortTitle: "daybreak",
			},
			Editions: []store.Edition{{
				ID: "ed-" + titleID, Name: "",
				Files: []store.File{{
					ID: fileID, Path: path, Container: "matroska",
					VideoCodec: "hevc", AudioCodec: "aac", Width: 1920, Height: 1080,
					Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000,
				}},
			}},
		},
		SeasonNumber:  season,
		EpisodeNumber: episode,
	}
}
