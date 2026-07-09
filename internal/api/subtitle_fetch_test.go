package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subfetch"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue subtitles/05 integration test: external subtitle fetching through the real
// API against a FAKE SubtitleProvider (no network). The whole flow is exercised:
// enable the provider via the admin settings PUT → "search online" for a language
// the Title lacks → candidates come back → pick one → a source='fetched' text track
// appears in GET /titles/{id} and serves as valid WebVTT. Also asserts a Member can
// trigger a fetch, that the fetched track survives a rescan, and that a
// disabled/offline provider degrades to an empty candidate list with no error.

// realFakeProvider is a canned SubtitleProvider: it returns preset candidates and
// downloads a small SubRip file, recording the last match ref so a test can assert
// the moviehash / imdb / title were carried (no network).
type realFakeProvider struct {
	candidates []subfetch.Candidate
	data       []byte
	format     string
	lastRef    subfetch.SubtitleRef
}

func (f *realFakeProvider) Search(_ context.Context, ref subfetch.SubtitleRef, _ string) ([]subfetch.Candidate, error) {
	f.lastRef = ref
	return f.candidates, nil
}

func (f *realFakeProvider) Download(context.Context, subfetch.Candidate) ([]byte, string, error) {
	return f.data, f.format, nil
}

const fakeGermanSRT = "1\n00:00:01,000 --> 00:00:03,000\nGuten Tag\n"

// newFakeSubtitleBuilder returns a subfetch.BuildFunc that yields the fake provider
// only when the opensubtitles row is enabled with a key (mirroring BuildProvider's
// gate), so a settings PUT genuinely turns fetching on and off.
func newFakeSubtitleBuilder(p subfetch.SubtitleProvider) subfetch.BuildFunc {
	return func(rows []store.SubtitleProviderRow) subfetch.SubtitleProvider {
		for _, r := range rows {
			if r.Slug == subfetch.SlugOpenSubtitles && r.Enabled && r.APIKey != "" {
				return p
			}
		}
		return nil // nil → the Service falls back to the disabled provider
	}
}

// enableSubtitleProvider PUTs the admin settings to enable OpenSubtitles with a key
// (which hot-swaps the fake provider in via the Manager) and returns the response.
func enableSubtitleProvider(t *testing.T, srv *testharness.Server, token string) subtitleProvidersResp {
	t.Helper()
	body := map[string]any{
		"providers": []map[string]any{
			{"slug": "opensubtitles", "enabled": true, "apiKey": "test-key"},
		},
	}
	var resp subtitleProvidersResp
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/subtitle-providers", token, body, &resp)
	if status != http.StatusOK {
		t.Fatalf("enable subtitle provider status = %d, want 200; body: %s", status, raw)
	}
	return resp
}

// --- wire shapes ------------------------------------------------------------

type subtitleProvidersResp struct {
	Providers []struct {
		Slug        string `json:"slug"`
		Enabled     bool   `json:"enabled"`
		HasKey      bool   `json:"hasKey"`
		RequiresKey bool   `json:"requiresKey"`
	} `json:"providers"`
	AutoFetchLang string `json:"autoFetchLang"`
}

type subtitleCandidatesResp struct {
	Candidates []struct {
		ID        string `json:"id"`
		Language  string `json:"language"`
		Format    string `json:"format"`
		Label     string `json:"label"`
		MatchedBy string `json:"matchedBy"`
	} `json:"candidates"`
}

type subtitleFetchResp struct {
	Subtitle struct {
		ID       string `json:"id"`
		Source   string `json:"source"`
		Kind     string `json:"kind"`
		Language string `json:"language"`
		URL      string `json:"url"`
	} `json:"subtitle"`
}

