package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box test for ADR-0057 decision 4: a Plugin this binary does not ship,
// registered with Role authoritative and Class full for the music kind, can be
// pointed at as a Library's Authoritative provider through its Enrichment policy
// (ADR-0027) — exactly as MusicBrainz can — and the next pass leads with it.
//
// It registers a fake through the SAME registry the Built-ins use and substitutes
// nothing else: the real Catalog, the real builder, the real settings API and the
// real policy resolver decide whether it is selectable, whether it is reachable,
// and whether it leads. Substituting the builder (as the other policy tests do)
// would have proved only that a fake can be handed to the Service, which was never
// in doubt; the question here is whether a REGISTRATION is enough.

const fakeMusicSlug = "fakemusic"

// fakeMusicPlugin is a contract-level Metadata provider Plugin: wire types in,
// wire types out, no knowledge of the enrichment domain at all. It is what an
// Installed plugin will be, written against internal/pluginapi/v1 alone.
type fakeMusicPlugin struct {
	mu      sync.Mutex
	lookups int
	kinds   map[string]int
}

func (p *fakeMusicPlugin) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	p.mu.Lock()
	p.lookups++
	if p.kinds == nil {
		p.kinds = map[string]int{}
	}
	p.kinds[req.Ref.Kind]++
	p.mu.Unlock()

	switch req.Ref.Kind {
	case "artist", "album", "track":
		return pluginapi.LookupResponse{
			Outcome: pluginapi.OutcomeMatched,
			Record: pluginapi.MetadataRecord{
				Matched:    true,
				Name:       req.Ref.Title,
				Overview:   fakeMusicOverview,
				Genres:     []string{"Test Genre"},
				ExternalID: "fake-" + req.Ref.Kind,
				Source:     fakeMusicSlug,
			},
		}, nil
	default:
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
}

func (p *fakeMusicPlugin) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (p *fakeMusicPlugin) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (p *fakeMusicPlugin) lookupCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lookups
}

// fakeMusicOverview is the tell: nothing else in the server would write it, so a
// Track carrying it was decorated by the Plugin and by nothing else.
const fakeMusicOverview = "Decorated by a Plugin this binary does not ship."

// fakeMusicRegistration is the whole of what the Plugin hands the host: what it
// is, and how to build it. Authoritative + Full is what makes it selectable as a
// lead; RequiresKey is what makes "keyed" a real gate, exactly as it is for AniDB.
func fakeMusicRegistration(p *fakeMusicPlugin) pluginapi.MetadataProviderRegistration {
	return pluginapi.MetadataProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:         fakeMusicSlug,
			Name:         "Fake Music Source",
			Kinds:        []string{pluginapi.KindMusic},
			Role:         pluginapi.RoleAuthoritative,
			Class:        pluginapi.ClassFull,
			RequiresKey:  true,
			Capabilities: []pluginapi.Capability{pluginapi.CapabilitySearch},
			DefaultURL:   "https://fake-music.test/v1",
			Description:  "A Full music provider registered only for this test.",
		},
		New: func(pluginapi.Settings) (pluginapi.MetadataProvider, error) { return p, nil },
	}
}

