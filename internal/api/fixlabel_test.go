package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// item-editing issue 03 black-box tests: the Edit-item "Fix label" flow — an Admin
// hand-edits the full descriptive field set (each edit Locks that field) and picks
// a specific provider image for a role (which sets + Locks the role), on a leaf
// (Movie) AND a parent (Album/Artist/Show), driven through the HTTP API with the
// FAKE MetadataProvider (its new ArtworkCandidates seam), zero network. The hard
// invariants: re-enrichment never overwrites a Locked field, local artwork still
// wins over a picked image, a hand-edit never cascades to children, and a rename
// leaves identity/watch state and the active override untouched (ADR-0002/0014).

// --- wire shapes ------------------------------------------------------------

// labelDetailResp reads a leaf Title detail incl. the Fix-label surface
// (lockedFields, the full descriptive fields, displayTitle, artwork).
type labelDetailResp struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Year           int      `json:"year"`
	Overview       string   `json:"overview"`
	Tagline        string   `json:"tagline"`
	ContentRating  string   `json:"contentRating"`
	ReleaseDate    string   `json:"releaseDate"`
	RuntimeMinutes int      `json:"runtimeMinutes"`
	Studio         string   `json:"studio"`
	Genres         []string `json:"genres"`
	DisplayTitle   string   `json:"displayTitle"`
	Watched        bool     `json:"watched"`
	LockedFields   []string `json:"lockedFields"`
	Cast           []struct {
		Person string `json:"person"`
	} `json:"cast"`
	Artwork []enrichedArtworkResp `json:"artwork"`
}

type artworkCandidateResp struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Source string `json:"source"`
}

type artworkCandidatesResp struct {
	Role       string                 `json:"role"`
	Candidates []artworkCandidateResp `json:"candidates"`
}

// --- helpers ----------------------------------------------------------------

func getLabelDetail(t *testing.T, srv *testharness.Server, token, titleID string) labelDetailResp {
	t.Helper()
	var d labelDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title = %d, want 200; body: %s", status, body)
	}
	return d
}

func editLabel(t *testing.T, srv *testharness.Server, token, titleID string, body map[string]any) labelDetailResp {
	t.Helper()
	var d labelDetailResp
	status, raw := srv.JSON(http.MethodPut, "/api/v1/titles/"+titleID+"/metadata", token, body, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT metadata = %d, want 200; body: %s", status, raw)
	}
	return d
}

// posterSourceOf returns the source ("local"/"fetched") of the poster artwork the
// detail lists (one entry per role — the winning row), or "" when absent.
func posterSourceOf(d labelDetailResp) string {
	for _, a := range d.Artwork {
		if a.Role == "poster" {
			return a.Source
		}
	}
	return ""
}

// --- Leaf: full-field hand-edit + lock + honored re-enrich + release ---------

