package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/bundled"
	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The Bundled plugins, end to end (ADR-0059, .scratch/bundled-plugins issue 04):
// a fresh server installs the metadata providers it SHIPS, on first boot, as
// WebAssembly modules under <dataDir>/plugins/ — and from there they are Installed
// plugins like any other, through the real loader, the real sandbox and the real
// settings API.
//
// This file also holds the two helpers the pre-existing install / signing /
// catalog suites needed once a fresh server stopped being empty. Those suites
// assert that a refused install left NOTHING behind, which used to be spelled
// `len(plugins) == 0`; what they actually mean is that nothing THE ADMIN TRIED
// landed, and notShipped is that sentence.

// shippedIDs is what this server installs on its own, as a set.
func shippedIDs() map[string]bool {
	out := map[string]bool{}
	for _, id := range bundled.Present() {
		out[id] = true
	}
	return out
}

// notShipped is the plugins on a server that the SERVER did not put there — what
// an Admin installed, and nothing else. It keeps saying the same thing as issues
// 05-07 add the other six.
func notShipped(list []installedPluginResp) []installedPluginResp {
	shipped := shippedIDs()
	out := make([]installedPluginResp, 0, len(list))
	for _, p := range list {
		if !shipped[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// notShippedDirs is notShipped for the plugins DIRECTORY, for the one suite that
// checks an install left no staging leftovers on disk.
func notShippedDirs(entries []os.DirEntry) []string {
	shipped := shippedIDs()
	var out []string
	for _, e := range entries {
		if !shipped[e.Name()] {
			out = append(out, e.Name())
		}
	}
	return out
}

// A fresh server, with no plugins directory in its data dir, boots with the
// shipped metadata providers installed: the files on disk, the row recorded as
// bundled, and the module compiled by the ordinary loader.
func TestAFreshServerInstallsTheShippedPlugins(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)

	got := pluginNamed(t, readPlugins(t, srv, token), "tmdb")
	if got.Origin != plugins.OriginBundled {
		t.Errorf("origin = %q, want %q — the Plugins screen would have no way to tell it apart "+
			"from something the operator uploaded", got.Origin, plugins.OriginBundled)
	}
	if !got.Enabled || got.DisabledByFailure || got.LastError != "" {
		t.Fatalf("the shipped TMDB plugin did not come up: %+v", got)
	}
	if got.Version == "" || got.APIVersion != 1 {
		t.Errorf("version/apiVersion = %q/%d, want the manifest's", got.Version, got.APIVersion)
	}
	if len(got.Provides) != 1 || got.Provides[0] != "metadata-provider" {
		t.Errorf("provides = %v, want [metadata-provider]", got.Provides)
	}

	// On disk, exactly where an Admin's upload would be.
	dir := filepath.Join(srv.DataDir, plugins.DirName, "tmdb")
	for _, name := range []string{plugins.ManifestFile, plugins.DefaultModuleFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is missing from %s: %v", name, dir, err)
		}
	}

	// And it LEADS video: registered ahead of the Built-ins, so the existing
	// "first authoritative Full provider of a kind" rule picks it with no provider
	// name anywhere in the host.
	providers := readMetadataProviders(t, srv, token)
	if len(providers) == 0 || providers[0].Slug != "tmdb" {
		t.Fatalf("the providers screen leads with %+v, want tmdb first", providers)
	}
	if !providers[0].HasAPIKey || !providers[0].Enabled {
		t.Errorf("TMDB is listed but not keyed/enabled: %+v — OBELO_TMDB_API_KEY must still plant "+
			"into the tmdb row by id", providers[0])
	}
}

// Uninstalling a Bundled plugin means uninstalled: it stays gone across a
// restart, it is listed as declined so the screen can offer it back, and
// reinstall-shipped brings it back as bundled.
func TestUninstallingAShippedPluginIsRememberedAcrossARestart(t *testing.T) {
	dataDir := t.TempDir()
	first := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, first)

	if status, body := first.JSON(http.MethodDelete, pluginsPath+"/tmdb", token, nil, nil); status != http.StatusOK {
		t.Fatalf("DELETE tmdb status = %d; body: %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, plugins.DirName, "tmdb")); !os.IsNotExist(err) {
		t.Fatalf("the files are still there after an uninstall (err = %v)", err)
	}
	first.Close()

	// Restart against the SAME data directory: the re-assert must leave it alone.
	second := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithEnrichmentKey("test-key"))
	token2 := second.LoginAs("brandon", "hunter2hunter2")

	if _, err := os.Stat(filepath.Join(dataDir, plugins.DirName, "tmdb")); !os.IsNotExist(err) {
		t.Fatal("the shipped plugin came back on the next boot, so uninstall meant 'until you restart'")
	}
	declined := pluginNamed(t, readPlugins(t, second, token2), "tmdb")
	if declined.State != plugins.StateDeclined || declined.Origin != plugins.OriginBundled {
		t.Fatalf("the declined row reads %+v, want state=declined origin=bundled so the screen can "+
			"offer the shipped version back", declined)
	}

	// And the way back.
	if status, body := second.JSON(http.MethodPost, pluginsPath+"/tmdb/reinstall-shipped", token2, nil, nil); status != http.StatusOK {
		t.Fatalf("reinstall-shipped status = %d; body: %s", status, body)
	}
	back := pluginNamed(t, readPlugins(t, second, token2), "tmdb")
	if back.Origin != plugins.OriginBundled || back.State != "" || !back.Enabled || back.DisabledByFailure {
		t.Fatalf("after reinstall-shipped the row reads %+v, want an ordinary installed bundled plugin", back)
	}
	if _, err := os.Stat(filepath.Join(dataDir, plugins.DirName, "tmdb", plugins.DefaultModuleFile)); err != nil {
		t.Errorf("the module was not written back: %v", err)
	}
	// It leads video again without a restart: the reinstall rebuilt and swapped.
	providers := readMetadataProviders(t, second, token2)
	if len(providers) == 0 || providers[0].Slug != "tmdb" {
		t.Fatalf("after reinstall-shipped the providers screen leads with %+v, want tmdb first", providers)
	}
}

