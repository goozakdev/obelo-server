package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// slotCountingStore counts the Episode-slot reads a catalog call makes.
type slotCountingStore struct {
	catalog.Store
	slotReads int
}

func (s *slotCountingStore) ShowEpisodeSlots(showID string) ([]store.EpisodeSlot, error) {
	s.slotReads++
	return s.Store.ShowEpisodeSlots(showID)
}

// R04-09: the Needs-Fixing queue never reads a Show's Episode slots, so it must
// not pay a query per Show for them.
func TestShowProblemsSkipsEpisodeSlotReads(t *testing.T) {
	f := newRescanFixture(t, "Season 01/Show - S01E01.mkv", "Season 01/Show - S01E02.mkv")
	cs := &slotCountingStore{Store: f.db}
	cat := catalog.NewService(cs, t.TempDir())

	if _, err := cat.ShowProblems("libtv"); err != nil {
		t.Fatalf("show problems: %v", err)
	}
	if cs.slotReads != 0 {
		t.Errorf("ShowProblems read Episode slots %d times, want 0", cs.slotReads)
	}

	// The matcher DOES need them: the guard must not have been removed from it.
	if _, err := cat.ShowMatcher(context.Background(), f.showID, nil); err != nil {
		t.Fatalf("show matcher: %v", err)
	}
	if cs.slotReads == 0 {
		t.Errorf("ShowMatcher read no Episode slots; it needs them")
	}
}

// R04-12: a scoped user must get ErrNotFound for a Season or Album outside their
// Library scope even when it has no children, rather than its header.
func TestEpisodesOfAnOutOfScopeEmptySeasonIsNotFound(t *testing.T) {
	f := newRescanFixture(t, "Season 01/Show - S01E01.mkv")
	mustExec(t, f.db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s-empty', ?, 9, 'empty|s09')`, f.showID)
	out := access.Scope{LibraryIDs: []string{"some-other-library"}}
	if _, _, err := f.cat.Episodes(out, "s-empty"); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("Episodes err = %v, want ErrNotFound for an out-of-scope empty Season", err)
	}
	in := access.Scope{LibraryIDs: []string{"libtv"}}
	if _, _, err := f.cat.Episodes(in, "s-empty"); err != nil {
		t.Errorf("Episodes in scope err = %v, want nil", err)
	}
}

func TestTracksOfAnOutOfScopeEmptyAlbumIsNotFound(t *testing.T) {
	f := newRescanFixture(t, "Season 01/Show - S01E01.mkv")
	mustExec(t, f.db, `INSERT INTO artists (id, library_id, name, identity_key, sort_name) VALUES ('ar-empty', 'libtv', 'A', 'a', 'a')`)
	mustExec(t, f.db, `INSERT INTO albums (id, artist_id, title, identity_key, sort_title) VALUES ('al-empty', 'ar-empty', 'Al', 'a|al', 'al')`)
	out := access.Scope{LibraryIDs: []string{"some-other-library"}}
	if _, _, _, err := f.cat.Tracks(out, "al-empty"); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("Tracks err = %v, want ErrNotFound for an out-of-scope empty Album", err)
	}
	in := access.Scope{LibraryIDs: []string{"libtv"}}
	if _, _, _, err := f.cat.Tracks(in, "al-empty"); err != nil {
		t.Errorf("Tracks in scope err = %v, want nil", err)
	}
}
