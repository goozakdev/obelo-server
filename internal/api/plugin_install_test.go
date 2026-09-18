package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for INSTALLING a Plugin (ADR-0058, .scratch/plugin-system issue
// 10): an Admin puts a module on a running server through the web app, switches it
// on and off, forgives a failure, and takes it away again — no restart, and no
// shell on the box.
//
// Everything asserted here is what an Admin can observe: the Plugins screen's own
// response, the Event Sinks screen beside it, a document arriving at their HTTP
// server, and a directory that is or is not on disk. The module is compiled from
// source by the suite (internal/plugins/plugintest), so an upload carries a real
// guest across the real ABI rather than a stand-in.

// --- wire shapes ----------------------------------------------------------------

type installedPluginResp struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	APIVersion        int      `json:"apiVersion"`
	Provides          []string `json:"provides"`
	Enabled           bool     `json:"enabled"`
	DisabledByFailure bool     `json:"disabledByFailure"`
	LastError         string   `json:"lastError"`
	Source            string   `json:"source"`
	InstalledAt       string   `json:"installedAt"`
	// Origin and State arrived with the Bundled plugins (ADR-0059): where this
	// plugin came from ("bundled" | "admin"), and — for a shipped plugin the Admin
	// uninstalled — "declined", which is the row the screen offers it back on.
	Origin string `json:"origin"`
	State  string `json:"state"`
}

type pluginsResp struct {
	Plugins []installedPluginResp `json:"plugins"`
}

type apiErrorResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const pluginsPath = "/api/v1/settings/plugins"

