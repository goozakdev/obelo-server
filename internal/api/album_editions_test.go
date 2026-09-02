package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// GET /albums/{id}/editions — the route that makes ADR-0052's last decision real:
// an edition can be chosen WITHOUT LEAVING OBELO.
//
// The whole flow is here end to end, because the acceptance criterion is a
// WORKFLOW rather than a field: list the release-group's editions, choose one,
// apply it through the album's existing override with the cascade, and see the
// list come back saying the chosen edition is the one in use.

// --- wire shapes --------------------------------------------------------------

type albumEditionResp struct {
	ReleaseID      string `json:"releaseId"`
	Date           string `json:"date"`
	Country        string `json:"country"`
	Format         string `json:"format"`
	TrackCount     int    `json:"trackCount"`
	Disambiguation string `json:"disambiguation"`
}

type albumEditionsResp struct {
	AlbumID         string             `json:"albumId"`
	ReleaseGroupID  string             `json:"releaseGroupId"`
	ChosenReleaseID string             `json:"chosenReleaseId"`
	InUseReleaseID  string             `json:"inUseReleaseId"`
	InUseSource     string             `json:"inUseSource"`
	LocalTrackCount int                `json:"localTrackCount"`
	Editions        []albumEditionResp `json:"editions"`
}

// --- the fake provider --------------------------------------------------------

const (
	editionRG    = "629a5133-b9e6-43c5-8cb6-594a7cbfbfed"
	editionOrig  = "11111111-1111-4111-8111-111111111111"
	editionDelux = "22222222-2222-4222-8222-222222222222"
	editionJapan = "33333333-3333-4333-8333-333333333333"
)

// editionListerProvider is a fakeProvider that can also list a release-group's
// editions — the optional AlbumEditionLister capability the route type-asserts for.
// It counts its calls, because "opening the list twice costs one request" is a
// property of this route and not only of the Service beneath it.
type editionListerProvider struct {
	*fakeProvider
	mu    sync.Mutex
	calls int
	eds   []enrich.ReleaseEdition
	err   error
}

func (p *editionListerProvider) ReleaseGroupEditions(_ context.Context, rgID string) ([]enrich.ReleaseEdition, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	if rgID != editionRG {
		return nil, nil
	}
	return p.eds, nil
}

func (p *editionListerProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// editionsFixture scans the music library, matches OK Computer (2 local tracks) to a
// release-group, and returns the album ready to have its edition chosen.
func editionsFixture(t *testing.T, prov enrich.MetadataProvider) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	albumID, _, _ := okComputerAlbum(t, srv, token, libID)
	// The album has to BE something before it has editions: pin the release-group,
	// which is what an album is (ADR-0038).
	if st, body := srv.JSON(http.MethodPut, "/api/v1/albums/"+albumID+"/enrichmentOverride",
		token, map[string]any{"externalId": editionRG}, nil); st != http.StatusOK {
		t.Fatalf("seed album record = %d; body: %s", st, body)
	}
	return srv, token, albumID
}

// threeEditions is a release-group as MusicBrainz holds one: the original that fits
// the local album exactly, a deluxe with bonus tracks, and a Japanese pressing.
func threeEditions() []enrich.ReleaseEdition {
	return []enrich.ReleaseEdition{
		{ReleaseID: editionOrig, Date: "1997-05-21", Country: "GB", Format: "CD", TrackCount: 2},
		{ReleaseID: editionDelux, Date: "2017-06-23", Country: "XE", Format: "2×CD", TrackCount: 12,
			Disambiguation: "OKNOTOK 1997 2017"},
		{ReleaseID: editionJapan, Date: "1997-05-28", Country: "JP", Format: "CD", TrackCount: 3,
			Disambiguation: "with bonus track"},
	}
}

func getEditions(t *testing.T, srv *testharness.Server, token, albumID string) albumEditionsResp {
	t.Helper()
	var out albumEditionsResp
	st, body := srv.AuthGET("/api/v1/albums/"+albumID+"/editions", token, &out)
	if st != http.StatusOK {
		t.Fatalf("GET editions = %d, want 200; body: %s", st, body)
	}
	return out
}

// --- the tests ----------------------------------------------------------------

