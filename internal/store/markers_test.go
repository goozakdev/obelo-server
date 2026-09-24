package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Markers (ADR-0065) are keyed by the File's path, so they must outlive the
// delete-and-reinsert every rescan does to a Title's files rows.

func markerFixture(t *testing.T) (*store.DB, string) {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov', 'Movies', 'movie')`)
	path := "/media/Dune (2021)/Dune (2021).mkv"
	tree := ambiguousTree("t1", "dune|2021", "Dune", false, []store.File{
		{ID: "f1", Path: path, DurationMs: 600_000},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return db, path
}

func TestMarkersSurviveARescan(t *testing.T) {
	db, path := markerFixture(t)
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 70_000},
		{Kind: "credits", Source: "local", StartMs: 540_000, EndMs: 600_000},
	}
	if err := db.ReplaceLocalMarkers(path, want); err != nil {
		t.Fatalf("ReplaceLocalMarkers: %v", err)
	}

	// The rescan rebuilds the Edition under a fresh id; the File keeps its id by path.
	tree := ambiguousTree("t1", "dune|2021", "Dune", false, []store.File{
		{ID: "f-new", Path: path, DurationMs: 600_000},
	})
	tree.Editions[0].ID = "ed-rebuilt"
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	f, err := db.LoadStoredFile(path)
	if err != nil {
		t.Fatalf("LoadStoredFile: %v", err)
	}
	got, err := db.MarkersForFile(f.ID)
	if err != nil {
		t.Fatalf("MarkersForFile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("markers after rescan = %+v, want %+v", got, want)
	}
}

func TestReplaceLocalMarkersLeavesOtherSources(t *testing.T) {
	db, path := markerFixture(t)
	mustExec(t, db, `INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		VALUES ('d1', ?, 'recap', 'detected', 0, 30000)`, path)
	if err := db.ReplaceLocalMarkers(path, []store.Marker{{Kind: "intro", StartMs: 1, EndMs: 2}}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := db.ReplaceLocalMarkers(path, nil); err != nil {
		t.Fatalf("clearing replace: %v", err)
	}
	got, err := db.MarkersForFile("f1")
	if err != nil {
		t.Fatalf("MarkersForFile: %v", err)
	}
	want := []store.Marker{{Kind: "recap", Source: "detected", StartMs: 0, EndMs: 30_000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want only the detected one %+v", got, want)
	}
}

// TestMarkerKindIsClosed: there is no Commercial kind, and the schema says so.
func TestMarkerKindIsClosed(t *testing.T) {
	db, path := markerFixture(t)
	if err := db.ReplaceLocalMarkers(path, []store.Marker{{Kind: "commercial", StartMs: 1, EndMs: 2}}); err == nil {
		t.Fatal("a commercial marker was stored, want the kind CHECK to refuse it")
	}
}
