package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The bulk readers answer what the per-path ones answer.
func TestStoredFilesByLibraryMatchesLoadStoredFile(t *testing.T) {
	db := ambiguousFixture(t)
	files := []store.File{
		{ID: "f1", Path: "/m/A (2001)/A (2001).mkv", Streams: []store.Stream{
			{ID: "s1", Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080},
			{ID: "s2", Index: 1, Kind: "audio", Codec: "aac", Language: "eng", IsDefault: true, Channels: 2},
		}},
		{ID: "f2", Path: "/m/B (2002)/B (2002).mkv"},
	}
	for i, f := range files {
		id := "t" + string(rune('1'+i))
		if err := db.UpsertTitleTree(ambiguousTree(id, id+"|k", id, false, []store.File{f})); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.StoredFilesByLibrary("libmov")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d files, want 2", len(got))
	}
	for _, f := range files {
		want, err := db.LoadStoredFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[f.Path], want) {
			t.Errorf("%s: bulk = %+v, per-path = %+v", f.Path, got[f.Path], want)
		}
	}
	if other, err := db.StoredFilesByLibrary("nope"); err != nil || len(other) != 0 {
		t.Errorf("other library = %v, %v; want empty", other, err)
	}
}

// A multi-episode file is one path under two Titles (two files rows): the bulk read
// answers one row with its own Streams, as LoadStoredFile does, not both merged.
func TestStoredFilesByLibraryWithAPathUnderTwoTitles(t *testing.T) {
	db := ambiguousFixture(t)
	path := "/m/Range/S01E01-E02.mkv"
	for i, id := range []string{"t1", "t2"} {
		f := store.File{ID: "f" + id, Path: path, Streams: []store.Stream{
			{ID: "s-" + id, Index: 0, Kind: "video", Codec: "h264"},
		}}
		if err := db.UpsertTitleTree(ambiguousTree(id, id+"|k", id, false, []store.File{f})); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	got, err := db.StoredFilesByLibrary("libmov")
	if err != nil {
		t.Fatal(err)
	}
	want, err := db.LoadStoredFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[path], want) {
		t.Errorf("bulk = %+v, per-path = %+v", got[path], want)
	}
	if len(got[path].Streams) != 1 {
		t.Errorf("streams = %d, want the one row's 1", len(got[path].Streams))
	}
}

func TestLocalMarkersByLibrary(t *testing.T) {
	db, path := markerFixture(t)
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 70_000},
		{Kind: "credits", Source: "local", StartMs: 540_000, EndMs: 600_000},
	}
	if err := db.ReplaceEDLMarkers(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := db.LocalMarkersByLibrary("libmov")
	if err != nil {
		t.Fatal(err)
	}
	st, ok := got[path]
	if !ok || !st.FromEDL || !reflect.DeepEqual(st.Markers, want) {
		t.Errorf("state = %+v (%v), want FromEDL with %v", st, ok, want)
	}
	if !store.SameMarkerSpans(st.Markers, []store.Marker{want[1], want[0]}) {
		t.Error("SameMarkerSpans should ignore order")
	}
	if store.SameMarkerSpans(st.Markers, want[:1]) {
		t.Error("SameMarkerSpans accepted a shorter list")
	}
}

func TestAddUnmatchedKeepsTheRest(t *testing.T) {
	db := ambiguousFixture(t)
	if err := db.ReplaceUnmatched("libmov", []store.UnmatchedFile{{ID: "u1", Path: "/m/x.mkv", Reason: "r"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUnmatched("libmov", []store.UnmatchedFile{
		{ID: "u2", Path: "/m/y.mkv", Kind: store.UnmatchedUnreadable, Reason: "bad"},
		{ID: "u3", Path: "/m/x.mkv", Kind: store.UnmatchedUnreadable, Reason: "now bad"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListUnmatched("libmov")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "/m/x.mkv" || got[0].Kind != store.UnmatchedUnreadable ||
		got[0].Reason != "now bad" || got[1].Path != "/m/y.mkv" {
		t.Errorf("unmatched = %+v", got)
	}
}
