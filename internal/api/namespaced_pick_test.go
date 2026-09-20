package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for ADR-0060 decision 5 at the API: a pick carries the namespace
// it was found in. The candidate JSON says where its id means something (`source`),
// the apply sends it back, an apply without one means the Library's current lead,
// an unclaimed namespace is a 400, and the paste preview's namespace round-trips.
//
// Two contract-level music Plugins are registered beside the Built-ins and the
// Library is pointed at one of them, so the lead's namespace and a picked
// namespace can differ without either being MusicBrainz — a namespace the Built-in
// would answer over the network.

const (
	nsLeadSlug  = "nslead"
	nsOtherSlug = "nsother"
)

// namespacedMusicPlugin answers every music call from its own namespace: a lookup
// resolves the id the host holds in THIS plugin's namespace (else an automatic one),
// a search offers one hit, and a paste of "ref:<id>" reads as <id>.
type namespacedMusicPlugin struct{ slug string }

func (p *namespacedMusicPlugin) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	switch req.Ref.Kind {
	case "artist", "album", "track":
	default:
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
	id := req.Ref.ID(p.slug)
	if id == "" {
		id = "auto-" + req.Ref.Kind
	}
	return pluginapi.LookupResponse{
		Outcome: pluginapi.OutcomeMatched,
		Record: pluginapi.MetadataRecord{
			Matched:    true,
			Name:       req.Ref.Title,
			Overview:   "Decorated by " + p.slug,
			ExternalID: id,
			Source:     p.slug,
		},
	}, nil
}

func (p *namespacedMusicPlugin) Search(_ context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched, Candidates: []pluginapi.SearchCandidate{
		{ExternalID: p.slug + "-hit", Title: req.Query, Kind: req.Kind},
	}}, nil
}

func (p *namespacedMusicPlugin) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (p *namespacedMusicPlugin) ParseExternalRef(_ context.Context, req pluginapi.ExternalRefRequest) (pluginapi.ExternalRefResponse, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(req.Pasted), "ref:")
	if !ok || id == "" {
		return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeRefInvalid}, nil
	}
	return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeMatched, ExternalID: id}, nil
}

func namespacedMusicRegistration(slug string) pluginapi.MetadataProviderRegistration {
	p := &namespacedMusicPlugin{slug: slug}
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "Namespaced " + slug,
			Kinds:       []string{pluginapi.KindMusic},
			Role:        pluginapi.RoleAuthoritative,
			Class:       pluginapi.ClassFull,
			RequiresKey: true,
			Capabilities: []pluginapi.Capability{
				pluginapi.CapabilitySearch, pluginapi.CapabilityExternalRef,
			},
			DefaultURL:  "https://" + slug + ".test/v1",
			Description: "A Full music provider registered only for this test.",
		},
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) { return p, nil },
	}
}

// namespacedCandidate is the picker wire shape with the field under test.
type namespacedCandidate struct {
	ExternalID string `json:"externalId"`
	Source     string `json:"source"`
}

type namespacedDetail struct {
	ID           string `json:"id"`
	RecordSource string `json:"recordSource"`
	Overview     string `json:"overview"`
}

// namespacedMusicServer is a music Library led by nslead, with nsother keyed and
// registered as a second Authoritative music source.
func namespacedMusicServer(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	requireMusicFixtures(t)
	srv := testharness.New(t,
		testharness.WithMetadataPlugins(
			namespacedMusicRegistration(nsLeadSlug),
			namespacedMusicRegistration(nsOtherSlug),
		),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": nsLeadSlug, "enabled": true, "apiKey": "k"},
		{"slug": nsOtherSlug, "enabled": true, "apiKey": "k"},
	}}, http.StatusOK)
	policy := putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": nsLeadSlug}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != nsLeadSlug {
		t.Fatalf("effective lead = %q, want %q", policy.EffectiveAuthoritative.Slug, nsLeadSlug)
	}
	return srv, token, libID
}

func putTitleOverride(t *testing.T, srv *testharness.Server, token, titleID string, body map[string]any, want int) namespacedDetail {
	t.Helper()
	var d namespacedDetail
	status, raw := srv.JSON(http.MethodPut, "/api/v1/titles/"+titleID+"/enrichmentOverride", token, body, &d)
	if status != want {
		t.Fatalf("PUT enrichmentOverride %v = %d, want %d; body: %s", body, status, want, raw)
	}
	return d
}

func titleRecordSource(t *testing.T, srv *testharness.Server, token, titleID string) string {
	t.Helper()
	var d namespacedDetail
	if status, raw := srv.AuthGET("/api/v1/titles/"+titleID, token, &d); status != http.StatusOK {
		t.Fatalf("GET title = %d; body: %s", status, raw)
	}
	return d.RecordSource
}