// An Admin's OWN plugin under a shipped id wins, and a server restart leaves it
// byte for byte alone. This is the promise that makes re-asserting on every boot
// safe to do at all.
func TestAnAdminsOwnPluginUnderAShippedIDSurvivesARestart(t *testing.T) {
	dataDir := t.TempDir()
	first := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, first)

	// Remove the shipped one first — installing over an id that is already taken
	// is refused, which is itself right, and removing it is what an Admin would do.
	if status, body := first.JSON(http.MethodDelete, pluginsPath+"/tmdb", token, nil, nil); status != http.StatusOK {
		t.Fatalf("DELETE tmdb status = %d; body: %s", status, body)
	}
	manifest := plugintest.SinkManifest("tmdb")
	manifest.Version = "9.9.9"
	manifest.Name = "My own TMDB"
	status, body := first.MultipartFiles(http.MethodPost, pluginsPath, token, []testharness.MultipartFile{
		{Field: "manifest", Name: plugins.ManifestFile, Content: plugintest.ManifestJSON(t, manifest)},
		{Field: "module", Name: plugins.DefaultModuleFile, Content: plugintest.Guest(t)},
	}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("uploading an own tmdb status = %d; body: %s", status, body)
	}
	modulePath := filepath.Join(dataDir, plugins.DirName, "tmdb", plugins.DefaultModuleFile)
	before, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("reading the uploaded module: %v", err)
	}
	first.Close()

	second := testharness.New(t, testharness.WithDataDir(dataDir))
	token2 := second.LoginAs("brandon", "hunter2hunter2")

	after, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("reading the module after a restart: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("a server restart overwrote the Admin's own plugin with the shipped one")
	}
	got := pluginNamed(t, readPlugins(t, second, token2), "tmdb")
	if got.Origin != plugins.OriginAdmin || got.Version != "9.9.9" {
		t.Fatalf("after a restart the row reads %+v, want the Admin's own (origin=admin, version 9.9.9)", got)
	}
}

