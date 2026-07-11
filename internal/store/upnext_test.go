package store_test

import (
	"database/sql"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Store-level tests for the resume-point Up Next algorithm (ADR-0028,
// up-next-resume-point/01). They seed a Show with four regular Episodes (e1..e4)
// and one Special (sp1, Season 0) plus explicit watch_state — including the new
// played_at recency — so the anchor / next-after / wrap / exclusion behaviour is
// exercised precisely, free of fixture episode-count limits. The black-box API
// coverage (driving real handlers) lives in internal/api/upnext_test.go.

// seedResumeShow inserts library libtv, user u1, and Show sh1 with Season 1
// (e1..e4) and Season 0 (sp1). Episodes carry no watch_state yet.
func seedResumeShow(t *testing.T, db *store.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u1','u1','member')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('sh1','libtv','Show','sh|1','show')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s1','sh1',1,'sh|1|s01')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s0','sh1',0,'sh|1|s00')`)
	insEp(t, db, "e1", "s1", 1, 1, "e01")
	insEp(t, db, "e2", "s1", 1, 2, "e02")
	insEp(t, db, "e3", "s1", 1, 3, "e03")
	insEp(t, db, "e4", "s1", 1, 4, "e04")
	insEp(t, db, "sp1", "s0", 0, 1, "s00e01")
}

func insEp(t *testing.T, db *store.DB, id, seasonID string, seasonNo, epNo int, sortTitle string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number)
		 VALUES (?, 'libtv', 'episode', ?, ?, ?, ?, ?, ?)`,
		id, id, id, sortTitle, seasonID, seasonNo, epNo)
}

// setWS writes a watch_state row for u1/titleID with an explicit played_at
// ("" → NULL, i.e. a marks-only row that never carries playback recency).
func setWS(t *testing.T, db *store.DB, titleID string, resume int64, watched bool, playedAt string) {
	t.Helper()
	w := 0
	if watched {
		w = 1
	}
	var pa any
	if playedAt != "" {
		pa = playedAt
	}
	mustExec(t, db,
		`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, played_at, updated_at)
		 VALUES (?, 'u1', ?, ?, ?, ?, '2025-01-01T00:00:00.000Z')`,
		"ws-"+titleID, titleID, resume, w, pa)
}

// upNextPick returns the resume-point Episode id UpNext surfaces for sh1 under the
// given filter, or "" when the Show is absent from the Home row.
func upNextPick(t *testing.T, db *store.DB, filter store.AccessFilter) string {
	t.Helper()
	rows, err := db.UpNext("u1", 20, filter)
	if err != nil {
		t.Fatalf("UpNext: %v", err)
	}
	for _, r := range rows {
		if r.ShowID == "sh1" {
			return r.ID
		}
	}
	return ""
}

func TestUpNextResumePoint(t *testing.T) {
	all := store.AllAccess()

	t.Run("not started → absent", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		if got := upNextPick(t, db, all); got != "" {
			t.Errorf("UpNext for an untouched Show = %q, want absent", got)
		}
	})

	t.Run("in-progress anchor excluded from Home (Continue Watching's job)", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e2 is mid-band (resume>0, unwatched) and the most-recently-played → the
		// anchor is in progress, so Home's Up Next drops the whole Show (disjoint
		// from Continue Watching, no double-listing).
		setWS(t, db, "e2", 5000, false, "2025-02-01T00:00:00.000Z")
		if got := upNextPick(t, db, all); got != "" {
			t.Errorf("UpNext with an in-progress anchor = %q, want absent (belongs to Continue Watching)", got)
		}
	})

	t.Run("watched anchor → next after, skip not nagged", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e2 played to completion; e1 never played (a skipped earlier Episode). The
		// anchor is e2 (watched); the resume point is the first unwatched AFTER it =
		// e3 — NOT the skipped e1.
		setWS(t, db, "e2", 0, true, "2025-02-01T00:00:00.000Z")
		if got := upNextPick(t, db, all); got != "e3" {
			t.Errorf("UpNext after playing e2 (e1 skipped) = %q, want e3", got)
		}
	})

	t.Run("wrap surfaces the skipped Episode once, at the end", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e1 skipped (unwatched); e2,e3,e4 and the Special all played+watched, the
		// Special most recently → the anchor is the Special (last in Show order), so
		// the resume point WRAPS to the first unwatched from the start = e1.
		setWS(t, db, "e2", 0, true, "2025-02-01T00:00:00.000Z")
		setWS(t, db, "e3", 0, true, "2025-02-02T00:00:00.000Z")
		setWS(t, db, "e4", 0, true, "2025-02-03T00:00:00.000Z")
		setWS(t, db, "sp1", 0, true, "2025-02-04T00:00:00.000Z")
		if got := upNextPick(t, db, all); got != "e1" {
			t.Errorf("UpNext after reaching the end = %q, want the once-skipped e1 (the wrap)", got)
		}
	})

	t.Run("marks-only Show → first unwatched (a later mark does not move it)", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e3 marked watched with NO played_at (a bookkeeping mark). No anchor, so the
		// Show degenerates to first-unwatched = e1 — a later mark never moves resume
		// forward.
		setWS(t, db, "e3", 0, true, "")
		if got := upNextPick(t, db, all); got != "e1" {
			t.Errorf("UpNext for a marks-only Show = %q, want first-unwatched e1", got)
		}
	})

	t.Run("manual mark does not move the anchor", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e4 played+watched → anchor e4 (last regular Episode). Then e2 marked watched
		// with no played_at. The anchor stays e4, so the resume point is the first
		// unwatched AFTER e4 = the Special. Had the mark moved the anchor to e2, the
		// pick would be e3 — asserting the Special proves the anchor did not move.
		setWS(t, db, "e4", 0, true, "2025-02-01T00:00:00.000Z")
		setWS(t, db, "e2", 0, true, "")
		if got := upNextPick(t, db, all); got != "sp1" {
			t.Errorf("UpNext = %q, want the Special (anchor stayed on e4 despite the manual mark of e2)", got)
		}
	})

	t.Run("manual mark removes the Episode from the unwatched set", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// e1 played+watched → anchor e1, next-after = e2. Marking e2 watched (no
		// playback) advances the resume point past it to e3 without moving the anchor.
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		setWS(t, db, "e2", 0, true, "")
		if got := upNextPick(t, db, all); got != "e3" {
			t.Errorf("UpNext after marking e2 watched = %q, want e3 (advanced past the marked Episode)", got)
		}
	})

	t.Run("Specials deferred to last of the progression", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// The whole regular run is watched (e4 most recent → anchor e4); only the
		// Special remains. Season 0 is deferred, so it surfaces only now.
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		setWS(t, db, "e2", 0, true, "2025-02-02T00:00:00.000Z")
		setWS(t, db, "e3", 0, true, "2025-02-03T00:00:00.000Z")
		setWS(t, db, "e4", 0, true, "2025-02-04T00:00:00.000Z")
		if got := upNextPick(t, db, all); got != "sp1" {
			t.Errorf("UpNext with only the Special left = %q, want sp1", got)
		}
	})

	t.Run("fully watched → drops out", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		for _, id := range []string{"e1", "e2", "e3", "e4", "sp1"} {
			setWS(t, db, id, 0, true, "2025-02-01T00:00:00.000Z")
		}
		if got := upNextPick(t, db, all); got != "" {
			t.Errorf("UpNext for a fully-watched Show = %q, want absent", got)
		}
	})
}

// TestUpNextExclusions covers the hidden/Missing/library/rating drop-outs the
// resume-point query preserves from the old lowest-unwatched query (ADR-0008,
// per-User access + Rating ceiling).
func TestUpNextExclusions(t *testing.T) {
	all := store.AllAccess()

	t.Run("hidden Show drops out", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		mustExec(t, db, `UPDATE shows SET hidden = 1 WHERE id = 'sh1'`)
		if got := upNextPick(t, db, all); got != "" {
			t.Errorf("UpNext for a hidden Show = %q, want absent", got)
		}
	})

	t.Run("all-Missing (hidden) Episodes drop the Show", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		// Every Episode gone Missing → hidden; no visible candidate remains (ADR-0008).
		mustExec(t, db, `UPDATE titles SET hidden = 1 WHERE library_id = 'libtv'`)
		if got := upNextPick(t, db, all); got != "" {
			t.Errorf("UpNext with all Episodes Missing = %q, want absent", got)
		}
	})

	t.Run("resume-point Episode in an inaccessible Library drops the Show", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		// A filter that grants only some OTHER Library → libtv is invisible.
		filter := store.AccessFilter{LibraryIDs: []string{"other"}}
		if got := upNextPick(t, db, filter); got != "" {
			t.Errorf("UpNext for an inaccessible Library = %q, want absent", got)
		}
	})

	t.Run("above-ceiling next Episode is skipped", func(t *testing.T) {
		db := openTemp(t)
		seedResumeShow(t, db)
		// Anchor e1; next-after would be e2, but e2 is above the ceiling, so the pick
		// skips to the next accessible unwatched = e3.
		setWS(t, db, "e1", 0, true, "2025-02-01T00:00:00.000Z")
		mustExec(t, db, `UPDATE titles SET content_rating = 'TV-MA' WHERE id = 'e2'`)
		filter := store.AccessFilter{AllLibraries: true, BlockedRatings: []string{"TV-MA"}}
		if got := upNextPick(t, db, filter); got != "e3" {
			t.Errorf("UpNext with an above-ceiling e2 = %q, want e3 (e2 skipped)", got)
		}
	})
}

// TestSaveWatchStatePlayedAtSplit verifies the load-bearing write-path split
// (ADR-0028): the PLAYBACK path (played=true) stamps played_at and propagates it
// to a multi-episode file's siblings, while the MANUAL path (played=false) never
// sets it — so a mark can never seed the Up Next anchor.
func TestSaveWatchStatePlayedAtSplit(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u1','u1','member')`)

	// A combined-episode file (S04E19-20) → two Episode Titles sharing one path.
	const path = "/media/tv/BSG (2003)/Season 4/BSG (2003) - S04E19-20 - Daybreak.mkv"
	tree := store.ShowTree{
		Show: store.Show{ID: "show1", LibraryID: "libtv", Title: "BSG", Year: 2003, IdentityKey: "bsg|2003", SortTitle: "bsg"},
		Seasons: []store.SeasonTree{{
			SeasonNumber: 4, IdentityKey: "bsg|2003|s04",
			Episodes: []store.EpisodeTree{
				episodeTree("t19", "bsg|2003|s04e19", 4, 19, "fa", path),
				episodeTree("t20", "bsg|2003|s04e20", 4, 20, "fb", path),
			},
		}},
	}
	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("seed show tree: %v", err)
	}

	playedAt := func(titleID string) sql.NullString {
		t.Helper()
		var pa sql.NullString
		if err := db.QueryRow(`SELECT played_at FROM watch_state WHERE user_id = 'u1' AND title_id = ?`, titleID).Scan(&pa); err != nil {
			t.Fatalf("read played_at for %s: %v", titleID, err)
		}
		return pa
	}

	// Manual mark (played=false) on t19: neither it nor its sibling gets a played_at.
	if err := db.SaveWatchState("u1", "t19", 0, true, false); err != nil {
		t.Fatalf("manual SaveWatchState: %v", err)
	}
	if pa := playedAt("t19"); pa.Valid {
		t.Errorf("manual mark stamped played_at on t19 (%q); it must stay NULL", pa.String)
	}
	if pa := playedAt("t20"); pa.Valid {
		t.Errorf("manual mark propagated a played_at to the sibling t20 (%q); it must stay NULL", pa.String)
	}

	// Playback (played=true) on t19: it AND its co-File sibling t20 get a played_at.
	if err := db.SaveWatchState("u1", "t19", 0, true, true); err != nil {
		t.Fatalf("playback SaveWatchState: %v", err)
	}
	if pa := playedAt("t19"); !pa.Valid {
		t.Errorf("playback did not stamp played_at on t19")
	}
	if pa := playedAt("t20"); !pa.Valid {
		t.Errorf("playback did not propagate played_at to the sibling t20")
	}
}
