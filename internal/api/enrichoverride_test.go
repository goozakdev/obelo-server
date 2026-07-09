package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// item-editing issue 01 black-box tests: the Edit-item "Fix info" flow — an Admin
// searches the authoritative provider for a leaf (Movie/Episode/Track), picks a
// candidate, and applies it as a durable Enrichment override (ADR-0019). Driven
// through the HTTP API with the FAKE MetadataProvider (its new Search seam),
// zero network. The hard invariant asserted throughout: identity_key, parsed
// year, and watch state are NEVER touched (ADR-0002/0014).

// --- wire shapes ------------------------------------------------------------

type candidateResp struct {
	ExternalID     string `json:"externalId"`
	Title          string `json:"title"`
	Year           int    `json:"year"`
	ThumbnailURL   string `json:"thumbnailUrl"`
	Disambiguation string `json:"disambiguation"`
	Kind           string `json:"kind"`
}

type candidatesResp struct {
	Candidates []candidateResp `json:"candidates"`
}

// --- helpers ----------------------------------------------------------------

// searchCandidates drives GET /titles/{id}/enrichmentCandidates and asserts 200.
func searchCandidates(t *testing.T, srv *testharness.Server, token, titleID, q string) candidatesResp {
	t.Helper()
	var res candidatesResp
	status, body := srv.AuthGET(
		"/api/v1/titles/"+titleID+"/enrichmentCandidates?q="+q, token, &res)
	if status != http.StatusOK {
		t.Fatalf("GET enrichmentCandidates = %d, want 200; body: %s", status, body)
	}
	return res
}

// applyOverride drives PUT /titles/{id}/enrichmentOverride, asserting 200, and
// returns the updated Title detail.
func applyOverride(t *testing.T, srv *testharness.Server, token, titleID, externalID string) enrichedDetailResp {
	t.Helper()
	var d enrichedDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+titleID+"/enrichmentOverride",
		token, map[string]any{"externalId": externalID}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT enrichmentOverride = %d, want 200; body: %s", status, body)
	}
	return d
}

// movieOverrideFake wires a provider whose Search returns two same-named Dune
// candidates and whose Lookup resolves BY the picked TMDB id — while a by-name
// (auto) lookup returns a deliberately WRONG record, so a re-enrich that reverted
// to auto would be observable. Returns the fake so a test can inspect its refs.
func movieOverrideFake() *fakeProvider {
	return &fakeProvider{
		searchFn: func(kind, query string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "999", Title: "Dune", Year: 2021, Kind: kind,
					ThumbnailURL: "https://img.example/dune-2021.jpg", Disambiguation: "Paul Atreides on Arrakis."},
				{ExternalID: "111", Title: "Dune", Year: 1984, Kind: kind,
					ThumbnailURL: "https://img.example/dune-1984.jpg", Disambiguation: "Lynch's 1984 adaptation."},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.TMDBID {
			case "999":
				m := richMeta()
				m.Overview = "The CORRECT Dune (2021)."
				m.ExternalID = "999"
				return m, nil
			case "111":
				m := richMeta()
				m.Overview = "Lynch's 1984 Dune."
				m.ExternalID = "111"
				return m, nil
			default:
				// by-name auto-match → a WRONG record (a documentary about Dune).
				m := richMeta()
				m.Overview = "WRONG: a documentary about the making of Dune."
				m.Tagline = "auto-matched tagline"
				m.ExternalID = "222"
				return m, nil
			}
		},
	}
}

// --- Movie: the tracer-bullet path -----------------------------------------

