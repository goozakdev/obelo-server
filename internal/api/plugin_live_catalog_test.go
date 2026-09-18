package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for the enrichment catalog following the LIVE Plugin registry
// (.scratch/plugin-system issue 19, ADR-0027, ADR-0057 decision 5).
//
// Issue 10 made the registry a copy-on-write value with an atomic Swap and
// promised that every reader — catalog, builder, settings API, policy resolver,
// sink manager — picks up the new value on its next read. The enrichment catalog
// did not: enrich.NewCatalog copied the Metadata provider Descriptors into a slice
// at construction and the enrichment Manager held that value for the life of the
// process. What an operator saw was a Plugin in two halves: a Metadata provider
// installed from the Plugins screen appeared on the metadata-provider settings
// screen and could be keyed and switched on at once (those handlers derive a fresh
// Catalog per request), while the Enrichment policy refused to make it a Library's
// lead with 422 PROVIDER_NOT_AUTHORITATIVE, would not let it be forced on or off
// per Library, and never composed it into a chain — until a restart.
//
// So every Plugin here is UPLOADED AFTER BOOT, through the Plugins screen's own
// endpoint, and every assertion is one an Admin can make from a screen: the policy
// response, an item's overview after a pass, the Plugins list. Nothing is placed
// on disk before the server starts, which is exactly the workaround these suites
// used to need.

const (
	// The Full video Authoritative an operator installs and then leads with.
	liveLeadSource = "installed-today-lead"
	// The fill-only video Supplement installed beside it.
	liveSuppSource = "installed-today-supplement"
	// What the operator types into the key box.
	liveOperatorKey = "an-operator-key"
)

// uploadGuestProvider installs a Metadata provider through POST /settings/plugins
// — the multipart upload the Plugins screen posts — and points its default URL at
// the guest mode this test wants it to play.
func uploadGuestProvider(t *testing.T, srv *testharness.Server, token, id, mode string, p pluginapi.ManifestProvides) {
	t.Helper()
	m := plugintest.MetadataProviderManifest(id, p)
	m.Settings.DefaultURL = guestBaseURL(mode)
	m.Settings.DefaultURL2 = guestImageHost
	status, body := uploadPlugin(t, srv, token, plugintest.ManifestJSON(t, m), plugintest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("installing %s: status = %d, want 201; body: %s", id, status, body)
	}
}

// firstMovieTitleID is the item every pass in this file is read back from.
func firstMovieTitleID(t *testing.T, srv *testharness.Server, token, libID string) string {
	t.Helper()
	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	return titles[0].ID
}

// --- the lead ------------------------------------------------------------------

// TestAProviderInstalledAfterBootLeadsALibraryWithNoRestart is the first
// acceptance criterion whole: upload, key, and the Enrichment policy offers the
// Plugin as an Authoritative candidate, takes it as the Library's lead, and the
// next pass leads with it.
//
// Before this issue the PUT answered 422 PROVIDER_NOT_AUTHORITATIVE at the
// repointing step, because the write-side guard asks the enrichment Manager for
// its usable Full providers and the Manager's catalog was taken at boot.
func TestAProviderInstalledAfterBootLeadsALibraryWithNoRestart(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Nothing claims the slug yet, which is what makes the assertion after the
	// upload mean something.
	if hasAuthoritativeCandidate(getPolicy(t, srv, token, libID), liveLeadSource) {
		t.Fatalf("a Plugin that was never installed is offered as a lead")
	}

	uploadGuestProvider(t, srv, token, liveLeadSource, "", leadVideoProvides())

	// It is on the metadata-provider settings screen — that half always worked.
	if got := providerBySlug(getProviders(t, srv, token), liveLeadSource); got.Slug != liveLeadSource {
		t.Fatalf("the Plugin installed after boot is missing from the settings screen: %+v", got)
	}
	// And it is still not a lead, for the RIGHT reason: it requires a key and has
	// none (ADR-0027 — enabled and keyed are separate gates).
	if hasAuthoritativeCandidate(getPolicy(t, srv, token, libID), liveLeadSource) {
		t.Error("an unkeyed key-requiring Plugin was offered as a lead")
	}

	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": liveLeadSource, "enabled": true, "apiKey": liveOperatorKey},
	}}, http.StatusOK)

	policy := getPolicy(t, srv, token, libID)
	if !hasAuthoritativeCandidate(policy, liveLeadSource) {
		t.Fatalf("a keyed Full video Plugin installed after boot is not offered as a lead; candidates: %+v",
			policy.AuthoritativeCandidates)
	}

	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": liveLeadSource}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != liveLeadSource {
		t.Fatalf("effective lead = %q, want the Plugin installed today", policy.EffectiveAuthoritative.Slug)
	}
	if !policy.Effective.Video {
		t.Fatalf("video is off for a Library whose keyed Full lead is the Plugin: %+v", policy.Effective)
	}

	enrichLib(t, srv, token, libID, "full")
	d := getEnrichedDetail(t, srv, token, firstMovieTitleID(t, srv, token, libID))
	if d.Overview != guestOverview {
		t.Fatalf("overview = %q, want the guest's — the Plugin installed today did not lead the pass", d.Overview)
	}
}

