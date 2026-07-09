package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// External-metadata-enrichment issue 04 black-box tests: the Locked field loop.
// An Admin hand-edits a descriptive field via PUT /titles/{id}/metadata, the edit
// Locks that field, and a subsequent enrich pass refreshes only UNLOCKED fields —
// the hand-edit sticks until the Admin releases the lock. All driven through the
// HTTP API with the FAKE MetadataProvider + ArtworkFetcher (zero network).

// lockedDetailResp mirrors the Title detail with the fields issue 04 surfaces:
// lockedFields[] plus the descriptive fields a hand-edit touches.
type lockedDetailResp struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Overview         string   `json:"overview"`
	Tagline          string   `json:"tagline"`
	ContentRating    string   `json:"contentRating"`
	Studio           string   `json:"studio"`
	Genres           []string `json:"genres"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
	DisplayTitle     string   `json:"displayTitle"`
	LockedFields     []string `json:"lockedFields"`
	Cast             []struct {
		Person    string `json:"person"`
		Character string `json:"character"`
	} `json:"cast"`
	Artwork []enrichedArtworkResp `json:"artwork"`
}

func getLockedDetail(t *testing.T, srv *testharness.Server, token, id string) lockedDetailResp {
	t.Helper()
	var d lockedDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title = %d, want 200; body: %s", status, body)
	}
	return d
}

func hasLock(d lockedDetailResp, field string) bool {
	for _, f := range d.LockedFields {
		if f == field {
			return true
		}
	}
	return false
}

// TestHandEditLocksAndSurvivesReEnrich is the core acceptance loop:
// edit→lock→re-enrich-preserves-locked-refreshes-unlocked→release-lock→re-enrich-
// updates. The provider returns different metadata on the second pass so a locked
// field is visibly preserved while an unlocked one moves.
func TestHandEditLocksAndSurvivesReEnrich(t *testing.T) {
	requireFixtures(t)
	// version flips the provider's payload between passes so a refresh is visible.
	version := 1
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		m := richMeta()
		if version == 2 {
			m.Overview = "A wholly different overview from the provider."
			m.Studio = "Warner Bros"
			m.Genres = []string{"Adventure"}
		}
		return m, nil
	}}
	fetch := &fakeFetcher{data: []byte("x")}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// 1. Hand-edit overview + studio (lock those), leave genres unlocked.
	editBody := map[string]any{
		"overview": "My hand-written summary.",
		"studio":   "My Studio",
	}
	var edited lockedDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token, editBody, &edited)
	if status != http.StatusOK {
		t.Fatalf("PUT metadata = %d, want 200; body: %s", status, body)
	}
	if edited.Overview != "My hand-written summary." || edited.Studio != "My Studio" {
		t.Fatalf("edit not reflected in PUT response: %+v", edited)
	}
	if !hasLock(edited, "overview") || !hasLock(edited, "studio") {
		t.Fatalf("edited fields not locked: %v", edited.LockedFields)
	}

	// GET reflects the edits + lists them in lockedFields[].
	d := getLockedDetail(t, srv, token, id)
	if d.Overview != "My hand-written summary." || d.Studio != "My Studio" {
		t.Fatalf("GET does not reflect edit: %+v", d)
	}
	if !hasLock(d, "overview") || !hasLock(d, "studio") {
		t.Fatalf("lockedFields missing edits: %v", d.LockedFields)
	}

	// 2. Re-enrich (full) with DIFFERENT provider values. Locked fields stay; the
	//    unlocked genres refresh to the new provider value.
	version = 2
	enrichLib(t, srv, token, libID, "full")
	d = getLockedDetail(t, srv, token, id)
	if d.Overview != "My hand-written summary." {
		t.Errorf("locked overview overwritten by re-enrich: %q", d.Overview)
	}
	if d.Studio != "My Studio" {
		t.Errorf("locked studio overwritten by re-enrich: %q", d.Studio)
	}
	if len(d.Genres) != 1 || d.Genres[0] != "Adventure" {
		t.Errorf("unlocked genres did not refresh: %v", d.Genres)
	}

	// 3. Release the overview lock; the next pass refreshes it again.
	status, body = srv.JSON(http.MethodDelete, "/api/v1/titles/"+id+"/metadata/locks/overview", token, nil, &d)
	if status != http.StatusOK {
		t.Fatalf("DELETE lock = %d, want 200; body: %s", status, body)
	}
	if hasLock(d, "overview") {
		t.Fatalf("overview still locked after release: %v", d.LockedFields)
	}
	if !hasLock(d, "studio") {
		t.Fatalf("releasing overview dropped the studio lock too: %v", d.LockedFields)
	}

	enrichLib(t, srv, token, libID, "full")
	d = getLockedDetail(t, srv, token, id)
	if d.Overview != "A wholly different overview from the provider." {
		t.Errorf("released overview did not re-enrich: %q", d.Overview)
	}
	// Studio is still locked, so it stays the hand-edit.
	if d.Studio != "My Studio" {
		t.Errorf("still-locked studio changed: %q", d.Studio)
	}
}

// TestLockArtworkPinsImage: locking the poster role pins the chosen image so a
// re-enrich (with the fetcher now returning different bytes) leaves it unchanged.
func TestLockArtworkPinsImage(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	fetch := &fakeFetcher{data: []byte("POSTER_V1"), contentType: "image/jpeg"}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// Dune has no local poster, so the fetched V1 poster is what serves.
	id := titleIDByName(t, srv, token, libID, "Dune")
	if status, body := authBytes(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); status != http.StatusOK || string(body) != "POSTER_V1" {
		t.Fatalf("poster before lock = %d %q, want 200 POSTER_V1", status, body)
	}

	// Lock the poster role (pin the chosen image).
	var d lockedDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token,
		map[string]any{"lockArtwork": []string{"poster"}}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT lockArtwork = %d, want 200; body: %s", status, body)
	}
	if !hasLock(d, "poster") {
		t.Fatalf("poster role not locked: %v", d.LockedFields)
	}

	// Re-enrich with the fetcher now serving different bytes; the pinned poster
	// must not be replaced.
	fetch.data = []byte("POSTER_V2")
	enrichLib(t, srv, token, libID, "full")
	if status, body := authBytes(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); status != http.StatusOK || string(body) != "POSTER_V1" {
		t.Errorf("pinned poster changed after re-enrich: %d %q, want POSTER_V1", status, body)
	}
}

// TestMetadataEndpointsRequireAdmin: a Member is refused (403) on both the edit
// and the lock-release endpoints.
func TestMetadataEndpointsRequireAdmin(t *testing.T) {
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

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", mTok,
		map[string]any{"overview": "nope"}, nil)
	if status != http.StatusForbidden {
		t.Errorf("member PUT metadata = %d, want 403", status)
	}
	status, _ = srv.JSON(http.MethodDelete, "/api/v1/titles/"+id+"/metadata/locks/overview", mTok, nil, nil)
	if status != http.StatusForbidden {
		t.Errorf("member DELETE lock = %d, want 403", status)
	}

	// The Member's refused edit wrote nothing.
	d := getLockedDetail(t, srv, token, id)
	if d.Overview == "nope" {
		t.Errorf("member edit leaked through: %+v", d)
	}
}
