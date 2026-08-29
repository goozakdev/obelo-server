package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Black-box tests for the row context the Admin Needs-Fixing queue is built on.
//
// The defect these guard against is not a crash — the old endpoints answered 200
// perfectly happily. It is that their answers were unactionable: an enrichment row
// for an Episode was `{id, kind, title}`, so a client could only print "Pilot",
// with no Show, no season/episode numbers and no file. Two Shows can both have a
// "Pilot", so that row named nothing an Admin could act on.
//
// So these assert the CONTEXT: that every attention row identifies its item and
// names its file, and that the needs-review rows say why they were flagged.

type fixContextResp struct {
	Path          string `json:"path"`
	ShowTitle     string `json:"showTitle"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeLabel  string `json:"episodeLabel"`
	ArtistName    string `json:"artistName"`
	AlbumTitle    string `json:"albumTitle"`
	DiscNumber    int    `json:"discNumber"`
	TrackNumber   int    `json:"trackNumber"`
	ShowID        string `json:"showId"`
	AlbumID       string `json:"albumId"`
	EnrichedTitle string `json:"enrichedTitle"`
	ReleaseDate   string `json:"releaseDate"`
}

type needsReviewContextItem struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	FolderPath       string `json:"folderPath"`
	Reason           string `json:"reason"`
	EnrichmentStatus string `json:"enrichmentStatus"`
	fixContextResp
}

type needsReviewContextResp struct {
	Items []needsReviewContextItem `json:"items"`
}

// TestNeedsReviewCarriesEpisodeContext: a flagged Episode names its Show, its
// season/episode numbers, and the file it came from — the four facts that make the
// row actionable. Its own title alone never was.
func TestNeedsReviewCarriesEpisodeContext(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	var res needsReviewContextResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &res)
	if status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}

	var episodes int
	for _, it := range res.Items {
		if it.Kind != "episode" {
			continue
		}
		episodes++
		if it.ShowTitle == "" {
			t.Errorf("episode %q carries no showTitle — the row cannot say which show it belongs to", it.Title)
		}
		if it.Path == "" {
			t.Errorf("episode %q carries no path — the row cannot say which file it refers to", it.Title)
		}
		// A flagged Episode is flagged precisely because its numbering was not
		// SxxExx, so the reason must say so rather than defaulting to "no year".
		if it.Reason != "episode-numbering" {
			t.Errorf("episode %q reason = %q, want episode-numbering", it.Title, it.Reason)
		}
	}
	if episodes == 0 {
		t.Fatalf("no flagged Episodes to check context on: %+v", res.Items)
	}
}

// TestNeedsReviewShowCarriesPathAndReason: a Show row names a file under it (so the
// Admin can see the folder it was filed from) and reports the no-year rule.
func TestNeedsReviewShowCarriesPathAndReason(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	var res needsReviewContextResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &res); status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}
	for _, it := range res.Items {
		if it.Kind != "show" {
			continue
		}
		if it.Path == "" {
			t.Errorf("show %q carries no path", it.Title)
		}
		if it.Reason != "no-year" {
			t.Errorf("show %q reason = %q, want no-year", it.Title, it.Reason)
		}
		return
	}
	t.Fatalf("no flagged Shows to check context on: %+v", res.Items)
}

// TestNeedsReviewMovieCarriesPath: a Movie row names its file, not only its folder
// anchor — the anchor is where a fix is written, the path is what the Admin reads.
func TestNeedsReviewMovieCarriesPath(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	var res needsReviewContextResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &res); status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}
	for _, it := range res.Items {
		if it.Title != "Yearless Movie" {
			continue
		}
		if !strings.HasSuffix(it.Path, ".mp4") && !strings.HasSuffix(it.Path, ".mkv") {
			t.Errorf("path = %q, want the movie's file", it.Path)
		}
		if it.Reason != "no-year" {
			t.Errorf("reason = %q, want no-year", it.Reason)
		}
		return
	}
	t.Fatalf("Yearless Movie not on the list: %+v", res.Items)
}

// --- Library-scoped candidate search ----------------------------------------

// TestLibraryEnrichmentCandidatesSearches: the search an Unmatched file needs.
// Every other candidate route is anchored to an existing entity, and an Unmatched
// file has no Title by construction — which is why its only correction tool used to
// be a raw-id form.
func TestLibraryEnrichmentCandidatesSearches(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	var res struct {
		Candidates []struct {
			ExternalID string `json:"externalId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"candidates"`
	}
	status, body := srv.AuthGET(
		"/api/v1/libraries/"+libID+"/enrichmentCandidates?q=Dune", token, &res)
	// A server with no provider configured answers 503 SEARCH_UNAVAILABLE, which is
	// the documented contract and not a failure of this route.
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		t.Fatalf("library candidates = %d, want 200 or 503; body: %s", status, body)
	}
	if status == http.StatusOK {
		for _, c := range res.Candidates {
			if c.Kind != "movie" {
				t.Errorf("candidate kind = %q, want movie (from the Library's kind)", c.Kind)
			}
		}
	}
}

