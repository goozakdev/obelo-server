package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Multi-valued Studios (Movie) and Networks (Show): every production company /
// network a provider names is stored in order, beside the single denormalized
// "first" column the current readers still use.

func titleStudios(t *testing.T, db *store.DB, id string) []string {
	t.Helper()
	m, err := db.StudiosForTitles([]string{id})
	if err != nil {
		t.Fatalf("StudiosForTitles: %v", err)
	}
	return m[id]
}

func firstStudioColumn(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT studio FROM titles WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read studio: %v", err)
	}
	return s
}

func TestAMovieStoresEveryStudioInOrder(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Studio: "Legendary", Studios: []string{"Legendary", "Warner Bros.", "Villeneuve Films"}, Source: "tmdb",
	}, nil); err != nil {
		t.Fatalf("WriteTitleEnrichment: %v", err)
	}
	want := []string{"Legendary", "Warner Bros.", "Villeneuve Films"}
	if got := titleStudios(t, db, "m1"); !reflect.DeepEqual(got, want) {
		t.Errorf("studios = %v, want %v", got, want)
	}
	if got := firstStudioColumn(t, db, "m1"); got != "Legendary" {
		t.Errorf("denormalized studio = %q, want the first", got)
	}
}

func TestAStudioOnlyRecordStillStoresThatOneStudio(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{Studio: "A24", Source: "x"}, nil); err != nil {
		t.Fatalf("WriteTitleEnrichment: %v", err)
	}
	if got := titleStudios(t, db, "m1"); !reflect.DeepEqual(got, []string{"A24"}) {
		t.Errorf("studios = %v, want [A24]", got)
	}
}

func TestEachPassReplacesTheStudioSet(t *testing.T) {
	db := seedTitle(t, "m1")
	for _, s := range [][]string{{"A", "B", "C"}, {"D"}} {
		if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{Studios: s, Source: "x"}, nil); err != nil {
			t.Fatalf("WriteTitleEnrichment: %v", err)
		}
	}
	if got := titleStudios(t, db, "m1"); !reflect.DeepEqual(got, []string{"D"}) {
		t.Errorf("studios = %v, want [D]", got)
	}
	if got := firstStudioColumn(t, db, "m1"); got != "D" {
		t.Errorf("denormalized studio = %q, want D", got)
	}
}

func TestALockedStudioKeepsTheWholeSet(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{Studios: []string{"A", "B"}, Source: "x"}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{Studios: []string{"Z"}, Source: "x"},
		map[string]bool{"studio": true}); err != nil {
		t.Fatalf("locked write: %v", err)
	}
	if got := titleStudios(t, db, "m1"); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("locked studios = %v, want [A B] untouched", got)
	}
	if got := firstStudioColumn(t, db, "m1"); got != "A" {
		t.Errorf("locked studio column = %q, want A", got)
	}
}

func TestAHandEditedStudioBecomesTheWholeSet(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{Studios: []string{"A", "B"}, Source: "x"}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mine := "Mine"
	if err := db.WriteTitleMetadata("m1", store.MetadataEdit{Studio: &mine}); err != nil {
		t.Fatalf("WriteTitleMetadata: %v", err)
	}
	if got := titleStudios(t, db, "m1"); !reflect.DeepEqual(got, []string{"Mine"}) {
		t.Errorf("studios after hand edit = %v, want [Mine]", got)
	}
}

