package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// albums.musicbrainz_release_id (migration 0055, ADR-0050) is the exact edition
// the FILES name. Like the release-group and artist ids beside it, it is FILLED
// but NEVER BLANKED: an Album is assembled from many files, and an incremental
// scan legitimately re-upserts it having seen only the untagged half of them.
// Blanking on that pass would take the album's tracklist anchor away and send
// every track back to the search this ADR exists to stop.

const (
	standardRelease = "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"
	remasterRelease = "c1f6a1f8-9c34-4b3e-91b6-7e0b2a5d7c11"
)

// albumTree builds a one-track Artist subtree for the album "She", carrying the
// given release id (empty for the untagged case).
func albumTree(releaseID string) store.ArtistTree {
	return store.ArtistTree{
		Artist: store.Artist{
			ID: "a1", LibraryID: "libmus", Name: "Harry Connick, Jr.",
			IdentityKey: "harry connick jr", SortName: "harry connick jr",
		},
		Albums: []store.AlbumTree{{
			Title: "She", Year: 1994, IdentityKey: "harry connick jr|album:she",
			SortTitle:            "she",
			MusicbrainzID:        "b84ee12a-09ef-421b-82de-0441a926375b",
			MusicbrainzReleaseID: releaseID,
			Tracks: []store.TrackTree{{
				TitleTree: store.TitleTree{
					Title: store.Title{
						ID: "tr1", LibraryID: "libmus", Kind: "track",
						Title: "(I Could Only) Whisper Your Name", SortTitle: "i could only whisper your name",
						IdentityKey: "harry connick jr|album:she|d01t03",
					},
					Editions: []store.Edition{{
						ID: "ed1",
						Files: []store.File{{
							ID: "f1", EditionID: "ed1", Present: true,
							Path: "/media/Music/Harry Connick, Jr./She/03 Whisper Your Name.flac",
						}},
					}},
				},
				DiscNumber: 1, TrackNumber: 3,
			}},
		}},
	}
}

func releaseIDOf(t *testing.T, db *store.DB) string {
	t.Helper()
	var got string
	if err := db.QueryRow(
		`SELECT musicbrainz_release_id FROM albums WHERE identity_key = 'harry connick jr|album:she'`,
	).Scan(&got); err != nil {
		t.Fatalf("read musicbrainz_release_id: %v", err)
	}
	return got
}

func TestAlbumReleaseIDIsFilledButNeverBlanked(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)

	// A first scan that saw no id at all: the column is the migration's default.
	if err := db.UpsertArtistTree(albumTree("")); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if got := releaseIDOf(t, db); got != "" {
		t.Fatalf("musicbrainz_release_id = %q on an untagged album, want empty", got)
	}

	// A rescan that finally reads a tagged file fills it (the id appears only after
	// a rescan — no backfill is possible, ADR-0049/0050).
	if err := db.UpsertArtistTree(albumTree(standardRelease)); err != nil {
		t.Fatalf("tagged rescan: %v", err)
	}
	if got := releaseIDOf(t, db); got != standardRelease {
		t.Fatalf("musicbrainz_release_id = %q after a tagged rescan, want %q", got, standardRelease)
	}

	// The incremental case this guard exists for: a pass that re-upserts the Album
	// having seen only an untagged track of it must LEAVE the id alone.
	if err := db.UpsertArtistTree(albumTree("")); err != nil {
		t.Fatalf("incremental rescan: %v", err)
	}
	if got := releaseIDOf(t, db); got != standardRelease {
		t.Errorf("an incremental scan of one untagged track blanked the column to %q, want %q",
			got, standardRelease)
	}

	// A genuine retag still moves it — the column is scanner-owned and re-derived
	// from disk, not write-once (ADR-0049).
	if err := db.UpsertArtistTree(albumTree(remasterRelease)); err != nil {
		t.Fatalf("retag rescan: %v", err)
	}
	if got := releaseIDOf(t, db); got != remasterRelease {
		t.Errorf("a retag left musicbrainz_release_id = %q, want %q", got, remasterRelease)
	}
}

// The column is DECORATION, never identity (ADR-0002/0038/0050). Every one of the
// upserts above must have resolved to the SAME album row, and left its
// identity_key byte-identical — a release id that moved a key would re-point the
// album, and the watch state hanging off its tracks, on the day its owner retagged
// a standard pressing to the remaster.
func TestAlbumReleaseIDDoesNotTouchIdentity(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)

	if err := db.UpsertArtistTree(albumTree("")); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	var albumID, key string
	if err := db.QueryRow(`SELECT id, identity_key FROM albums`).Scan(&albumID, &key); err != nil {
		t.Fatalf("read album: %v", err)
	}

	for _, rid := range []string{standardRelease, "", remasterRelease} {
		if err := db.UpsertArtistTree(albumTree(rid)); err != nil {
			t.Fatalf("rescan (release id %q): %v", rid, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&count); err != nil {
		t.Fatalf("count albums: %v", err)
	}
	if count != 1 {
		t.Fatalf("albums = %d after retags, want 1 — the release id split the album", count)
	}
	var gotID, gotKey string
	if err := db.QueryRow(`SELECT id, identity_key FROM albums`).Scan(&gotID, &gotKey); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if gotID != albumID {
		t.Errorf("album id = %q, want the original %q", gotID, albumID)
	}
	if gotKey != key {
		t.Errorf("identity_key = %q, want %q byte-identical", gotKey, key)
	}

	// And the read path returns what was stored.
	al, err := db.AlbumByID(albumID)
	if err != nil {
		t.Fatalf("AlbumByID: %v", err)
	}
	if al.MusicbrainzReleaseID != remasterRelease {
		t.Errorf("AlbumByID.MusicbrainzReleaseID = %q, want %q", al.MusicbrainzReleaseID, remasterRelease)
	}
	albums, err := db.AlbumsForArtist("a1")
	if err != nil {
		t.Fatalf("AlbumsForArtist: %v", err)
	}
	if len(albums) != 1 || albums[0].MusicbrainzReleaseID != remasterRelease {
		t.Errorf("AlbumsForArtist gave %+v, want one album carrying %q", albums, remasterRelease)
	}
}