// TestAPickIsPinnedInTheNamespaceItNames: the search stamps its candidates with the
// Library's lead, a pick naming another Authoritative namespace pins THAT one, a
// pick naming none pins the lead's, and a namespace nothing claims is refused.
func TestAPickIsPinnedInTheNamespaceItNames(t *testing.T) {
	srv, token, libID := namespacedMusicServer(t)
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}

	// The candidate says where its id means something: the Library's lead, because
	// the search asked the Library's lead (not the global one, MusicBrainz).
	var cands struct {
		Candidates []namespacedCandidate `json:"candidates"`
	}
	if status, raw := srv.AuthGET("/api/v1/titles/"+trackID+"/enrichmentCandidates?q=anything", token, &cands); status != http.StatusOK {
		t.Fatalf("candidates = %d; body: %s", status, raw)
	}
	if len(cands.Candidates) == 0 || cands.Candidates[0].Source != nsLeadSlug {
		t.Fatalf("candidates = %+v, want one stamped source %q", cands.Candidates, nsLeadSlug)
	}
	if cands.Candidates[0].ExternalID != nsLeadSlug+"-hit" {
		t.Errorf("candidate id = %q, want the lead's hit", cands.Candidates[0].ExternalID)
	}

	// A pick with a source pins that namespace — not the lead's.
	d := putTitleOverride(t, srv, token, trackID,
		map[string]any{"externalId": "other-1", "source": nsOtherSlug}, http.StatusOK)
	if d.RecordSource != nsOtherSlug {
		t.Errorf("recordSource after a %s pick = %q, want %q", nsOtherSlug, d.RecordSource, nsOtherSlug)
	}
	if d.Overview != "Decorated by "+nsOtherSlug {
		t.Errorf("overview = %q: the pick was not resolved by the namespace it named", d.Overview)
	}
	if got := titleRecordSource(t, srv, token, trackID); got != nsOtherSlug {
		t.Errorf("GET recordSource = %q, want %q", got, nsOtherSlug)
	}

	// A pick without one means the Library's current lead, which is what an older
	// client has always meant.
	d = putTitleOverride(t, srv, token, trackID, map[string]any{"externalId": "lead-1"}, http.StatusOK)
	if d.RecordSource != nsLeadSlug {
		t.Errorf("recordSource after a pick with no source = %q, want the lead %q", d.RecordSource, nsLeadSlug)
	}

	// A namespace no registered Authoritative music source claims is a 400 with a
	// sentence, and pins nothing.
	for _, ns := range []string{"nope", "imdb"} {
		var e pasteErrorResp
		status, raw := srv.JSON(http.MethodPut, "/api/v1/titles/"+trackID+"/enrichmentOverride", token,
			map[string]any{"externalId": "x-1", "source": ns}, &e)
		if status != http.StatusBadRequest {
			t.Fatalf("source %q = %d, want 400; body: %s", ns, status, raw)
		}
		if !strings.Contains(e.Error.Message, ns) {
			t.Errorf("400 message %q does not name the source %q", e.Error.Message, ns)
		}
	}
	if got := titleRecordSource(t, srv, token, trackID); got != nsLeadSlug {
		t.Errorf("a refused pick changed the record: recordSource = %q, want %q", got, nsLeadSlug)
	}
}

// TestAPastedRefRoundTripsItsNamespace: the paste preview returns the namespace the
// paste was read in, and applying it with that source pins it there.
func TestAPastedRefRoundTripsItsNamespace(t *testing.T) {
	srv, token, libID := namespacedMusicServer(t)
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}

	var c namespacedCandidate
	status, raw := srv.AuthGET("/api/v1/titles/"+trackID+"/externalPreview?ref="+url.QueryEscape("ref:pasted-9"), token, &c)
	if status != http.StatusOK {
		t.Fatalf("preview = %d; body: %s", status, raw)
	}
	if c.ExternalID != "pasted-9" || c.Source != nsLeadSlug {
		t.Fatalf("preview = %+v, want pasted-9 in %q", c, nsLeadSlug)
	}

	d := putTitleOverride(t, srv, token, trackID,
		map[string]any{"externalId": c.ExternalID, "source": c.Source}, http.StatusOK)
	if d.RecordSource != c.Source {
		t.Errorf("recordSource = %q, want the preview's %q", d.RecordSource, c.Source)
	}
}

// TestAParentPickIsPinnedInTheNamespaceItNames: the same rule on a browse parent.
func TestAParentPickIsPinnedInTheNamespaceItNames(t *testing.T) {
	srv, token, libID := namespacedMusicServer(t)
	var albumID string
	for _, a := range listArtists(t, srv, token, libID).Artists {
		if als := artistAlbums(t, srv, token, a.ID).Albums; len(als) > 0 {
			albumID = als[0].ID
			break
		}
	}
	if albumID == "" {
		t.Skip("no albums in music fixture")
	}
	path := "/api/v1/albums/" + albumID + "/enrichmentOverride"

	var d namespacedDetail
	status, raw := srv.JSON(http.MethodPut, path, token, map[string]any{"externalId": "alb-1", "source": nsOtherSlug}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT album override = %d; body: %s", status, raw)
	}
	if d.RecordSource != nsOtherSlug {
		t.Errorf("album recordSource = %q, want %q", d.RecordSource, nsOtherSlug)
	}

	d = namespacedDetail{}
	status, raw = srv.JSON(http.MethodPut, path, token, map[string]any{"externalId": "alb-2"}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT album override = %d; body: %s", status, raw)
	}
	if d.RecordSource != nsLeadSlug {
		t.Errorf("album recordSource with no source = %q, want the lead %q", d.RecordSource, nsLeadSlug)
	}

	if status, raw := srv.JSON(http.MethodPut, path, token, map[string]any{"externalId": "alb-3", "source": "nope"}, nil); status != http.StatusBadRequest {
		t.Fatalf("album source nope = %d, want 400; body: %s", status, raw)
	}
}
