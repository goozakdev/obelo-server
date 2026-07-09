package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for the identity needs-review attention surface (resolvable).
// The scanner flags a Title/Show filed from an uncertain parse (no year, or a
// non-SxxExx episode) as needs_review. These exercise the new Admin endpoints:
//   GET  /libraries/{id}/needs-review  — lists every still-flagged item (any kind)
//   POST /titles/{id}/review           — dismiss a Movie / Episode / Track
//   POST /shows/{id}/review            — dismiss a Show
// The dismissal sticks across rescans (the scanner never resurrects a reviewed
// flag). The TV case is the regression guard: the old client-side page-walk read
// the browse listing, which for a TV Library is Shows (not Titles), so it found
// nothing and every TV library wrongly reported "Nothing needs review".

type needsReviewItemResp struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	FolderPath string `json:"folderPath"`
}

type needsReviewResp struct {
	Items []needsReviewItemResp `json:"items"`
}

func listNeedsReview(t *testing.T, srv *testharness.Server, token, libID string) needsReviewResp {
	t.Helper()
	var res needsReviewResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &res)
	if status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}
	return res
}

func needsReviewHas(res needsReviewResp, title string) (needsReviewItemResp, bool) {
	for _, it := range res.Items {
		if it.Title == title {
			return it, true
		}
	}
	return needsReviewItemResp{}, false
}

// TestNeedsReviewMovieResolve: a yearless Movie surfaces on the needs-review list
// with its folder (so a fix-match can be driven), an Admin dismisses it, and the
// dismissal clears the browse flag AND survives a rescan.
func TestNeedsReviewMovieResolve(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	res := listNeedsReview(t, srv, token, libID)
	ym, ok := needsReviewHas(res, "Yearless Movie")
	if !ok {
		t.Fatalf("Yearless Movie missing from needs-review list: %+v", res.Items)
	}
	if ym.Kind != "movie" {
		t.Errorf("kind = %q, want movie", ym.Kind)
	}
	if ym.Year != 0 {
		t.Errorf("year = %d, want 0", ym.Year)
	}
	// A Movie carries its folder (the fix-match override key).
	if !strings.HasSuffix(ym.FolderPath, "Yearless Movie") {
		t.Errorf("folderPath = %q, want it to end with the movie folder", ym.FolderPath)
	}

	// Dismiss it.
	if status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+ym.ID+"/review", token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("POST review = %d, want 204; body: %s", status, body)
	}

	// It leaves the needs-review list and the browse flag is cleared.
	if _, ok := needsReviewHas(listNeedsReview(t, srv, token, libID), "Yearless Movie"); ok {
		t.Errorf("Yearless Movie still on needs-review list after dismissal")
	}
	for _, ti := range listAllTitles(t, srv, token, libID).Titles {
		if ti.Title == "Yearless Movie" && ti.NeedsReview {
			t.Errorf("browse needsReview still set after dismissal")
		}
	}

	// Sticky across a rescan: the scanner must not resurrect the flag.
	scanLib(t, srv, token, libID, "")
	if _, ok := needsReviewHas(listNeedsReview(t, srv, token, libID), "Yearless Movie"); ok {
		t.Errorf("rescan resurrected the dismissed needs-review flag")
	}
}

// TestNeedsReviewTVSurfacesShowAndEpisodes is the regression guard: a TV Library's
// flagged items (a yearless Show + non-SxxExx Episodes) MUST appear on the
// needs-review surface, even though a TV Library's browse listing is Shows, not
// Titles. Dismissing each removes it.
func TestNeedsReviewTVSurfacesShowAndEpisodes(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	res := listNeedsReview(t, srv, token, libID)
	if len(res.Items) == 0 {
		t.Fatalf("TV needs-review list is empty — the TV-empty bug is back")
	}

	// A yearless Show (Anime Show) surfaces at the Show level, and carries the Show
	// folder as its fix-identity anchor (derived from a nested Episode file).
	show, ok := needsReviewHas(res, "Anime Show")
	if !ok || show.Kind != "show" {
		t.Fatalf("Anime Show (yearless) not on TV needs-review list as a show: %+v", res.Items)
	}
	if !strings.HasSuffix(show.FolderPath, filepath.Join("Anime Show")) || show.FolderPath == "" {
		t.Errorf("show anchor = %q, want it to end with the Show folder", show.FolderPath)
	}
	// At least one non-SxxExx Episode surfaces too — but an Episode has NO anchor
	// (its numbering is an Enrichment problem, not a folder override).
	var anyEpisode bool
	for _, it := range res.Items {
		if it.Kind == "episode" {
			anyEpisode = true
			if it.FolderPath != "" {
				t.Errorf("episode %q has a fix anchor %q, want none", it.Title, it.FolderPath)
			}
		}
	}
	if !anyEpisode {
		t.Errorf("no flagged Episodes on TV needs-review list: %+v", res.Items)
	}

	// Dismiss the Show via the show endpoint; it leaves the list.
	if status, body := srv.JSON(http.MethodPost, "/api/v1/shows/"+show.ID+"/review", token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("POST show review = %d, want 204; body: %s", status, body)
	}
	if _, ok := needsReviewHas(listNeedsReview(t, srv, token, libID), "Anime Show"); ok {
		t.Errorf("Anime Show still flagged after dismissal")
	}
}