// --- the Supplement, and its per-Library tri-state ------------------------------

// ONE PASS PER LIBRARY, and every case below is shaped by it. Enrichment is
// fill-only, and a leaf that already carries a resolved external id short-circuits
// on the next pass without asking the provider again — so "this Supplement is in
// the chain" and "this Supplement is not" are both only observable on a Library
// that has not enriched yet. Each case therefore arranges its world completely —
// installs, keys, and switches off whatever it means to be off — and only then
// saves the Enrichment policy, which is the act that starts the Library's first
// real pass (ADR-0027 re-enriches on a policy change). That is also why each case
// gets its own server rather than driving one Library through several states.

// newSupplementChain boots a server whose lead is the suite's in-process blank
// Full video provider — it matches everything and fills nothing but the name, so
// whatever appears on the item came from the Supplement — scans a movie Library,
// UPLOADS the Supplement after boot, and keys both. It stops short of the policy
// on purpose: see above.
func newSupplementChain(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	requireFixtures(t)
	srv := testharness.New(t,
		testharness.WithMetadataPlugins(blankVideoRegistration(&blankVideoPlugin{})),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("image-bytes")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	uploadGuestProvider(t, srv, token, liveSuppSource, "video-supplement", supplementVideoProvides())
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": leadVideoSlug, "enabled": true, "apiKey": "lead-key"},
		{"slug": liveSuppSource, "enabled": true, "apiKey": liveOperatorKey},
	}}, http.StatusOK)
	return srv, token, libID
}