// The list itself: every edition of the album's release-group, with the four facts
// that tell them apart, the LOCAL track count beside them, and the one in use marked.
func TestAlbumEditionsListsTheReleaseGroupsEditions(t *testing.T) {
	requireMusicFixtures(t)
	prov := &editionListerProvider{fakeProvider: albumRecordProvider(), eds: threeEditions()}
	srv, token, albumID := editionsFixture(t, prov)

	got := getEditions(t, srv, token, albumID)
	if got.ReleaseGroupID != editionRG {
		t.Errorf("releaseGroupId = %q, want %q", got.ReleaseGroupID, editionRG)
	}
	if len(got.Editions) != 3 {
		t.Fatalf("listed %d editions, want 3: %+v", len(got.Editions), got.Editions)
	}
	first := got.Editions[0]
	if first.ReleaseID != editionOrig || first.Date != "1997-05-21" || first.Country != "GB" ||
		first.Format != "CD" || first.TrackCount != 2 {
		t.Errorf("first edition = %+v, want the 1997 GB CD with 2 tracks", first)
	}
	if got.Editions[1].Disambiguation != "OKNOTOK 1997 2017" {
		t.Errorf("disambiguation dropped: %+v", got.Editions[1])
	}
	// The number that makes the choice obvious without arithmetic.
	if got.LocalTrackCount != 2 {
		t.Errorf("localTrackCount = %d, want 2 — the album's own tracks, not the flagged ones",
			got.LocalTrackCount)
	}
	// Nobody has chosen, so the marked edition is the system's guess, named as one.
	if got.InUseReleaseID != editionOrig || got.InUseSource != "fit" {
		t.Errorf("in use = %q (%s), want %q by fit", got.InUseReleaseID, got.InUseSource, editionOrig)
	}
	if got.ChosenReleaseID != "" {
		t.Errorf("chosenReleaseId = %q, want empty", got.ChosenReleaseID)
	}
}

// THE acceptance criterion: choose an edition and apply it, entirely inside Obelo.
// The apply is the album's EXISTING override carrying the release id (ADR-0052) —
// there is no second apply path — it cascades, it reports the counts, and the list
// then says the chosen edition is the one in use, on the human's authority.
func TestChoosingAnEditionAppliesThroughTheAlbumOverrideAndIsThenInUse(t *testing.T) {
	requireMusicFixtures(t)
	prov := &editionListerProvider{fakeProvider: albumRecordProvider(), eds: threeEditions()}
	srv, token, albumID := editionsFixture(t, prov)

	listed := getEditions(t, srv, token, albumID)
	if listed.InUseReleaseID == editionDelux {
		t.Fatalf("the deluxe is already in use; this test proves CHOOSING changes it")
	}

	var applied cascadeDetailResp
	st, body := srv.JSON(http.MethodPut, "/api/v1/albums/"+albumID+"/enrichmentOverride", token,
		map[string]any{"externalId": listed.ReleaseGroupID, "releaseId": editionDelux, "cascade": true},
		&applied)
	if st != http.StatusOK {
		t.Fatalf("apply edition = %d, want 200; body: %s", st, body)
	}
	// The cascade summary is the operator's proof the pin was right — one pick, N
	// tracks. Without it a good choice and a nonsense one look identical.
	if applied.Cascade == nil {
		t.Fatal("apply reported no cascade summary — the row's whole promise is the count")
	}
	if applied.Cascade.Updated+applied.Cascade.Attention != 2 {
		t.Errorf("cascade summary = %+v, want it to account for both local tracks", applied.Cascade)
	}
	// The stored edition is SURFACED (issue 10 left this field for the picker).
	if applied.EnrichmentOverride == nil || applied.EnrichmentOverride.ReleaseID != editionDelux {
		t.Errorf("enrichmentOverride = %+v, want releaseId %q", applied.EnrichmentOverride, editionDelux)
	}

	after := getEditions(t, srv, token, albumID)
	if after.ChosenReleaseID != editionDelux {
		t.Errorf("chosenReleaseId = %q after choosing, want %q", after.ChosenReleaseID, editionDelux)
	}
	if after.InUseReleaseID != editionDelux || after.InUseSource != "chosen" {
		t.Errorf("in use = %q (%s), want the CHOSEN %q — the human's choice outranks the guess",
			after.InUseReleaseID, after.InUseSource, editionDelux)
	}
}

