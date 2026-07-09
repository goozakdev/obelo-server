package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// item-editing issue 02 black-box tests: the Edit-item "Fix info" + Locked-field
// surface extended from the leaves to the browse PARENTS — Show / Artist / Album
// (ADR-0019). Driven through the HTTP API with the FAKE MetadataProvider (its
// Search seam answering the parent kinds), zero network. Asserts observable
// behavior: the parent detail JSON reflects the picked record, a hand-set parent
// lock is honored on re-enrich, the pin is durable across a full pass, an Episode
// override survives a full pass (the durability gap deferred from slice 01), an
// album candidate carries its tracklist, Seasons have no edit affordance, and every
// action is Admin-only + emits SSE.

// --- wire shapes (parent detail reads) --------------------------------------

type entityOverrideResp struct {
	ExternalID string `json:"externalId"`
	Source     string `json:"source"`
	Status     string `json:"status"`
}

type entityDetailResp struct {
	EntityType         string              `json:"entityType"`
	EntityID           string              `json:"entityId"`
	Overview           string              `json:"overview"`
	Genres             []string            `json:"genres"`
	EnrichmentStatus   string              `json:"enrichmentStatus"`
	LockedFields       []string            `json:"lockedFields"`
	EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
}

type parentCandidateResp struct {
	ExternalID     string `json:"externalId"`
	Title          string `json:"title"`
	Year           int    `json:"year"`
	Kind           string `json:"kind"`
	Disambiguation string `json:"disambiguation"`
	Tracklist      []struct {
		Disc     int    `json:"disc"`
		Position int    `json:"position"`
		Title    string `json:"title"`
	} `json:"tracklist"`
}

type parentCandidatesResp struct {
	Candidates []parentCandidateResp `json:"candidates"`
}

// showDetailResp reads the Show detail (GET /shows/{id}/seasons → show object),
// including the item-editing/02 lockedFields + enrichmentOverride surface.
type showDetailResp struct {
	Show struct {
		ID                 string              `json:"id"`
		Overview           string              `json:"overview"`
		Genres             []string            `json:"genres"`
		EnrichmentStatus   string              `json:"enrichmentStatus"`
		LockedFields       []string            `json:"lockedFields"`
		EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
	} `json:"show"`
}

type artistDetailResp struct {
	Artist struct {
		ID                 string              `json:"id"`
		Overview           string              `json:"overview"`
		Genres             []string            `json:"genres"`
		LockedFields       []string            `json:"lockedFields"`
		EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
	} `json:"artist"`
}

type albumDetailResp struct {
	Album struct {
		ID                 string              `json:"id"`
		Genres             []string            `json:"genres"`
		LockedFields       []string            `json:"lockedFields"`
		EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
	} `json:"album"`
}

// --- helpers ----------------------------------------------------------------

func searchEntityCandidates(t *testing.T, srv *testharness.Server, token, kindPath, id, q string) parentCandidatesResp {
	t.Helper()
	var res parentCandidatesResp
	status, body := srv.AuthGET("/api/v1/"+kindPath+"/"+id+"/enrichmentCandidates?q="+q, token, &res)
	if status != http.StatusOK {
		t.Fatalf("GET %s candidates = %d, want 200; body: %s", kindPath, status, body)
	}
	return res
}

func applyEntityOverride(t *testing.T, srv *testharness.Server, token, kindPath, id, externalID string) entityDetailResp {
	t.Helper()
	var d entityDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/"+kindPath+"/"+id+"/enrichmentOverride",
		token, map[string]any{"externalId": externalID}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT %s override = %d, want 200; body: %s", kindPath, status, body)
	}
	return d
}

func getShowDetail(t *testing.T, srv *testharness.Server, token, showID string) showDetailResp {
	t.Helper()
	var d showDetailResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &d)
	if status != http.StatusOK {
		t.Fatalf("GET show detail = %d, want 200; body: %s", status, body)
	}
	return d
}

