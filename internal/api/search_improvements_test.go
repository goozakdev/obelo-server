package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for item-editing/search-improvements: the paste-a-MusicBrainz-id/URL
// escape hatch (preview + kind-validation + 404 on a stale id) and the artist-scoping +
// paging opts threaded from the picker through the API to the provider Search. Driven
// through the HTTP API with the FAKE provider, zero network.

// firstTrackID walks Artist → Album → Track to find any track id (skips when the
// fixture has none), mirroring the enrich-override track test.
func firstTrackID(t *testing.T, srv *testharness.Server, token, libID string) string {
	t.Helper()
	for _, a := range listArtists(t, srv, token, libID).Artists {
		for _, al := range artistAlbums(t, srv, token, a.ID).Albums {
			tr := albumTracks(t, srv, token, al.ID)
			if len(tr.Tracks) > 0 {
				return tr.Tracks[0].ID
			}
		}
	}
	return ""
}

// TestExternalPreviewPasteMusicBrainzID: a pasted MusicBrainz URL previews the record
// (by-id Lookup, no search); a stale/unknown id is 404 "not found" (not a hang/500);
// and an artist URL pasted on a Track is a 400 kind mismatch.
func TestExternalPreviewPasteMusicBrainzID(t *testing.T) {
	requireMusicFixtures(t)
	const goodID = "11111111-1111-1111-1111-111111111111"
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "seed", Title: "Seed", Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			// The good pasted MBID resolves to a record; anything else (a stale id) is
			// the normal "no record" outcome, which must preview as 404.
			if ref.MusicbrainzID == goodID {
				return enrich.TitleMetadata{Matched: true, Source: "musicbrainz",
					ExternalID: goodID, Name: "Pasted Track"}, nil
			}
			return enrich.TitleMetadata{}, enrich.ErrNoMatch
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

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}

	// A good pasted URL previews the record by id.
	var prev candidateResp
	status, body := srv.AuthGET(
		"/api/v1/titles/"+trackID+"/externalPreview?ref=https://musicbrainz.org/recording/"+goodID,
		token, &prev)
	if status != http.StatusOK {
		t.Fatalf("preview good id = %d, want 200; body: %s", status, body)
	}
	if prev.ExternalID != goodID || prev.Title != "Pasted Track" || prev.Kind != "track" {
		t.Errorf("preview candidate = %+v", prev)
	}

	// A stale/unknown id previews as 404 (not a hang or 500).
	stale := "22222222-2222-2222-2222-222222222222"
	if status, _ := srv.AuthGET("/api/v1/titles/"+trackID+"/externalPreview?ref="+stale, token, nil); status != http.StatusNotFound {
		t.Errorf("stale id preview = %d, want 404", status)
	}

	// An artist URL pasted on a Track is a 400 kind mismatch (never pinned blind).
	if status, _ := srv.AuthGET(
		"/api/v1/titles/"+trackID+"/externalPreview?ref=https://musicbrainz.org/artist/"+goodID,
		token, nil); status != http.StatusBadRequest {
		t.Errorf("artist-url-on-track preview = %d, want 400", status)
	}

	// After previewing, the pasted id applies through the EXISTING override endpoint.
	applied := applyOverride(t, srv, token, trackID, goodID)
	if applied.ID != trackID {
		t.Errorf("apply of pasted id returned wrong detail: %+v", applied)
	}
}

// TestExternalPreviewPasteReleaseURL: a pasted MusicBrainz /release/ URL on an Album
// isn't itself an album pin — the server resolves it to its parent release-group (the
// album) and previews THAT, with the release-group id as the pin the apply will use.
func TestExternalPreviewPasteReleaseURL(t *testing.T) {
	requireMusicFixtures(t)
	const releaseID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const rgID = "629a5133-b9e6-43c5-8cb6-594a7cbfbfed"
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "seed", Title: "Seed", Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			// The /release/ URL threads through as a ReleaseMBID (never a release-group
			// id); we answer with the parent release-group record.
			if ref.ReleaseMBID == releaseID {
				return enrich.TitleMetadata{Matched: true, Source: "musicbrainz",
					ExternalID: rgID, Name: "Anastasia"}, nil
			}
			return enrich.TitleMetadata{}, enrich.ErrNoMatch
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
	albumID, _, _ := okComputerAlbum(t, srv, token, libID)

	var prev candidateResp
	status, body := srv.AuthGET(
		"/api/v1/albums/"+albumID+"/externalPreview?ref=https://musicbrainz.org/release/"+releaseID,
		token, &prev)
	if status != http.StatusOK {
		t.Fatalf("preview release url = %d, want 200; body: %s", status, body)
	}
	if prev.ExternalID != rgID || prev.Title != "Anastasia" || prev.Kind != "album" {
		t.Errorf("release-url preview candidate = %+v, want release-group %q", prev, rgID)
	}
}

// TestSearchArtistScopingAndPagingThreaded: the picker's artist + page query params
// reach the provider Search as SearchOptions (artist narrowing + a paged offset).
func TestSearchArtistScopingAndPagingThreaded(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "mb-1", Title: "Hit", Kind: kind}}, nil
		},
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
			return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "seed"}, nil
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

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}

	var res candidatesResp
	status, body := srv.AuthGET(
		"/api/v1/titles/"+trackID+"/enrichmentCandidates?q=Greatest+Hits&artist=Queen&page=1",
		token, &res)
	if status != http.StatusOK {
		t.Fatalf("scoped search = %d, want 200; body: %s", status, body)
	}
	last := prov.lastSearch()
	if last.opts.Artist != "Queen" {
		t.Errorf("provider Search artist = %q, want Queen", last.opts.Artist)
	}
	if last.opts.Offset != enrich.SearchCandidateLimit {
		t.Errorf("provider Search offset = %d, want a paged %d", last.opts.Offset, enrich.SearchCandidateLimit)
	}
}