// leadAndEnrich points the Library at the blank lead (with any per-provider
// overrides the case wants), runs the pass, and returns the first item's overview
// — the whole observable these cases are written against.
func leadAndEnrich(t *testing.T, srv *testharness.Server, token, libID string, overrides map[string]any) string {
	t.Helper()
	body := map[string]any{"authoritativeProvider": leadVideoSlug}
	if overrides != nil {
		body["providerOverrides"] = overrides
	}
	policy := putPolicy(t, srv, token, libID, body, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != leadVideoSlug {
		t.Fatalf("effective lead = %q, want the blank in-process lead", policy.EffectiveAuthoritative.Slug)
	}
	titleID := firstMovieTitleID(t, srv, token, libID)
	enrichLib(t, srv, token, libID, "full")
	return getEnrichedDetail(t, srv, token, titleID).Overview
}

// TestASupplementInstalledAfterBootFillsWhatTheLeadLeftBlank is the second
// acceptance criterion's first half: a video Supplement uploaded after boot is
// composed into the chain on the next pass, with no restart.
func TestASupplementInstalledAfterBootFillsWhatTheLeadLeftBlank(t *testing.T) {
	srv, token, libID := newSupplementChain(t)
	if got := leadAndEnrich(t, srv, token, libID, nil); got != guestOverview {
		t.Fatalf("overview = %q, want the Supplement's — it was installed today and the lead left it blank", got)
	}
}

// TestTheTriStateReachesASupplementInstalledAfterBoot is the second half: the
// per-Library force-on/force-off control reaches a Plugin the server did not have
// at boot, in both directions.
func TestTheTriStateReachesASupplementInstalledAfterBoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
		want  string
	}{
		{name: "forced on", force: true, want: guestOverview},
		{name: "forced off", force: false, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, token, libID := newSupplementChain(t)
			// It is offered per-Library at all, which is the read side of the tri-state
			// and is answered from the enrichment Manager's own catalog.
			if _, present := getPolicy(t, srv, token, libID).supplementOverride(liveSuppSource); !present {
				t.Fatalf("a Supplement installed after boot is not offered per-Library")
			}
			got := leadAndEnrich(t, srv, token, libID, map[string]any{liveSuppSource: tc.force})
			override, present := getPolicy(t, srv, token, libID).supplementOverride(liveSuppSource)
			if !present || override == nil || *override != tc.force {
				t.Fatalf("the override did not stick: %v/%v", override, present)
			}
			if got != tc.want {
				t.Fatalf("overview = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- uninstall, through a Library that never pointed at the Plugin --------------

// TestUninstallingASupplementTakesItOutOfTheChain is the third acceptance
// criterion, and it is deliberately asserted through a Library whose lead is
// somebody else and whose policy never named the uninstalled Plugin. Issue 17
// clears a Library's Authoritative pointer on uninstall and that clearing is still
// in place here — it simply has nothing to do with this Library. What is under
// test is the catalog: an uninstalled Plugin stops being offered as a Supplement
// and stops being composed, on the next Reload.
//
// The "kept" case beside it is the control. Without it a blank overview would
// prove only that some Plugin somewhere did not run.
func TestUninstallingASupplementTakesItOutOfTheChain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uninstall bool
		want      string
	}{
		{name: "uninstalled", uninstall: true, want: ""},
		{name: "kept", uninstall: false, want: guestOverview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, token, libID := newSupplementChain(t)
			if _, present := getPolicy(t, srv, token, libID).supplementOverride(liveSuppSource); !present {
				t.Fatalf("the Supplement was never offered, so there is nothing to remove")
			}

			if tc.uninstall {
				status, body := srv.JSON(http.MethodDelete, pluginsPath+"/"+liveSuppSource, token, nil, nil)
				if status != http.StatusOK {
					t.Fatalf("DELETE = %d, want 200; body: %s", status, body)
				}
				policy := getPolicy(t, srv, token, libID)
				if _, present := policy.supplementOverride(liveSuppSource); present {
					t.Errorf("an uninstalled Plugin is still offered as a Supplement: %+v", policy.Supplements)
				}
				if hasAuthoritativeCandidate(policy, liveSuppSource) {
					t.Errorf("an uninstalled Plugin is still offered as a lead: %+v", policy.AuthoritativeCandidates)
				}
			}

			if got := leadAndEnrich(t, srv, token, libID, nil); got != tc.want {
				t.Fatalf("overview = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- disable and re-enable ------------------------------------------------------

// TestDisablingAndEnablingAProviderReachesTheChain is the fourth acceptance
// criterion. Disable UN-REGISTERS a Plugin (issue 10 deviation 5), so the switch on
// the Plugins screen and the catalog are the same fact. The second case also pins
// the other half of issue 17's policy — disable REMEMBERS, so the operator's key
// and enable switch are still there afterwards and nothing has to be retyped.
func TestDisablingAndEnablingAProviderReachesTheChain(t *testing.T) {
	switchPlugin := func(t *testing.T, srv *testharness.Server, token, verb string) {
		t.Helper()
		status, body := srv.JSON(http.MethodPost, pluginsPath+"/"+liveSuppSource+"/"+verb, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s = %d, want 200; body: %s", verb, status, body)
		}
	}

	t.Run("disabled", func(t *testing.T) {
		srv, token, libID := newSupplementChain(t)
		switchPlugin(t, srv, token, "disable")
		if p := pluginNamed(t, readPlugins(t, srv, token), liveSuppSource); p.Enabled {
			t.Fatalf("the Plugins screen still reports the Plugin as on after Disable: %+v", p)
		}
		// Un-registered, so the policy cannot offer it per-Library either.
		if _, present := getPolicy(t, srv, token, libID).supplementOverride(liveSuppSource); present {
			t.Errorf("a switched-off Plugin is still offered as a per-Library Supplement")
		}
		if got := leadAndEnrich(t, srv, token, libID, nil); got != "" {
			t.Fatalf("overview = %q after Disable; a switched-off Plugin is still in the chain", got)
		}
	})

	t.Run("disabled then enabled", func(t *testing.T) {
		srv, token, libID := newSupplementChain(t)
		switchPlugin(t, srv, token, "disable")
		switchPlugin(t, srv, token, "enable")
		// Disable remembers: the operator's row is still enabled and still keyed.
		if got := providerBySlug(getProviders(t, srv, token), liveSuppSource); !got.Enabled || !got.HasKey {
			t.Errorf("the operator's provider settings did not survive a Disable/Enable: %+v", got)
		}
		if _, present := getPolicy(t, srv, token, libID).supplementOverride(liveSuppSource); !present {
			t.Errorf("a re-enabled Plugin is not offered as a per-Library Supplement again")
		}
		if got := leadAndEnrich(t, srv, token, libID, nil); got != guestOverview {
			t.Fatalf("overview = %q after Enable, want the Supplement's — the switch never reached the chain", got)
		}
	})
}