// A data directory holding an OLDER bundled copy is upgraded in place on boot,
// and everything keyed to the id — the operator's API key above all — survives,
// because the id did not change.
func TestAnOlderBundledCopyIsReplacedOnBoot(t *testing.T) {
	dataDir := t.TempDir()
	first := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, first)
	shippedVersion := pluginNamed(t, readPlugins(t, first, token), "tmdb").Version
	first.Close()

	// Age the installed copy: rewrite its manifest's version to 0.9.0 and put a
	// stub where the module was, so "it was replaced" is visible in both files.
	dir := filepath.Join(dataDir, plugins.DirName, "tmdb")
	raw, err := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if err != nil {
		t.Fatalf("reading the installed manifest: %v", err)
	}
	aged := strings.Replace(string(raw), `"version": "`+shippedVersion+`"`, `"version": "0.9.0"`, 1)
	if aged == string(raw) {
		t.Fatalf("could not age the installed manifest; it reads %s", raw)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(aged), 0o644); err != nil {
		t.Fatalf("writing the aged manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.DefaultModuleFile), []byte("an old module"), 0o644); err != nil {
		t.Fatalf("writing the aged module: %v", err)
	}

	second := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithEnrichmentKey("test-key"))
	token2 := second.LoginAs("brandon", "hunter2hunter2")

	got := pluginNamed(t, readPlugins(t, second, token2), "tmdb")
	if got.Version != shippedVersion {
		t.Fatalf("after the upgrade the row says version %q, want the shipped %q", got.Version, shippedVersion)
	}
	if got.DisabledByFailure || got.LastError != "" {
		t.Fatalf("the replaced plugin did not come up: %+v", got)
	}
	module, err := os.ReadFile(filepath.Join(dir, plugins.DefaultModuleFile))
	if err != nil || string(module) == "an old module" {
		t.Fatalf("the old module is still on disk (err = %v)", err)
	}
	// The key is the thing an operator would notice losing.
	for _, p := range readMetadataProviders(t, second, token2) {
		if p.Slug == "tmdb" && !p.HasAPIKey {
			t.Fatal("the operator's TMDB key did not survive the plugin being replaced")
		}
	}
}

// Reinstalling a shipped plugin that is not declined, and one this server does
// not ship at all, are two different refusals.
func TestReinstallShippedRefusals(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	status, body := srv.JSON(http.MethodPost, pluginsPath+"/tmdb/reinstall-shipped", token, nil, nil)
	if status == http.StatusOK {
		t.Fatalf("reinstalling a plugin that is already installed succeeded; body: %s", body)
	}
	status, body = srv.JSON(http.MethodPost, pluginsPath+"/nosuchplugin/reinstall-shipped", token, nil, nil)
	if status == http.StatusOK {
		t.Fatalf("reinstalling an id this server does not ship succeeded; body: %s", body)
	}
}

// --- a small reader for the providers screen ------------------------------------

type metadataProviderRow struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	HasAPIKey bool   `json:"hasKey"`
}

func readMetadataProviders(t *testing.T, srv *testharness.Server, token string) []metadataProviderRow {
	t.Helper()
	var resp struct {
		Providers []metadataProviderRow `json:"providers"`
	}
	status, body := srv.AuthGET("/api/v1/settings/metadata-providers", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET metadata-providers status = %d; body: %s", status, body)
	}
	return resp.Providers
}