// tvParentFake wires a provider whose Search returns show candidates and whose
// Lookup resolves BY the picked show TMDB id (a corrected record), while a by-name
// (auto) show lookup returns a deliberately WRONG record. Season/episode lookups
// resolve minimally so the pass completes.
func tvParentFake() *fakeProvider {
	return &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "show-right", Title: "The Right Show", Year: 2019, Kind: kind,
					ThumbnailURL: "https://img.example/show.jpg", Disambiguation: "the correct series"},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "show":
				m := enrich.TitleMetadata{Matched: true, Source: "tmdb", Genres: []string{"Drama"}}
				if ref.TMDBID == "show-right" {
					m.Overview = "CORRECTED show overview."
					m.ExternalID = "show-right"
				} else {
					m.Overview = "WRONG auto show overview."
					m.ExternalID = "show-auto"
				}
				return m, nil
			case "episode":
				m := enrich.TitleMetadata{Matched: true, Source: "tmdb"}
				if ref.TMDBID == "ep-right-show" {
					m.Overview = "CORRECTED episode overview."
				} else {
					m.Overview = "auto episode overview."
				}
				return m, nil
			default:
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "x", ExternalID: "show-auto"}, nil
			}
		},
	}
}

// --- Show: parent Fix-info + durability -------------------------------------

// TestEnrichOverrideShowSearchApplyDurable: an Admin searches TMDB tv for a Show,
// applies a candidate → the Show detail carries the picked record; the pin is
// durable across a subsequent full pass (it does not revert to the wrong
// auto-match), and the parent detail surfaces the active override.
func TestEnrichOverrideShowSearchApplyDurable(t *testing.T) {
	requireTVFixtures(t)
	prov := tvParentFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
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

	before := getShowDetail(t, srv, token, showID)
	if !strings.Contains(before.Show.Overview, "WRONG") {
		t.Fatalf("precondition: expected wrong auto overview, got %q", before.Show.Overview)
	}

	// (AC) Search surfaces candidates.
	cands := searchEntityCandidates(t, srv, token, "shows", showID, "Show")
	if len(cands.Candidates) == 0 || cands.Candidates[0].Kind != "show" {
		t.Fatalf("show search candidates unexpected: %+v", cands.Candidates)
	}

	// (AC) Apply → detail reflects the picked record + the override is in effect.
	applied := applyEntityOverride(t, srv, token, "shows", showID, "show-right")
	if applied.Overview != "CORRECTED show overview." {
		t.Errorf("apply did not carry the picked record: overview=%q", applied.Overview)
	}
	if applied.EnrichmentOverride == nil || applied.EnrichmentOverride.ExternalID != "show-right" {
		t.Errorf("apply detail missing active override: %+v", applied.EnrichmentOverride)
	}

	after := getShowDetail(t, srv, token, showID)
	if after.Show.Overview != "CORRECTED show overview." {
		t.Errorf("show detail did not reflect override: %q", after.Show.Overview)
	}
	if after.Show.EnrichmentOverride == nil || after.Show.EnrichmentOverride.ExternalID != "show-right" {
		t.Errorf("show detail does not surface the active override: %+v", after.Show.EnrichmentOverride)
	}

	// (AC) Durable: a full pass looks up BY the pinned id, does not revert.
	enrichLib(t, srv, token, libID, "full")
	durable := getShowDetail(t, srv, token, showID)
	if durable.Show.Overview != "CORRECTED show overview." {
		t.Errorf("show override reverted on re-enrich: overview=%q", durable.Show.Overview)
	}
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "show" && ref.TMDBID == "show-right" {
			sawPinned = true
		}
	}
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("re-enrich never looked up the Show by the pinned id")
	}
}

// TestEnrichParentLockHonored: a hand-set lock on a parent (Show) field is NOT
// overwritten by a subsequent full re-enrich, while the parent detail surfaces the
// Locked field. Exercises the new entity_field_locks surface end-to-end.
func TestEnrichParentLockHonored(t *testing.T) {
	requireTVFixtures(t)
	prov := tvParentFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
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

	// Hand-edit (and thereby Lock) the Show overview.
	var edited entityDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/metadata", token,
		map[string]any{"overview": "MY hand-written show overview."}, &edited)
	if status != http.StatusOK {
		t.Fatalf("PUT show metadata = %d, want 200; body: %s", status, body)
	}
	if !contains(edited.LockedFields, "overview") {
		t.Errorf("overview not reported locked after edit: %+v", edited.LockedFields)
	}

	// A full re-enrich must NOT overwrite the locked overview (but may refresh genres).
	enrichLib(t, srv, token, libID, "full")
	after := getShowDetail(t, srv, token, showID)
	if after.Show.Overview != "MY hand-written show overview." {
		t.Errorf("locked Show overview overwritten by re-enrich: %q", after.Show.Overview)
	}
	if !contains(after.Show.LockedFields, "overview") {
		t.Errorf("show detail does not surface the locked field: %+v", after.Show.LockedFields)
	}

	// Releasing the lock lets the next pass refresh it back to the auto record.
	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/shows/"+showID+"/metadata/locks/overview", token, nil, nil); st != http.StatusOK {
		t.Fatalf("release lock = %d, want 200", st)
	}
	enrichLib(t, srv, token, libID, "full")
	released := getShowDetail(t, srv, token, showID)
	if released.Show.Overview == "MY hand-written show overview." {
		t.Errorf("overview stayed pinned after lock release: %q", released.Show.Overview)
	}
}