// TestEnrichOverrideMovieSearchApplyDurable is the core acceptance loop for a
// Movie: search shows disambiguable candidates; applying one carries the picked
// record's fields; identity_key/year and watch state are unchanged; and the pin
// is durable across a subsequent full enrichment pass (it does not revert to the
// wrong auto-match).
func TestEnrichOverrideMovieSearchApplyDurable(t *testing.T) {
	requireFixtures(t)
	prov := movieOverrideFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("POSTERBYTES")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Dune")

	// Auto-enrichment landed the WRONG record (by-name lookup).
	before := getEnrichedDetail(t, srv, token, id)
	if !strings.Contains(before.Overview, "WRONG") {
		t.Fatalf("precondition: expected the wrong auto-matched overview, got %q", before.Overview)
	}

	// A watch state that must survive the correction (ADR-0014).
	srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/watchState", token,
		map[string]any{"watched": true}, nil)

	// (AC) Search surfaces candidates each with title, year, thumbnail, disambiguation.
	cands := searchCandidates(t, srv, token, id, "Dune")
	if len(cands.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2; %+v", len(cands.Candidates), cands.Candidates)
	}
	c := cands.Candidates[0]
	if c.ExternalID == "" || c.Title == "" || c.Year == 0 || c.ThumbnailURL == "" || c.Disambiguation == "" {
		t.Errorf("candidate missing a display field: %+v", c)
	}
	if c.Kind != "movie" {
		t.Errorf("candidate kind = %q, want movie", c.Kind)
	}

	// (AC) Apply a candidate → detail carries the picked record + supplements re-fill
	// off the pinned id (the fetched poster URL from the picked record's artwork).
	applied := applyOverride(t, srv, token, id, "999")
	if applied.EnrichmentStatus != "matched" {
		t.Errorf("status after apply = %q, want matched", applied.EnrichmentStatus)
	}
	if applied.Overview != "The CORRECT Dune (2021)." || applied.TMDBID != "999" {
		t.Errorf("apply did not carry the picked record: overview=%q tmdbId=%q", applied.Overview, applied.TMDBID)
	}
	if len(applied.Artwork) == 0 {
		t.Errorf("apply did not re-fill artwork off the pinned id")
	}

	// (AC) identity_key (via parsed year) + watch state unchanged before/after.
	after := getEnrichedDetail(t, srv, token, id)
	if after.Year != before.Year {
		t.Errorf("parsed year changed by Enrichment override: before=%d after=%d", before.Year, after.Year)
	}
	if after.Title != before.Title {
		t.Errorf("parsed identity title changed: before=%q after=%q", before.Title, after.Title)
	}
	if !after.Watched {
		t.Errorf("watch state lost across Enrichment override (identity must be preserved)")
	}

	// (AC) Durable: a subsequent FULL enrichment pass looks up BY the pinned id and
	// keeps the picked record — it does NOT revert to the wrong auto-match.
	enrichLib(t, srv, token, libID, "full")
	durable := getEnrichedDetail(t, srv, token, id)
	if durable.Overview != "The CORRECT Dune (2021)." {
		t.Errorf("override reverted on re-enrich: overview=%q", durable.Overview)
	}
	if durable.TMDBID != "999" {
		t.Errorf("pinned id lost on re-enrich: tmdbId=%q", durable.TMDBID)
	}
	// The re-enrich resolved by the pinned id, not a re-search.
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "movie" && ref.TMDBID == "999" {
			sawPinned = true
		}
	}
	nSearches := len(prov.searches)
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("re-enrich never looked up by the pinned id 999")
	}
	// Only the one interactive search happened; the durable pass did not re-search.
	if nSearches != 1 {
		t.Errorf("provider searched %d times, want exactly 1 (the interactive picker)", nSearches)
	}
}

// TestEnrichOverrideHonorsLockedFields: a locked field is NOT overwritten by the
// newly-picked record, while unlocked fields refresh from it.
func TestEnrichOverrideHonorsLockedFields(t *testing.T) {
	requireFixtures(t)
	prov := movieOverrideFake()
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

	// Hand-edit (and thereby Lock) the overview.
	var edited enrichedDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token,
		map[string]any{"overview": "MY hand-written overview."}, &edited)
	if status != http.StatusOK {
		t.Fatalf("PUT metadata = %d, want 200; body: %s", status, body)
	}

	// Apply an override whose record has a different overview + tagline.
	applied := applyOverride(t, srv, token, id, "999")
	if applied.Overview != "MY hand-written overview." {
		t.Errorf("locked overview was overwritten by the picked record: %q", applied.Overview)
	}
	if applied.Tagline == "" {
		t.Errorf("unlocked tagline did not refresh from the picked record")
	}
}

// TestEnrichOverrideSSE: applying an override emits a libraryUpdated event so
// browse reflects the fix live (ADR-0016).
func TestEnrichOverrideSSE(t *testing.T) {
	requireFixtures(t)
	prov := movieOverrideFake()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)

	applyOverride(t, srv, token, id, "999")

	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: "+"libraryUpdated") ||
			strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}

