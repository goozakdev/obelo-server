package api_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for .scratch/bundled-plugins issue 12: a picker search READS A
// PASTE. The web app no longer decides with a per-provider regex whether the box
// holds a search term or an id — only the Library's lead knows what its ids look
// like — so every enrichmentCandidates endpoint first lets the lead read the query
// as a reference, and answers a resolved one with `resolvedRef`.

type candidateSearchResp struct {
	Candidates  []namespacedCandidate `json:"candidates"`
	HasMore     bool                  `json:"hasMore"`
	ResolvedRef bool                  `json:"resolvedRef"`
}

func getCandidateSearch(t *testing.T, srv *testharness.Server, token, path, q string, want int) candidateSearchResp {
	t.Helper()
	var out candidateSearchResp
	status, raw := srv.AuthGET(path+"?q="+url.QueryEscape(q), token, &out)
	if status != want {
		t.Fatalf("GET %s?q=%s = %d, want %d; body: %s", path, q, status, want, raw)
	}
	return out
}

// TestASearchResolvesAPasteThroughTheLibrarysLead: in a Library led by an Installed
// plugin, a query that plugin reads as a reference (its own `ref:<id>` shape, which
// no regex in the web app could ever have known) is resolved in its namespace; a
// term is searched.
func TestASearchResolvesAPasteThroughTheLibrarysLead(t *testing.T) {
	srv, token, libID := namespacedMusicServer(t)
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	path := "/api/v1/titles/" + trackID + "/enrichmentCandidates"

	got := getCandidateSearch(t, srv, token, path, "ref:pasted-9", http.StatusOK)
	if !got.ResolvedRef || len(got.Candidates) != 1 ||
		got.Candidates[0].ExternalID != "pasted-9" || got.Candidates[0].Source != nsLeadSlug {
		t.Errorf("paste = %+v, want one resolved candidate pasted-9 in %q", got, nsLeadSlug)
	}

	got = getCandidateSearch(t, srv, token, path, "Paranoid Android", http.StatusOK)
	if got.ResolvedRef || len(got.Candidates) != 1 || got.Candidates[0].ExternalID != nsLeadSlug+"-hit" {
		t.Errorf("term = %+v, want the lead's search hit, unresolved", got)
	}

	// The same on a browse parent.
	albumID := ""
	for _, a := range listArtists(t, srv, token, libID).Artists {
		if als := artistAlbums(t, srv, token, a.ID).Albums; len(als) > 0 {
			albumID = als[0].ID
			break
		}
	}
	if albumID == "" {
		t.Skip("no albums in music fixture")
	}
	got = getCandidateSearch(t, srv, token, "/api/v1/albums/"+albumID+"/enrichmentCandidates", "ref:rg-7", http.StatusOK)
	if !got.ResolvedRef || len(got.Candidates) != 1 || got.Candidates[0].ExternalID != "rg-7" {
		t.Errorf("album paste = %+v, want one resolved candidate rg-7", got)
	}
}

// TestASearchResolvesAMusicBrainzPasteAndKeepsThePastesErrors: the shipped lead's
// shapes still resolve, and a paste fails as a paste — a stale id is 404 "no
// record", a wrong-kind link is the 400 that says which link to paste — never a
// free-text search for a URL. A "show more" page is always a search.
func TestASearchResolvesAMusicBrainzPasteAndKeepsThePastesErrors(t *testing.T) {
	requireMusicFixtures(t)
	const goodID = "11111111-1111-1111-1111-111111111111"
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "seed", Title: "Seed", Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			if ref.MusicbrainzID == goodID {
				return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: goodID, Name: "Pasted Track"}, nil
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
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	path := "/api/v1/titles/" + trackID + "/enrichmentCandidates"

	got := getCandidateSearch(t, srv, token, path, "https://musicbrainz.org/recording/"+goodID, http.StatusOK)
	if !got.ResolvedRef || len(got.Candidates) != 1 || got.Candidates[0].ExternalID != goodID ||
		got.Candidates[0].Source != pluginapi.NamespaceMusicBrainz || got.HasMore {
		t.Errorf("url paste = %+v, want one resolved candidate %s in musicbrainz", got, goodID)
	}
	getCandidateSearch(t, srv, token, path, "22222222-2222-2222-2222-222222222222", http.StatusNotFound)
	getCandidateSearch(t, srv, token, path, "https://musicbrainz.org/artist/"+goodID, http.StatusBadRequest)

	var page candidateSearchResp
	status, raw := srv.AuthGET(path+"?page=1&q="+url.QueryEscape(goodID), token, &page)
	if status != http.StatusOK || page.ResolvedRef || len(page.Candidates) != 1 || page.Candidates[0].ExternalID != "seed" {
		t.Errorf("page 1 = %d %+v (%s), want the search's next page, never a paste", status, page, raw)
	}
}

// TestALeadThatReadsNoPastesIsNotHandedTheHostsReading: the host's own readers
// (a bare TMDB number, a MusicBrainz UUID) answer only for a Library those sources
// lead. A Library led by a source that declares no external-ref capability used to
// have a MusicBrainz UUID read as a MusicBrainz id and looked up at a lead that
// has never heard of MusicBrainz; now it is a search term.
func TestALeadThatReadsNoPastesIsNotHandedTheHostsReading(t *testing.T) {
	requireMusicFixtures(t)
	const slug = "nsnoparse"
	reg := namespacedMusicRegistration(slug)
	reg.Descriptor.Capabilities = []pluginapi.Capability{pluginapi.CapabilitySearch}
	srv := testharness.New(t,
		testharness.WithMetadataPlugins(reg),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": slug, "enabled": true, "apiKey": "k"},
	}}, http.StatusOK)
	if p := putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": slug}, http.StatusOK); p.EffectiveAuthoritative.Slug != slug {
		t.Fatalf("effective lead = %q, want %q", p.EffectiveAuthoritative.Slug, slug)
	}
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}

	got := getCandidateSearch(t, srv, token, "/api/v1/titles/"+trackID+"/enrichmentCandidates",
		"11111111-1111-1111-1111-111111111111", http.StatusOK)
	if got.ResolvedRef || len(got.Candidates) != 1 || got.Candidates[0].ExternalID != slug+"-hit" {
		t.Errorf("uuid under a non-reading lead = %+v, want the lead's search hit, unresolved", got)
	}
}
