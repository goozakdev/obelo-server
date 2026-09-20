package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box test for what an UNINSTALL takes with it (.scratch/plugin-system
// issue 16, ADR-0058 decision 5): the key-value namespace an Installed plugin
// wrote for itself.
//
// Issue 11 gave every Plugin that namespace and promised in three places — the
// schema, the comment on the kv_* host functions, and its own hand-off — that
// uninstall drops it. Issue 10's uninstall never took the hand-off, so a
// guest's cursors and caches outlived the Plugin, and a reinstall under the same
// id — by the same author, or by anyone who picked the same slug — read them back
// through kv_get as if it had written them. An uninstall is supposed to leave
// exactly the identity-keyed artwork and subtitles a Plugin produced, and nothing
// else.
//
// Everything here is what an Admin and a viewer can do: upload two files, turn
// the Plugin on, search for subtitles, press Uninstall, upload the same two files
// again. The guest reports what its own namespace held through the one channel
// that reaches a person — the candidate id in "search online" — because no
// endpoint exposes plugin_kv and none should.

const (
	kvProbePluginID = "kv-probe-subs"
	// The three strings the guest writes, from its own source
	// (internal/plugins/plugintest/testdata/guest/main.go).
	kvProbeSecret   = "obelo-mode=kv-probe"
	kvProbeAbsent   = "namespace-absent"
	kvProbeSurvived = "namespace-survived"
)

// TestUninstallingAPluginDropsItsKeyValueNamespace is the acceptance criterion
// whole: install, write, uninstall, reinstall, and the key is absent again.
//
// It asks the guest THREE times before the uninstall, not one, because "absent"
// on its own proves nothing: the first probe has to be followed by one that reads
// the value back, or a namespace that was never written would pass this test
// exactly as a namespace that was properly dropped does.
func TestUninstallingAPluginDropsItsKeyValueNamespace(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	titleID := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	// The Admin's two steps: upload the module, then switch the provider on with a
	// key. Both are the ordinary endpoints — the same ones the Webhook and the
	// OpenSubtitles Built-in are driven by.
	install := func() {
		t.Helper()
		manifest := plugintest.ManifestJSON(t, plugintest.SubtitleManifest(kvProbePluginID))
		status, body := uploadPlugin(t, srv, token, manifest, plugintest.Guest(t))
		if status != http.StatusCreated {
			t.Fatalf("installing %s: status = %d, want 201; body: %s", kvProbePluginID, status, body)
		}
		configureProvider(t, srv, token, map[string]any{
			"slug": kvProbePluginID, "enabled": true, "apiKey": kvProbeSecret,
		})
	}

	// probe is one viewer's "search online". The guest reads its key, reports what
	// was there as the candidate's id, and writes the key before answering.
	probe := func(when string) string {
		t.Helper()
		cands := searchOnline(t, srv, token, titleID, "en")
		if len(cands.Candidates) != 1 {
			t.Fatalf("%s: search-online returned %d candidates, want the probe's one: %+v",
				when, len(cands.Candidates), cands.Candidates)
		}
		return cands.Candidates[0].ID
	}

	install()
	if got := probe("on a fresh install"); got != kvProbeAbsent {
		t.Fatalf("a Plugin installed for the first time found %q in its namespace, want it empty", got)
	}
	if got := probe("on the second ask"); got != kvProbeSurvived {
		t.Fatalf("the guest's own write is not readable back: %q — the probe proves nothing without it", got)
	}

	// Uninstall, through the Plugins screen's own verb.
	status, body := srv.JSON(http.MethodDelete, pluginsPath+"/"+kvProbePluginID, token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body: %s", status, body)
	}
	if got := readPlugins(t, srv, token); hasPlugin(got, kvProbePluginID) {
		t.Fatalf("an uninstalled Plugin is still on the Plugins screen: %+v", got.Plugins)
	}

	// The same module, under the same id, installed again.
	install()
	if got := probe("after a reinstall"); got != kvProbeAbsent {
		t.Fatalf("a reinstalled Plugin read the earlier install's namespace back (%q); "+
			"uninstall did not drop it", got)
	}
}

// TestUninstallingOnePluginLeavesAnothersNamespaceAlone is the isolation half,
// after a delete rather than after a write: two Plugins hold the SAME key, one is
// uninstalled, and the other still reads its own value. It is the sibling of the
// scoping tests in internal/store and internal/plugins, at the layer where the
// uninstall actually happens.
//
// The two are driven one at a time because one Subtitle provider serves a search
// at a time (the first enabled, keyed row wins) — which is fine: what is under
// test is whose rows the DELETE took, not who answers a viewer.
func TestUninstallingOnePluginLeavesAnothersNamespaceAlone(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	titleID := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	const alpha, beta = "kv-probe-alpha", "kv-probe-beta"
	for _, id := range []string{alpha, beta} {
		manifest := plugintest.ManifestJSON(t, plugintest.SubtitleManifest(id))
		status, body := uploadPlugin(t, srv, token, manifest, plugintest.Guest(t))
		if status != http.StatusCreated {
			t.Fatalf("installing %s: status = %d, want 201; body: %s", id, status, body)
		}
	}

	// ask turns one of the still-installed Plugins on, alone, and reports what its
	// namespace held. The whole list is written every time because the settings
	// endpoint only knows about Plugins that are installed right now — naming an
	// uninstalled one is a 422, and rightly so.
	ask := func(id string, installed ...string) string {
		t.Helper()
		providers := []map[string]any{}
		for _, other := range installed {
			providers = append(providers, map[string]any{
				"slug": other, "enabled": other == id, "apiKey": kvProbeSecret,
			})
		}
		var resp installedProvidersResp
		status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/subtitle-providers", token,
			map[string]any{"providers": providers}, &resp)
		if status != http.StatusOK {
			t.Fatalf("PUT subtitle-providers status = %d, want 200; body: %s", status, raw)
		}
		cands := searchOnline(t, srv, token, titleID, "en")
		if len(cands.Candidates) != 1 {
			t.Fatalf("%s: search-online returned %d candidates, want the probe's one: %+v",
				id, len(cands.Candidates), cands.Candidates)
		}
		return cands.Candidates[0].ID
	}

	// Both write the same key, in their own namespaces.
	if got := ask(alpha, alpha, beta); got != kvProbeAbsent {
		t.Fatalf("alpha started with %q in its namespace, want it empty", got)
	}
	if got := ask(beta, alpha, beta); got != kvProbeAbsent {
		t.Fatalf("beta saw alpha's value under the same key: %q", got)
	}

	status, body := srv.JSON(http.MethodDelete, pluginsPath+"/"+alpha, token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body: %s", status, body)
	}

	// Beta's namespace is untouched by its neighbour's uninstall.
	if got := ask(beta, beta); got != kvProbeSurvived {
		t.Fatalf("beta's own value did not survive alpha's uninstall: %q", got)
	}
}