// The operator's rate policy reaches an Installed Metadata provider guest THROUGH
// THE SANDBOX, from the row it is saved in — the end-to-end half of
// .scratch/bundled-plugins issue 01, which could only prove it at the host seam.
//
// It matters because ADR-0059 decision 5 withdrew the host-side throttle and made
// pacing the guest's own business. A plugin that paces itself has exactly one way
// to learn what the operator asked for, and it is this one: the number is saved on
// the enrichment settings screen, resolved into the fixed Settings, and read back
// by the guest through settings_get inside a call. Every link in that chain is a
// different package, and this is the only test that crosses all of them.
func TestTheOperatorsRateLimitReachesAGuestThroughTheSandbox(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}))
	token := adminToken(t, srv)

	// The SDK guest in "settings" mode echoes what settings_get answered, as JSON,
	// in the record's overview — which is where an Admin would read it.
	uploadSDKGuest(t, srv, token, "sdk-source", "settings")
	keyProvider(t, srv, token, "sdk-source")
	putProviders(t, srv, token, map[string]any{"musicBrainzRateLimitMs": 750}, http.StatusOK)

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "sdk-source"}, http.StatusOK)
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "full")

	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	var saw struct {
		RateLimitMillis *int `json:"rateLimitMillis"`
	}
	overview := getEnrichedDetail(t, srv, token, titles[0].ID).Overview
	if err := json.Unmarshal([]byte(overview), &saw); err != nil {
		t.Fatalf("the guest did not echo its settings as JSON: %v\noverview: %s", err, overview)
	}
	// The POINTER is the point: absent is "pace yourself however you like" and 0 is
	// the operator explicitly turning throttling off, and 750 has to arrive as 750.
	if saw.RateLimitMillis == nil || *saw.RateLimitMillis != 750 {
		t.Fatalf("the guest saw rateLimitMillis %v, want 750 — the operator's pacing never "+
			"reached the code that is supposed to honour it", saw.RateLimitMillis)
	}
}

// A SOURCE OUTAGE RETRIES THE ITEMS AND DOES NOT STRIKE THE PLUGIN
// (.scratch/bundled-plugins: issue 04 follow-up).
//
// This is the end-to-end proof of the chain ADR-0059 decision 6 and ADR-0048
// describe together, and every link in it lives in a different package: the real
// bundled TMDB module, inside wazero, fetches a stand-in that answers 503; the SDK
// classifies that as "could not answer"; the guest replies OutcomeUnavailable; the
// host adapter turns that into a TRANSIENT error; and the pass schedules a retry
// rather than parking the Title.
//
// The number that matters is THREE. A Go error from a guest is a STRIKE, and
// internal/plugins disables a Plugin after three consecutive ones — so if any link
// here were wrong, a source having a bad afternoon would not merely park three
// movies, it would take TMDB off this server until an Admin pressed Re-enable. Two
// full passes over the three movie fixtures is six consecutive failing lookups,
// twice the threshold.
//
// Note what is NOT asserted: the Title's enrichmentStatus. A retry and a park both
// write 'failed' (store.SetTitleEnrichmentRetry) and are told apart by the attempts
// and retry-at columns, which the pass summary reports as Retrying against Failed.
// Those two counters are the honest observable here.
func TestASourceOutageRetriesTheItemsAndDoesNotStrikeThePlugin(t *testing.T) {
	requireFixtures(t)

	var calls int32
	outage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status_message":"the service is temporarily unavailable"}`))
	}))
	defer outage.Close()

	srv := testharness.New(t)
	token := adminToken(t, srv)
	// Point the SHIPPED plugin at the stand-in. The operator's own base URL is
	// reachable beside the manifest allowlist, which is what lets a bundled plugin
	// be driven against a loopback server at all.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "tmdb", "enabled": true, "apiKey": "an-operator-key", "baseURL": outage.URL},
	}}, http.StatusOK)

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	for pass := 1; pass <= 2; pass++ {
		res := enrichLib(t, srv, token, libID, "full")
		if res.Total == 0 {
			t.Skip("no titles in the movie fixture")
		}
		if res.Failed != 0 {
			t.Fatalf("pass %d: %d of %d titles were PARKED as failed by a 503 — a source outage "+
				"says nothing about the item (ADR-0048)", pass, res.Failed, res.Total)
		}
		if res.Retrying != res.Total {
			t.Fatalf("pass %d: retrying = %d of %d, want every title scheduled for a retry",
				pass, res.Retrying, res.Total)
		}
	}
	if got := atomic.LoadInt32(&calls); got < 6 {
		t.Fatalf("the stand-in saw %d requests; two passes over three fixtures should have made "+
			"at least six, so the guest was not the thing talking to it", got)
	}

	// And the plugin came through it: no strike, no recorded error, still leading.
	row := pluginNamed(t, readPlugins(t, srv, token), "tmdb")
	if row.DisabledByFailure {
		t.Fatalf("the TMDB plugin was DISABLED by a source outage: %+v — six consecutive "+
			"unavailable answers must cost it nothing (ADR-0059 decision 6)", row)
	}
	if row.LastError != "" {
		t.Errorf("the plugin recorded %q; an outage the guest ANSWERED is not the plugin failing",
			row.LastError)
	}
	if !row.Enabled {
		t.Errorf("the plugin is no longer enabled: %+v", row)
	}
	if providers := readMetadataProviders(t, srv, token); len(providers) == 0 || providers[0].Slug != "tmdb" {
		t.Errorf("after the outage the providers screen leads with %+v, want tmdb first", providers)
	}
}