// TestAPluginCanLeadAMusicLibrary: register, key, point, pass.
func TestAPluginCanLeadAMusicLibrary(t *testing.T) {
	requireMusicFixtures(t)
	plugin := &fakeMusicPlugin{}
	srv := testharness.New(t,
		testharness.WithMetadataPlugins(fakeMusicRegistration(plugin)),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")

	// It shows up on the settings screen like any other source, with the facts it
	// registered — a registration is all it took to be configurable.
	if got := providerBySlug(getProviders(t, srv, token), fakeMusicSlug); got.Slug != fakeMusicSlug {
		t.Fatalf("the registered Plugin is missing from the provider list: %+v", got)
	}

	// Not yet selectable: it requires a key and has none, so it is not a USABLE Full
	// provider (ADR-0027 — enabled and keyed are separate gates).
	if hasAuthoritativeCandidate(getPolicy(t, srv, token, libID), fakeMusicSlug) {
		t.Error("an unkeyed key-requiring Plugin was offered as a lead")
	}

	// Key it. Now it is a candidate, beside MusicBrainz.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": fakeMusicSlug, "enabled": true, "apiKey": "fake-key"},
	}}, http.StatusOK)
	policy := getPolicy(t, srv, token, libID)
	if !hasAuthoritativeCandidate(policy, fakeMusicSlug) {
		t.Fatalf("a keyed Full music Plugin is not offered as a lead; candidates: %+v", policy.AuthoritativeCandidates)
	}
	if policy.EffectiveAuthoritative.Slug != "musicbrainz" {
		t.Errorf("before repointing, the lead = %q, want musicbrainz (the kind default)", policy.EffectiveAuthoritative.Slug)
	}

	// Point the Library at it. The policy change re-enriches the Library (ADR-0027);
	// run one more pass so the assertion below is about a settled pass rather than a
	// race with the background one.
	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": fakeMusicSlug}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != fakeMusicSlug {
		t.Fatalf("effective lead = %q, want the Plugin", policy.EffectiveAuthoritative.Slug)
	}
	if !policy.Effective.Music {
		t.Fatalf("music is off for a Library whose keyed Full lead is the Plugin: %+v", policy.Effective)
	}
	enrichLib(t, srv, token, libID, "full")

	// The pass led with it: a Track carries the Plugin's record.
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	d := getEnrichedDetail(t, srv, token, trackID)
	if d.EnrichmentStatus != "matched" {
		t.Errorf("track status = %q, want matched", d.EnrichmentStatus)
	}
	if d.Overview != fakeMusicOverview {
		t.Errorf("track overview = %q, want the Plugin's record", d.Overview)
	}
	if plugin.lookupCount() == 0 {
		t.Error("the Plugin was never asked; something else led the pass")
	}

	// Clearing the pointer hands the Library back to the kind default, live.
	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": nil}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != "musicbrainz" {
		t.Errorf("after clearing, the lead = %q, want musicbrainz", policy.EffectiveAuthoritative.Slug)
	}
}

// pasteErrorResp is the error envelope the paste box renders (api-contract.md).
type pasteErrorResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestAPastedRefIsRefusedByThePluginWithTheSameTwoMessages: the two distinct 400s
// an Admin can hit in the paste box now come out of the MusicBrainz PLUGIN — the
// service asks it through the contract's external-ref capability and its refusal,
// with its got/want kinds, is what the handler renders.
//
// It is driven with the real builder and the real Built-in (no injected provider),
// and it makes no outbound call at all: reading a paste happens before any lookup,
// which is exactly why external-ref is a parse call and not a fetch.
func TestAPastedRefIsRefusedByThePluginWithTheSameTwoMessages(t *testing.T) {
	requireMusicFixtures(t)
	const id = "b1392450-e666-3926-a536-22c65f834433"
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
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
			// An artist link on a Track: the message names what was pasted AND what the
			// item is, which is only possible because the mismatch's got/want kinds
			// crossed the contract.
			name: "an artist url on a track",
			ref:  "https://musicbrainz.org/artist/" + id,
			want: "that looks like a MusicBrainz artist link, but this item is a track — paste a recording (track) id or URL instead",
		},
		{
			name: "a work url",
			ref:  "https://musicbrainz.org/work/" + id,
			want: "that MusicBrainz link is the wrong kind of record — paste a release-group (album), artist, or recording (track) id or URL",
		},
		{
			name: "a label url",
			ref:  "https://musicbrainz.org/label/" + id,
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

// hasAuthoritativeCandidate reports whether a slug is offered as a Library's lead.
func hasAuthoritativeCandidate(v enrichmentPolicyView, slug string) bool {
	for _, c := range v.AuthoritativeCandidates {
		if c.Slug == slug {
			return true
		}
	}
	return false
}