// --- Episode durability (closing the gap deferred from slice 01) ------------

// TestEnrichEpisodeOverrideDurable: an Episode Enrichment override now survives a
// full re-enrichment pass — the per-episode pinned id is honored, not overwritten
// by the show-derived ref (collectTVLeaves threads ep.tmdb_id).
func TestEnrichEpisodeOverrideDurable(t *testing.T) {
	requireTVFixtures(t)
	prov := tvParentFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows in tv fixture")
	}
	seasons := showSeasons(t, srv, token, shows.Shows[0].ID)
	if len(seasons.Seasons) == 0 {
		t.Skip("no seasons")
	}
	eps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	if len(eps.Episodes) == 0 {
		t.Skip("no episodes")
	}
	epID := eps.Episodes[0].ID

	// Apply an Episode override (via the leaf endpoint) → corrected overview now.
	applied := applyOverride(t, srv, token, epID, "ep-right-show")
	if applied.Overview != "CORRECTED episode overview." {
		t.Fatalf("episode override not applied: overview=%q", applied.Overview)
	}

	// (AC) Durable across a FULL pass: the per-episode pin is honored, not
	// overwritten by the show-derived id.
	enrichLib(t, srv, token, libID, "full")
	after := getEnrichedDetail(t, srv, token, epID)
	if after.Overview != "CORRECTED episode overview." {
		t.Errorf("episode override reverted on full re-enrich: overview=%q", after.Overview)
	}
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "episode" && ref.TMDBID == "ep-right-show" {
			sawPinned = true
		}
	}
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("full pass never resolved the episode by its pinned show id")
	}
}

// --- Artist + Album ---------------------------------------------------------

// TestEnrichOverrideArtist: an Admin searches MusicBrainz for an Artist and applies
// a candidate; the Artist detail reflects it and the pin is durable across a pass.
func TestEnrichOverrideArtist(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "art-right", Title: "The Right Nirvana", Kind: kind,
					Disambiguation: "the 90s grunge band"},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Overview: "auto artist", ExternalID: "art-auto"}
			if ref.Kind == "artist" && ref.MusicbrainzID == "art-right" {
				m.Overview = "CORRECTED artist bio."
				m.ExternalID = "art-right"
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
	if len(artists.Artists) == 0 {
		t.Skip("no artists in music fixture")
	}
	artistID := artists.Artists[0].ID

	cands := searchEntityCandidates(t, srv, token, "artists", artistID, "Nirvana")
	if len(cands.Candidates) == 0 || cands.Candidates[0].Kind != "artist" {
		t.Fatalf("artist search candidates unexpected: %+v", cands.Candidates)
	}
	applied := applyEntityOverride(t, srv, token, "artists", artistID, "art-right")
	if applied.Overview != "CORRECTED artist bio." {
		t.Errorf("artist override not applied: overview=%q", applied.Overview)
	}

	enrichLib(t, srv, token, libID, "full")
	var d artistDetailResp
	if st, body := srv.AuthGET("/api/v1/artists/"+artistID+"/albums", token, &d); st != http.StatusOK {
		t.Fatalf("artist detail = %d; body: %s", st, body)
	}
	if d.Artist.Overview != "CORRECTED artist bio." {
		t.Errorf("artist override reverted on re-enrich: %q", d.Artist.Overview)
	}
	if d.Artist.EnrichmentOverride == nil || d.Artist.EnrichmentOverride.ExternalID != "art-right" {
		t.Errorf("artist detail missing active override: %+v", d.Artist.EnrichmentOverride)
	}
}