// TestFixLabelLeafFullFieldEditAndLock: an Admin hand-edits EVERY descriptive
// field on a Movie; each edit is written and Locked; a full re-enrich refreshes
// none of the locked fields; releasing one lets it re-enrich.
func TestFixLabelLeafFullFieldEditAndLock(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// A watch state that must survive a Fix-label edit (identity is never touched).
	srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/watchState", token, map[string]any{"watched": true}, nil)

	edited := editLabel(t, srv, token, id, map[string]any{
		"overview":       "MY overview.",
		"tagline":        "MY tagline.",
		"contentRating":  "R",
		"releaseDate":    "1999-01-01",
		"runtimeMinutes": 200,
		"studio":         "MY Studio",
		"title":          "MY Display Title",
		"genres":         []string{"Noir", "Thriller"},
		"cast":           []map[string]any{{"person": "Someone", "character": "Hero"}},
	})

	// Every edited value landed on the detail.
	if edited.Overview != "MY overview." || edited.Tagline != "MY tagline." || edited.ContentRating != "R" ||
		edited.ReleaseDate != "1999-01-01" || edited.RuntimeMinutes != 200 || edited.Studio != "MY Studio" {
		t.Errorf("scalar edits not applied: %+v", edited)
	}
	if edited.DisplayTitle != "MY Display Title" {
		t.Errorf("display title edit not applied: %q", edited.DisplayTitle)
	}
	if len(edited.Genres) != 2 || edited.Genres[0] != "Noir" {
		t.Errorf("genres edit not applied: %v", edited.Genres)
	}
	if len(edited.Cast) != 1 || edited.Cast[0].Person != "Someone" {
		t.Errorf("cast edit not applied: %+v", edited.Cast)
	}
	// Every field is now Locked.
	for _, f := range []string{"overview", "tagline", "content_rating", "release_date", "runtime_minutes", "studio", "title", "genres", "cast"} {
		if !contains(edited.LockedFields, f) {
			t.Errorf("field %q not Locked after edit: %+v", f, edited.LockedFields)
		}
	}
	// Identity + watch state untouched by the label edit.
	if !edited.Watched {
		t.Errorf("watch state lost across a Fix-label edit")
	}

	// A full re-enrich (auto record = richMeta) must overwrite NONE of the locked fields.
	enrichLib(t, srv, token, libID, "full")
	after := getLabelDetail(t, srv, token, id)
	if after.Overview != "MY overview." || after.Tagline != "MY tagline." || after.ContentRating != "R" ||
		after.Studio != "MY Studio" || after.DisplayTitle != "MY Display Title" {
		t.Errorf("re-enrich overwrote a locked field: %+v", after)
	}
	if len(after.Genres) != 2 || after.Genres[0] != "Noir" {
		t.Errorf("re-enrich overwrote locked genres: %v", after.Genres)
	}

	// Releasing the overview lock lets the next pass refresh it from the record.
	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/titles/"+id+"/metadata/locks/overview", token, nil, nil); st != http.StatusOK {
		t.Fatalf("release lock = %d, want 200", st)
	}
	enrichLib(t, srv, token, libID, "full")
	released := getLabelDetail(t, srv, token, id)
	if released.Overview != richMeta().Overview {
		t.Errorf("overview did not re-enrich after lock release: %q", released.Overview)
	}
	if released.Tagline != "MY tagline." {
		t.Errorf("releasing overview wrongly released tagline: %q", released.Tagline)
	}
}

// --- Leaf: image picker sets + locks the role; local artwork still wins ------

// TestFixLabelLeafImagePickAndLock: the picker lists the provider's poster images;
// picking one sets + Locks the poster role, and a re-enrich keeps it. For a Title
// WITH a local poster, local still wins over the picked image.
func TestFixLabelLeafImagePickAndLock(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
		artworkFn: func(_ enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{
				{URL: "https://img.example/alt-" + role + "-1.jpg", Width: 2000, Height: 3000, Source: "tmdb"},
				{URL: "https://img.example/alt-" + role + "-2.jpg", Width: 1000, Height: 1500, Source: "tmdb"},
			}, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("PICKEDBYTES")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// (AC) "Pinned Movie" has no local poster → the picked image is served + Locked.
	dune := titleIDByName(t, srv, token, libID, "Pinned Movie")

	var cands artworkCandidatesResp
	if st, body := srv.AuthGET("/api/v1/titles/"+dune+"/artworkCandidates?role=poster", token, &cands); st != http.StatusOK {
		t.Fatalf("GET artworkCandidates = %d; body: %s", st, body)
	}
	if len(cands.Candidates) != 2 || cands.Candidates[0].URL == "" || cands.Candidates[0].Width == 0 {
		t.Fatalf("poster candidates unexpected: %+v", cands.Candidates)
	}
	if cands.Role != "poster" {
		t.Errorf("candidates role = %q, want poster", cands.Role)
	}

	picked := cands.Candidates[1].URL
	var afterPick labelDetailResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+dune+"/artwork", token,
		map[string]any{"role": "poster", "url": picked}, &afterPick); st != http.StatusOK {
		t.Fatalf("PUT artwork = %d; body: %s", st, body)
	}
	if !contains(afterPick.LockedFields, "poster") {
		t.Errorf("poster role not Locked after pick: %+v", afterPick.LockedFields)
	}
	// The picked bytes are served (fetched via the ArtworkFetcher, no local poster).
	if src := posterSourceOf(afterPick); src != "fetched" {
		t.Errorf("poster source after pick = %q, want fetched", src)
	}
	st, body := authBytes(t, srv, token, "/api/v1/titles/"+dune+"/artwork/poster")
	if st != http.StatusOK || string(body) != "PICKEDBYTES" {
		t.Errorf("served poster after pick = %d %q, want the picked bytes", st, body)
	}
	// A full re-enrich keeps the locked, hand-picked poster (role skipped on refetch).
	enrichLib(t, srv, token, libID, "full")
	if !contains(getLabelDetail(t, srv, token, dune).LockedFields, "poster") {
		t.Errorf("poster lock lost across re-enrich")
	}

	// (AC) "Extras Movie" ships a local poster.jpg → picking still leaves LOCAL winning.
	extras := titleIDByName(t, srv, token, libID, "Extras Movie")
	if st, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+extras+"/artwork", token,
		map[string]any{"role": "poster", "url": "https://img.example/alt-poster-1.jpg"}, nil); st != http.StatusOK {
		t.Fatalf("PUT artwork (extras) = %d; body: %s", st, body)
	}
	extrasDetail := getLabelDetail(t, srv, token, extras)
	if src := posterSourceOf(extrasDetail); src != "local" {
		t.Errorf("poster source with a local file after pick = %q, want local (local wins)", src)
	}
	st, body = authBytes(t, srv, token, "/api/v1/titles/"+extras+"/artwork/poster")
	if st != http.StatusOK {
		t.Fatalf("served extras poster = %d", st)
	}
	if string(body) == "PICKEDBYTES" {
		t.Errorf("served the picked poster for a Title with a local one; local must win")
	}
}

