package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for what an UNINSTALL takes from the OPERATOR's settings
// (.scratch/plugin-system issue 17, ADR-0007, ADR-0027).
//
// Issue 16 closed the guest's side — the key-value namespace goes with the
// plugins, plugin_settings and event_sinks rows. The operator's side was still
// open one table over: an Installed Metadata provider's enable switch, key and
// base URL live in metadata_providers keyed by slug and a Subtitle provider's in
// subtitle_providers, exactly as the Built-ins' do, and neither was touched. The
// row then sat under a slug nothing registers — invisible to every settings
// screen, and picked straight back up by a reinstall under the same id, by the
// same author or by anyone who chose the same slug. The per-Library Enrichment
// policy had the same problem in two shapes: a Supplement forced on or off, and a
// Library repointed at the Plugin as its lead.
//
// THE DECISION IS THAT UNINSTALL FORGETS AND DISABLE REMEMBERS. "Uninstall deletes
// files and rows" is what the PRD promised; a key left behind is a credential the
// operator believes is gone; and an operator who wants the settings kept has a
// verb for it. So every assertion below is made the way an Admin would make it —
// install, key, uninstall, install the same id again, and look at the screen.

const (
	// The Plugin whose settings must be forgotten, and the neighbour that proves
	// only its own rows went.
	forgetfulSource = "forgetful-source"
	neighbourSource = "neighbour-source"
	// The Built-in in each table whose row an uninstall must never reach.
	builtinMetadataProvider = "tmdb"
	builtinSubtitleProvider = "opensubtitles"
	// What the operator typed, and the manifest default a fresh install must fall
	// back to when it is forgotten.
	operatorKey         = "the-operators-own-key"
	operatorMirrorURL   = "https://mirror.example.test/v1"
	uninstallSourceURL  = "https://uninstall.example.test/v1"
	uninstallSubtitles  = "https://subs.example.test/v1" // plugintest.SubtitleManifest's default
	uninstallLeadSource = "lead-source"
	uninstallSuppSource = "supp-source"
)

// uploadMetadataProviderPlugin installs a Metadata provider through the Plugins
// screen's own endpoint (rather than by placing files before boot), because an
// uninstall and a REINSTALL are what this file is about and both are API verbs.
func uploadMetadataProviderPlugin(t *testing.T, srv *testharness.Server, token, id string, p pluginapi.ManifestProvides) {
	t.Helper()
	m := plugintest.MetadataProviderManifest(id, p)
	m.Settings.DefaultURL = uninstallSourceURL
	status, body := uploadPlugin(t, srv, token, plugintest.ManifestJSON(t, m), plugintest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("installing %s: status = %d, want 201; body: %s", id, status, body)
	}
}

// uploadSubtitleProviderPlugin is the same for the Subtitle seam.
func uploadSubtitleProviderPlugin(t *testing.T, srv *testharness.Server, token, id string) {
	t.Helper()
	status, body := uploadPlugin(t, srv, token, plugintest.ManifestJSON(t, plugintest.SubtitleManifest(id)), plugintest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("installing %s: status = %d, want 201; body: %s", id, status, body)
	}
}

// uninstallPluginVerb presses Uninstall and fails unless the server took it.
func uninstallPluginVerb(t *testing.T, srv *testharness.Server, token, id string) {
	t.Helper()
	status, body := srv.JSON(http.MethodDelete, pluginsPath+"/"+id, token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE %s = %d, want 200; body: %s", id, status, body)
	}
	if hasPlugin(readPlugins(t, srv, token), id) {
		t.Fatalf("an uninstalled Plugin is still on the Plugins screen: %s", id)
	}
}

// supplementVideoProvides is a Full video Supplement — the Installed provider the
// per-Library Supplement tri-state can force on or off.
func supplementVideoProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindVideo},
		Role:  pluginapi.RoleSupplement,
		Class: pluginapi.ClassFull,
	}
}

// leadVideoProvides is a Full video Authoritative — the Installed provider a
// Library can be repointed at as its lead (ADR-0027).
func leadVideoProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds:        []string{pluginapi.KindVideo},
		Role:         pluginapi.RoleAuthoritative,
		Class:        pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{pluginapi.CapabilitySearch},
	}
}

// --- the Metadata provider -----------------------------------------------------