func readPlugins(t *testing.T, srv *testharness.Server, token string) pluginsResp {
	t.Helper()
	var resp pluginsResp
	status, body := srv.AuthGET(pluginsPath, token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET plugins status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func pluginNamed(t *testing.T, resp pluginsResp, id string) installedPluginResp {
	t.Helper()
	for _, p := range resp.Plugins {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no plugin %q on the Plugins screen; got %+v", id, resp.Plugins)
	return installedPluginResp{}
}

func hasPlugin(resp pluginsResp, id string) bool {
	for _, p := range resp.Plugins {
		if p.ID == id {
			return true
		}
	}
	return false
}

// uploadPlugin posts a manifest and a module as the browser's form does, and
// returns the status and the decoded body — both, because half these tests are
// about a refusal.
func uploadPlugin(t *testing.T, srv *testharness.Server, token string, manifest, module []byte) (int, []byte) {
	t.Helper()
	return srv.MultipartFiles(http.MethodPost, pluginsPath, token, []testharness.MultipartFile{
		{Field: "manifest", Name: plugins.ManifestFile, ContentType: "application/json", Content: manifest},
		{Field: "module", Name: plugins.DefaultModuleFile, ContentType: "application/wasm", Content: module},
	}, nil)
}

// installGuestPlugin uploads the suite's guest under an id and fails unless the
// server took it.
func installGuestPlugin(t *testing.T, srv *testharness.Server, token, id string) {
	t.Helper()
	m := plugintest.SinkManifest(id)
	status, body := uploadPlugin(t, srv, token, plugintest.ManifestJSON(t, m), plugintest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("installing %s: status = %d, want 201; body: %s", id, status, body)
	}
}

// refusalOf decodes the error envelope, failing unless the status matches.
func refusalOf(t *testing.T, wantStatus, status int, body []byte) apiErrorResp {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", status, wantStatus, body)
	}
	var out apiErrorResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the refusal is not an error envelope: %v\nbody: %s", err, body)
	}
	return out
}

// pluginSource serves a manifest and the module beside it, the way a plugin author
// publishes one: two files in one directory, under the names they take on disk.
func pluginSource(t *testing.T, m pluginapi.Manifest) *httptest.Server {
	t.Helper()
	manifest := plugintest.ManifestJSON(t, m)
	guest := plugintest.Guest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case plugins.ManifestFile:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifest)
		case plugins.DefaultModuleFile:
			w.Header().Set("Content-Type", "application/wasm")
			_, _ = w.Write(guest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- the tracer -------------------------------------------------------------------

// TestUploadingAPluginMakesItAnEventSinkOnTheRunningServer is the acceptance
// criterion, whole. An Admin uploads a module and a manifest; the Plugin appears
// on the Event Sinks screen inside the SAME server process, with no restart;
// subscribing it and running a scan delivers a signed document to their own HTTP
// server; and uninstalling it takes it off the screen and off the disk.
func TestUploadingAPluginMakesItAnEventSinkOnTheRunningServer(t *testing.T) {
	receiver := newSinkReceiver(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	// Nothing an ADMIN installed, and the Event Sinks screen is the Webhook alone.
	// (A fresh server does carry the plugins it ships — ADR-0059 — which is what
	// notShipped filters out here.)
	if got := notShipped(readPlugins(t, srv, token).Plugins); len(got) != 0 {
		t.Fatalf("a fresh server lists %+v, want no installed plugins", got)
	}
	if got := readSinks(t, srv, token); len(got.Sinks) != 1 {
		t.Fatalf("before the install the sink screen lists %+v, want just the Webhook", got.Sinks)
	}

	installGuestPlugin(t, srv, token, "example-sink")

	view := pluginNamed(t, readPlugins(t, srv, token), "example-sink")
	if view.Name != "Test Sink (example-sink)" || view.Version != "1.0.0" || view.APIVersion != pluginapi.APIVersion {
		t.Fatalf("the Plugins screen shows %+v, want the manifest's own facts", view)
	}
	if !view.Enabled || view.DisabledByFailure || view.LastError != "" {
		t.Fatalf("a freshly installed Plugin reads as %+v, want enabled and working", view)
	}
	if view.Source != plugins.SourceUpload || view.InstalledAt == "" {
		t.Fatalf("the Plugins screen shows %+v, want the provenance and the install time", view)
	}
	if len(view.Provides) != 1 || view.Provides[0] != string(pluginapi.ExtensionEventSink) {
		t.Fatalf("provides = %v, want the one Extension point the manifest names", view.Provides)
	}

	// It is an Event sink on the OTHER screen, in this same process, with no
	// restart — which is the whole point of the issue.
	sink := sinkNamed(t, readSinks(t, srv, token), "example-sink")
	if !sink.Installed || sink.Disabled {
		t.Fatalf("the uploaded Plugin reads as %+v on the Event Sinks screen, want a working Installed sink", sink)
	}

	// Configured through the same endpoint as the Webhook, and it delivers.
	configureSink(t, srv, token, map[string]any{
		"slug": "example-sink", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL, "events": []string{"scan.completed"},
	})
	scanLib(t, srv, token, libID, "")
	posts := receiver.waitForPosts(t, 1)
	verifySignature(t, "topsecret", posts[0])

	// Uninstalling takes it off both screens and off the disk.
	status, body := srv.JSON(http.MethodDelete, pluginsPath+"/example-sink", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body: %s", status, body)
	}
	if got := readPlugins(t, srv, token); hasPlugin(got, "example-sink") {
		t.Fatalf("an uninstalled Plugin is still on the Plugins screen: %+v", got.Plugins)
	}
	sinks := readSinks(t, srv, token)
	if len(sinks.Sinks) != 1 || sinks.Sinks[0].Slug != "webhook" {
		t.Fatalf("after an uninstall the sink screen lists %+v, want just the Webhook again", sinks.Sinks)
	}
	dir := filepath.Join(srv.DataDir, plugins.DirName, "example-sink")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s is still on disk after an uninstall (err = %v)", dir, err)
	}

	// And the settings row went with it: a reinstall starts with no secret and no
	// target, rather than quietly reviving the last operator's configuration.
	installGuestPlugin(t, srv, token, "example-sink")
	fresh := sinkNamed(t, readSinks(t, srv, token), "example-sink")
	if fresh.HasSecret || fresh.URL != "" || len(fresh.Events) != 0 {
		t.Fatalf("a reinstalled Plugin came back configured as %+v, want a clean row", fresh)
	}
}

// --- install from a URL ------------------------------------------------------------

// TestInstallingAPluginFromAPastedURL: one URL — the manifest's — and the module is
// fetched from beside it. Same guest, same result, through the safe fetcher.
func TestInstallingAPluginFromAPastedURL(t *testing.T) {
	source := pluginSource(t, plugintest.SinkManifest("url-sink"))
	receiver := newSinkReceiver(t)
	// The fixture is served from 127.0.0.1, which the install path refuses by
	// default precisely because a Plugin is code this server will run. There is no
	// hermetic public address to serve it from, so the suite says so explicitly —
	// and the refusal itself is the next test.
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	manifestURL := source.URL + "/" + plugins.ManifestFile
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": manifestURL}, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST from-url status = %d, want 201; body: %s", status, body)
	}

	view := pluginNamed(t, readPlugins(t, srv, token), "url-sink")
	if view.Source != manifestURL {
		t.Fatalf("source = %q, want the URL the Admin pasted", view.Source)
	}
	if view.DisabledByFailure || view.LastError != "" {
		t.Fatalf("a Plugin fetched from a URL reads as %+v, want it working", view)
	}

	// It is the same Plugin an upload produces: it delivers.
	configureSink(t, srv, token, map[string]any{
		"slug": "url-sink", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL, "events": []string{"scan.completed"},
	})
	scanLib(t, srv, token, libID, "")
	verifySignature(t, "topsecret", receiver.waitForPosts(t, 1)[0])
}

