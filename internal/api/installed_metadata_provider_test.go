package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for an INSTALLED Metadata provider (ADR-0057 decisions 3 and 4,
// ADR-0058; .scratch/plugin-system issue 11): a WebAssembly module and a manifest
// an Admin placed by hand under <dataDir>/plugins/<id>/, loaded at boot into the
// same registry the Built-ins were registered into.
//
// Everything asserted here is what an Admin can observe: a settings response, a
// policy response, the overview on an item after a pass, and the sentence the
// Edit-item box shows. Nothing is substituted — the real Catalog, the real
// builder, the real settings API and the real Enrichment policy decide whether the
// Plugin is selectable, whether it is reachable, and whether it leads. The module
// is compiled from source by the suite (internal/plugins/plugintest), so the
// sandbox, the ABI and the host functions are all real.
//
// It is the template from internal/api/plugin_authoritative_test.go with the fake
// in-process Plugin replaced by a guest behind a sandbox boundary, which is the
// whole of what issue 11 had to prove.

// The strings the guest writes. They live in its source
// (internal/plugins/plugintest/testdata/guest/main.go) and nothing else in this
// server would write them, so an item carrying one was decorated by the guest.
const (
	guestOverview   = "Filled from inside the sandbox by an Installed plugin."
	guestWrongTitle = "An Entirely Different Record"
	guestImageHost  = "https://images.example.test"
	guestPosterURL  = guestImageHost + "/art/poster.jpg"
)

// guestBaseURL is the manifest's default endpoint, carrying the mode marker that
// selects which part this one module plays. A real Plugin's default URL is just
// its source's API; the marker is the only thing a test can set that reaches
// inside the guest.
func guestBaseURL(mode string) string {
	return "https://source.example.test/v1?obelo-mode=" + mode
}

// installGuestProvider places a manifest and the compiled module on disk, the way
// an Admin does by hand in this slice, and returns nothing because there is
// nothing to return: everything after this is the ordinary settings API.
func installGuestProvider(t *testing.T, dataDir, id, mode string, p pluginapi.ManifestProvides) {
	t.Helper()
	m := plugintest.MetadataProviderManifest(id, p)
	m.Settings.DefaultURL = guestBaseURL(mode)
	m.Settings.DefaultURL2 = guestImageHost
	plugintest.Install(t, dataDir, m)
}

// keyProvider turns a Plugin on through the real settings endpoint, with a key,
// exactly as an Admin does. A key-requiring Plugin with no key is registered and
// configurable and never composed — which is the negative half of this file.
func keyProvider(t *testing.T, srv *testharness.Server, token, slug string) {
	t.Helper()
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": slug, "enabled": true, "apiKey": "an-operator-key"},
	}}, http.StatusOK)
}

// --- the video Supplement ------------------------------------------------------

// leadVideoSlug is an in-process Full video provider that matches everything and
// fills NOTHING but the name. It stands in for TMDB, which this suite has no key
// for, and its blankness is the point: what the Installed Supplement contributes
// is exactly what this lead left empty.
const leadVideoSlug = "blankvideo"

type blankVideoPlugin struct {
	mu      sync.Mutex
	lookups int
}

func (p *blankVideoPlugin) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	p.mu.Lock()
	p.lookups++
	p.mu.Unlock()
	switch req.Ref.Kind {
	case "movie", "show", "season", "episode":
		return pluginapi.LookupResponse{
			Outcome: pluginapi.OutcomeMatched,
			Record: pluginapi.MetadataRecord{
				Matched:    true,
				Name:       req.Ref.Title,
				ExternalID: "lead-" + req.Ref.Kind,
				Source:     leadVideoSlug,
				// No overview, no artwork, on purpose.
			},
		}, nil
	default:
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
}

func (p *blankVideoPlugin) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (p *blankVideoPlugin) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func blankVideoRegistration(p *blankVideoPlugin) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:         leadVideoSlug,
			Name:         "Blank Video Lead",
			Kinds:        []string{pluginapi.KindVideo},
			Role:         pluginapi.RoleAuthoritative,
			Class:        pluginapi.ClassFull,
			RequiresKey:  true,
			Capabilities: []pluginapi.Capability{pluginapi.CapabilitySearch},
			DefaultURL:   "https://lead.example.test/v1",
			Description:  "A Full video provider registered only for this test.",
		},
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) { return p, nil },
	}
}

