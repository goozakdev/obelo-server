package store_test

import "testing"

// The store half of ADR-0053's amendment: the one fact a recheck reads to decide
// that a `matched` Artist is worth doubting.
//
// It is a COUNT, not a walk, and every case below is a way the count could quietly
// mean something else — a hidden Album the pass never visits, an Album that was
// never enriched at all, an Album belonging to a different Artist. Getting any of
// them wrong either spends a provider request per Artist on a healthy library or
// leaves the sixteen wrong rows unreachable, which are the two failures this
// feature sits between.
func TestArtistHasMatchedAlbum(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
	artist := func(id string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO artists (id, library_id, name, identity_key, sort_name)
		                 VALUES (?, 'lib', ?, ?, ?)`, id, id, "artist:"+id, id)
	}
	album := func(id, artistID string, hidden int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO albums (id, artist_id, title, identity_key, sort_title, hidden)
		                 VALUES (?, ?, ?, ?, ?, ?)`, id, artistID, id, artistID+"|"+id, id, hidden)
	}
	enriched := func(entityID, status string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status)
		                 VALUES ('album', ?, ?)`, entityID, status)
	}

	// The motivating shape: "The Eagles", matched, whose whole discography failed.
	artist("ar_uncorroborated")
	album("al_unmatched", "ar_uncorroborated", 0)
	album("al_failed", "ar_uncorroborated", 0)
	enriched("al_unmatched", "unmatched")
	enriched("al_failed", "failed")

	// One matched Album is enough — corroboration is not a majority vote.
	artist("ar_corroborated")
	album("al_unmatched2", "ar_corroborated", 0)
	album("al_matched", "ar_corroborated", 0)
	enriched("al_unmatched2", "unmatched")
	enriched("al_matched", "matched")

	// Never enriched: no entity_enrichment row at all, which reads 'pending' and is
	// not a match. It must not count as corroboration on the strength of existing.
	artist("ar_never_enriched")
	album("al_no_row", "ar_never_enriched", 0)

	// A hidden Album is one the pass never walks (ADR-0008), so it can neither
	// corroborate its Artist nor hint at it. Counting it would silence the doubt
	// with evidence the Artist's lookup is not allowed to use.
	artist("ar_hidden_match")
	album("al_hidden", "ar_hidden_match", 1)
	enriched("al_hidden", "matched")

	// An empty discography. The FALSE here is what makes the pass's own
	// len(albums) == 0 guard the thing that decides it, not this read.
	artist("ar_no_albums")

	for _, tc := range []struct {
		artist string
		want   bool
		why    string
	}{
		{"ar_uncorroborated", false, "every Album settled without a record — the signature"},
		{"ar_corroborated", true, "one matched Album is corroboration"},
		{"ar_never_enriched", false, "an Album with no enrichment row has not matched"},
		{"ar_hidden_match", false, "a hidden Album is not part of the discography the pass sees"},
		{"ar_no_albums", false, "nothing to corroborate with"},
	} {
		got, err := db.ArtistHasMatchedAlbum(tc.artist)
		if err != nil {
			t.Fatalf("ArtistHasMatchedAlbum(%s): %v", tc.artist, err)
		}
		if got != tc.want {
			t.Errorf("ArtistHasMatchedAlbum(%s) = %v, want %v — %s", tc.artist, got, tc.want, tc.why)
		}
	}

	// An unknown Artist is not an error: the pass reads it only for Artists it just
	// listed, and answering false keeps a deleted row from making a request.
	if got, err := db.ArtistHasMatchedAlbum("ar_nonexistent"); err != nil || got {
		t.Errorf("ArtistHasMatchedAlbum(unknown) = %v, %v, want false, nil", got, err)
	}

	// And the entity_type is part of the key: a MATCHED entity of another kind that
	// happens to share an id must not corroborate anything.
	mustExec(t, db, `INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status)
	                 VALUES ('artist', 'al_unmatched', 'matched')`)
	if got, err := db.ArtistHasMatchedAlbum("ar_uncorroborated"); err != nil || got {
		t.Errorf("ArtistHasMatchedAlbum = %v, %v after a same-id 'artist' row matched — "+
			"entity_enrichment is keyed by (entity_type, entity_id) and the read must say so",
			got, err)
	}
}
