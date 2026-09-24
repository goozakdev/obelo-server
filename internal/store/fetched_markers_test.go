package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestSaveFetchedMarkersReplacesOnlyFetchedAndRemembersTheQuestion: a second
// save replaces the first answer, leaves the File's own Markers alone, and a
// miss is remembered as one.
func TestSaveFetchedMarkersReplacesOnlyFetchedAndRemembersTheQuestion(t *testing.T) {
	db, path := markerFixture(t)
	if _, asked, err := db.MarkerFetchQuestion(path); err != nil || asked {
		t.Fatalf("MarkerFetchQuestion before any save = %v, %v; want never asked", asked, err)
	}
	local := store.Marker{Kind: "credits", Source: "local", StartMs: 540_000, EndMs: 600_000}
	if err := db.ReplaceLocalMarkers(path, []store.Marker{local}); err != nil {
		t.Fatalf("ReplaceLocalMarkers: %v", err)
	}
	if err := db.SaveFetchedMarkers(path, "q1", []store.Marker{{Kind: "intro", StartMs: 1_000, EndMs: 2_000}}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	recap := store.Marker{Kind: "recap", Source: "fetched", StartMs: 3_000, EndMs: 4_000}
	if err := db.SaveFetchedMarkers(path, "q2", []store.Marker{{Kind: "recap", StartMs: 3_000, EndMs: 4_000}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if got, want := markersOf(t, db), []store.Marker{recap, local}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
	if q, asked, err := db.MarkerFetchQuestion(path); err != nil || !asked || q != "q2" {
		t.Fatalf("MarkerFetchQuestion = %q, %v, %v; want q2", q, asked, err)
	}

	if err := db.SaveFetchedMarkers(path, "q3", nil); err != nil {
		t.Fatalf("saving a miss: %v", err)
	}
	if got, want := markersOf(t, db), []store.Marker{local}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers after a miss = %+v, want %+v", got, want)
	}
	if q, asked, _ := db.MarkerFetchQuestion(path); !asked || q != "q3" {
		t.Fatalf("MarkerFetchQuestion after a miss = %q, %v; want q3", q, asked)
	}
}