// TestUninstallingAnInstalledMetadataProviderForgetsItsKeyAndBaseURL is the
// acceptance criterion whole, at the only layer that can observe the row: install,
// enable and key through the settings API, uninstall, reinstall the same id, and
// the screen shows a provider that has never been configured.
//
// The neighbour and the Built-in are keyed FIRST and asserted slug by slug after,
// because the way to break this is to widen a WHERE clause, and the Built-ins'
// rows live in the same table under the same column.
func TestUninstallingAnInstalledMetadataProviderForgetsItsKeyAndBaseURL(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	uploadMetadataProviderPlugin(t, srv, token, forgetfulSource, supplementVideoProvides())
	uploadMetadataProviderPlugin(t, srv, token, neighbourSource, supplementVideoProvides())

	// The operator turns three sources on and keys all three — a Built-in, the
	// Plugin about to go, and the Plugin that stays.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": builtinMetadataProvider, "enabled": true, "apiKey": operatorKey, "baseURL": operatorMirrorURL},
		{"slug": forgetfulSource, "enabled": true, "apiKey": operatorKey, "baseURL": operatorMirrorURL},
		{"slug": neighbourSource, "enabled": true, "apiKey": operatorKey, "baseURL": operatorMirrorURL},
	}}, http.StatusOK)

	before := providerBySlug(getProviders(t, srv, token), forgetfulSource)
	if !before.Enabled || !before.HasKey || before.BaseURL != operatorMirrorURL {
		t.Fatalf("the Plugin was never configured, so there is nothing to forget: %+v", before)
	}

	uninstallPluginVerb(t, srv, token, forgetfulSource)

	// The same module, under the same id, installed again.
	uploadMetadataProviderPlugin(t, srv, token, forgetfulSource, supplementVideoProvides())

	view := getProviders(t, srv, token)
	after := providerBySlug(view, forgetfulSource)
	if after.Enabled {
		t.Errorf("a reinstalled Plugin arrived ENABLED; the previous operator's switch outlived the uninstall")
	}
	if after.HasKey {
		t.Errorf("a reinstalled Plugin arrived KEYED; the previous operator's credential outlived the uninstall")
	}
	if after.BaseURL != uninstallSourceURL {
		t.Errorf("baseURL = %q after a reinstall, want the manifest default %q — the operator's override outlived the uninstall",
			after.BaseURL, uninstallSourceURL)
	}

	// Slug by slug: nothing else in the table moved.
	for _, slug := range []string{builtinMetadataProvider, neighbourSource} {
		got := providerBySlug(view, slug)
		if !got.Enabled || !got.HasKey || got.BaseURL != operatorMirrorURL {
			t.Errorf("%s's settings changed when a different Plugin was uninstalled: %+v", slug, got)
		}
	}
}

// --- the Subtitle provider -----------------------------------------------------

// TestUninstallingAnInstalledSubtitleProviderForgetsItsKeyAndBaseURL is the same
// criterion one table over. It matters on its own because the asymmetry issue 10
// left ran the other way here: an Installed Event sink already forgot its secret
// and target on uninstall while an Installed Subtitle provider remembered its key.
func TestUninstallingAnInstalledSubtitleProviderForgetsItsKeyAndBaseURL(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	uploadSubtitleProviderPlugin(t, srv, token, forgetfulSource)
	uploadSubtitleProviderPlugin(t, srv, token, neighbourSource)

	for _, slug := range []string{builtinSubtitleProvider, forgetfulSource, neighbourSource} {
		configureProvider(t, srv, token, map[string]any{
			"slug": slug, "enabled": true, "apiKey": operatorKey, "baseURL": operatorMirrorURL,
		})
	}

	before := providerNamed(t, readSubtitleProviders(t, srv, token), forgetfulSource)
	if !before.Enabled || !before.HasKey || before.BaseURL != operatorMirrorURL {
		t.Fatalf("the Plugin was never configured, so there is nothing to forget: %+v", before)
	}

	uninstallPluginVerb(t, srv, token, forgetfulSource)
	uploadSubtitleProviderPlugin(t, srv, token, forgetfulSource)

	view := readSubtitleProviders(t, srv, token)
	after := providerNamed(t, view, forgetfulSource)
	if after.Enabled || after.HasKey {
		t.Errorf("a reinstalled Subtitle provider arrived enabled/keyed: %+v", after)
	}
	if after.BaseURL != uninstallSubtitles {
		t.Errorf("baseURL = %q after a reinstall, want the manifest default %q", after.BaseURL, uninstallSubtitles)
	}
	for _, slug := range []string{builtinSubtitleProvider, neighbourSource} {
		got := providerNamed(t, view, slug)
		if !got.Enabled || !got.HasKey || got.BaseURL != operatorMirrorURL {
			t.Errorf("%s's settings changed when a different Plugin was uninstalled: %+v", slug, got)
		}
	}
}

// --- the per-Library Enrichment policy -----------------------------------------

