package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for an INSTALLED Event sink (ADR-0058, .scratch/plugin-system
// issue 09): a WebAssembly module and a manifest an Admin placed by hand under
// <dataDir>/plugins/<id>/, loaded at boot into the same registry the Built-ins were
// registered into.
//
// Everything asserted here is what an Admin can observe: a settings response, a
// document arriving at their own HTTP server, and — when a Plugin misbehaves — a
// sentence on the same screen saying why it stopped. The module is compiled from
// source by the suite (internal/plugins/plugintest), so the loader is exercised by
// a real guest across the real ABI rather than by a stand-in.
//
// The receiver, the signature check and configureSink are shared with
// event_sinks_test.go, which is the point: an Installed sink is configured through
// exactly the same endpoint, with exactly the same body, as the Webhook Built-in.

// installedSinkResp is the sink view with the four fields issue 09 adds. It is a
// separate decoding of the same response body, so the existing suite's shape is
// untouched and a client that has not heard of Installed plugins still reads it.
type installedSinkResp struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	HasSecret bool     `json:"hasSecret"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Installed bool     `json:"installed"`
	Disabled  bool     `json:"disabled"`
	LastError string   `json:"lastError"`
	Version   string   `json:"version"`
	Counters  struct {
		Delivered int64 `json:"delivered"`
		Dropped   int64 `json:"dropped"`
		Failed    int64 `json:"failed"`
	} `json:"counters"`
}

type installedSinksResp struct {
	Sinks           []installedSinkResp `json:"sinks"`
	AvailableEvents []string            `json:"availableEvents"`
}

func readSinks(t *testing.T, srv *testharness.Server, token string) installedSinksResp {
	t.Helper()
	var resp installedSinksResp
	status, body := srv.AuthGET("/api/v1/settings/event-sinks", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET event-sinks status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func sinkNamed(t *testing.T, resp installedSinksResp, slug string) installedSinkResp {
	t.Helper()
	for _, s := range resp.Sinks {
		if s.Slug == slug {
			return s
		}
	}
	t.Fatalf("no sink %q on the settings screen; got %+v", slug, resp.Sinks)
	return installedSinkResp{}
}

// waitForSink polls the settings API until want says the sink is in the state the
// test is about, which is how an Admin finds out too — they refresh the screen.
func waitForSink(t *testing.T, srv *testharness.Server, token, slug string, want func(installedSinkResp) bool) installedSinkResp {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last installedSinkResp
	for {
		last = sinkNamed(t, readSinks(t, srv, token), slug)
		if want(last) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sink %q never reached the expected state; last view: %+v", slug, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- the tracer ---------------------------------------------------------------

// TestAnInstalledSinkIsListedBesideTheWebhookAndPostsAfterAScan is the acceptance
// criterion, whole: an Admin places two files, boots, sees the Plugin on the
// event-sink screen next to the Webhook, subscribes it to scan.completed through
// the ordinary settings endpoint, runs a scan, and their own HTTP server receives
// one document the GUEST signed.
func TestAnInstalledSinkIsListedBesideTheWebhookAndPostsAfterAScan(t *testing.T) {
	dataDir := t.TempDir()
	// The Admin's hand-placement, before the server ever starts.
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))

	receiver := newSinkReceiver(t)
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	view := readSinks(t, srv, token)
	if len(view.Sinks) != 2 {
		t.Fatalf("the screen lists %d sinks, want the Webhook Built-in and the Installed one: %+v", len(view.Sinks), view.Sinks)
	}
	builtin := sinkNamed(t, view, "webhook")
	if builtin.Installed {
		t.Fatal("the Webhook Built-in is reported as an Installed plugin")
	}
	installed := sinkNamed(t, view, "example-sink")
	if !installed.Installed {
		t.Fatalf("the hand-placed Plugin is not reported as Installed: %+v", installed)
	}
	if installed.Disabled || installed.LastError != "" {
		t.Fatalf("a Plugin that loaded cleanly reads as %+v, want no error", installed)
	}
	if installed.Name != "Test Sink (example-sink)" || installed.Version != "1.0.0" {
		t.Fatalf("the screen shows %+v, want the manifest's name and version", installed)
	}

	// Configured through the SAME endpoint, with the same body, as the Webhook.
	configureSink(t, srv, token, map[string]any{
		"slug":    "example-sink",
		"enabled": true,
		"secret":  "topsecret",
		"url":     receiver.srv.URL,
		"events":  []string{"scan.completed"},
	})

	scanLib(t, srv, token, libID, "")

	posts := receiver.waitForPosts(t, 1)
	if len(posts) != 1 {
		t.Fatalf("received %d documents for one scan, want exactly 1", len(posts))
	}
	post := posts[0]

	// Signed BY THE GUEST — the host never saw this body. The receiving script
	// recomputes the MAC over the raw bytes and would reject anything else.
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(post.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); post.Signature != want {
		t.Fatalf("signature = %q, want %q — a receiver would reject this document", post.Signature, want)
	}

	var ev struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Library struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"library"`
		Scan *struct {
			TitlesFound int `json:"titlesFound"`
		} `json:"scan"`
	}
	if err := json.Unmarshal(post.Body, &ev); err != nil {
		t.Fatalf("the body is not a sink event: %v\nbody: %s", err, post.Body)
	}
	if ev.Type != "scan.completed" || ev.Library.ID != libID || ev.Library.Name != "Movies" {
		t.Fatalf("document = %+v, want the scanned Library's scan.completed", ev)
	}
	if ev.Scan == nil {
		t.Fatalf("no scan block on a scan.completed document: %s", post.Body)
	}
	// An Installed plugin gets the same curated event a Built-in does, and no more:
	// ids, names and kinds, never a path.
	if strings.Contains(string(post.Body), srv.DataDir) || strings.Contains(string(post.Body), "rootFolders") {
		t.Fatalf("the event carried filesystem or catalog detail: %s", post.Body)
	}

	// The counters on the screen count a guest exactly as they count the Webhook.
	got := waitForSink(t, srv, token, "example-sink", func(s installedSinkResp) bool {
		return s.Counters.Delivered >= 1
	})
	if got.Counters.Failed != 0 || got.Disabled {
		t.Fatalf("after a clean delivery the sink reads %+v, want one delivery and no failures", got)
	}
}

