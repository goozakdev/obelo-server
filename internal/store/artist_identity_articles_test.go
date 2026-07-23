package store_test

import "testing"

// Migration 0042 (ADR-0037) makes Artist identity article-insensitive and
// converges pre-existing rows: "The Smashing Pumpkins" merges into "Smashing
// Pumpkins" (the row already holding the target key survives, keeping its id),
// album/track identity-key prefixes are rewritten, same-titled albums merge with
// their Tracks re-pointed, and an article-only artist with no counterpart is
// rekeyed in place.
//
// The migration is data-only (no schema change), so the test replays it over
// seeded old-shape rows: migrate fully, un-record 0042, seed, migrate again.
func TestArtistIdentityArticlesMigration(t *testing.T) {
	db := openTemp(t)

	if _, err := db.Exec(
		`DELETE FROM schema_migrations WHERE version = '0042_artist_identity_articles'`); err != nil {
		t.Fatalf("un-record 0042: %v", err)
	}

	// Seed the pre-0042 shape (FK order: library → artists → albums → titles).
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Music', 'music')`, nil},

		// The split pair: article spelling + bare spelling.
		{`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		  VALUES ('a-the', 'lib1', 'The Smashing Pumpkins', 'artist:the smashing pumpkins', 'smashing pumpkins')`, nil},
		{`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		  VALUES ('a-bare', 'lib1', 'Smashing Pumpkins', 'artist:smashing pumpkins', 'smashing pumpkins')`, nil},
		// Article-only artist with no bare counterpart: rekeyed in place.
		{`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		  VALUES ('a-cure', 'lib1', 'The Cure', 'artist:the cure', 'cure')`, nil},

		// Album only under the article spelling → moves to the survivor.
		{`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		  VALUES ('al-siamese', 'a-the', 'Siamese Dream', 'artist:the smashing pumpkins|album:siamese dream', 'siamese dream')`, nil},
		// Same album under BOTH spellings → merges into the survivor's copy.
		{`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		  VALUES ('al-gish-the', 'a-the', 'Gish', 'artist:the smashing pumpkins|album:gish', 'gish')`, nil},
		{`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		  VALUES ('al-gish-bare', 'a-bare', 'Gish', 'artist:smashing pumpkins|album:gish', 'gish')`, nil},
		{`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		  VALUES ('al-disint', 'a-cure', 'Disintegration', 'artist:the cure|album:disintegration', 'disintegration')`, nil},

		{`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		  VALUES ('t1', 'lib1', 'track', 'Cherub Rock', 'artist:the smashing pumpkins|album:siamese dream|d01t01:cherub rock', 'cherub rock', 'al-siamese', 1, 1)`, nil},
		// t2/t3: the SAME track ripped under both spellings — t2's rewrite would
		// collide with t3, so OR IGNORE leaves t2 on its old key.
		{`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		  VALUES ('t2', 'lib1', 'track', 'I Am One', 'artist:the smashing pumpkins|album:gish|d01t01:i am one', 'i am one', 'al-gish-the', 1, 1)`, nil},
		{`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		  VALUES ('t3', 'lib1', 'track', 'I Am One', 'artist:smashing pumpkins|album:gish|d01t01:i am one', 'i am one', 'al-gish-bare', 1, 1)`, nil},
		// t4: only under the article spelling — clean rewrite + move.
		{`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		  VALUES ('t4', 'lib1', 'track', 'Siva', 'artist:the smashing pumpkins|album:gish|d01t02:siva', 'siva', 'al-gish-the', 1, 2)`, nil},
		{`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		  VALUES ('t5', 'lib1', 'track', 'Plainsong', 'artist:the cure|album:disintegration|d01t01:plainsong', 'plainsong', 'al-disint', 1, 1)`, nil},

		// Enrichment pinned on the loser moves to the survivor; derived genre
		// rows for the loser are dropped (rebuilt by the next enrich pass).
		{`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
		  VALUES ('artist', 'a-the', 'mbid-1', 'matched')`, nil},
		{`INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('artist', 'a-the', 'alternative rock')`, nil},
	}
	for _, s := range seed {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.q, err)
		}
	}

	// Replay: only 0042 is unapplied, so this runs exactly that migration.
	if err := db.Migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	assertRow := func(desc, query string, want ...string) {
		t.Helper()
		got := make([]string, len(want))
		dest := make([]any, len(want))
		for i := range got {
			dest[i] = &got[i]
		}
		if err := db.QueryRow(query).Scan(dest...); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: col %d = %q, want %q", desc, i, got[i], want[i])
			}
		}
	}
	assertGone := func(desc, query string) {
		t.Helper()
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows remain, want 0", desc, n)
		}
	}

	// The bare-spelling row survives with its id and key; the article row is gone.
	assertRow("survivor artist",
		`SELECT identity_key FROM artists WHERE id = 'a-bare'`, "artist:smashing pumpkins")
	assertGone("loser artist deleted", `SELECT COUNT(*) FROM artists WHERE id = 'a-the'`)
	assertRow("one merged artist per key",
		`SELECT COUNT(*) FROM artists WHERE library_id = 'lib1' AND identity_key = 'artist:smashing pumpkins'`, "1")

	// No counterpart → rekeyed in place, same row id.
	assertRow("in-place rekey", `SELECT identity_key FROM artists WHERE id = 'a-cure'`, "artist:cure")

	// Siamese Dream moved under the survivor with its prefix rewritten.
	assertRow("moved album",
		`SELECT artist_id || '|' || identity_key FROM albums WHERE id = 'al-siamese'`,
		"a-bare|artist:smashing pumpkins|album:siamese dream")
	// Gish merged: the survivor's copy remains, the loser's copy is gone.
	assertGone("merged album deleted", `SELECT COUNT(*) FROM albums WHERE id = 'al-gish-the'`)
	assertRow("surviving gish holds all three tracks",
		`SELECT COUNT(*) FROM titles WHERE album_id = 'al-gish-bare'`, "3")

	// Track keys: rewritten where clean, left on the old key where the duplicate
	// rip collided (t2), untouched where never affected (t3).
	assertRow("t1 rewritten", `SELECT identity_key FROM titles WHERE id = 't1'`,
		"artist:smashing pumpkins|album:siamese dream|d01t01:cherub rock")
	assertRow("t4 rewritten + moved", `SELECT album_id || '|' || identity_key FROM titles WHERE id = 't4'`,
		"al-gish-bare|artist:smashing pumpkins|album:gish|d01t02:siva")
	assertRow("t2 collision keeps old key, still re-pointed",
		`SELECT album_id || '|' || identity_key FROM titles WHERE id = 't2'`,
		"al-gish-bare|artist:the smashing pumpkins|album:gish|d01t01:i am one")
	assertRow("t3 untouched", `SELECT identity_key FROM titles WHERE id = 't3'`,
		"artist:smashing pumpkins|album:gish|d01t01:i am one")
	assertRow("t5 rewritten under in-place rekey", `SELECT identity_key FROM titles WHERE id = 't5'`,
		"artist:cure|album:disintegration|d01t01:plainsong")

	// Pinned enrichment followed the merge; derived genres were dropped.
	assertRow("enrichment moved to survivor",
		`SELECT external_id FROM entity_enrichment WHERE entity_type = 'artist' AND entity_id = 'a-bare'`,
		"mbid-1")
	assertGone("loser enrichment gone",
		`SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'artist' AND entity_id = 'a-the'`)
	assertGone("loser genres dropped",
		`SELECT COUNT(*) FROM entity_genres WHERE entity_type = 'artist' AND entity_id = 'a-the'`)
}