// TestAnInstalledVideoSupplementFillsWhatTheLeadLeftBlank is the first acceptance
// criterion: a guest declaring a video Supplement fills the overviews the
// Authoritative provider left blank on the next pass, and the artwork its record
// points at is downloaded BY THE HOST — the guest returns a URL and never bytes,
// so the identity-keyed cache and the guarded fetcher are unchanged.
func TestAnInstalledVideoSupplementFillsWhatTheLeadLeftBlank(t *testing.T) {
	requireFixtures(t)
	dataDir := t.TempDir()
	// The Admin's hand-placement, before the server ever starts.
	installGuestProvider(t, dataDir, "example-source", "video-supplement", pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindVideo},
		Role:  pluginapi.RoleSupplement,
		Class: pluginapi.ClassFull,
	})

	lead := &blankVideoPlugin{}
	fetcher := &fakeFetcher{data: []byte("image-bytes")}
	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithMetadataPlugins(blankVideoRegistration(lead)),
		testharness.WithArtworkFetcher(fetcher),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// It is on the metadata-provider settings screen, with the facts its manifest
	// declared — a registration is all it took to be configurable.
	got := providerBySlug(getProviders(t, srv, token), "example-source")
	if got.Slug != "example-source" {
		t.Fatalf("the Installed provider is missing from the settings screen: %+v", got)
	}

	// Key the lead and the Supplement, and point the Library at the lead.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": leadVideoSlug, "enabled": true, "apiKey": "lead-key"},
		{"slug": "example-source", "enabled": true, "apiKey": "an-operator-key"},
	}}, http.StatusOK)
	policy := putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": leadVideoSlug}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != leadVideoSlug {
		t.Fatalf("effective lead = %q, want the blank in-process lead", policy.EffectiveAuthoritative.Slug)
	}

	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "full")

	if lead.lookups == 0 {
		t.Fatal("the lead was never asked; something else led the pass")
	}
	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	d := getEnrichedDetail(t, srv, token, titles[0].ID)
	if d.Overview != guestOverview {
		t.Fatalf("overview = %q, want the Supplement's — the lead left it blank and the guest filled it", d.Overview)
	}

	// The artwork the guest POINTED at was fetched by the host's own fetcher. The
	// manifest's network allowlist does not bind that fetch and must not: it is the
	// host downloading a third party's URL under safefetch's policy, exactly as it
	// does for every Built-in's poster.
	if !fetcher.fetched(guestPosterURL) {
		t.Fatalf("the host never fetched the guest's artwork URL; it fetched %v", fetcher.seen())
	}
	if !hasArtworkRole(d, "poster") {
		t.Fatalf("no poster on the enriched title: %+v", d.Artwork)
	}
}

// --- the music Full provider ---------------------------------------------------

// TestAnInstalledPluginCanLeadAMusicLibrary is ADR-0057 decision 4 with a real
// sandbox under it: register (by placing two files), key, point, pass. It is the
// issue 04 template, and it keeps the negative that matters — an unkeyed
// key-requiring Plugin is never offered as a lead.
func TestAnInstalledPluginCanLeadAMusicLibrary(t *testing.T) {
	requireMusicFixtures(t)
	dataDir := t.TempDir()
	installGuestProvider(t, dataDir, "example-source", "", pluginapi.ManifestProvides{
		Kinds:        []string{pluginapi.KindMusic},
		Role:         pluginapi.RoleAuthoritative,
		Class:        pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{pluginapi.CapabilitySearch},
	})

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")

	// Not yet selectable: it requires a key and has none, so it is not a USABLE
	// Full provider (ADR-0027 — enabled and keyed are separate gates).
	if hasAuthoritativeCandidate(getPolicy(t, srv, token, libID), "example-source") {
		t.Error("an unkeyed key-requiring Installed plugin was offered as a lead")
	}

	keyProvider(t, srv, token, "example-source")
	policy := getPolicy(t, srv, token, libID)
	if !hasAuthoritativeCandidate(policy, "example-source") {
		t.Fatalf("a keyed Full music Plugin is not offered as a lead; candidates: %+v", policy.AuthoritativeCandidates)
	}
	if policy.EffectiveAuthoritative.Slug != "musicbrainz" {
		t.Errorf("before repointing, the lead = %q, want musicbrainz (the kind default)", policy.EffectiveAuthoritative.Slug)
	}

	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "example-source"}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != "example-source" {
		t.Fatalf("effective lead = %q, want the Installed plugin", policy.EffectiveAuthoritative.Slug)
	}
	if !policy.Effective.Music {
		t.Fatalf("music is off for a Library whose keyed Full lead is the Plugin: %+v", policy.Effective)
	}
	enrichLib(t, srv, token, libID, "full")

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	d := getEnrichedDetail(t, srv, token, trackID)
	if d.EnrichmentStatus != "matched" {
		t.Errorf("track status = %q, want matched", d.EnrichmentStatus)
	}
	if d.Overview != guestOverview {
		t.Errorf("track overview = %q, want the guest's record", d.Overview)
	}

	// Clearing the pointer hands the Library back to the kind default, live.
	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": nil}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != "musicbrainz" {
		t.Errorf("after clearing, the lead = %q, want musicbrainz", policy.EffectiveAuthoritative.Slug)
	}
}

