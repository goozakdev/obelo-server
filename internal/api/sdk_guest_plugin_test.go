package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/sdkguesttest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for a plugin built with the Go SDK (.scratch/bundled-plugins
// issue 03).
//
// The SDK-free guest already proves the host: installed_metadata_provider_test.go
// drives a module written against the raw ABI through the same endpoints. What is
// proved HERE is the other module — that a provider written as three Go methods
// against pluginsdk.Host, handed to metadata.Serve from init() and compiled by the
// ordinary `GOOS=wasip1 GOARCH=wasm -buildmode=c-shared` command, is
// indistinguishable from it once it is on a server.
//
// It matters because after issue 04 this is how the SEVEN PROVIDERS THE
// MAINTAINER RUNS are built. A bug in the SDK's dispatcher or its host-function
// wrappers is a bug in TMDB, and the first person to find it would be the operator
// whose library stopped enriching.
//
// Everything asserted is what an Admin can observe: an upload's 201, the Plugins
// screen, the providers screen, and the overview and artwork on an item after a
// pass.
//
// The picker endpoints — the Edit-item box and the artwork grid — are NOT driven
// here, and that is a fact about the host rather than about the SDK: those two ask
// the SERVER-WIDE chain for the item's kind, whose lead is the kind default (TMDB
// / MusicBrainz) and cannot be repointed at an Installed provider by any endpoint
// this build has. A Library's policy repoints the PASS, which is what this file
// drives. The SDK's metadata_search and metadata_artwork_candidates exports are
// driven across the real sandbox in internal/plugins/sdk_guest_test.go instead,
// which is the seam that belongs to this issue.

// The strings the SDK guest writes, restated here for the reason
// installed_metadata_provider_test.go restates the other guest's: they live in
// pluginsdk/internal/testprovider, which is internal to that module. Nothing else
// in this server writes them.
const (
	sdkGuestOverview  = "Filled from inside the sandbox by a plugin built with the Obelo Go SDK."
	sdkGuestPosterURL = sdkguesttest.ImageHost + "/art/poster.jpg"
)

// sdkGuestProvides is the manifest entry the SDK guest registers with: a Full
// source for both kinds that can lead, declaring the two capabilities its provider
// implements.
func sdkGuestProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindVideo, pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			pluginapi.CapabilityArtworkCandidates,
		},
	}
}

// uploadSDKGuest installs the SDK-built module through the real endpoint an Admin
// uses — a multipart POST from the browser — and fails unless the server took it.
func uploadSDKGuest(t *testing.T, srv *testharness.Server, token, id, mode string) {
	t.Helper()
	m := sdkguesttest.Manifest(id, mode, sdkGuestProvides())
	status, body := uploadPlugin(t, srv, token, plugintest.ManifestJSON(t, m), sdkguesttest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("installing the SDK guest: status = %d, want 201; body: %s", status, body)
	}
}

// TestASDKBuiltPluginUploadedThroughTheAPILeadsALibrary is the whole of this
// issue's end-to-end claim, in one pass: an Admin uploads the module on a running
// server, it appears on both screens, they key it and point a Library at it, and
// the next enrichment decorates the Library's titles from inside the sandbox.
//
// No restart, no hand-placed directory, no shell on the box.
func TestASDKBuiltPluginUploadedThroughTheAPILeadsALibrary(t *testing.T) {
	requireFixtures(t)
	fetcher := &fakeFetcher{data: []byte("image-bytes")}
	srv := testharness.New(t, testharness.WithArtworkFetcher(fetcher))
	token := adminToken(t, srv)

	uploadSDKGuest(t, srv, token, "sdk-source", "")

	// The Plugins screen: it is installed, enabled and carries no failure.
	row := pluginNamed(t, readPlugins(t, srv, token), "sdk-source")
	if !row.Enabled || row.DisabledByFailure || row.LastError != "" {
		t.Fatalf("the uploaded SDK plugin is not healthy: %+v", row)
	}
	if len(row.Provides) != 1 || row.Provides[0] != string(pluginapi.ExtensionMetadataProvider) {
		t.Errorf("provides = %v, want one metadata-provider entry", row.Provides)
	}

	// The metadata-provider screen, live: a registration is all it took to be
	// configurable, and the catalog reads the registry rather than a fixed list.
	got := providerBySlug(getProviders(t, srv, token), "sdk-source")
	if got.Slug != "sdk-source" {
		t.Fatalf("the SDK plugin is missing from the metadata-provider screen: %+v", got)
	}

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	keyProvider(t, srv, token, "sdk-source")

	policy := putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "sdk-source"}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != "sdk-source" {
		t.Fatalf("effective lead = %q, want the SDK plugin", policy.EffectiveAuthoritative.Slug)
	}

	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "full")

	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	d := getEnrichedDetail(t, srv, token, titles[0].ID)
	if d.Overview != sdkGuestOverview {
		t.Fatalf("overview = %q, want the SDK guest's record", d.Overview)
	}

	// The artwork the guest POINTED at was downloaded by the host's own fetcher. A
	// plugin returns URLs and never bytes, whichever toolchain built it.
	if !fetcher.fetched(sdkGuestPosterURL) {
		t.Fatalf("the host never fetched the SDK guest's artwork URL; it fetched %v", fetcher.seen())
	}
	if !hasArtworkRole(d, "poster") {
		t.Fatalf("no poster on the enriched title: %+v", d.Artwork)
	}
}

// TestASDKBuiltPluginKeepsItsOwnKeyValueNamespace: the guest writes its own secret
// under a key through the SDK's kv wrappers and reads it back, and what comes out
// reaches the Admin as the item's overview.
//
// The namespace is the HOST's doing, from the manifest on disk. A guest never
// spells its own id and has no request field to put one in, so this also says what
// two plugins sharing a key would see: their own values.
func TestASDKBuiltPluginKeepsItsOwnKeyValueNamespace(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}))
	token := adminToken(t, srv)

	uploadSDKGuest(t, srv, token, "sdk-source", "kv")

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	keyProvider(t, srv, token, "sdk-source")
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "sdk-source"}, http.StatusOK)

	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "full")

	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	d := getEnrichedDetail(t, srv, token, titles[0].ID)
	// keyProvider saves this key; the guest wrote it, read it back and reported it.
	if d.Overview != "an-operator-key" {
		t.Fatalf("overview = %q, want the value the guest round-tripped through its own namespace", d.Overview)
	}
}
