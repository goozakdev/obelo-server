package enrich

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// parentReadCountingStore counts the per-parent enrichment reads a pass makes, and
// passes the optional pending read through to the real store.
type parentReadCountingStore struct {
	Store
	db    *store.DB
	reads int
}

func (s *parentReadCountingStore) EntityEnrichmentByID(entityType, entityID string) (store.EntityEnrichment, error) {
	s.reads++
	return s.Store.EntityEnrichmentByID(entityType, entityID)
}

func (s *parentReadCountingStore) EnrichmentPending(libraryID string) (bool, []string, error) {
	return s.db.EnrichmentPending(libraryID)
}

// R04-15: a ModeNew pass over a TV Library with nothing pending (and no retry due)
// does not walk the tree, and one with a pending Episode still does and enriches it.
func TestModeNewSkipsTheWalkWhenNothingIsPending(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title, tmdb_id)
	           VALUES ('sh1', 'tv', 'Game of Thrones', 'got', 'got', '1399')`)
	f.exec(t, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se1', 'sh1', 1, 'got|s01')`)
	f.exec(t, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	             season_id, season_number, episode_number)
	           VALUES ('ep1', 'tv', 'episode', 'Winter Is Coming', 'got|e1', 'winter', 'se1', 1, 1)`)
	cs := &parentReadCountingStore{Store: f.svc.store, db: f.db}
	f.svc.store = cs

	f.pass(t, "tv", ModeNew) // enriches everything
	if got := f.title(t, "ep1"); got.EnrichmentStatus != "matched" {
		t.Fatalf("first pass left the Episode %q, want matched", got.EnrichmentStatus)
	}
	cs.reads = 0
	f.pass(t, "tv", ModeNew)
	if cs.reads != 0 {
		t.Errorf("a ModeNew pass with nothing pending read %d parent rows, want 0", cs.reads)
	}

	// A newly scanned Episode is pending: the walk runs and enriches it.
	f.exec(t, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	             season_id, season_number, episode_number)
	           VALUES ('ep2', 'tv', 'episode', 'The Kingsroad', 'got|e2', 'kingsroad', 'se1', 1, 2)`)
	tmdb.byName["The Kingsroad"] = "1399"
	f.pass(t, "tv", ModeNew)
	if cs.reads == 0 {
		t.Error("a ModeNew pass with a pending Episode skipped its walk")
	}
	if got := f.title(t, "ep2"); got.EnrichmentStatus != "matched" {
		t.Errorf("the new Episode is %q, want matched", got.EnrichmentStatus)
	}

	// A failed Episode whose retry has not arrived does not wake the walk; one whose
	// retry is due does.
	future := f.svc.clock().Add(time.Hour).UTC().Format(time.RFC3339)
	f.exec(t, `UPDATE titles SET enrichment_status = 'failed', enrichment_retry_at = ? WHERE id = 'ep2'`, future)
	cs.reads = 0
	f.pass(t, "tv", ModeNew)
	if cs.reads != 0 {
		t.Errorf("a retry not yet due woke the walk (%d reads)", cs.reads)
	}
	past := f.svc.clock().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.exec(t, `UPDATE titles SET enrichment_retry_at = ? WHERE id = 'ep2'`, past)
	f.pass(t, "tv", ModeNew)
	if cs.reads == 0 {
		t.Error("a retry that is due did not wake the walk")
	}
}