// TestNeedsReviewBareFileAnchor is the regression guard for the fix-match anchor:
// a yearless movie dropped LOOSE at a Library root must anchor to the FILE itself
// (not the shared root), so it is individually fixable, while a foldered movie
// anchors to its folder. The override, keyed to the file path, then re-files just
// that bare movie on rescan.
func TestNeedsReviewBareFileAnchor(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	root := namingRoot(t)

	res := listNeedsReview(t, srv, token, libID)

	// Foldered movie → anchor is the FOLDER.
	if foldered, ok := needsReviewHas(res, "Yearless Movie"); !ok {
		t.Fatalf("Yearless Movie missing: %+v", res.Items)
	} else if want := filepath.Join(root, "Yearless Movie"); foldered.FolderPath != want {
		t.Errorf("foldered anchor = %q, want the folder %q", foldered.FolderPath, want)
	}

	// Bare loose movie → anchor is the FILE itself, not the root.
	bare, ok := needsReviewHas(res, "Loose Yearless")
	if !ok {
		t.Fatalf("Loose Yearless (bare) missing from needs-review: %+v", res.Items)
	}
	wantFile := filepath.Join(root, "Loose Yearless.mp4")
	if bare.FolderPath != wantFile {
		t.Fatalf("bare anchor = %q, want the FILE %q (not the shared root %q)", bare.FolderPath, wantFile, root)
	}

	// Fix-match keyed to the file anchor, then rescan: the bare movie re-files to
	// the corrected identity (proving a loose movie is fixable, not just listed).
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token, map[string]any{
		"folderPath": bare.FolderPath,
		"title":      "Loose Fixed",
		"year":       2015,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("fix-match on bare file = %d, want 200; body: %s", status, body)
	}
	scanLib(t, srv, token, libID, "")

	var fixed bool
	for _, ti := range listAllTitles(t, srv, token, libID).Titles {
		if ti.Title == "Loose Fixed" && ti.Year == 2015 {
			fixed = true
		}
	}
	if !fixed {
		t.Errorf("bare-file fix-match did not re-file the loose movie on rescan")
	}
}

// TestFixIdentityByIDResolvesTitleAndYear: fixing a yearless movie with ONLY a
// TMDB id resolves the canonical title + year from the id (no typing), so the
// re-filed movie is fully identified and no longer needs review.
func TestFixIdentityByIDResolvesTitleAndYear(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		if ref.TMDBID == "603" {
			return enrich.TitleMetadata{Matched: true, Name: "The Matrix", Year: 1999, Overview: "Neo wakes up."}, nil
		}
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")

	ym, ok := needsReviewHas(listNeedsReview(t, srv, token, libID), "Yearless Movie")
	if !ok {
		t.Fatalf("Yearless Movie not flagged for review")
	}

	// Fix by ID ONLY — no title, no year in the request.
	var ov overrideResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token, map[string]any{
		"folderPath": ym.FolderPath,
		"tmdbId":     "603",
	}, &ov)
	if status != http.StatusOK {
		t.Fatalf("fix-match by id = %d, want 200; body: %s", status, body)
	}
	// The override carries the title + year RESOLVED from the id (not typed).
	if ov.Title != "The Matrix" || ov.Year != 1999 {
		t.Fatalf("override identity = %q/%d, want The Matrix/1999 resolved from the id", ov.Title, ov.Year)
	}

	// Rescan: the movie re-files fully identified, no longer needing review.
	scanLib(t, srv, token, libID, "")
	var found, stillFlagged bool
	for _, ti := range listAllTitles(t, srv, token, libID).Titles {
		if ti.Title == "The Matrix" && ti.Year == 1999 {
			found = true
			stillFlagged = ti.NeedsReview
		}
	}
	if !found {
		t.Errorf("movie not re-filed as The Matrix (1999) from the id alone")
	}
	if stillFlagged {
		t.Errorf("id-identified movie still flagged needs-review")
	}
}

// TestNeedsReviewRequiresAdmin: a Member cannot read the list or dismiss an item.
func TestNeedsReviewRequiresAdmin(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	ym, ok := needsReviewHas(listNeedsReview(t, srv, token, libID), "Yearless Movie")
	if !ok {
		t.Fatalf("Yearless Movie not found for setup")
	}

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	if status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", mTok, nil); status != http.StatusForbidden {
		t.Errorf("member GET needs-review = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+ym.ID+"/review", mTok, nil, nil); status != http.StatusForbidden {
		t.Errorf("member POST title review = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/whatever/review", mTok, nil, nil); status != http.StatusForbidden {
		t.Errorf("member POST show review = %d, want 403", status)
	}
}

// TestNeedsReviewUnknownIs404: dismissing an unknown Title/Show is 404.
func TestNeedsReviewUnknownIs404(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, _ := scanNamingLibrary(t)

	if status, _ := srv.JSON(http.MethodPost, "/api/v1/titles/does-not-exist/review", token, nil, nil); status != http.StatusNotFound {
		t.Errorf("review unknown Title = %d, want 404", status)
	}
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/does-not-exist/review", token, nil, nil); status != http.StatusNotFound {
		t.Errorf("review unknown Show = %d, want 404", status)
	}
}