// TestEnrichOverrideSearchGracefulAndLimited covers the failure + limiting AC:
// an empty query returns an empty list; an unconfigured/disabled provider returns
// 503 (the box reports why, no hang); an unreachable provider likewise 503; and
// results are capped to a sensible page size.
func TestEnrichOverrideSearchGracefulAndLimited(t *testing.T) {
	requireFixtures(t)

	t.Run("empty query returns empty list", func(t *testing.T) {
		prov := movieOverrideFake()
		srv := testharness.New(t,
			testharness.WithEnrichmentKey("test-key"),
			testharness.WithMetadataProvider(prov),
			testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		)
		token := adminToken(t, srv)
		libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
		scanLib(t, srv, token, libID, "")
		id := titleIDByName(t, srv, token, libID, "Dune")
		res := searchCandidates(t, srv, token, id, "")
		if len(res.Candidates) != 0 {
			t.Errorf("empty query returned %d candidates, want 0", len(res.Candidates))
		}
	})

	t.Run("provider disabled → 503", func(t *testing.T) {
		// No WithEnrichmentKey → Video enrichment is OFF, so a Movie search is
		// unavailable and reports why rather than hanging.
		prov := movieOverrideFake()
		srv := testharness.New(t,
			testharness.WithMetadataProvider(prov),
			testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		)
		token := adminToken(t, srv)
		libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
		scanLib(t, srv, token, libID, "")
		id := titleIDByName(t, srv, token, libID, "Dune")
		status, body := srv.AuthGET("/api/v1/titles/"+id+"/enrichmentCandidates?q=Dune", token, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("disabled-provider search = %d, want 503; body: %s", status, body)
		}
	})

	t.Run("provider unreachable → 503", func(t *testing.T) {
		prov := &fakeProvider{
			fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
			searchFn: func(string, string) ([]enrich.Candidate, error) {
				return nil, errors.New("connection refused")
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
		id := titleIDByName(t, srv, token, libID, "Dune")
		status, _ := srv.AuthGET("/api/v1/titles/"+id+"/enrichmentCandidates?q=Dune", token, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("unreachable-provider search = %d, want 503", status)
		}
	})

	t.Run("results are limited", func(t *testing.T) {
		prov := &fakeProvider{
			fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
			searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
				cands := make([]enrich.Candidate, 50)
				for i := range cands {
					cands[i] = enrich.Candidate{ExternalID: "id", Title: "Many", Kind: kind}
				}
				return cands, nil
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
		id := titleIDByName(t, srv, token, libID, "Dune")
		res := searchCandidates(t, srv, token, id, "Many")
		if len(res.Candidates) != enrich.SearchCandidateLimit {
			t.Errorf("results = %d, want capped at %d", len(res.Candidates), enrich.SearchCandidateLimit)
		}
	})
}

// TestEnrichOverrideAdminOnly: a Member cannot search or apply (both 403); an
// unknown Title is 404; a missing externalId is 400.
func TestEnrichOverrideAdminOnly(t *testing.T) {
	requireFixtures(t)
	prov := movieOverrideFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	srv.CreateMember("m", "memberpass123")
	mTok := srv.LoginAs("m", "memberpass123")

	if status, _ := srv.AuthGET("/api/v1/titles/"+id+"/enrichmentCandidates?q=Dune", mTok, nil); status != http.StatusForbidden {
		t.Errorf("member GET candidates = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentOverride", mTok,
		map[string]any{"externalId": "999"}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT override = %d, want 403", status)
	}
	// Admin: unknown Title → 404; missing externalId → 400.
	if status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/nope/enrichmentOverride", token,
		map[string]any{"externalId": "999"}, nil); status != http.StatusNotFound {
		t.Errorf("override on unknown Title = %d, want 404", status)
	}
	if status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentOverride", token,
		map[string]any{}, nil); status != http.StatusBadRequest {
		t.Errorf("override with no externalId = %d, want 400", status)
	}
}

// --- Episode: the identical path on a TV leaf ------------------------------

// TestEnrichOverrideEpisode: the same search-and-apply path works on an Episode.
// Applying re-points the episode at the picked record and re-enriches it; the
// parsed identity (title, season/episode) is untouched.
func TestEnrichOverrideEpisode(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "42", Title: "The Right Show", Year: 2019, Kind: kind,
					ThumbnailURL: "https://img.example/show.jpg", Disambiguation: "the correct series"},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "seed", Overview: "seed"}
			if ref.TMDBID == "42" {
				m.Overview = "Corrected episode overview."
				m.ExternalID = "42"
			}
			return m, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// Reach any Episode via Show → Season → Episode.
	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows in tv fixture")
	}
	seasons := showSeasons(t, srv, token, shows.Shows[0].ID)
	if len(seasons.Seasons) == 0 {
		t.Skip("no seasons in tv fixture")
	}
	eps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	if len(eps.Episodes) == 0 {
		t.Skip("no episodes in tv fixture")
	}
	epID := eps.Episodes[0].ID
	before := getEnrichedDetail(t, srv, token, epID)

	cands := searchCandidates(t, srv, token, epID, "Show")
	if len(cands.Candidates) == 0 || cands.Candidates[0].Kind != "episode" {
		t.Fatalf("episode search candidates unexpected: %+v", cands.Candidates)
	}

	applied := applyOverride(t, srv, token, epID, "42")
	if applied.Overview != "Corrected episode overview." {
		t.Errorf("episode override not applied: overview=%q", applied.Overview)
	}
	if applied.Title != before.Title {
		t.Errorf("episode identity title changed: before=%q after=%q", before.Title, applied.Title)
	}
}

// --- Track: the identical path on a Music leaf -----------------------------

