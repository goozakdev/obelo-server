package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// item-editing issue 04 black-box tests: the "Wrong item" destructive identity
// correction (ADR-0019/ADR-0014). Offered on Movies and Shows only. Applying a
// picked candidate writes a folder-keyed Match override + re-keys identity, resets
// watch state, clears Locked fields, and re-enriches to the new record. Driven
// through the HTTP API with the FAKE MetadataProvider (its Search seam), zero
// network. Asserts: identity change + watch reset + locks cleared on a Movie AND a
// Show; the override survives a rescan; the endpoint is rejected for Music/Episode;
// Admin-only; and the correction propagates over SSE.

// --- wire shapes ------------------------------------------------------------

// showIdentityResp reads the identity + Edit-item surface off the Show detail
// (GET /shows/{id}/seasons → show object).
type showIdentityResp struct {
	Show struct {
		ID                 string              `json:"id"`
		IdentityKey        string              `json:"identityKey"`
		Overview           string              `json:"overview"`
		LockedFields       []string            `json:"lockedFields"`
		EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
	} `json:"show"`
}

// --- helpers ----------------------------------------------------------------

// wrongItemFake wires a provider whose Search returns a corrected candidate and
// whose Lookup resolves BY the picked id (movie or show), while a by-name (auto)
// lookup returns a deliberately WRONG record so a revert would be observable.
func wrongItemFake() *fakeProvider {
	return &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "555", Title: "The Correct Work", Year: 2018, Kind: kind,
					ThumbnailURL: "https://img.example/correct.jpg", Disambiguation: "the genuinely right work"},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "show":
				m := enrich.TitleMetadata{Matched: true, Source: "tmdb", Genres: []string{"Drama"}}
				if ref.TMDBID == "555" {
					m.Overview = "CORRECTED show overview."
					m.ExternalID = "555"
				} else {
					m.Overview = "WRONG auto show overview."
					m.ExternalID = "auto"
				}
				return m, nil
			case "movie":
				m := richMeta()
				if ref.TMDBID == "555" {
					m.Overview = "CORRECTED movie overview."
					m.ExternalID = "555"
				} else {
					m.Overview = "WRONG auto movie overview."
					m.ExternalID = "auto"
				}
				return m, nil
			default:
				// episode/season lookups resolve minimally so a pass completes.
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "x", ExternalID: "auto"}, nil
			}
		},
	}
}

// putWrongItem drives PUT /{kindPath}/{id}/identityCorrection with the picked
// candidate, returning the raw status + body so a caller can assert success or a
// rejection.
func putWrongItem(t *testing.T, srv *testharness.Server, token, kindPath, id string, body map[string]any, out any) (int, string) {
	t.Helper()
	status, b := srv.JSON(http.MethodPut, "/api/v1/"+kindPath+"/"+id+"/identityCorrection", token, body, out)
	return status, string(b)
}

func getShowIdentity(t *testing.T, srv *testharness.Server, token, showID string) showIdentityResp {
	t.Helper()
	var d showIdentityResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &d)
	if status != http.StatusOK {
		t.Fatalf("GET show detail = %d, want 200; body: %s", status, body)
	}
	return d
}

// --- Movie: the core Wrong-item loop ----------------------------------------