// Opening the section twice costs ONE provider request. The list is what an Admin
// toggles while they decide, at the host ADR-0049 watched shed load.
func TestOpeningTheEditionListTwiceCostsOneProviderRequest(t *testing.T) {
	requireMusicFixtures(t)
	prov := &editionListerProvider{fakeProvider: albumRecordProvider(), eds: threeEditions()}
	srv, token, albumID := editionsFixture(t, prov)

	getEditions(t, srv, token, albumID)
	getEditions(t, srv, token, albumID)
	if got := prov.count(); got != 1 {
		t.Errorf("made %d provider requests for two opens, want 1", got)
	}
}

// A release-group with exactly ONE release still renders. It is the confirmation
// that there was no choice to make, and an empty section reads as a broken one.
func TestAReleaseGroupWithOneEditionStillRenders(t *testing.T) {
	requireMusicFixtures(t)
	prov := &editionListerProvider{fakeProvider: albumRecordProvider(),
		eds: []enrich.ReleaseEdition{{ReleaseID: editionOrig, Date: "1997-05-21", Country: "GB",
			Format: "CD", TrackCount: 2}}}
	srv, token, albumID := editionsFixture(t, prov)

	got := getEditions(t, srv, token, albumID)
	if len(got.Editions) != 1 || got.Editions[0].ReleaseID != editionOrig {
		t.Fatalf("listed %+v, want the single edition", got.Editions)
	}
	if got.InUseReleaseID != editionOrig {
		t.Errorf("in use = %q, want the only edition there is", got.InUseReleaseID)
	}
}

// A provider that cannot list degrades to the SAME 503 SEARCH_UNAVAILABLE every
// other provider-backed list gives — which is what lets the client keep showing the
// paste-a-URL escape hatch instead of an error page.
func TestAlbumEditionsAreUnavailableWhenTheProviderCannotList(t *testing.T) {
	requireMusicFixtures(t)
	// A plain fakeProvider has no AlbumEditionLister capability at all.
	srv, token, albumID := editionsFixture(t, albumRecordProvider())

	st, body := srv.AuthGET("/api/v1/albums/"+albumID+"/editions", token, nil)
	if st != http.StatusServiceUnavailable {
		t.Fatalf("editions with no capability = %d, want 503; body: %s", st, body)
	}
	if !strings.Contains(string(body), "SEARCH_UNAVAILABLE") {
		t.Errorf("error body = %s, want the SEARCH_UNAVAILABLE code the picker degrades on", body)
	}
}

// An unknown album is 404 (not an empty list), and the route is Admin-only like
// every other read that reaches a provider on the server's behalf.
func TestAlbumEditionsRequires404AndAdmin(t *testing.T) {
	requireMusicFixtures(t)
	prov := &editionListerProvider{fakeProvider: albumRecordProvider(), eds: threeEditions()}
	srv, token, albumID := editionsFixture(t, prov)

	if st, _ := srv.AuthGET("/api/v1/albums/no-such-album/editions", token, nil); st != http.StatusNotFound {
		t.Errorf("unknown album editions = %d, want 404", st)
	}
	srv.CreateMember("edmember", "memberpass123")
	member := login(t, srv, "edmember", "memberpass123", "P", "ios", "edmc").Token
	if st, _ := srv.AuthGET("/api/v1/albums/"+albumID+"/editions", member, nil); st != http.StatusForbidden {
		t.Errorf("member editions = %d, want 403", st)
	}
}

// albumRecordProvider answers an album lookup by id, which is all these tests need
// from the metadata half — the editions come from the capability beside it.
func albumRecordProvider() *fakeProvider {
	return &fakeProvider{
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			return enrich.TitleMetadata{Matched: true, Source: "musicbrainz",
				ExternalID: firstNonEmpty(ref.MusicbrainzID, "mb-auto"), Name: ref.Title}, nil
		},
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: editionRG, Title: "OK Computer", Kind: kind}}, nil
		},
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