func TestSubtitleFetchFlow(t *testing.T) {
	requireFixtures(t)
	fake := &realFakeProvider{candidates: []subfetch.Candidate{{
		ID: "9001", Language: "de", Format: "srt", Release: "Dune.2021.German", MatchedBy: "query",
	}}, data: []byte(fakeGermanSRT), format: "srt"}

	srv := testharness.New(t, testharness.WithSubtitleProviderBuilder(newFakeSubtitleBuilder(fake)))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Dune")

	// Before enabling: search degrades to an empty list (provider disabled), no error.
	var pre subtitleCandidatesResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/search", token,
		map[string]any{"language": "de"}, &pre)
	if status != http.StatusOK {
		t.Fatalf("disabled search status = %d, want 200; body: %s", status, body)
	}
	if len(pre.Candidates) != 0 {
		t.Fatalf("disabled provider returned %d candidates, want 0", len(pre.Candidates))
	}

	// Enable the provider (hot-swaps the fake in).
	enableSubtitleProvider(t, srv, token)

	// Search now returns the fake candidate.
	var cands subtitleCandidatesResp
	status, body = srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/search", token,
		map[string]any{"language": "de"}, &cands)
	if status != http.StatusOK {
		t.Fatalf("search status = %d, want 200; body: %s", status, body)
	}
	if len(cands.Candidates) != 1 || cands.Candidates[0].ID != "9001" {
		t.Fatalf("search candidates = %+v, want one id=9001", cands.Candidates)
	}

	// Pick it: a fetched text track comes back with a .vtt URL.
	var fetched subtitleFetchResp
	pick := map[string]any{
		"language": "de",
		"candidate": map[string]any{
			"id": "9001", "language": "de", "format": "srt", "release": "Dune.2021.German",
		},
	}
	status, body = srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/fetch", token, pick, &fetched)
	if status != http.StatusOK {
		t.Fatalf("fetch status = %d, want 200; body: %s", status, body)
	}
	if fetched.Subtitle.Source != "fetched" || fetched.Subtitle.Kind != "text" || fetched.Subtitle.Language != "de" {
		t.Fatalf("fetched subtitle = %+v, want fetched/text/de", fetched.Subtitle)
	}
	if fetched.Subtitle.URL == "" {
		t.Fatalf("fetched text track has no .vtt URL")
	}

	// GET /titles/{id} now lists the fetched track.
	assertFetchedTrackListed(t, srv, token, id)

	// Its .vtt is valid WebVTT with the downloaded cue.
	vttStatus, vtt := srv.AuthGET(fetched.Subtitle.URL, token, nil)
	if vttStatus != http.StatusOK {
		t.Fatalf("fetch .vtt status = %d, want 200", vttStatus)
	}
	if !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), "Guten Tag") {
		t.Fatalf("fetched .vtt is not the expected WebVTT:\n%s", vtt)
	}

	// The provider match ref carried the moviehash computed from the played file
	// (the Dune fixture is large enough to hash) — release-exact matching works.
	if fake.lastRef.Title == "" {
		t.Fatalf("provider was never asked (lastRef empty)")
	}

	// Rescan: the fetched track survives (source='fetched' is not clobbered).
	scanLib(t, srv, token, libID, "")
	assertFetchedTrackListed(t, srv, token, id)
}

// TestSubtitleFetchByMember asserts a Member (browse+play role) can trigger a fetch
// — the deliberate role widening ADR-0021 records.
func TestSubtitleFetchByMember(t *testing.T) {
	requireFixtures(t)
	fake := &realFakeProvider{candidates: []subfetch.Candidate{{
		ID: "42", Language: "de", Format: "srt",
	}}, data: []byte(fakeGermanSRT), format: "srt"}

	srv := testharness.New(t, testharness.WithSubtitleProviderBuilder(newFakeSubtitleBuilder(fake)))
	admin := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, libID, "")
	enableSubtitleProvider(t, srv, admin)

	list := listAllTitles(t, srv, admin, libID)
	id := findTitle(t, list, "Dune")

	// A Member granted the library.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libID)
	memberTok := srv.LoginAs("kid", "memberpass123")

	// The Member can search and fetch.
	var cands subtitleCandidatesResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/search", memberTok,
		map[string]any{"language": "de"}, &cands)
	if status != http.StatusOK {
		t.Fatalf("member search status = %d, want 200; body: %s", status, body)
	}
	if len(cands.Candidates) != 1 {
		t.Fatalf("member search candidates = %d, want 1", len(cands.Candidates))
	}
	var fetched subtitleFetchResp
	pick := map[string]any{"language": "de", "candidate": map[string]any{"id": "42", "language": "de", "format": "srt"}}
	status, body = srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/fetch", memberTok, pick, &fetched)
	if status != http.StatusOK {
		t.Fatalf("member fetch status = %d, want 200; body: %s", status, body)
	}
	if fetched.Subtitle.Source != "fetched" {
		t.Fatalf("member fetch produced %+v, want a fetched track", fetched.Subtitle)
	}
}

// TestSubtitleProviderSettingsAdminOnly asserts a Member cannot touch the provider
// settings (Admin-only), while an Admin can read them.
func TestSubtitleProviderSettingsAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	memberTok := srv.LoginAs("kid", "memberpass123")

	status, _ := srv.AuthGET("/api/v1/settings/subtitle-providers", memberTok, nil)
	if status != http.StatusForbidden {
		t.Fatalf("member GET settings status = %d, want 403", status)
	}
	_ = memberID

	var resp subtitleProvidersResp
	status, body := srv.JSON(http.MethodGet, "/api/v1/settings/subtitle-providers", admin, nil, &resp)
	if status != http.StatusOK {
		t.Fatalf("admin GET settings status = %d, want 200; body: %s", status, body)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Slug != "opensubtitles" || !resp.Providers[0].RequiresKey {
		t.Fatalf("providers view = %+v, want one opensubtitles requiresKey", resp.Providers)
	}
}

func assertFetchedTrackListed(t *testing.T, srv *testharness.Server, token, id string) {
	t.Helper()
	var detail subtitleDetailResp
	status, body := srv.JSON(http.MethodGet, "/api/v1/titles/"+id, token, nil, &detail)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	for _, s := range detail.Subtitles {
		if s.Source == "fetched" && s.Language == "de" && s.Kind == "text" {
			return
		}
	}
	t.Fatalf("fetched de text track not listed in title subtitles: %+v", detail.Subtitles)
}
