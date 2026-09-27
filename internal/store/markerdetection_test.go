package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Marker detection's store half (ADR-0065 §4): the per-Library toggle, the
// Season listing detection walks, and Detected Markers written beside — and
// ranked below — the Local ones.

// TestLocalMarkerWinsOverDetectedOnTheSameSpan: where a Local and a Detected
// Marker cover the same span, only the Local one is served; a Detected Marker
// elsewhere in the File still is.
func TestLocalMarkerWinsOverDetectedOnTheSameSpan(t *testing.T) {
	db, path := markerFixture(t)
	if err := db.ReplaceLocalMarkers(path, []store.Marker{{Kind: "intro", StartMs: 10_000, EndMs: 70_000}}); err != nil {
		t.Fatalf("ReplaceLocalMarkers: %v", err)
	}
	if err := db.SaveDetectedMarkers(path, []store.Marker{
		{Kind: "intro", StartMs: 11_500, EndMs: 69_000},
		{Kind: "credits", StartMs: 540_000, EndMs: 600_000},
	}); err != nil {
		t.Fatalf("SaveDetectedMarkers: %v", err)
	}
	got, err := db.MarkersForFile("f1")
	if err != nil {
		t.Fatalf("MarkersForFile: %v", err)
	}
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 70_000},
		{Kind: "credits", Source: "detected", StartMs: 540_000, EndMs: 600_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want the Local intro and the unshadowed Detected credits %+v", got, want)
	}
}