// TestAGuestsSearchHitWithADifferentTitleIsRejectedByTheHost: the guest states
// only that it SEARCHED, and hands over the candidate's own title. The host runs
// the ADR-0050 acceptance test on what came back and settles the Track with the
// `search-rejected` reason. The guest is never asked to judge, and it could not:
// there is nothing in the contract for it to say "I believe this" with.
func TestAGuestsSearchHitWithADifferentTitleIsRejectedByTheHost(t *testing.T) {
	requireMusicFixtures(t)
	dataDir := t.TempDir()
	installGuestProvider(t, dataDir, "example-source", "music-search-hit", pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			// Declared, and in this mode the guest answers "unavailable" for it. That
			// is not incidental scaffolding — the ALBUM tier outranks the search
			// outcome on purpose (ADR-0050: "fix the Album" is the action that clears
			// the largest bucket), so a Track reaches its own search verdict only when
			// the tier has nothing to say about its Album. An unreadable tracklist is
			// exactly that: an outage, not a diagnosis.
			pluginapi.CapabilityAlbumTracklist,
		},
	})

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	keyProvider(t, srv, token, "example-source")
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "example-source"}, http.StatusOK)
	enrichLib(t, srv, token, libID, "full")

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	d := getEnrichedDetail(t, srv, token, trackID)
	if d.EnrichmentStatus == "matched" {
		t.Fatalf("the track was matched to a candidate with a different title: %+v", d)
	}
	// The rejected candidate's fields did not leak into the row: a confident wrong
	// overview is worse than an empty one (ADR-0049).
	if d.Overview != "" {
		t.Errorf("overview = %q, want empty — a rejected candidate must leave nothing behind", d.Overview)
	}

	// And the Admin is told WHY, with the reason that names the action.
	attention := listEnrichmentAttention(t, srv, token, libID)
	var reasons []string
	found := false
	for _, ti := range attention.Titles {
		reasons = append(reasons, ti.Title+"="+ti.EnrichmentReason)
		if ti.ID == trackID && ti.EnrichmentReason == "search-rejected" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no search-rejected reason for the track; the attention list says %v", reasons)
	}
}

// --- the capability gates -------------------------------------------------------

// TestAGuestWithoutSearchIsUnavailableInTheEditItemBox: an undeclared capability
// costs no call into the guest at all and produces the same sentence an
// unconfigured kind produces. That is the "unavailable" of ADR-0057 decision 3,
// unchanged by a sandbox being under it.
func TestAGuestWithoutSearchIsUnavailableInTheEditItemBox(t *testing.T) {
	requireMusicFixtures(t)
	dataDir := t.TempDir()
	// No capabilities at all: it can look up, and that is all it claims.
	installGuestProvider(t, dataDir, "example-source", "", pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
	})

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	keyProvider(t, srv, token, "example-source")
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "example-source"}, http.StatusOK)

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	var got pasteErrorResp
	status, body := srv.AuthGET("/api/v1/titles/"+trackID+"/enrichmentCandidates?q=anything", token, &got)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("candidates = %d, want 503 for a provider that declares no search; body: %s", status, body)
	}
	const want = "metadata provider search is unavailable for this item — the provider is unconfigured or disabled"
	if got.Error.Message != want {
		t.Errorf("message = %q,\n           want %q", got.Error.Message, want)
	}
}

// TestAGuestWithExternalRefProducesTheSameTwoBadRequests: the two distinct 400s an
// Admin can hit in the paste box come out of the GUEST — the service asks it
// through the contract's external-ref capability, and its refusal, with its
// got/want kinds, is what the handler renders. They are word for word the two the
// MusicBrainz Built-in produces, which is the measure of whether a Plugin can
// really own its source's id shapes.
//
// It makes no outbound call at all: reading a paste happens before any lookup,
// which is exactly why external-ref is a parse call and not a fetch.
func TestAGuestWithExternalRefProducesTheSameTwoBadRequests(t *testing.T) {
	requireMusicFixtures(t)
	const id = "b1392450-e666-3926-a536-22c65f834433"
	dataDir := t.TempDir()
	installGuestProvider(t, dataDir, "example-source", "", pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch, pluginapi.CapabilityExternalRef,
		},
	})

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	keyProvider(t, srv, token, "example-source")
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "example-source"}, http.StatusOK)

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{
			// The guest answered ref-kind-mismatch with BOTH kinds, which is the only
			// reason the host can name both in the sentence.
			name: "an artist url on a track",
			ref:  "https://source.example.test/artist/" + id,
			want: "that looks like a MusicBrainz artist link, but this item is a track — paste a recording (track) id or URL instead",
		},
		{
			name: "a work url",
			ref:  "https://source.example.test/work/" + id,
			want: "that MusicBrainz link is the wrong kind of record — paste a release-group (album), artist, or recording (track) id or URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got pasteErrorResp
			status, body := srv.AuthGET("/api/v1/titles/"+trackID+"/externalPreview?ref="+tc.ref, token, &got)
			if status != http.StatusBadRequest {
				t.Fatalf("preview = %d, want 400; body: %s", status, body)
			}
			if got.Error.Message != tc.want {
				t.Errorf("message = %q,\n           want %q", got.Error.Message, tc.want)
			}
		})
	}
}

// --- small helpers --------------------------------------------------------------

func (f *fakeFetcher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.urls))
	copy(out, f.urls)
	return out
}

func (f *fakeFetcher) fetched(url string) bool {
	for _, u := range f.seen() {
		if strings.EqualFold(u, url) {
			return true
		}
	}
	return false
}

func hasArtworkRole(d enrichedDetailResp, role string) bool {
	for _, a := range d.Artwork {
		if a.Role == role {
			return true
		}
	}
	return false
}
