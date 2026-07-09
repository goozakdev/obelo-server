package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// artwork-management issue 02 black-box test: the Artist Photo picker now returns a
// real candidate grid. With the music image sources composed behind the faked
// MetadataProvider (fanart.tv's artistthumb[] + TheAudioDB), the artist
// artworkCandidates endpoint returns the LIST (not the single auto-picked image),
// an album still returns its cover candidates, and all four actions stay Admin-only
// — driven through the HTTP API with zero network.

// TestArtistPhotoCandidatesList: an Artist's poster-role picker returns every
// artist photo the (faked) provider offers — a grid, not one image — and each
// candidate carries its source + dimensions. An Album's cover picker is unchanged.
func TestArtistPhotoCandidatesList(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			// Enrichment resolves each music entity to a pinned id so the picker has a
			// record to list against (the artist branch composes its image sources).
			return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "mb-" + ref.Kind}, nil
		},
		artworkFn: func(ref enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			if ref.Kind == "artist" {
				// The chain surfaces the full artist-photo list (fanart.tv artistthumb[]
				// + TheAudioDB), not the one image Lookup auto-picks.
				return []enrich.ArtworkCandidate{
					{URL: "https://img.example/artist-1.jpg", Width: 1000, Height: 1000, Source: "fanart.tv"},
					{URL: "https://img.example/artist-2.jpg", Width: 500, Height: 500, Source: "fanart.tv"},
					{URL: "https://img.example/artist-3.jpg", Source: "theaudiodb"},
				}, nil
			}
			// An album still lists its Cover Art Archive covers, unchanged.
			return []enrich.ArtworkCandidate{
				{URL: "https://img.example/cover-" + role + ".jpg", Source: "coverartarchive"},
			}, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("ARTISTART")}),
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

	// (AC) The artist picker returns the LIST of artist photos, not a single image.
	var cands artworkCandidatesResp
	if st, body := srv.AuthGET("/api/v1/artists/"+artistID+"/artworkCandidates?role=poster", token, &cands); st != http.StatusOK {
		t.Fatalf("GET artist artworkCandidates = %d; body: %s", st, body)
	}
	if len(cands.Candidates) != 3 {
		t.Fatalf("artist poster candidates = %d, want 3 (a grid, not one image): %+v", len(cands.Candidates), cands)
	}
	if cands.Candidates[0].URL != "https://img.example/artist-1.jpg" || cands.Candidates[0].Source != "fanart.tv" {
		t.Errorf("candidate[0] = %+v, want the first fanart.tv artist photo", cands.Candidates[0])
	}
	if cands.Candidates[0].Width != 1000 || cands.Candidates[0].Height != 1000 {
		t.Errorf("candidate[0] dims = %dx%d, want 1000x1000", cands.Candidates[0].Width, cands.Candidates[0].Height)
	}

	// Picking one applies + Locks the poster role on the artist (existing behavior).
	var afterPick entityDetailResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/artists/"+artistID+"/artwork", token,
		map[string]any{"role": "poster", "url": cands.Candidates[1].URL}, &afterPick); st != http.StatusOK {
		t.Fatalf("PUT artist artwork = %d; body: %s", st, body)
	}
	if !contains(afterPick.LockedFields, "poster") {
		t.Errorf("artist poster not Locked after pick: %+v", afterPick.LockedFields)
	}

	// An album still returns its cover candidates (the album path is unchanged).
	albums := artistAlbums(t, srv, token, artistID)
	if len(albums.Albums) > 0 {
		var albCands artworkCandidatesResp
		if st, body := srv.AuthGET("/api/v1/albums/"+albums.Albums[0].ID+"/artworkCandidates?role=cover", token, &albCands); st != http.StatusOK {
			t.Fatalf("GET album artworkCandidates = %d; body: %s", st, body)
		}
		if len(albCands.Candidates) != 1 || albCands.Candidates[0].Source != "coverartarchive" {
			t.Errorf("album cover candidates = %+v, want a single coverartarchive cover", albCands.Candidates)
		}
	}
}

// TestArtistPhotoCandidatesAdminOnly: a Member cannot list artist photo candidates,
// pick one, or release the role — every artwork action on an artist is Admin-only.
func TestArtistPhotoCandidatesAdminOnly(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "mb-" + ref.Kind}, nil
		},
		artworkFn: func(_ enrich.TitleRef, _ string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{{URL: "https://img.example/artist.jpg", Source: "fanart.tv"}}, nil
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

	srv.CreateMember("artmember", "memberpass123")
	mTok := srv.LoginAs("artmember", "memberpass123")

	if st, _ := srv.AuthGET("/api/v1/artists/"+artistID+"/artworkCandidates?role=poster", mTok, nil); st != http.StatusForbidden {
		t.Errorf("member GET artist artworkCandidates = %d, want 403", st)
	}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/artists/"+artistID+"/artwork", mTok,
		map[string]any{"role": "poster", "url": "https://img.example/artist.jpg"}, nil); st != http.StatusForbidden {
		t.Errorf("member PUT artist artwork = %d, want 403", st)
	}
	// An Admin still gets a real grid (proves the endpoint works, not just that the
	// Member is blocked).
	var cands artworkCandidatesResp
	if st, _ := srv.AuthGET("/api/v1/artists/"+artistID+"/artworkCandidates?role=poster", token, &cands); st != http.StatusOK {
		t.Errorf("admin GET artist artworkCandidates failed: %d", st)
	}
	if len(cands.Candidates) == 0 {
		t.Errorf("admin got no artist candidates: %+v", cands)
	}
}