// TestAnUnsupportedAPIVersionIsRefusedAndTheServerStillStarts: the ADR-0055 posture
// applied to a Plugin, observed where an Admin observes it. The server boots, the
// Webhook still works, and the refused Plugin is on the screen with a message
// naming which side has to move.
func TestAnUnsupportedAPIVersionIsRefusedAndTheServerStillStarts(t *testing.T) {
	dataDir := t.TempDir()
	m := plugintest.SinkManifest("future-sink")
	m.APIVersion = 2
	plugintest.Install(t, dataDir, m)

	receiver := newSinkReceiver(t)
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	refused := sinkNamed(t, readSinks(t, srv, token), "future-sink")
	if !refused.Disabled {
		t.Fatalf("a Plugin built for a contract this server does not speak reads as %+v, want it disabled", refused)
	}
	if !strings.Contains(refused.LastError, "upgrade the server") {
		t.Fatalf("lastError = %q, want it to name which side to upgrade", refused.LastError)
	}
	if !strings.Contains(refused.LastError, "v1") || !strings.Contains(refused.LastError, "v2") {
		t.Fatalf("lastError = %q, want both versions named", refused.LastError)
	}

	// And the server is otherwise entirely normal: the Webhook delivers.
	configureSink(t, srv, token, map[string]any{
		"slug": "webhook", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL, "events": []string{"scan.completed"},
	})
	scanLib(t, srv, token, libID, "")
	if got := receiver.waitForPosts(t, 1); len(got) != 1 {
		t.Fatalf("the Webhook delivered %d documents beside a refused Plugin, want 1", len(got))
	}
}

// TestAPanickingInstalledPluginLeavesEverythingElseAlone: the promise that makes an
// Installed plugin safe to ship. The guest fails on every call; boot completes, the
// Webhook keeps delivering, and after a run of failures the screen shows the Plugin
// disabled with its error.
func TestAPanickingInstalledPluginLeavesEverythingElseAlone(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("bad-sink"))

	good := newSinkReceiver(t)
	bad := newSinkReceiver(t)
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	configureSink(t, srv, token, map[string]any{
		"slug": "webhook", "enabled": true, "secret": "topsecret",
		"url": good.srv.URL, "events": []string{"scan.completed"},
	})
	configureSink(t, srv, token, map[string]any{
		"slug": "bad-sink", "enabled": true, "secret": "topsecret",
		"url": bad.srv.URL + "/?obelo-mode=panic", "events": []string{"scan.completed"},
	})

	for i := 0; i < 3; i++ {
		scanLib(t, srv, token, libID, "")
	}

	// The Built-in is untouched by the Plugin failing next to it.
	if got := good.waitForPosts(t, 3); len(got) < 3 {
		t.Fatalf("the Webhook delivered %d documents, want one per scan", len(got))
	}
	// And the Plugin is disabled, with a sentence.
	view := waitForSink(t, srv, token, "bad-sink", func(s installedSinkResp) bool { return s.Disabled })
	if view.LastError == "" {
		t.Fatal("a disabled Plugin with no last error tells an Admin nothing")
	}
	if view.Counters.Failed == 0 {
		t.Fatalf("the failed counter is 0 for a Plugin that failed every delivery: %+v", view)
	}
	// It is still ENABLED in settings — the Admin turned it on and this server
	// turned it off, which is exactly the state that needs explaining.
	if !view.Enabled {
		t.Fatalf("the Admin's enabled toggle was silently flipped: %+v", view)
	}
}