// TestAURLResolvingToAPrivateAddressIsRefused. Every OTHER outbound fetch in this
// server deliberately allows a private first hop — an operator pointing a provider
// at a mirror on their own LAN is the point of the product, and safefetch says so
// at length. This one does not, because what comes back is executed.
func TestAURLResolvingToAPrivateAddressIsRefused(t *testing.T) {
	source := pluginSource(t, plugintest.SinkManifest("url-sink"))
	srv := testharness.New(t) // deliberately WITHOUT the private-source option
	token := adminToken(t, srv)

	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": source.URL + "/" + plugins.ManifestFile}, nil)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if refusal.Error.Code != "PLUGIN_SOURCE_REFUSED" {
		t.Fatalf("code = %q, want PLUGIN_SOURCE_REFUSED; body: %s", refusal.Error.Code, body)
	}
	if !strings.Contains(refusal.Error.Message, "upload the file instead") {
		t.Fatalf("message = %q, want it to say what to do instead", refusal.Error.Message)
	}
	if got := notShipped(readPlugins(t, srv, token).Plugins); len(got) != 0 {
		t.Fatalf("a refused URL install left %+v behind", got)
	}
}

// TestAURLThatIsNotAURL and the two ways a source can answer with nothing usable.
func TestARefusedSourceIsNotAnInstall(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(empty.Close)

	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)

	for _, tc := range []struct {
		name   string
		url    string
		wantIn string
	}{
		{"not a URL at all", "not-a-url", "absolute http:// or https:// URL"},
		{"a file URL", "file:///etc/passwd", "absolute http:// or https:// URL"},
		{"a source with no manifest", empty.URL + "/" + plugins.ManifestFile, "answered 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
				map[string]any{"url": tc.url}, nil)
			refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
			if refusal.Error.Code != "PLUGIN_SOURCE_REFUSED" {
				t.Fatalf("code = %q, want PLUGIN_SOURCE_REFUSED", refusal.Error.Code)
			}
			if !strings.Contains(refusal.Error.Message, tc.wantIn) {
				t.Fatalf("message = %q, want it to contain %q", refusal.Error.Message, tc.wantIn)
			}
		})
	}
}

// --- refusals ---------------------------------------------------------------------

