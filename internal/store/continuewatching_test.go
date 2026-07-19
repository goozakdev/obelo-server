package store_test

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestContinueWatchingVideoOnly confirms Continue Watching is video only: a movie
// and an episode with an in-progress resume surface, but a music track carrying
// its own resume never does. Music is ambient, not "watched" — the kind predicate
// on the query is the single point that keeps Tracks out of the row.
func TestContinueWatchingVideoOnly(t *testing.T) {
	db := openTemp(t)

	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov','Movies','movie')`)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u1','u1','member')`)

	// A movie, an episode, and a music track — each with an in-progress resume.
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		 VALUES ('mv1','libmov','movie','Movie','mv|1','movie')`)
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		 VALUES ('ep1','libmov','episode','Episode','ep|1','episode')`)
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		 VALUES ('tr1','libmus','track','Track','tr|1','track')`)

	setWS(t, db, "mv1", 30000, false, "2025-01-01T00:00:00.000Z")
	setWS(t, db, "ep1", 30000, false, "2025-01-01T00:00:00.000Z")
	setWS(t, db, "tr1", 30000, false, "2025-01-01T00:00:00.000Z")

	rows, err := db.ContinueWatching("u1", 20, store.AllAccess())
	if err != nil {
		t.Fatalf("ContinueWatching: %v", err)
	}

	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got["mv1"] {
		t.Errorf("movie mv1 missing from Continue Watching, want present")
	}
	if !got["ep1"] {
		t.Errorf("episode ep1 missing from Continue Watching, want present")
	}
	if got["tr1"] {
		t.Errorf("music track tr1 present in Continue Watching, want excluded")
	}
}