func TestAShowStoresEveryNetwork(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('sh1', 'lib', 'Show', 'show', 'show')`)

	if err := db.WriteEntityEnrichment(store.EntityShow, "sh1", store.EntityEnrichmentWrite{
		Network: "HBO", Networks: []string{"HBO", "Max"}, Source: "tmdb",
	}, nil); err != nil {
		t.Fatalf("WriteEntityEnrichment: %v", err)
	}
	got, err := db.NetworksForEntities(store.EntityShow, []string{"sh1"})
	if err != nil {
		t.Fatalf("NetworksForEntities: %v", err)
	}
	if !reflect.DeepEqual(got["sh1"], []string{"HBO", "Max"}) {
		t.Errorf("networks = %v, want [HBO Max]", got["sh1"])
	}

	// A network-only record still stores that one value; a locked network keeps the set.
	if err := db.WriteEntityEnrichment(store.EntityShow, "sh1", store.EntityEnrichmentWrite{Network: "Z", Source: "x"},
		map[string]bool{"network": true}); err != nil {
		t.Fatalf("locked write: %v", err)
	}
	got, _ = db.NetworksForEntities(store.EntityShow, []string{"sh1"})
	if !reflect.DeepEqual(got["sh1"], []string{"HBO", "Max"}) {
		t.Errorf("locked networks = %v, want untouched", got["sh1"])
	}
	if err := db.WriteEntityEnrichment(store.EntityShow, "sh1", store.EntityEnrichmentWrite{Network: "Solo", Source: "x"}, nil); err != nil {
		t.Fatalf("solo write: %v", err)
	}
	got, _ = db.NetworksForEntities(store.EntityShow, []string{"sh1"})
	if !reflect.DeepEqual(got["sh1"], []string{"Solo"}) {
		t.Errorf("network-only networks = %v, want [Solo]", got["sh1"])
	}
}

// The migration carries every existing non-empty studio / network into the new
// tables. The new tables are dropped and the migration forgotten so Migrate runs
// 0006 again over a database that already holds single-valued rows.
func TestTheMigrationCarriesExistingStudiosAndNetworks(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lm', 'Movies', 'movie'), ('lt', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, studio) VALUES
		('t1', 'lm', 'movie', 'A', 'a', 'a', 'Legendary'), ('t2', 'lm', 'movie', 'B', 'b', 'b', '')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('s1', 'lt', 'S', 's', 's')`)
	mustExec(t, db, `INSERT INTO entity_enrichment (entity_type, entity_id, network) VALUES ('show', 's1', 'HBO')`)
	mustExec(t, db, `DROP TABLE title_studios`)
	mustExec(t, db, `DROP TABLE entity_networks`)
	mustExec(t, db, `DELETE FROM schema_migrations WHERE version = '0006_studios_networks'`)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got := titleStudios(t, db, "t1"); !reflect.DeepEqual(got, []string{"Legendary"}) {
		t.Errorf("t1 studios = %v, want [Legendary]", got)
	}
	if got := titleStudios(t, db, "t2"); len(got) != 0 {
		t.Errorf("t2 (no studio) studios = %v, want none", got)
	}
	nets, _ := db.NetworksForEntities(store.EntityShow, []string{"s1"})
	if !reflect.DeepEqual(nets["s1"], []string{"HBO"}) {
		t.Errorf("s1 networks = %v, want [HBO]", nets["s1"])
	}
}

// A mirror lands the sharer's whole Studio list; a sharer that predates the list
// (only `studio`) still lands that one value.
func TestTheMirrorCarriesTheStudioList(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")
	feed := []store.MirrorEntity{
		{Type: store.ExportTitle, RemoteID: "t1", Data: map[string]any{
			"title": "A", "identityKey": "a", "sortTitle": "a",
			"studio": "S1", "studios": []any{"S1", "S2"},
		}},
		{Type: store.ExportTitle, RemoteID: "t2", Data: map[string]any{
			"title": "B", "identityKey": "b", "sortTitle": "b", "studio": "Old",
		}},
	}
	if err := db.ApplyMirror(lib.ID, feed, true); err != nil {
		t.Fatalf("ApplyMirror: %v", err)
	}
	var id1, id2 string
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id = 't1'`).Scan(&id1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id = 't2'`).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	if got := titleStudios(t, db, id1); !reflect.DeepEqual(got, []string{"S1", "S2"}) {
		t.Errorf("mirrored studios = %v, want [S1 S2]", got)
	}
	if got := titleStudios(t, db, id2); !reflect.DeepEqual(got, []string{"Old"}) {
		t.Errorf("old-sharer studios = %v, want [Old]", got)
	}
}