// TestEachInstallRefusalIsItsOwnCodeAndSentence. Five things can go wrong and they
// want five different things done about them, so an Admin is told which one
// happened rather than "it did not work" in the same words every time.
func TestEachInstallRefusalIsItsOwnCodeAndSentence(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	// One Plugin already installed, so the duplicate case has something to collide
	// with.
	installGuestPlugin(t, srv, token, "example-sink")

	future := plugintest.SinkManifest("future-sink")
	future.APIVersion = pluginapi.APIVersion + 1
	older := plugintest.SinkManifest("older-sink")
	older.APIVersion = 0

	for _, tc := range []struct {
		name       string
		manifest   []byte
		module     []byte
		wantStatus int
		wantCode   string
		wantIn     string
	}{
		{
			name:       "a manifest that is not JSON",
			manifest:   []byte("{nope"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PLUGIN_INVALID_MANIFEST",
			wantIn:     "is not valid JSON",
		},
		{
			name:       "an apiVersion this server does not speak",
			manifest:   plugintest.ManifestJSON(t, future),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PLUGIN_API_VERSION",
			wantIn:     "upgrade the server",
		},
		{
			name:       "an apiVersion the plugin has outgrown",
			manifest:   plugintest.ManifestJSON(t, older),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PLUGIN_API_VERSION",
			wantIn:     "upgrade the plugin",
		},
		{
			name:       "an id another plugin already has",
			manifest:   plugintest.ManifestJSON(t, plugintest.SinkManifest("example-sink")),
			wantStatus: http.StatusConflict,
			wantCode:   "PLUGIN_DUPLICATE",
			wantIn:     "example-sink",
		},
		{
			name:       "an id a Built-in already has",
			manifest:   plugintest.ManifestJSON(t, plugintest.SinkManifest("webhook")),
			wantStatus: http.StatusConflict,
			wantCode:   "PLUGIN_DUPLICATE",
			wantIn:     "already claimed by a plugin this server ships",
		},
		{
			name:       "a module that is not WebAssembly",
			manifest:   plugintest.ManifestJSON(t, plugintest.SinkManifest("broken-sink")),
			module:     []byte("#!/bin/sh\necho definitely not wasm\n"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "PLUGIN_INVALID_MODULE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := tc.module
			if module == nil {
				module = plugintest.Guest(t)
			}
			status, body := uploadPlugin(t, srv, token, tc.manifest, module)
			refusal := refusalOf(t, tc.wantStatus, status, body)
			if refusal.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q; body: %s", refusal.Error.Code, tc.wantCode, body)
			}
			if tc.wantIn != "" && !strings.Contains(refusal.Error.Message, tc.wantIn) {
				t.Fatalf("message = %q, want it to contain %q", refusal.Error.Message, tc.wantIn)
			}
		})
	}

	// Nothing was installed by any of that, and the one Plugin that WAS there is
	// untouched and still working.
	got := notShipped(readPlugins(t, srv, token).Plugins)
	if len(got) != 1 || got[0].ID != "example-sink" || got[0].DisabledByFailure {
		t.Fatalf("after six refusals the screen shows %+v, want only the one good Plugin", got)
	}
	// Including on disk: no staging leftovers, no half-written directory.
	entries, err := os.ReadDir(filepath.Join(srv.DataDir, plugins.DirName))
	if err != nil {
		t.Fatalf("reading the plugins directory: %v", err)
	}
	if names := notShippedDirs(entries); len(names) != 1 || names[0] != "example-sink" {
		t.Fatalf("the plugins directory holds %v, want just the one that installed", names)
	}
}

// TestAnUploadMissingHalfOfAPluginSaysWhichHalf.
func TestAnUploadMissingHalfOfAPluginSaysWhichHalf(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	status, body := srv.MultipartFiles(http.MethodPost, pluginsPath, token,
		[]testharness.MultipartFile{{Field: "module", Name: "plugin.wasm", Content: plugintest.Guest(t)}}, nil)
	refusal := refusalOf(t, http.StatusBadRequest, status, body)
	if !strings.Contains(refusal.Error.Message, "manifest") {
		t.Fatalf("message = %q, want it to name the missing part", refusal.Error.Message)
	}

	status, body = srv.MultipartFiles(http.MethodPost, pluginsPath, token,
		[]testharness.MultipartFile{{Field: "manifest", Name: plugins.ManifestFile,
			Content: plugintest.ManifestJSON(t, plugintest.SinkManifest("lonely"))}}, nil)
	refusal = refusalOf(t, http.StatusBadRequest, status, body)
	if !strings.Contains(refusal.Error.Message, "module") {
		t.Fatalf("message = %q, want it to name the missing part", refusal.Error.Message)
	}
}

// --- lifecycle ----------------------------------------------------------------------