// TestWrongItemMovieRekeysResetsAndClears is the central acceptance loop for a
// Movie: apply a different work → identity_key changes to the picked work, watch
// state is reset, Locked fields are cleared, the item re-enriches to the new
// record, a folder-keyed Match override exists, and the correction survives a
// rescan.
func TestWrongItemMovieRekeysResetsAndClears(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Mistaken Movie (2001)")
	makeMovie(t, filepath.Join(folder, "Mistaken Movie (2001).mp4"))

	prov := wrongItemFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("POSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Mistaken Movie")

	// Establish the state a correction must WIPE: a watch state + a hand-edited
	// (Locked) field, plus the original identity to compare against.
	before := getEnrichedDetail(t, srv, token, id)
	if before.IdentityKey == "" {
		t.Fatalf("precondition: title has no identity key")
	}
	srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/watchState", token, map[string]any{"watched": true}, nil)
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token,
		map[string]any{"overview": "MY hand note"}, nil); st != http.StatusOK {
		t.Fatalf("seed lock: PUT metadata != 200")
	}

	// (AC) Apply the Wrong-item correction to the searched candidate.
	var applied enrichedDetailResp
	status, body := putWrongItem(t, srv, token, "titles", id,
		map[string]any{"externalId": "555", "title": "The Correct Work", "year": 2018}, &applied)
	if status != http.StatusOK {
		t.Fatalf("wrong-item apply = %d, want 200; body: %s", status, body)
	}

	// (AC) identity_key re-keyed to the picked work (tmdb:555), not the old parse.
	if applied.IdentityKey != "tmdb:555" {
		t.Errorf("identity key = %q, want tmdb:555 (identity changed to the picked work)", applied.IdentityKey)
	}
	if applied.IdentityKey == before.IdentityKey {
		t.Errorf("identity key unchanged (%q); a Wrong-item correction must re-key", applied.IdentityKey)
	}
	if applied.Title != "The Correct Work" {
		t.Errorf("title = %q, want The Correct Work", applied.Title)
	}
	// (AC) watch state reset.
	if applied.Watched {
		t.Errorf("watch state not reset after Wrong-item correction")
	}
	// (AC) Locked fields cleared (the hand note is gone; overview is the new record's).
	if len(applied.LockedFields) != 0 {
		t.Errorf("locked fields = %v, want empty (cleared on identity change)", applied.LockedFields)
	}
	// (AC) re-enriched to the picked record.
	if applied.Overview != "CORRECTED movie overview." {
		t.Errorf("overview = %q, want the picked record's; did it re-enrich?", applied.Overview)
	}

	// (AC) A folder-keyed Match override now exists for the movie folder.
	ovs := listOverrides(t, srv, token, libID)
	if len(ovs.Overrides) != 1 || ovs.Overrides[0].FolderPath != folder {
		t.Fatalf("match override not written for the folder: %+v", ovs.Overrides)
	}
	if ovs.Overrides[0].IdentityKey != "tmdb:555" {
		t.Errorf("override identity key = %q, want tmdb:555", ovs.Overrides[0].IdentityKey)
	}

	// (AC) The correction survives a rescan (the scanner keeps the corrected identity).
	scanLib(t, srv, token, libID, "")
	afterRescan := getEnrichedDetail(t, srv, token, id)
	if afterRescan.IdentityKey != "tmdb:555" {
		t.Errorf("identity reverted on rescan: key=%q, want tmdb:555", afterRescan.IdentityKey)
	}
	if afterRescan.Title != "The Correct Work" {
		t.Errorf("title reverted on rescan: %q", afterRescan.Title)
	}
	if got := len(listOverrides(t, srv, token, libID).Overrides); got != 1 {
		t.Errorf("override dropped after rescan (got %d, want 1)", got)
	}
}

// --- Show: the Wrong-item loop on a parent ----------------------------------

// TestWrongItemShowRekeysResetsAndClears mirrors the Movie loop on a Show: identity
// re-keys, every Episode's watch state resets, the Show's Locked fields clear, the
// Show re-enriches to the picked record, and the correction survives a rescan.
func TestWrongItemShowRekeysResetsAndClears(t *testing.T) {
	requireTVFixtures(t)
	prov := wrongItemFake()
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
	showID := findShow(t, shows, "The Bear")

	beforeID := getShowIdentity(t, srv, token, showID)
	if beforeID.Show.IdentityKey == "" {
		t.Fatalf("precondition: show has no identity key")
	}

	// Seed state to wipe: an Episode watch state + a hand-set (Locked) Show field.
	seasons := showSeasons(t, srv, token, showID)
	var epID string
	for _, s := range seasons.Seasons {
		eps := seasonEpisodes(t, srv, token, s.ID)
		if len(eps.Episodes) > 0 {
			epID = eps.Episodes[0].ID
			break
		}
	}
	if epID == "" {
		t.Skip("no episodes")
	}
	srv.JSON(http.MethodPut, "/api/v1/titles/"+epID+"/watchState", token, map[string]any{"watched": true}, nil)
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/metadata", token,
		map[string]any{"overview": "MY hand show note"}, nil); st != http.StatusOK {
		t.Fatalf("seed show lock: PUT metadata != 200")
	}

	// (AC) Apply the Wrong-item correction.
	var applied entityDetailResp
	status, body := putWrongItem(t, srv, token, "shows", showID,
		map[string]any{"externalId": "555", "title": "The Correct Work", "year": 2018}, &applied)
	if status != http.StatusOK {
		t.Fatalf("show wrong-item apply = %d, want 200; body: %s", status, body)
	}
	// (AC) re-enriched to the picked record + Locked fields cleared.
	if applied.Overview != "CORRECTED show overview." {
		t.Errorf("show overview = %q, want the picked record's", applied.Overview)
	}
	if len(applied.LockedFields) != 0 {
		t.Errorf("show locked fields = %v, want empty (cleared)", applied.LockedFields)
	}
	if applied.EnrichmentOverride == nil || applied.EnrichmentOverride.ExternalID != "555" {
		t.Errorf("show detail missing active override: %+v", applied.EnrichmentOverride)
	}

	// (AC) identity_key re-keyed to the picked work.
	afterID := getShowIdentity(t, srv, token, showID)
	if afterID.Show.IdentityKey != "tmdb:555" {
		t.Errorf("show identity key = %q, want tmdb:555", afterID.Show.IdentityKey)
	}
	if afterID.Show.IdentityKey == beforeID.Show.IdentityKey {
		t.Errorf("show identity unchanged (%q); Wrong-item must re-key", afterID.Show.IdentityKey)
	}

	// (AC) Episode watch state reset (read before a rescan re-keys episodes).
	ep := getEnrichedDetail(t, srv, token, epID)
	if ep.Watched {
		t.Errorf("episode watch state not reset after Show Wrong-item correction")
	}

	// (AC) A folder-keyed Match override exists and survives a rescan (identity kept).
	if got := len(listOverrides(t, srv, token, libID).Overrides); got != 1 {
		t.Fatalf("show match override not written (got %d, want 1)", got)
	}
	scanLib(t, srv, token, libID, "")
	if got := getShowIdentity(t, srv, token, showID); got.Show.IdentityKey != "tmdb:555" {
		t.Errorf("show identity reverted on rescan: key=%q, want tmdb:555", got.Show.IdentityKey)
	}
}