// THE OTHER HALF, WRITTEN DOWN BECAUSE IT IS A DECISION AND NOT AN ACCIDENT: a
// REJECTED KEY still disables the plugin after three lookups.
//
// This is a characterization test. It asserts what this server does today so that
// nobody has to rediscover it, and it will fail the day somebody changes it — at
// which point the change was deliberate and this test says what it replaced.
//
// The reasoning, such as it is. A 401 describes OUR REQUEST: the key is wrong, and
// asking again with the same key gets the same answer forever, so the plugin
// answers a Go error and the item is parked where an Admin will see it. That much
// is right, and it is exactly what the Built-in did. What comes WITH it is that
// the host counts a guest's Go error as a strike, so three parked items also stop
// the plugin — which the Built-in never did, because a Built-in has no strikes.
//
// Whether that is the behaviour anybody wants is a real question and is NOT this
// issue's to answer:
//
//   - FOR: a source that answers 401 to everything is genuinely not working, and a
//     disabled plugin with "status 401" on the Plugins screen is a louder, more
//     findable statement than three quietly parked movies. The Admin fixes the key
//     and presses Re-enable, which is two clicks.
//   - AGAINST: it conflates "the operator's credential is wrong" with "this code is
//     broken", and those want different words on the screen. It also means ONE bad
//     key takes out a provider that would otherwise keep serving the items it can.
//
// Either way the OUTAGE case above is settled and this one is not, so this test
// pins the current answer rather than asserting a preference.
func TestARejectedKeyStillDisablesThePluginAfterThreeLookups(t *testing.T) {
	requireFixtures(t)

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status_message":"Invalid API key"}`))
	}))
	defer rejecting.Close()

	srv := testharness.New(t)
	token := adminToken(t, srv)
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "tmdb", "enabled": true, "apiKey": "a-key-this-source-rejects", "baseURL": rejecting.URL},
	}}, http.StatusOK)

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	res := enrichLib(t, srv, token, libID, "full")
	if res.Total < 3 {
		t.Skipf("the movie fixture holds %d titles; this needs at least the failure threshold", res.Total)
	}

	// The items are PARKED, not retried — which is the half that matches the
	// Built-in exactly.
	if res.Retrying != 0 {
		t.Errorf("retrying = %d, want 0: a rejected key is not worth asking again", res.Retrying)
	}
	if res.Failed == 0 {
		t.Errorf("failed = 0 of %d; a 401 must reach the Admin's attention list", res.Total)
	}

	// And the plugin is stopped, which the Built-in it replaced never was.
	row := pluginNamed(t, readPlugins(t, srv, token), "tmdb")
	if !row.DisabledByFailure {
		t.Fatalf("the plugin survived three rejected lookups: %+v — if this is now deliberate, "+
			"replace this test with one that says so", row)
	}
	if !strings.Contains(row.LastError, "401") {
		t.Errorf("lastError = %q, want the status in it so the Admin can tell a rejected key "+
			"from a broken module", row.LastError)
	}
	// The Admin's own switch is untouched: it is the LOADER that stopped calling
	// it, and Re-enable is the verb that forgives that.
	if !row.Enabled {
		t.Errorf("the Admin's enable switch was flipped by a failure: %+v", row)
	}
}
