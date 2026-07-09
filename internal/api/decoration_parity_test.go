package api_test

import (
	"net/http"
	"reflect"
	"testing"
)

// Cross-cutting decoration-parity test for collections-playlists issue 05.
//
// This is the consolidating proof that the SAME Title, viewed as (a) a browse-list
// member, (b) a Collection member, and (c) a Playlist member, carries the IDENTICAL
// toTitleSummary projection — field-for-field. The three surfaces all flow through
// one decoration code path (catalog's WatchStatesForTitles / GenresForTitles /
// ArtworkVersionsForTitles bulk readers + toTitleSummary; see decorateMembers in
// collection_handlers.go and handleListTitles in catalog_handlers.go), so a future
// change to the browse projection that does NOT flow through to the groupings must
// fail this test. The Collection/Playlist per-surface parity tests
// (TestCollectionMemberDecorationParity, TestPlaylistDetailOrderAndDecorationParity)
// each pin one grouping against browse; this one pins all three against each other
// in a single fixture with non-default decoration so the comparison has teeth.

// fullTitleSummary mirrors api.titleSummaryJSON field-for-field (every JSON key on
// the browse-list shape), so reflect.DeepEqual across the three surfaces compares
// the WHOLE projection — not just the handful of fields a leaner struct would carry.
// A new field added to titleSummaryJSON without updating all three decoration paths
// would surface here as a DeepEqual mismatch.
type fullTitleSummary struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Title            string   `json:"title"`
	Year             int      `json:"year"`
	NeedsReview      bool     `json:"needsReview"`
	Ambiguous        bool     `json:"ambiguous"`
	TMDBID           string   `json:"tmdbId"`
	IMDBID           string   `json:"imdbId"`
	AddedAt          string   `json:"addedAt"`
	ResumePositionMs int64    `json:"resumePositionMs"`
	Watched          bool     `json:"watched"`
	Overview         string   `json:"overview"`
	ContentRating    string   `json:"contentRating"`
	ReleaseDate      string   `json:"releaseDate"`
	RuntimeMinutes   int      `json:"runtimeMinutes"`
	Studio           string   `json:"studio"`
	Genres           []string `json:"genres"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
	ArtworkVersion   string   `json:"artworkVersion"`
}

type fullBrowseListResp struct {
	Titles []fullTitleSummary `json:"titles"`
}

type fullCollectionDetailResp struct {
	Members []fullTitleSummary `json:"members"`
}

// fullPlaylistMember is a browse summary PLUS the playlist itemId; the parity
// comparison uses only the embedded titleSummary portion (itemId is grouping-local,
// never part of the shared projection).
type fullPlaylistMember struct {
	fullTitleSummary
	ItemID string `json:"itemId"`
}

type fullPlaylistDetailResp struct {
	Members []fullPlaylistMember `json:"members"`
}

func findFullSummary(titles []fullTitleSummary, id string) (fullTitleSummary, bool) {
	for _, t := range titles {
		if t.ID == id {
			return t, true
		}
	}
	return fullTitleSummary{}, false
}

// TestMemberDecorationParityAcrossSurfaces proves that the SAME Title carries an
// IDENTICAL toTitleSummary across browse, Collection, and Playlist. It populates
// NON-DEFAULT decoration first — a content rating (an enrichment-sourced field) and
// a real mid-band resume position driven through the playback flow (negotiate +
// progress, mirroring watchstate_test.go) — so the equality is over populated
// fields, not three equal-empty projections.
//
// Acceptance (issue 05): Collection and Playlist members carry the same
// toTitleSummary fields (watched/resume, genres, content rating, artwork + version)
// as the same Title in a browse listing.
func TestMemberDecorationParityAcrossSurfaces(t *testing.T) {
	requireFixtures(t)
	// Single Admin viewer: an Admin sees the Title in browse, in a Collection's full
	// membership, and in their own Playlist — sidestepping access-filter complications
	// so the test isolates the decoration projection.
	srv, admin, lib, _ := scanMovies(t)

	// Find Dune's id from a browse pass.
	var browse fullBrowseListResp
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse); st != http.StatusOK {
		t.Fatalf("browse list: status %d; body: %s", st, body)
	}
	var duneID string
	for _, ts := range browse.Titles {
		if ts.Title == "Dune" {
			duneID = ts.ID
		}
	}
	if duneID == "" {
		t.Fatal("Dune not in browse list")
	}

	// --- populate NON-DEFAULT decoration so the comparison has teeth ---

	// 1. An enrichment-sourced field: a content rating.
	srv.SetTitleContentRating(duneID, "PG-13")

	// 2. A real watch-state resume, driven through the playback flow (negotiate then a
	//    mid-band progress report) — exactly how a client produces resume state, so
	//    the watch-state decorator (WatchStatesForTitles) returns a non-zero resume.
	dur := titleDuration(t, srv, admin, duneID)
	dec := negotiateDune(t, srv, admin, duneID)
	mid := dur / 2 // ~50%: safely between the 2% floor and 90% watched ceiling
	prog := postProgress(t, srv, admin, dec.SessionID, mid, http.StatusOK)
	if prog.ResumePositionMs != mid || prog.Watched {
		t.Fatalf("seed resume = %d watched=%v, want %d / false", prog.ResumePositionMs, prog.Watched, mid)
	}

	// --- (a) browse-list member (after decoration is populated) ---
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse); st != http.StatusOK {
		t.Fatalf("browse list (post-seed): status %d; body: %s", st, body)
	}
	browseDune, ok := findFullSummary(browse.Titles, duneID)
	if !ok {
		t.Fatal("Dune missing from browse list after seeding")
	}
	// Guard: the seeded decoration actually landed on the browse projection, so an
	// all-empty DeepEqual can't pass vacuously.
	if browseDune.ContentRating != "PG-13" {
		t.Fatalf("browse contentRating = %q, want PG-13 (seed did not land)", browseDune.ContentRating)
	}
	if browseDune.ResumePositionMs != mid {
		t.Fatalf("browse resumePositionMs = %d, want %d (resume seed did not land)", browseDune.ResumePositionMs, mid)
	}

	// --- (b) Collection member (Admin adds Dune, reads full membership) ---
	colID := createCollection(t, srv, admin, "Parity", "")
	addCollectionItems(t, srv, admin, colID, duneID)
	var col fullCollectionDetailResp
	if st, body := srv.AuthGET("/api/v1/collections/"+colID, admin, &col); st != http.StatusOK {
		t.Fatalf("collection detail: status %d; body: %s", st, body)
	}
	collectionDune, ok := findFullSummary(col.Members, duneID)
	if !ok {
		t.Fatalf("Dune missing from collection %s", colID)
	}

	// --- (c) Playlist member (Admin owns the Playlist, appends Dune) ---
	plID := createPlaylist(t, srv, admin, "Parity Queue")
	if st, env := appendPlaylistItem(t, srv, admin, plID, duneID); st != http.StatusNoContent {
		t.Fatalf("append dune = %d/%s, want 204", st, env.Error.Code)
	}
	var pl fullPlaylistDetailResp
	if st, body := srv.AuthGET("/api/v1/playlists/"+plID, admin, &pl); st != http.StatusOK {
		t.Fatalf("playlist detail: status %d; body: %s", st, body)
	}
	if len(pl.Members) != 1 {
		t.Fatalf("playlist members = %d, want 1", len(pl.Members))
	}
	playlistDune := pl.Members[0].fullTitleSummary // titleSummary portion only; itemId excluded
	if pl.Members[0].ID != duneID {
		t.Fatalf("playlist member id = %q, want %q", pl.Members[0].ID, duneID)
	}
	if pl.Members[0].ItemID == "" {
		t.Error("playlist member itemId is empty, want populated (the grouping-local field)")
	}

	// --- field-for-field EQUALITY across all three surfaces ---
	if !reflect.DeepEqual(browseDune, collectionDune) {
		t.Errorf("browse vs collection parity mismatch:\n browse:     %+v\n collection: %+v", browseDune, collectionDune)
	}
	if !reflect.DeepEqual(browseDune, playlistDune) {
		t.Errorf("browse vs playlist parity mismatch:\n browse:   %+v\n playlist: %+v", browseDune, playlistDune)
	}

	// Belt-and-suspenders: the populated decoration fields are present and identical
	// on the grouping members, proving the shared decorators ran on all three paths.
	for name, m := range map[string]fullTitleSummary{"collection": collectionDune, "playlist": playlistDune} {
		if m.ContentRating != "PG-13" {
			t.Errorf("%s contentRating = %q, want PG-13", name, m.ContentRating)
		}
		if m.ResumePositionMs != mid {
			t.Errorf("%s resumePositionMs = %d, want %d", name, m.ResumePositionMs, mid)
		}
		if m.Watched {
			t.Errorf("%s watched = true, want false (mid-band resume)", name)
		}
	}
}
