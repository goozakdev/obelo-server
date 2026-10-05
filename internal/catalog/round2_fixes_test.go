package catalog_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
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

// contextCountingStore counts per-Title context reads and passes the bulk ones
// through to the real store.
type contextCountingStore struct {
	catalog.Store
	db        *store.DB
	perTitle  int
	bulkReads int
}

func (s *contextCountingStore) EpisodeContextForTitle(id string) (store.EpisodeContext, error) {
	s.perTitle++
	return s.Store.EpisodeContextForTitle(id)
}

func (s *contextCountingStore) EpisodeContextsForTitles(ids []string) (map[string]store.EpisodeContext, error) {
	s.bulkReads++
	return s.db.EpisodeContextsForTitles(ids)
}

func (s *contextCountingStore) TrackContextsForTitles(ids []string) (map[string]store.TrackContext, error) {
	s.bulkReads++
	return s.db.TrackContextsForTitles(ids)
}

// R04-10: Home decorates its cards with their Episode context in bulk reads, not a
// query per card, and the decoration is the same.
func TestHomeReadsEpisodeContextsInBulk(t *testing.T) {
	f := newRescanFixture(t, "Season 01/Show - S01E01.mkv", "Season 01/Show - S01E02.mkv")
	_, _, want, err := catalog.NewService(f.db, t.TempDir()).Home(access.AllAccess(), "u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	cs := &contextCountingStore{Store: f.db, db: f.db}
	_, _, got, err := catalog.NewService(cs, t.TempDir()).Home(access.AllAccess(), "u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Titles) != 2 {
		t.Fatalf("recently added = %d cards, want 2", len(got.Titles))
	}
	if cs.perTitle != 0 || cs.bulkReads != 2 {
		t.Errorf("per-Title reads = %d (want 0), bulk reads = %d (want 2)", cs.perTitle, cs.bulkReads)
	}
	for i := range got.Titles {
		if got.Titles[i].Episode == nil || *got.Titles[i].Episode != *want.Titles[i].Episode {
			t.Errorf("card %d episode context = %+v, want %+v", i, got.Titles[i].Episode, want.Titles[i].Episode)
		}
	}
}
