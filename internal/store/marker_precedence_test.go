package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Marker precedence at read time (ADR-0065 §1): Local > Detected > Fetched. A
// lower source's Marker is served only where no higher source has one of that
// kind for the File, and never where it overlaps one a higher source kept.

func insertMarker(t *testing.T, db *store.DB, id, path string, m store.Marker) {
	t.Helper()
	mustExec(t, db, `INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		VALUES (?, ?, ?, ?, ?, ?)`, id, path, m.Kind, m.Source, m.StartMs, m.EndMs)
}

func markersOf(t *testing.T, db *store.DB) []store.Marker {
	t.Helper()
	got, err := db.MarkersForFile("f1")
	if err != nil {
		t.Fatalf("MarkersForFile: %v", err)
	}
	return got
}

// TestALocalIntroOutranksAFetchedIntro: a Fetched Intro is left out wherever it
// is timed, because the File's own Intro says where the Intro is; a Fetched
// Credits nothing else covers is served beside it.
func TestALocalIntroOutranksAFetchedIntro(t *testing.T) {
	db, path := markerFixture(t)
	local := store.Marker{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 70_000}
	credits := store.Marker{Kind: "credits", Source: "fetched", StartMs: 540_000, EndMs: 600_000}
	insertMarker(t, db, "l1", path, local)
	insertMarker(t, db, "f-overlap", path, store.Marker{Kind: "intro", Source: "fetched", StartMs: 12_000, EndMs: 72_000})
	insertMarker(t, db, "f-apart", path, store.Marker{Kind: "intro", Source: "fetched", StartMs: 200_000, EndMs: 260_000})
	insertMarker(t, db, "f-credits", path, credits)

	if got, want := markersOf(t, db), []store.Marker{local, credits}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
}

// TestADetectedRecapOutranksAFetchedRecap: the Server's own measurement of this
// File beats a provider's measurement of some other copy, even where the two do
// not overlap.
func TestADetectedRecapOutranksAFetchedRecap(t *testing.T) {
	db, path := markerFixture(t)
	detected := store.Marker{Kind: "recap", Source: "detected", StartMs: 0, EndMs: 30_000}
	insertMarker(t, db, "d1", path, detected)
	insertMarker(t, db, "f1", path, store.Marker{Kind: "recap", Source: "fetched", StartMs: 200_000, EndMs: 230_000})

	if got, want := markersOf(t, db), []store.Marker{detected}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want only the detected recap %+v", got, want)
	}
}

// TestALocalMarkerOutranksADetectedOneOfItsKind: the same rule one rank up — a
// Detected Credits apart from the File's own Credits is still left out.
func TestALocalMarkerOutranksADetectedOneOfItsKind(t *testing.T) {
	db, path := markerFixture(t)
	local := store.Marker{Kind: "credits", Source: "local", StartMs: 540_000, EndMs: 600_000}
	intro := store.Marker{Kind: "intro", Source: "detected", StartMs: 10_000, EndMs: 70_000}
	insertMarker(t, db, "l1", path, local)
	insertMarker(t, db, "d1", path, intro)
	insertMarker(t, db, "d2", path, store.Marker{Kind: "credits", Source: "detected", StartMs: 400_000, EndMs: 450_000})

	if got, want := markersOf(t, db), []store.Marker{intro, local}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
}

// TestAFetchedMarkerIsServedWhereNothingElseCoversItsKind: with only a Fetched
// Intro, the Fetched Intro is the answer.
func TestAFetchedMarkerIsServedWhereNothingElseCoversItsKind(t *testing.T) {
	db, path := markerFixture(t)
	fetched := store.Marker{Kind: "intro", Source: "fetched", StartMs: 5_000, EndMs: 65_000}
	insertMarker(t, db, "f1", path, fetched)

	if got, want := markersOf(t, db), []store.Marker{fetched}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
}

// TestAFetchedMarkerOverlappingAHigherOneIsLeftOut: a Fetched Recap its kind
// alone would allow still loses to the Detected Intro it overlaps — the span is
// already spoken for.
func TestAFetchedMarkerOverlappingAHigherOneIsLeftOut(t *testing.T) {
	db, path := markerFixture(t)
	intro := store.Marker{Kind: "intro", Source: "detected", StartMs: 10_000, EndMs: 70_000}
	insertMarker(t, db, "d1", path, intro)
	insertMarker(t, db, "f1", path, store.Marker{Kind: "recap", Source: "fetched", StartMs: 0, EndMs: 20_000})

	if got, want := markersOf(t, db), []store.Marker{intro}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
}