// --- Parent: rename (display name) changes only the label -------------------

// TestFixLabelParentRenameLeavesIdentityAndOverride: renaming an Album's display
// name (a Fix-label edit) changes only the shown title — the active Enrichment
// override and the album's Tracks are untouched (a hand-edit never cascades).
func TestFixLabelParentRenameNoCascade(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "alb-right", Title: "Right Album", Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "seed"}
			if ref.MusicbrainzID == "alb-right" {
				m.ExternalID = "alb-right"
			}
			return m, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	artists := listArtists(t, srv, token, libID)
	var albumID string
	for _, a := range artists.Artists {
		if albums := artistAlbums(t, srv, token, a.ID); len(albums.Albums) > 0 {
			albumID = albums.Albums[0].ID
			break
		}
	}
	if albumID == "" {
		t.Skip("no albums in music fixture")
	}

	// Capture the album's Tracks (display titles) before any label edit.
	tracksBefore := albumTracks(t, srv, token, albumID)
	if len(tracksBefore.Tracks) == 0 {
		t.Skip("album has no tracks")
	}

	// Pin an Enrichment override on the album so we can prove a rename doesn't drop it.
	applyEntityOverride(t, srv, token, "albums", albumID, "alb-right")

	// Rename the album (display label only).
	var renamed entityDetailResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/albums/"+albumID+"/metadata", token,
		map[string]any{"title": "My Renamed Album"}, &renamed); st != http.StatusOK {
		t.Fatalf("PUT album metadata (rename) = %d; body: %s", st, body)
	}
	if !contains(renamed.LockedFields, "title") {
		t.Errorf("title not Locked after rename: %+v", renamed.LockedFields)
	}
	// (AC) The active Enrichment override is unchanged by the rename.
	if renamed.EnrichmentOverride == nil || renamed.EnrichmentOverride.ExternalID != "alb-right" {
		t.Errorf("rename dropped the active override: %+v", renamed.EnrichmentOverride)
	}

	// (AC) The rename shows up as the album's display title on the browse detail.
	var albumDetail struct {
		Album struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"album"`
	}
	if st, _ := srv.AuthGET("/api/v1/albums/"+albumID+"/tracks", token, &albumDetail); st != http.StatusOK {
		t.Fatalf("album detail read failed")
	}
	if albumDetail.Album.Title != "My Renamed Album" {
		t.Errorf("album display title after rename = %q, want My Renamed Album", albumDetail.Album.Title)
	}

	// (AC) Never cascades: the album's Tracks are untouched by the album rename.
	tracksAfter := albumTracks(t, srv, token, albumID)
	if len(tracksAfter.Tracks) != len(tracksBefore.Tracks) {
		t.Fatalf("track count changed by album rename: before=%d after=%d",
			len(tracksBefore.Tracks), len(tracksAfter.Tracks))
	}
	for i := range tracksBefore.Tracks {
		if tracksAfter.Tracks[i].Title != tracksBefore.Tracks[i].Title {
			t.Errorf("album rename altered a track title: before=%q after=%q",
				tracksBefore.Tracks[i].Title, tracksAfter.Tracks[i].Title)
		}
	}
}

// --- Parent: full-field edit + lock honored by re-enrich --------------------

// TestFixLabelParentEditAndLockHonored: a Show hand-edit (overview + genres) Locks
// those fields; a full re-enrich refreshes neither, and the picker lists provider
// images for the Show poster.
func TestFixLabelParentEditLockAndImagePicker(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "auto show overview", Genres: []string{"Drama"}, ExternalID: "show-x"}
			return m, nil
		},
		artworkFn: func(_ enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{
				{URL: "https://img.example/show-" + role + ".jpg", Width: 1000, Height: 1500, Source: "tmdb"},
			}, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("SHOWART")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows in tv fixture")
	}
	showID := shows.Shows[0].ID

	// Hand-edit the Show overview + genres → both Locked.
	var edited entityDetailResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/metadata", token,
		map[string]any{"overview": "MY show overview.", "genres": []string{"Noir"}}, &edited); st != http.StatusOK {
		t.Fatalf("PUT show metadata = %d; body: %s", st, body)
	}
	if !contains(edited.LockedFields, "overview") || !contains(edited.LockedFields, "genres") {
		t.Errorf("show fields not Locked after edit: %+v", edited.LockedFields)
	}

	// (AC) The image picker lists the provider's Show poster candidates.
	var cands artworkCandidatesResp
	if st, body := srv.AuthGET("/api/v1/shows/"+showID+"/artworkCandidates?role=poster", token, &cands); st != http.StatusOK {
		t.Fatalf("GET show artworkCandidates = %d; body: %s", st, body)
	}
	if len(cands.Candidates) == 0 {
		t.Fatalf("no show poster candidates: %+v", cands)
	}
	// Pick one → poster role Locked on the Show.
	var afterPick entityDetailResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/artwork", token,
		map[string]any{"role": "poster", "url": cands.Candidates[0].URL}, &afterPick); st != http.StatusOK {
		t.Fatalf("PUT show artwork = %d; body: %s", st, body)
	}
	if !contains(afterPick.LockedFields, "poster") {
		t.Errorf("show poster not Locked after pick: %+v", afterPick.LockedFields)
	}

	// A full re-enrich refreshes none of the locked fields (overview/genres/poster).
	enrichLib(t, srv, token, libID, "full")
	after := getShowDetail(t, srv, token, showID)
	if after.Show.Overview != "MY show overview." {
		t.Errorf("locked show overview overwritten by re-enrich: %q", after.Show.Overview)
	}
	if len(after.Show.Genres) != 1 || after.Show.Genres[0] != "Noir" {
		t.Errorf("locked show genres overwritten by re-enrich: %v", after.Show.Genres)
	}
	if !contains(after.Show.LockedFields, "poster") {
		t.Errorf("show poster lock lost across re-enrich")
	}
}

// --- Access + SSE -----------------------------------------------------------

// TestFixLabelAdminOnlyAndSSE: a Member cannot list image candidates, pick an
// image, or hand-edit (all 403); an invalid role is 400; an Admin pick emits a
// libraryUpdated SSE nudge.
func TestFixLabelAdminOnlyAndSSE(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
		artworkFn: func(_ enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{{URL: "https://img.example/p.jpg", Source: "tmdb"}}, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	srv.CreateMember("labelmember", "memberpass123")
	mTok := srv.LoginAs("labelmember", "memberpass123")

	if st, _ := srv.AuthGET("/api/v1/titles/"+id+"/artworkCandidates?role=poster", mTok, nil); st != http.StatusForbidden {
		t.Errorf("member GET artworkCandidates = %d, want 403", st)
	}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/artwork", mTok,
		map[string]any{"role": "poster", "url": "https://img.example/p.jpg"}, nil); st != http.StatusForbidden {
		t.Errorf("member PUT artwork = %d, want 403", st)
	}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", mTok,
		map[string]any{"overview": "x"}, nil); st != http.StatusForbidden {
		t.Errorf("member PUT metadata = %d, want 403", st)
	}
	// Admin: an invalid role is 400.
	if st, _ := srv.AuthGET("/api/v1/titles/"+id+"/artworkCandidates?role=bogus", token, nil); st != http.StatusBadRequest {
		t.Errorf("invalid role = %d, want 400", st)
	}

	// SSE: an Admin image pick emits libraryUpdated.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/artwork", token,
		map[string]any{"role": "poster", "url": "https://img.example/p.jpg"}, nil); st != http.StatusOK {
		t.Fatalf("admin pick = %d, want 200", st)
	}
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: libraryUpdated") || strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}