// --- Rejection: absent for Music + Episode ----------------------------------

// TestWrongItemRejectedForEpisodeAndTrack: the identity-correction endpoint 422s
// (WRONG_KIND) on an Episode and a Track — Wrong-item is Movie/Show only (Episodes
// have no per-episode anchor; music identity is tag-anchored).
func TestWrongItemRejectedForEpisodeAndTrack(t *testing.T) {
	t.Run("episode → 422", func(t *testing.T) {
		requireTVFixtures(t)
		srv, token, libID := scanTVLibrary(t)
		shows := listShows(t, srv, token, libID)
		if len(shows.Shows) == 0 {
			t.Skip("no shows")
		}
		seasons := showSeasons(t, srv, token, shows.Shows[0].ID)
		var epID string
		for _, s := range seasons.Seasons {
			eps := seasonEpisodes(t, srv, token, s.ID)
			if len(eps.Episodes) > 0 {
				epID = eps.Episodes[0].ID
				break
			}
		}
		if epID == "" {
			t.Skip("no episodes")
		}
		status, body := putWrongItem(t, srv, token, "titles", epID,
			map[string]any{"externalId": "555", "title": "X"}, nil)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("episode wrong-item = %d, want 422; body: %s", status, body)
		}
		if !strings.Contains(body, "WRONG_KIND") {
			t.Errorf("episode rejection code missing WRONG_KIND; body: %s", body)
		}
	})

	t.Run("track → 422", func(t *testing.T) {
		requireMusicFixtures(t)
		srv, token, libID := scanMusicLibrary(t)
		artists := listArtists(t, srv, token, libID)
		if len(artists.Artists) == 0 {
			t.Skip("no artists")
		}
		var trackID string
		for _, a := range artists.Artists {
			albums := artistAlbums(t, srv, token, a.ID)
			for _, al := range albums.Albums {
				tracks := albumTracks(t, srv, token, al.ID)
				if len(tracks.Tracks) > 0 {
					trackID = tracks.Tracks[0].ID
					break
				}
			}
			if trackID != "" {
				break
			}
		}
		if trackID == "" {
			t.Skip("no tracks")
		}
		status, body := putWrongItem(t, srv, token, "titles", trackID,
			map[string]any{"externalId": "555", "title": "X"}, nil)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("track wrong-item = %d, want 422; body: %s", status, body)
		}
		if !strings.Contains(body, "WRONG_KIND") {
			t.Errorf("track rejection code missing WRONG_KIND; body: %s", body)
		}
	})
}

// --- Access: Admin-only + SSE -----------------------------------------------

// TestWrongItemRequiresAdmin: a Member cannot run a Wrong-item correction.
func TestWrongItemRequiresAdmin(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "Member Movie (2005)", "Member Movie (2005).mp4"))

	prov := wrongItemFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Member Movie")

	srv.CreateMember("wm", "memberpass123")
	mTok := login(t, srv, "wm", "memberpass123", "P", "ios", "wmc").Token
	status, _ := putWrongItem(t, srv, mTok, "titles", id,
		map[string]any{"externalId": "555", "title": "X"}, nil)
	if status != http.StatusForbidden {
		t.Errorf("member wrong-item = %d, want 403", status)
	}
}

// TestWrongItemSSE: applying a Wrong-item correction emits a libraryUpdated event
// so browse reflects the re-identification live (ADR-0016).
func TestWrongItemSSE(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "Live Movie (2007)", "Live Movie (2007).mp4"))

	prov := wrongItemFake()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Live Movie")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)

	if status, body := putWrongItem(t, srv, token, "titles", id,
		map[string]any{"externalId": "555", "title": "The Correct Work", "year": 2018}, nil); status != http.StatusOK {
		t.Fatalf("wrong-item apply = %d, want 200; body: %s", status, body)
	}

	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: libraryUpdated") ||
			strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}