// TestDisableStopsDeliveryImmediatelyAndEnableResumesIt. Disable is not a flag the
// delivery path consults — the Plugin is simply not in the registry the sink
// Manager rebuilds from, so there is no worker and nothing to deliver with.
func TestDisableStopsDeliveryImmediatelyAndEnableResumesIt(t *testing.T) {
	receiver := newSinkReceiver(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	installGuestPlugin(t, srv, token, "example-sink")
	configureSink(t, srv, token, map[string]any{
		"slug": "example-sink", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL, "events": []string{"scan.completed"},
	})
	scanLib(t, srv, token, libID, "")
	receiver.waitForPosts(t, 1)

	// Off.
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/example-sink/disable", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("disable status = %d, want 200; body: %s", status, body)
	}
	if view := pluginNamed(t, readPlugins(t, srv, token), "example-sink"); view.Enabled {
		t.Fatalf("after Disable the Plugin reads as %+v, want enabled false", view)
	}
	// It is off the Event Sinks screen entirely, because it is not registered.
	for _, s := range readSinks(t, srv, token).Sinks {
		if s.Slug == "example-sink" {
			t.Fatalf("a switched-off Plugin is still an Event sink: %+v", s)
		}
	}

	before := len(receiver.received())
	for i := 0; i < 2; i++ {
		scanLib(t, srv, token, libID, "")
	}
	// Give a delivery that should not happen every chance to happen.
	time.Sleep(300 * time.Millisecond)
	if after := len(receiver.received()); after != before {
		t.Fatalf("a switched-off Plugin delivered %d more documents", after-before)
	}

	// On again, and it resumes — the settings it had are still there.
	status, body = srv.JSON(http.MethodPost, pluginsPath+"/example-sink/enable", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("enable status = %d, want 200; body: %s", status, body)
	}
	back := sinkNamed(t, readSinks(t, srv, token), "example-sink")
	if !back.Enabled || !back.HasSecret || back.URL != receiver.srv.URL {
		t.Fatalf("a re-enabled Plugin came back as %+v, want its settings intact", back)
	}
	scanLib(t, srv, token, libID, "")
	receiver.waitForPosts(t, before+1)
}

// TestReenableAfterARecordedFailureClearsTheErrorAndResumes: the Re-enable button
// the PRD names. A guest that traps on every call is disabled by this server after
// a run of failures; the Admin fixes what it was pointed at, presses Re-enable, and
// the error goes and delivery comes back.
func TestReenableAfterARecordedFailureClearsTheErrorAndResumes(t *testing.T) {
	receiver := newSinkReceiver(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	installGuestPlugin(t, srv, token, "example-sink")
	configureSink(t, srv, token, map[string]any{
		"slug": "example-sink", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL + "/?obelo-mode=panic", "events": []string{"scan.completed"},
	})
	for i := 0; i < 3; i++ {
		scanLib(t, srv, token, libID, "")
	}
	waitForSink(t, srv, token, "example-sink", func(s installedSinkResp) bool { return s.Disabled })

	// The Plugins screen shows the same failure, in its own words.
	failed := pluginNamed(t, readPlugins(t, srv, token), "example-sink")
	if !failed.DisabledByFailure || failed.LastError == "" {
		t.Fatalf("the Plugins screen shows %+v for a Plugin this server stopped calling, want the error", failed)
	}
	if !failed.Enabled {
		t.Fatalf("the Admin's switch was flipped by a failure: %+v", failed)
	}

	// The Admin points it somewhere that works and forgives it.
	configureSink(t, srv, token, map[string]any{"slug": "example-sink", "url": receiver.srv.URL})
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/example-sink/reenable", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reenable status = %d, want 200; body: %s", status, body)
	}
	cleared := pluginNamed(t, readPlugins(t, srv, token), "example-sink")
	if cleared.DisabledByFailure || cleared.LastError != "" {
		t.Fatalf("after Re-enable the Plugin reads as %+v, want a clean record", cleared)
	}

	before := len(receiver.received())
	scanLib(t, srv, token, libID, "")
	receiver.waitForPosts(t, before+1)
}

// TestALifecycleVerbOnAPluginThatIsNotInstalled: a 404 with a sentence, not a 404
// that reads like a typo'd path.
func TestAPluginVerbOnSomethingNotInstalled(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, path := range []string{"/nobody/enable", "/nobody/disable", "/nobody/reenable"} {
		status, body := srv.JSON(http.MethodPost, pluginsPath+path, token, nil, nil)
		refusal := refusalOf(t, http.StatusNotFound, status, body)
		if refusal.Error.Code != "PLUGIN_UNKNOWN" {
			t.Fatalf("%s code = %q, want PLUGIN_UNKNOWN; body: %s", path, refusal.Error.Code, body)
		}
	}
	status, body := srv.JSON(http.MethodDelete, pluginsPath+"/nobody", token, nil, nil)
	if refusal := refusalOf(t, http.StatusNotFound, status, body); refusal.Error.Code != "PLUGIN_UNKNOWN" {
		t.Fatalf("DELETE code = %q, want PLUGIN_UNKNOWN", refusal.Error.Code)
	}
}