// TestAHangingInstalledPluginNeitherSlowsAScanNorSurvivesIt: a guest that never
// returns is stopped by the runtime. The scans finish at their normal speed —
// delivery is off the publish path — and the Plugin ends up disabled.
func TestAHangingInstalledPluginNeitherSlowsAScanNorSurvivesIt(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("slow-sink"))

	good := newSinkReceiver(t)
	hung := newSinkReceiver(t)
	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		// A short budget so three kills take a second rather than half a minute.
		// The mechanism is identical; only the number is smaller.
		testharness.WithPluginCallTimeout(400*time.Millisecond),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	configureSink(t, srv, token, map[string]any{
		"slug": "webhook", "enabled": true, "secret": "topsecret",
		"url": good.srv.URL, "events": []string{"scan.completed"},
	})
	configureSink(t, srv, token, map[string]any{
		"slug": "slow-sink", "enabled": true, "secret": "topsecret",
		"url": hung.srv.URL + "/?obelo-mode=hang", "events": []string{"scan.completed"},
	})

	start := time.Now()
	for i := 0; i < 3; i++ {
		scanLib(t, srv, token, libID, "")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("three scans took %v with a spinning Plugin attached — delivery is on the publish path", elapsed)
	}

	if got := good.waitForPosts(t, 3); len(got) < 3 {
		t.Fatalf("the Webhook delivered %d documents beside a spinning Plugin, want one per scan", len(got))
	}
	view := waitForSink(t, srv, token, "slow-sink", func(s installedSinkResp) bool { return s.Disabled })
	if view.LastError == "" {
		t.Fatal("a Plugin disabled for spinning must say so")
	}
}

// TestAnInstalledPluginThatBreaksItsAllowlistIsShownDisabled: the refusal an Admin
// sees. The guest reaches for a host its manifest never named; the host refuses it,
// the guest is told nothing but "refused", and the screen ends up naming the host
// it tried.
func TestAnInstalledPluginThatBreaksItsAllowlistIsShownDisabled(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("nosy-sink", "allowed.example.test"))

	receiver := newSinkReceiver(t)
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	configureSink(t, srv, token, map[string]any{
		"slug": "nosy-sink", "enabled": true, "secret": "topsecret",
		"url": receiver.srv.URL + "/?obelo-mode=forbidden", "events": []string{"scan.completed"},
	})
	for i := 0; i < 3; i++ {
		scanLib(t, srv, token, libID, "")
	}

	view := waitForSink(t, srv, token, "nosy-sink", func(s installedSinkResp) bool { return s.Disabled })
	if !strings.Contains(view.LastError, "not-allowed.example.test") {
		t.Fatalf("lastError = %q, want it to name the host the Plugin reached for", view.LastError)
	}
	// Nothing was sent to the host it was told it could not have, and nothing was
	// sent to its own target either — the delivery never got that far.
	if got := receiver.received(); len(got) != 0 {
		t.Fatalf("a Plugin whose fetch was refused still posted %d documents", len(got))
	}
}

// TestAServerWithNoInstalledPluginsIsUnchanged: the state every server is in today.
// The plugins directory does not exist, and the screen is the Webhook and nothing
// else.
func TestAServerWithNoInstalledPluginsIsUnchanged(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	view := readSinks(t, srv, token)
	if len(view.Sinks) != 1 || view.Sinks[0].Slug != "webhook" {
		t.Fatalf("sinks = %+v, want just the Webhook Built-in", view.Sinks)
	}
	if view.Sinks[0].Installed || view.Sinks[0].Disabled || view.Sinks[0].LastError != "" {
		t.Fatalf("the Webhook reads as %+v, want no Installed-plugin state at all", view.Sinks[0])
	}
}