// TestSaveDetectedMarkersLeavesLocal: a detection run replaces only its own rows.
func TestSaveDetectedMarkersLeavesLocal(t *testing.T) {
	db, path := markerFixture(t)
	if err := db.ReplaceLocalMarkers(path, []store.Marker{{Kind: "recap", StartMs: 0, EndMs: 5_000}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDetectedMarkers(path, []store.Marker{{Kind: "credits", StartMs: 500_000, EndMs: 560_000}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDetectedMarkers(path, nil); err != nil {
		t.Fatal(err)
	}
	got, err := db.MarkersForFile("f1")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Marker{{Kind: "recap", Source: "local", StartMs: 0, EndMs: 5_000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %+v, want only the Local recap %+v", got, want)
	}
}

// TestMarkerDetectionToggleExistsOnlyForTV: a TV Library is on by default and
// can be turned off; a music (or movie) Library has no toggle at all.
func TestMarkerDetectionToggleExistsOnlyForTV(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'Shows', 'tv'), ('mu', 'Music', 'music'), ('mo', 'Movies', 'movie')`)

	on, err := db.MarkerDetectionEnabled("tv")
	if err != nil || !on {
		t.Fatalf("TV default = %v, %v; want on", on, err)
	}
	if err := db.SetMarkerDetectionEnabled("tv", false); err != nil {
		t.Fatal(err)
	}
	if on, err := db.MarkerDetectionEnabled("tv"); err != nil || on {
		t.Fatalf("TV after turning off = %v, %v; want off", on, err)
	}
	for _, id := range []string{"mu", "mo"} {
		if _, err := db.MarkerDetectionEnabled(id); !errors.Is(err, store.ErrNoMarkerDetection) {
			t.Errorf("%s: read err = %v, want ErrNoMarkerDetection", id, err)
		}
		if err := db.SetMarkerDetectionEnabled(id, true); !errors.Is(err, store.ErrNoMarkerDetection) {
			t.Errorf("%s: set err = %v, want ErrNoMarkerDetection", id, err)
		}
	}
	if _, err := db.MarkerDetectionEnabled("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown library err = %v, want ErrNotFound", err)
	}
}

// TestDetectionSeasonsListsEpisodeFilesAndRemembersAnalysis: Seasons come back
// one per row group with their Episodes' Files in order, and a File listened to
// at its current mtime reads as analysed until it changes.
func TestDetectionSeasonsListsEpisodeFilesAndRemembersAnalysis(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'Shows', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('sh', 'tv', 'Show', 'show', 'show')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s1', 'sh', 1, 'show|s01'), ('s2', 'sh', 2, 'show|s02')`)
	for _, ep := range []struct {
		id, season, path string
		n                int
	}{
		{"e2", "s1", "/tv/S01E02.mkv", 2}, {"e1", "s1", "/tv/S01E01.mkv", 1}, {"e3", "s2", "/tv/S02E01.mkv", 1},
	} {
		mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, episode_number)
			VALUES (?, 'tv', 'episode', ?, ?, ?, ?, ?)`, ep.id, ep.id, ep.id, ep.id, ep.season, ep.n)
		mustExec(t, db, `INSERT INTO editions (id, title_id) VALUES (?, ?)`, "ed-"+ep.id, ep.id)
		mustExec(t, db, `INSERT INTO files (id, edition_id, path, duration_ms, mtime) VALUES (?, ?, ?, 60000, 'm1')`,
			"f-"+ep.id, "ed-"+ep.id, ep.path)
	}
	mustExec(t, db, `INSERT INTO streams (id, file_id, stream_index, kind, channels) VALUES
		('st-v', 'f-e1', 0, 'video', 0), ('st-a2', 'f-e1', 2, 'audio', 6), ('st-a1', 'f-e1', 1, 'audio', 2)`)
	if err := db.SaveDetectedMarkers("/tv/S01E01.mkv", nil); err != nil {
		t.Fatal(err)
	}
	got, err := db.DetectionSeasonsOfShow("sh")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.DetectionSeason{
		{ID: "s1", ShowID: "sh", Files: []store.DetectionFile{
			{TitleID: "e1", Path: "/tv/S01E01.mkv", DurationMs: 60_000, Analyzed: true, AudioChannels: 2},
			{TitleID: "e2", Path: "/tv/S01E02.mkv", DurationMs: 60_000},
		}},
		{ID: "s2", ShowID: "sh", Files: []store.DetectionFile{{TitleID: "e3", Path: "/tv/S02E01.mkv", DurationMs: 60_000}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seasons = %+v\nwant %+v", got, want)
	}
	mustExec(t, db, `UPDATE files SET mtime = 'm2' WHERE path = '/tv/S01E01.mkv'`)
	lib, err := db.DetectionSeasonsOfLibrary("tv")
	if err != nil {
		t.Fatal(err)
	}
	if lib[0].Files[0].Analyzed {
		t.Error("a File changed since it was listened to still reads as analysed")
	}
	if _, err := db.DetectionSeasonsOfShow("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown show err = %v, want ErrNotFound", err)
	}
}

// TestDetectionSeasonsCountConsecutiveDecodeFailures: each failure to decode a
// File at its current mtime adds one, a decode clears the count, and a File that
// changed starts again from nothing.
func TestDetectionSeasonsCountConsecutiveDecodeFailures(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'Shows', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('sh', 'tv', 'Show', 'show', 'show')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s1', 'sh', 1, 'show|s01')`)
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, episode_number)
		VALUES ('e1', 'tv', 'episode', 'e1', 'e1', 'e1', 's1', 1)`)
	mustExec(t, db, `INSERT INTO editions (id, title_id) VALUES ('ed', 'e1')`)
	mustExec(t, db, `INSERT INTO files (id, edition_id, path, duration_ms, mtime) VALUES ('f', 'ed', '/tv/S01E01.mkv', 60000, 'm1')`)
	const path = "/tv/S01E01.mkv"
	failures := func() int {
		t.Helper()
		ss, err := db.DetectionSeasonsOfShow("sh")
		if err != nil {
			t.Fatal(err)
		}
		return ss[0].Files[0].DecodeFailures
	}
	record := func(decoded bool) {
		t.Helper()
		if err := db.RecordDecode(path, decoded); err != nil {
			t.Fatal(err)
		}
	}

	record(false)
	record(false)
	if n := failures(); n != 2 {
		t.Fatalf("failures after two = %d, want 2", n)
	}
	record(true)
	if n := failures(); n != 0 {
		t.Fatalf("failures after a decode = %d, want 0", n)
	}
	record(false)
	record(false)
	record(false)
	if n := failures(); n != 3 {
		t.Fatalf("failures after three = %d, want 3", n)
	}
	mustExec(t, db, `UPDATE files SET mtime = 'm2' WHERE path = ?`, path)
	if n := failures(); n != 0 {
		t.Fatalf("failures of a changed File = %d, want 0", n)
	}
	record(false)
	if n := failures(); n != 1 {
		t.Fatalf("failures after one at the new mtime = %d, want 1", n)
	}
}