// TestEnrichOverrideTrack: the same search-and-apply path works on a Track. The
// pinned MusicBrainz id is durable — a re-enrich resolves BY it (the "wrong
// Nirvana" fix), never re-searching by name.
func TestEnrichOverrideTrack(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "mb-right", Title: "Right Song", Year: 1991, Kind: kind,
					Disambiguation: "the correct Nirvana"},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			// Music parents/tracks: return a valid record. A pinned recording MBID
			// carries the corrected synopsis.
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "seed"}
			if ref.MusicbrainzID == "mb-right" {
				m.Overview = "Corrected track synopsis."
				m.ExternalID = "mb-right"
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

	// Reach any Track via Artist → Album → Track.
	artists := listArtists(t, srv, token, libID)
	if len(artists.Artists) == 0 {
		t.Skip("no artists in music fixture")
	}
	var trackID string
	for _, a := range artists.Artists {
		albums := artistAlbums(t, srv, token, a.ID)
		for _, al := range albums.Albums {
			tr := albumTracks(t, srv, token, al.ID)
			if len(tr.Tracks) > 0 {
				trackID = tr.Tracks[0].ID
				break
			}
		}
		if trackID != "" {
			break
		}
	}
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	before := getEnrichedDetail(t, srv, token, trackID)

	cands := searchCandidates(t, srv, token, trackID, "Song")
	if len(cands.Candidates) == 0 || cands.Candidates[0].Kind != "track" {
		t.Fatalf("track search candidates unexpected: %+v", cands.Candidates)
	}

	applied := applyOverride(t, srv, token, trackID, "mb-right")
	if applied.Overview != "Corrected track synopsis." {
		t.Errorf("track override not applied: overview=%q", applied.Overview)
	}
	if applied.Title != before.Title {
		t.Errorf("track identity title changed: before=%q after=%q", before.Title, applied.Title)
	}

	// Durable: a full re-enrich resolves BY the pinned MBID.
	enrichLib(t, srv, token, libID, "full")
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "track" && ref.MusicbrainzID == "mb-right" {
			sawPinned = true
		}
	}
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("track re-enrich never looked up by the pinned MBID")
	}
	if got := getEnrichedDetail(t, srv, token, trackID).Overview; got != "Corrected track synopsis." {
		t.Errorf("track override reverted on re-enrich: overview=%q", got)
	}
}

// TestEnrichOverrideTrackPreservesTagTitle: applying a Track Enrichment override
// must NOT overwrite the tag-derived display title with the provider's canonical
// recording name — embedded tags are the Music display/identity authority
// (ADR-0002). The fake recording Lookup returns a non-empty Name; the track's
// display title must stay the tag title (the album full-pass already treats tracks
// as sparse-titled, and MatchTitle now does the same).
func TestEnrichOverrideTrackPreservesTagTitle(t *testing.T) {
	requireMusicFixtures(t)
	const canonicalName = "PROVIDER Canonical Recording Name"
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "mb-x", Title: "Right Song", Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "seed"}
			if ref.MusicbrainzID == "mb-x" {
				// A record that WOULD clobber the tag title if applied naively.
				m.Name = canonicalName
				m.Overview = "Applied synopsis."
				m.ExternalID = "mb-x"
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

	// Reach a Track that has a tag title (all fixture tracks do).
	artists := listArtists(t, srv, token, libID)
	var trackID string
	for _, a := range artists.Artists {
		for _, al := range artistAlbums(t, srv, token, a.ID).Albums {
			tr := albumTracks(t, srv, token, al.ID)
			if len(tr.Tracks) > 0 {
				trackID = tr.Tracks[0].ID
				break
			}
		}
		if trackID != "" {
			break
		}
	}
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	before := getEnrichedDetail(t, srv, token, trackID)
	if before.Title == "" {
		t.Fatalf("precondition: track has no tag title to protect")
	}

	applied := applyOverride(t, srv, token, trackID, "mb-x")
	// The override still decorated the Track (overview applied)...
	if applied.Overview != "Applied synopsis." {
		t.Errorf("override did not decorate the track: overview=%q", applied.Overview)
	}
	// ...but the DISPLAY title must remain the tag-derived title, never the
	// provider's canonical recording name (ADR-0002).
	if applied.DisplayTitle == canonicalName {
		t.Errorf("track display title clobbered by provider name: %q", applied.DisplayTitle)
	}
	if applied.Title != before.Title {
		t.Errorf("track identity title changed: before=%q after=%q", before.Title, applied.Title)
	}
	// The effective displayed title (displayTitle || title) is still the tag title.
	effective := applied.DisplayTitle
	if effective == "" {
		effective = applied.Title
	}
	if effective != before.Title {
		t.Errorf("effective track title changed to %q, want tag title %q", effective, before.Title)
	}
}
