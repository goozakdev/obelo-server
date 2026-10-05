package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A mirror sync must not undo a local display-name edit: a Show, Artist or Album
// whose "title" is Locked keeps its name and sort key when the sharer's feed
// carries another (the scanner's upserts already follow this rule).
func TestMirrorSyncHonoursALockedName(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")

	feed := func(show, artist, album string) []store.MirrorEntity {
		return []store.MirrorEntity{
			{Type: store.ExportShow, RemoteID: "sh1", Data: map[string]any{
				"title": show, "sortTitle": show, "identityKey": "show-1"}},
			{Type: store.ExportArtist, RemoteID: "ar1", Data: map[string]any{
				"name": artist, "sortName": artist, "identityKey": "artist-1"}},
			{Type: store.ExportAlbum, RemoteID: "al1", ParentID: "ar1", Data: map[string]any{
				"title": album, "sortTitle": album, "identityKey": "album-1"}},
		}
	}
	if err := db.ApplyMirror(lib.ID, feed("Show A", "Artist A", "Album A"), true); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	rename := func(entityType, table string) {
		t.Helper()
		var id string
		if err := db.QueryRow(`SELECT id FROM ` + table + ` WHERE remote_id IS NOT NULL`).Scan(&id); err != nil {
			t.Fatalf("finding mirrored %s: %v", entityType, err)
		}
		name := "My " + entityType
		if err := db.WriteEntityMetadata(entityType, id, store.EntityMetadataEdit{Name: &name}); err != nil {
			t.Fatalf("renaming %s: %v", entityType, err)
		}
	}
	rename(store.EntityShow, "shows")
	rename(store.EntityArtist, "artists")
	rename(store.EntityAlbum, "albums")

	if err := db.ApplyMirror(lib.ID, feed("Show B", "Artist B", "Album B"), true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, c := range []struct{ query, want string }{
		{`SELECT title FROM shows`, "My show"},
		{`SELECT name FROM artists`, "My artist"},
		{`SELECT title FROM albums`, "My album"},
	} {
		var got string
		if err := db.QueryRow(c.query).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s = %q (%v) after a re-sync, want the locked %q", c.query, got, err, c.want)
		}
	}
	// Sort keys are kept with the name; an unlocked field still follows the feed.
	var sortKey string
	if err := db.QueryRow(`SELECT sort_title FROM shows`).Scan(&sortKey); err != nil || sortKey == "Show B" {
		t.Errorf("a locked Show's sort_title = %q (%v), want it kept off the feed's", sortKey, err)
	}
}

// The lock is per-entity and per-field: a Show, Artist or Album nobody renamed
// keeps following the sharer's feed — names and sort keys — on a re-sync.
func TestMirrorSyncFollowsTheFeedForAnUnlockedName(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")

	feed := func(show, artist, album string) []store.MirrorEntity {
		return []store.MirrorEntity{
			{Type: store.ExportShow, RemoteID: "sh1", Data: map[string]any{
				"title": show, "sortTitle": "s " + show, "identityKey": "show-1"}},
			{Type: store.ExportArtist, RemoteID: "ar1", Data: map[string]any{
				"name": artist, "sortName": "s " + artist, "identityKey": "artist-1"}},
			{Type: store.ExportAlbum, RemoteID: "al1", ParentID: "ar1", Data: map[string]any{
				"title": album, "sortTitle": "s " + album, "identityKey": "album-1"}},
		}
	}
	if err := db.ApplyMirror(lib.ID, feed("Show A", "Artist A", "Album A"), true); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := db.ApplyMirror(lib.ID, feed("Show B", "Artist B", "Album B"), true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, c := range []struct{ query, want string }{
		{`SELECT title FROM shows`, "Show B"},
		{`SELECT sort_title FROM shows`, "s Show B"},
		{`SELECT name FROM artists`, "Artist B"},
		{`SELECT sort_name FROM artists`, "s Artist B"},
		{`SELECT title FROM albums`, "Album B"},
		{`SELECT sort_title FROM albums`, "s Album B"},
	} {
		var got string
		if err := db.QueryRow(c.query).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s = %q (%v) after a re-sync, want the feed's %q", c.query, got, err, c.want)
		}
	}
}