// TestEnrichOverrideAlbumCarriesTracklist: an Album search candidate carries its
// tracklist preview, and applying it re-points the Album's enrichment.
func TestEnrichOverrideAlbumCarriesTracklist(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "alb-right", Title: "OK Computer", Year: 1997, Kind: kind,
					Tracklist: []enrich.TrackCandidate{
						{Disc: 1, Position: 1, Title: "Airbag"},
						{Disc: 1, Position: 2, Title: "Paranoid Android"},
					}},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Genres: []string{"Rock"}, ExternalID: "alb-auto"}
			if ref.Kind == "album" && ref.MusicbrainzID == "alb-right" {
				m.ExternalID = "alb-right"
				m.Genres = []string{"Alt Rock"}
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
		albums := artistAlbums(t, srv, token, a.ID)
		if len(albums.Albums) > 0 {
			albumID = albums.Albums[0].ID
			break
		}
	}
	if albumID == "" {
		t.Skip("no albums in music fixture")
	}

	// (AC) The album candidate includes its tracklist.
	cands := searchEntityCandidates(t, srv, token, "albums", albumID, "Computer")
	if len(cands.Candidates) == 0 || cands.Candidates[0].Kind != "album" {
		t.Fatalf("album search candidates unexpected: %+v", cands.Candidates)
	}
	if len(cands.Candidates[0].Tracklist) != 2 || cands.Candidates[0].Tracklist[1].Title != "Paranoid Android" {
		t.Errorf("album candidate tracklist missing/wrong: %+v", cands.Candidates[0].Tracklist)
	}

	applied := applyEntityOverride(t, srv, token, "albums", albumID, "alb-right")
	if applied.EnrichmentOverride == nil || applied.EnrichmentOverride.ExternalID != "alb-right" {
		t.Errorf("album override not in effect: %+v", applied.EnrichmentOverride)
	}
}

// --- Seasons have no edit affordance; access + SSE --------------------------

// TestEnrichSeasonNoEditAffordance: a Season has no Fix-info route — a PUT to
// /seasons/{id}/enrichmentOverride is not served (405/404), so a Season is only
// edited at the Show or Episode grain (ADR-0019).
func TestEnrichSeasonNoEditAffordance(t *testing.T) {
	requireTVFixtures(t)
	prov := tvParentFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows")
	}
	seasons := showSeasons(t, srv, token, shows.Shows[0].ID)
	if len(seasons.Seasons) == 0 {
		t.Skip("no seasons")
	}
	seasonID := seasons.Seasons[0].ID
	status, _ := srv.JSON(http.MethodPut, "/api/v1/seasons/"+seasonID+"/enrichmentOverride", token,
		map[string]any{"externalId": "x"}, nil)
	if status == http.StatusOK {
		t.Errorf("season override PUT succeeded (%d) — Seasons must have no edit affordance", status)
	}
}

// TestEnrichParentAdminOnlyAndSSE: a Member cannot search or apply a parent
// correction (403), and an Admin apply emits a libraryUpdated SSE event.
func TestEnrichParentAdminOnlyAndSSE(t *testing.T) {
	requireTVFixtures(t)
	prov := tvParentFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows")
	}
	showID := shows.Shows[0].ID

	srv.CreateMember("m2", "memberpass123")
	mTok := srv.LoginAs("m2", "memberpass123")
	if st, _ := srv.AuthGET("/api/v1/shows/"+showID+"/enrichmentCandidates?q=Show", mTok, nil); st != http.StatusForbidden {
		t.Errorf("member GET show candidates = %d, want 403", st)
	}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/enrichmentOverride", mTok,
		map[string]any{"externalId": "show-right"}, nil); st != http.StatusForbidden {
		t.Errorf("member PUT show override = %d, want 403", st)
	}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/metadata", mTok,
		map[string]any{"overview": "x"}, nil); st != http.StatusForbidden {
		t.Errorf("member PUT show metadata = %d, want 403", st)
	}

	// SSE: an Admin apply emits libraryUpdated.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)
	applyEntityOverride(t, srv, token, "shows", showID, "show-right")
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: libraryUpdated") || strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}

// contains reports whether xs contains s.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
