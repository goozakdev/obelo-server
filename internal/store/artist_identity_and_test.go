package store_test

import "testing"

// Migration 0043 (ADR-0037 amendment) folds the word "and" out of Artist
// identity keys: "Marina and the Diamonds" merges into the row "Marina & the
// Diamonds" already produced (the ampersand collapsed at normalization, so the
// "and"-less key is the only derivable canonical form). Same replay technique
// as the 0042 test: migrate fully, un-record 0043, seed post-0042-shape rows,
// migrate again.
func TestArtistIdentityAndMigration(t *testing.T) {
	db := openTemp(t)

	if _, err := db.Exec(
		`DELETE FROM schema_migrations WHERE version = '0043_artist_identity_and'`); err != nil {
		t.Fatalf("un-record 0043: %v", err)
	}

	seeds := []string{
		`INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Music', 'music')`,

		// The split pair: the word spelling and the (already-collapsed) "&" one.
		`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		 VALUES ('a-and', 'lib1', 'Marina and the Diamonds', 'artist:marina and the diamonds', 'marina and the diamonds')`,
		`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		 VALUES ('a-amp', 'lib1', 'Marina & the Diamonds', 'artist:marina the diamonds', 'marina & the diamonds')`,
		// Word-form only, no "&" counterpart: rekeyed in place. The drop also
		// exposes no article here — plain word removal.
		`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		 VALUES ('a-one', 'lib1', 'And One', 'artist:and one', 'and one')`,
		// A name that is ONLY "and": must be left completely alone.
		`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		 VALUES ('a-bare-and', 'lib1', 'And', 'artist:and', 'and')`,

		`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		 VALUES ('al-jewels', 'a-and', 'The Family Jewels', 'artist:marina and the diamonds|album:the family jewels', 'family jewels')`,
		`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		 VALUES ('al-electra', 'a-amp', 'Electra Heart', 'artist:marina the diamonds|album:electra heart', 'electra heart')`,
		`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
		 VALUES ('al-anguish', 'a-one', 'Anguish', 'artist:and one|album:anguish', 'anguish')`,

		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		 VALUES ('t1', 'lib1', 'track', 'Hollywood', 'artist:marina and the diamonds|album:the family jewels|d01t01:hollywood', 'hollywood', 'al-jewels', 1, 1)`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		 VALUES ('t2', 'lib1', 'track', 'Primadonna', 'artist:marina the diamonds|album:electra heart|d01t01:primadonna', 'primadonna', 'al-electra', 1, 1)`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number)
		 VALUES ('t3', 'lib1', 'track', 'Sometimes', 'artist:and one|album:anguish|d01t01:sometimes', 'sometimes', 'al-anguish', 1, 1)`,
	}
	for _, q := range seeds {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	assertRow := func(desc, query, want string) {
		t.Helper()
		var got string
		if err := db.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", desc, got, want)
		}
	}

	// The "&" row (already holding the canonical key) survives; the word row is
	// merged away and its album moved with a rewritten prefix.
	assertRow("survivor key", `SELECT identity_key FROM artists WHERE id = 'a-amp'`,
		"artist:marina the diamonds")
	assertRow("loser deleted", `SELECT COUNT(*) FROM artists WHERE id = 'a-and'`, "0")
	assertRow("moved album",
		`SELECT artist_id || '|' || identity_key FROM albums WHERE id = 'al-jewels'`,
		"a-amp|artist:marina the diamonds|album:the family jewels")
	assertRow("moved track", `SELECT identity_key FROM titles WHERE id = 't1'`,
		"artist:marina the diamonds|album:the family jewels|d01t01:hollywood")
	assertRow("survivor's own album untouched",
		`SELECT identity_key FROM albums WHERE id = 'al-electra'`,
		"artist:marina the diamonds|album:electra heart")

	// Word-form with no counterpart: rekeyed in place, same row id, subtree keys
	// rewritten.
	assertRow("in-place rekey", `SELECT identity_key FROM artists WHERE id = 'a-one'`,
		"artist:one")
	assertRow("in-place album", `SELECT identity_key FROM albums WHERE id = 'al-anguish'`,
		"artist:one|album:anguish")
	assertRow("in-place track", `SELECT identity_key FROM titles WHERE id = 't3'`,
		"artist:one|album:anguish|d01t01:sometimes")

	// A band literally named "And" is untouched.
	assertRow("bare 'and' untouched", `SELECT identity_key FROM artists WHERE id = 'a-bare-and'`,
		"artist:and")
}