// TestLibraryEnrichmentCandidatesBlankQuery: a blank query is an empty answer, not
// an error — the picker mounts before the Admin has typed anything.
func TestLibraryEnrichmentCandidatesBlankQuery(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	var res struct {
		Candidates []struct{} `json:"candidates"`
	}
	if status, body := srv.AuthGET(
		"/api/v1/libraries/"+libID+"/enrichmentCandidates?q=", token, &res); status != http.StatusOK {
		t.Fatalf("blank query = %d, want 200; body: %s", status, body)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("blank query returned %d candidates, want 0", len(res.Candidates))
	}
}

// TestLibraryEnrichmentCandidatesUnknownLibraryIs404 keeps the hide-existence rule.
func TestLibraryEnrichmentCandidatesUnknownLibraryIs404(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, _ := scanNamingLibrary(t)

	if status, _ := srv.AuthGET(
		"/api/v1/libraries/does-not-exist/enrichmentCandidates?q=Dune", token, nil); status != http.StatusNotFound {
		t.Errorf("unknown library = %d, want 404", status)
	}
}

// TestLibraryEnrichmentCandidatesRequiresAdmin: it reaches an external provider on
// the server's behalf, so it is Admin-only like every other search route.
func TestLibraryEnrichmentCandidatesRequiresAdmin(t *testing.T) {
	requireNamingFixtures(t)
	srv, _, libID := scanNamingLibrary(t)
	srv.CreateMember("nfmember", "memberpass123")
	member := login(t, srv, "nfmember", "memberpass123", "P", "ios", "nfmc").Token

	if status, _ := srv.AuthGET(
		"/api/v1/libraries/"+libID+"/enrichmentCandidates?q=Dune", member, nil); status != http.StatusForbidden {
		t.Errorf("member search = %d, want 403", status)
	}
}

// --- Discarding an orphaned correction --------------------------------------

// TestDeleteOverrideDiscardsCorrection: an orphaned Match override — one whose
// anchor folder is gone — can be discarded, so the queue row that surfaces it has a
// real action rather than only a description of a broken thing.
func TestDeleteOverrideDiscardsCorrection(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	root := namingRoot(t)

	// Record a correction, then discard it.
	var created struct {
		ID string `json:"id"`
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token,
		map[string]any{"folderPath": root + "/Some Folder", "title": "Corrected", "year": 2001}, &created)
	if status != http.StatusOK {
		t.Fatalf("fix-match = %d, want 200; body: %s", status, body)
	}

	if status, body := srv.JSON(http.MethodDelete,
		"/api/v1/libraries/"+libID+"/overrides/"+created.ID, token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete override = %d, want 204; body: %s", status, body)
	}

	var list struct {
		Overrides []struct {
			ID string `json:"id"`
		} `json:"overrides"`
	}
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/overrides", token, &list); status != http.StatusOK {
		t.Fatalf("list overrides = %d, want 200; body: %s", status, body)
	}
	for _, o := range list.Overrides {
		if o.ID == created.ID {
			t.Errorf("override %s still listed after delete", created.ID)
		}
	}

	// Deleting it again is a 404, not a silent success.
	if status, _ := srv.JSON(http.MethodDelete,
		"/api/v1/libraries/"+libID+"/overrides/"+created.ID, token, nil, nil); status != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", status)
	}
}

// TestDeleteOverrideRequiresAdmin: it changes how a folder will be filed on the
// next scan, so it is Admin-only.
func TestDeleteOverrideRequiresAdmin(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	root := namingRoot(t)
	srv.CreateMember("nfmember2", "memberpass123")
	member := login(t, srv, "nfmember2", "memberpass123", "P", "ios", "nfmc2").Token

	var created struct {
		ID string `json:"id"`
	}
	if status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token,
		map[string]any{"folderPath": root + "/Another Folder", "title": "Corrected", "year": 2001}, &created); status != http.StatusOK {
		t.Fatalf("fix-match = %d; body: %s", status, body)
	}

	if status, _ := srv.JSON(http.MethodDelete,
		"/api/v1/libraries/"+libID+"/overrides/"+created.ID, member, nil, nil); status != http.StatusForbidden {
		t.Errorf("member delete = %d, want 403", status)
	}
}

// TestNeedsReviewCarriesConfirmationEvidence: a needs-review row asks the Admin to
// CONFIRM a filing ("looks right"), and an item is flagged for an uncertain parse —
// most often a missing year — so its own parsed name is exactly what cannot settle
// the question. The row must therefore carry what Enrichment matched it to, and the
// parent id whose artwork represents it.
func TestNeedsReviewCarriesConfirmationEvidence(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	var res needsReviewContextResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &res); status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}
	if len(res.Items) == 0 {
		t.Fatal("no needs-review items to check")
	}
	for _, it := range res.Items {
		// EnrichmentStatus must always be stated: "" would leave a client unable to
		// tell "matched to nothing" from "not asked yet", and it is the difference
		// between a confirmable row and an unconfirmable one.
		if it.EnrichmentStatus == "" {
			t.Errorf("%s %q carries no enrichmentStatus", it.Kind, it.Title)
		}
		// An Episode has no poster of its own, so it must name its Show for artwork.
		if it.Kind == "episode" && it.ShowID == "" {
			t.Errorf("episode %q carries no showId, so no artwork can be shown for it", it.Title)
		}
	}
}
