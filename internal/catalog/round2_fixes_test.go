package catalog_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// fileCountingStore counts per-Show ShowFiles reads and the bulk read, which it
// passes through to the real store as *store.DB offers it.
type fileCountingStore struct {
	catalog.Store
	db        *store.DB
	showReads int
	bulkReads int
}

func (s *fileCountingStore) ShowFiles(showID string) ([]store.ShowFile, error) {
	s.showReads++
	return s.Store.ShowFiles(showID)
}

func (s *fileCountingStore) ShowFilesByLibrary(libraryID string) (map[string][]store.ShowFile, error) {
	s.bulkReads++
	return s.db.ShowFilesByLibrary(libraryID)
}

// R04-09: the Needs-Fixing queue reads every Show's Files in one query, not one per
// Show, and gets the same answer as the per-Show read.
func TestShowProblemsReadsShowFilesInBulk(t *testing.T) {
	f := newRescanFixture(t, "Season 01/Show - S01E01.mkv", "Season 01/Show - S01E02.mkv")
	want, err := catalog.NewService(f.db, t.TempDir()).ShowProblems("libtv")
	if err != nil {
		t.Fatal(err)
	}

	cs := &fileCountingStore{Store: f.db, db: f.db}
	got, err := catalog.NewService(cs, t.TempDir()).ShowProblems("libtv")
	if err != nil {
		t.Fatal(err)
	}
	if cs.showReads != 0 || cs.bulkReads != 1 {
		t.Errorf("ShowFiles reads = %d (want 0), bulk reads = %d (want 1)", cs.showReads, cs.bulkReads)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d shows, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ShowID != want[i].ShowID || got[i].Orphaned != want[i].Orphaned {
			t.Errorf("show %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