// TestUninstallingALibrarysLeadHandsItBackToTheKindDefault is the ADR-0027 half:
// a Library that was repointed at an Installed Full provider, and one that forced
// an Installed Supplement off, must not keep a policy naming a slug nothing
// registers.
//
// Both halves are asserted from the POLICY RESPONSE rather than from the store,
// because "no stale slug left in the policy response" is the criterion and the
// response is what an Admin reads. The pass afterwards is the other half: the
// Library falls back to the kind's default lead with no error surfaced.
func TestUninstallingALibrarysLeadHandsItBackToTheKindDefault(t *testing.T) {
	requireFixtures(t)
	// BOTH Plugins are UPLOADED AFTER BOOT, through the Plugins screen's own
	// endpoint, so every verb in this test is one an Admin has (.scratch/plugin-system
	// issue 19). It used to place them on disk before the server started, because
	// the enrichment Manager took its catalog when it was composed: a provider
	// uploaded after boot reached the metadata-provider settings screen (that
	// catalog is derived per request) and yet was refused as a Library's lead with
	// 422 PROVIDER_NOT_AUTHORITATIVE until a restart. The Catalog now reads the live
	// registry, so an install and an uninstall are symmetrical here and the
	// workaround is gone.
	srv := testharness.New(t,
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	uploadMetadataProviderPlugin(t, srv, token, uninstallLeadSource, leadVideoProvides())
	uploadMetadataProviderPlugin(t, srv, token, uninstallSuppSource, supplementVideoProvides())
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Both keyed, which is what makes one selectable as a lead and the other
	// togglable as a Supplement.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": uninstallLeadSource, "enabled": true, "apiKey": operatorKey},
		{"slug": uninstallSuppSource, "enabled": true, "apiKey": operatorKey},
	}}, http.StatusOK)

	policy := putPolicy(t, srv, token, libID, map[string]any{
		"authoritativeProvider": uninstallLeadSource,
		"providerOverrides":     map[string]any{uninstallSuppSource: false},
	}, http.StatusOK)
	if policy.AuthoritativeProvider == nil || *policy.AuthoritativeProvider != uninstallLeadSource {
		t.Fatalf("stored lead = %v, want the Installed plugin", policy.AuthoritativeProvider)
	}
	if policy.EffectiveAuthoritative.Slug != uninstallLeadSource {
		t.Fatalf("effective lead = %q, want the Installed plugin", policy.EffectiveAuthoritative.Slug)
	}
	if override, present := policy.supplementOverride(uninstallSuppSource); !present || override == nil || *override {
		t.Fatalf("the Supplement was never forced off, so there is nothing to forget: %v/%v", override, present)
	}

	uninstallPluginVerb(t, srv, token, uninstallLeadSource)
	uninstallPluginVerb(t, srv, token, uninstallSuppSource)

	// The policy no longer names either slug. The lead reads as INHERIT — not as a
	// deliberate choice of a source that no longer exists — and resolves to the
	// kind's global default.
	policy = getPolicy(t, srv, token, libID)
	if policy.AuthoritativeProvider != nil {
		t.Errorf("the policy still names %q as the Admin's deliberate lead after that Plugin was uninstalled",
			*policy.AuthoritativeProvider)
	}
	if policy.EffectiveAuthoritative.Slug != builtinMetadataProvider {
		t.Errorf("effective lead = %q, want the video kind default %q",
			policy.EffectiveAuthoritative.Slug, builtinMetadataProvider)
	}
	if policy.AuthoritativeUnreachable != nil {
		t.Errorf("an uninstalled lead was surfaced as an unreachable one (%q); an uninstall is not a degradation to file",
			*policy.AuthoritativeUnreachable)
	}
	if override, present := policy.supplementOverride(uninstallSuppSource); present && override != nil {
		t.Errorf("the policy still forces an uninstalled Supplement %v: %+v", *override, policy.Supplements)
	}

	// The next pass runs, with no error surfaced: falling back to the kind default
	// is the ADR-0027 posture, and an uninstalled lead must never stall a Library.
	if res := enrichLib(t, srv, token, libID, "full"); res.Failed != 0 {
		t.Errorf("the pass after uninstalling the lead reported %d failures: %+v", res.Failed, res)
	}

	// And a reinstall under the same ids is born inheriting — the policy rows went
	// with the Plugin, so the next operator starts from the kind default.
	uploadMetadataProviderPlugin(t, srv, token, uninstallLeadSource, leadVideoProvides())
	uploadMetadataProviderPlugin(t, srv, token, uninstallSuppSource, supplementVideoProvides())

	policy = getPolicy(t, srv, token, libID)
	if policy.AuthoritativeProvider != nil {
		t.Errorf("a reinstalled Plugin was still this Library's stored lead: %q", *policy.AuthoritativeProvider)
	}
	if override, present := policy.supplementOverride(uninstallSuppSource); present && override != nil {
		t.Errorf("a reinstalled Supplement inherited the previous install's forced %v, want inherit (null)", *override)
	}
}