// TestAHandPlacedPluginIsOnTheScreenToo: every Installed plugin arrived by hand
// before this surface existed, and one still can. It is listed, and its buttons
// work.
func TestAHandPlacedPluginIsOnTheScreenToo(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("by-hand"))

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)

	view := pluginNamed(t, readPlugins(t, srv, token), "by-hand")
	if !view.Enabled || view.DisabledByFailure {
		t.Fatalf("a hand-placed Plugin reads as %+v, want it enabled and working", view)
	}
	if status, body := srv.JSON(http.MethodPost, pluginsPath+"/by-hand/disable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("disabling a hand-placed Plugin: status = %d; body: %s", status, body)
	}
	if got := pluginNamed(t, readPlugins(t, srv, token), "by-hand"); got.Enabled {
		t.Fatalf("a hand-placed Plugin could not be switched off: %+v", got)
	}
}

// TestADisabledPluginStaysDisabledAcrossARestart: the Admin's switch is durable,
// which is the only reason it is in the database at all.
func TestADisabledPluginStaysDisabledAcrossARestart(t *testing.T) {
	dataDir := t.TempDir()
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	installGuestPlugin(t, srv, token, "example-sink")
	if status, body := srv.JSON(http.MethodPost, pluginsPath+"/example-sink/disable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("disable status = %d; body: %s", status, body)
	}
	srv.Close()

	// The same install, booted again: setup is already complete, so the Admin logs
	// in rather than claiming the server a second time.
	again := testharness.New(t, testharness.WithDataDir(dataDir))
	token2 := again.LoginAs("brandon", "hunter2hunter2")
	view := pluginNamed(t, readPlugins(t, again, token2), "example-sink")
	if view.Enabled {
		t.Fatalf("after a restart the Plugin reads as %+v, want it still switched off", view)
	}
	for _, s := range readSinks(t, again, token2).Sinks {
		if s.Slug == "example-sink" {
			t.Fatalf("a Plugin switched off before a restart came back as an Event sink: %+v", s)
		}
	}
}

// --- access -------------------------------------------------------------------------

// TestPluginManagementIsAdminOnly: a Member gets the same refusal every other
// settings endpoint gives them. This surface installs CODE, so it gets no special
// treatment in either direction — it is behind the same middleware as the rest.
func TestPluginManagementIsAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")

	if status, _ := srv.AuthGET(pluginsPath, member, nil); status != http.StatusForbidden {
		t.Fatalf("member GET status = %d, want 403", status)
	}
	status, body := srv.MultipartFiles(http.MethodPost, pluginsPath, member, []testharness.MultipartFile{
		{Field: "manifest", Name: plugins.ManifestFile,
			Content: plugintest.ManifestJSON(t, plugintest.SinkManifest("sneaky"))},
		{Field: "module", Name: plugins.DefaultModuleFile, Content: plugintest.Guest(t)},
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("member upload status = %d, want 403; body: %s", status, body)
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, pluginsPath + "/from-url"},
		{http.MethodPost, pluginsPath + "/anything/enable"},
		{http.MethodPost, pluginsPath + "/anything/disable"},
		{http.MethodPost, pluginsPath + "/anything/reenable"},
		{http.MethodDelete, pluginsPath + "/anything"},
	} {
		status, body := srv.JSON(call.method, call.path, member, map[string]any{"url": "https://example.test/x"}, nil)
		if status != http.StatusForbidden {
			t.Fatalf("member %s %s status = %d, want 403; body: %s", call.method, call.path, status, body)
		}
	}
	// And nothing they tried landed.
	if got := notShipped(readPlugins(t, srv, admin).Plugins); len(got) != 0 {
		t.Fatalf("a Member installed %+v", got)
	}
}
