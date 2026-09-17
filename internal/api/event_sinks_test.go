package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Event sink Extension point (ADR-0057 decision 6,
// .scratch/plugin-system issue 05). Everything here is what an Admin can actually
// observe: a settings response, a document arriving at their own HTTP server, a
// signature their own script could verify, and a scan that is no slower for having
// a dead webhook attached.
//
// The receiver is an httptest server standing in for the operator's target — the
// pattern the key-rotation, relay and provider-image tests already use for
// outbound calls.

// --- wire shapes the Admin sees ---------------------------------------------

type eventSinkResp struct {
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	RequiresSecret bool     `json:"requiresSecret"`
	Enabled        bool     `json:"enabled"`
	HasSecret      bool     `json:"hasSecret"`
	URL            string   `json:"url"`
	Events         []string `json:"events"`
	Description    string   `json:"description"`
	DocsURL        string   `json:"docsURL"`
}

type eventSinksResp struct {
	Sinks           []eventSinkResp `json:"sinks"`
	AvailableEvents []string        `json:"availableEvents"`
}

// --- the receiver -----------------------------------------------------------

// sinkReceiver is the operator's target: an HTTP server that records every signed
// document posted to it, exactly as a receiving script would.
type sinkReceiver struct {
	mu    sync.Mutex
	posts []sinkPost
	srv   *httptest.Server
}

type sinkPost struct {
	Body      []byte
	Signature string
	EventType string
	EventID   string
}

func newSinkReceiver(t *testing.T) *sinkReceiver {
	t.Helper()
	r := &sinkReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.posts = append(r.posts, sinkPost{
			Body:      body,
			Signature: req.Header.Get("X-Obelo-Signature"),
			EventType: req.Header.Get("X-Obelo-Event"),
			EventID:   req.Header.Get("X-Obelo-Event-Id"),
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *sinkReceiver) received() []sinkPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sinkPost, len(r.posts))
	copy(out, r.posts)
	return out
}

// waitForPosts polls until at least n documents have arrived, or fails.
func (r *sinkReceiver) waitForPosts(t *testing.T, n int) []sinkPost {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := r.received()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d documents, want at least %d", len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// verifySignature is the receiving script's half of the bargain: recompute
// HMAC-SHA256 over the RAW BODY under the shared secret and compare.
func verifySignature(t *testing.T, secret string, post sinkPost) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(post.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if post.Signature != want {
		t.Fatalf("signature = %q, want %q — a receiver would reject this document",
			post.Signature, want)
	}
}

// configureSink PUTs one sink's settings and returns the masked view.
func configureSink(t *testing.T, srv *testharness.Server, token string, update map[string]any) eventSinksResp {
	t.Helper()
	var resp eventSinksResp
	body := map[string]any{"sinks": []map[string]any{update}}
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/event-sinks", token, body, &resp)
	if status != http.StatusOK {
		t.Fatalf("PUT event-sinks status = %d, want 200; body: %s", status, raw)
	}
	return resp
}

// --- settings ---------------------------------------------------------------

// TestEventSinkSettingsRoundTrip: the settings API round-trips the
// subscribed-events list, masks the signing secret, and offers exactly the event
// types this server can actually derive.
func TestEventSinkSettingsRoundTrip(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var initial eventSinksResp
	status, body := srv.AuthGET("/api/v1/settings/event-sinks", token, &initial)
	if status != http.StatusOK {
		t.Fatalf("GET event-sinks status = %d, want 200; body: %s", status, body)
	}
	if len(initial.Sinks) != 1 || initial.Sinks[0].Slug != "webhook" {
		t.Fatalf("sinks = %+v, want the one registered Webhook Built-in", initial.Sinks)
	}
	if !initial.Sinks[0].RequiresSecret {
		t.Fatal("the Webhook must require a secret — an unsigned POST is untrustworthy")
	}
	if initial.Sinks[0].Enabled || initial.Sinks[0].HasSecret || len(initial.Sinks[0].Events) != 0 {
		t.Fatalf("an unconfigured sink reads as %+v, want off with nothing on file", initial.Sinks[0])
	}
	// The only event the translator derives today is offered; promising an Admin
	// one that nothing produces would be worse than not offering it.
	if len(initial.AvailableEvents) != 1 || initial.AvailableEvents[0] != "scan.completed" {
		t.Fatalf("availableEvents = %v, want [scan.completed]", initial.AvailableEvents)
	}

	saved := configureSink(t, srv, token, map[string]any{
		"slug":    "webhook",
		"enabled": true,
		"secret":  "topsecret",
		"url":     "https://automation.example.test/obelo",
		"events":  []string{"scan.completed"},
	})
	got := saved.Sinks[0]
	if !got.Enabled || !got.HasSecret || got.URL != "https://automation.example.test/obelo" {
		t.Fatalf("saved sink = %+v, want it on with the target recorded", got)
	}
	if len(got.Events) != 1 || got.Events[0] != "scan.completed" {
		t.Fatalf("events = %v, want [scan.completed]", got.Events)
	}

	// The secret is never returned, in any field.
	if raw, _ := json.Marshal(saved); strings.Contains(string(raw), "topsecret") {
		t.Fatalf("the response leaked the signing secret: %s", raw)
	}

	// A re-read (a fresh process-level read of the row) says the same thing.
	var reread eventSinksResp
	if status, body := srv.AuthGET("/api/v1/settings/event-sinks", token, &reread); status != http.StatusOK {
		t.Fatalf("re-read status = %d; body: %s", status, body)
	}
	if len(reread.Sinks[0].Events) != 1 || reread.Sinks[0].Events[0] != "scan.completed" {
		t.Fatalf("events after re-read = %v, want [scan.completed]", reread.Sinks[0].Events)
	}

	// A partial update that omits the secret leaves it on file (omit = unchanged).
	narrowed := configureSink(t, srv, token, map[string]any{
		"slug":   "webhook",
		"events": []string{},
	})
	if !narrowed.Sinks[0].HasSecret {
		t.Fatal("omitting the secret cleared it — omit must mean unchanged")
	}
	if len(narrowed.Sinks[0].Events) != 0 {
		t.Fatalf("events = %v, want an empty subscription", narrowed.Sinks[0].Events)
	}
}

// TestEventSinkSettingsRefusals: the four things an Admin cannot save, each
// refused 422 with a code they can act on.
func TestEventSinkSettingsRefusals(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	cases := []struct {
		name   string
		update map[string]any
		code   string
	}{
		{
			name:   "unknown sink",
			update: map[string]any{"slug": "discord", "enabled": false},
			code:   "PROVIDER_UNKNOWN",
		},
		{
			name: "an event this server does not emit",
			update: map[string]any{
				"slug": "webhook", "events": []string{"playback.started"},
			},
			code: "PROVIDER_INVALID_SETTING",
		},
		{
			name: "enabled with no target",
			update: map[string]any{
				"slug": "webhook", "enabled": true, "secret": "s",
				"events": []string{"scan.completed"},
			},
			code: "PROVIDER_INVALID_SETTING",
		},
		{
			name: "enabled with no signing secret",
			update: map[string]any{
				"slug": "webhook", "enabled": true, "url": "https://x.example.test/hook",
				"events": []string{"scan.completed"},
			},
			code: "PROVIDER_KEY_REQUIRED",
		},
		{
			name:   "a target that is not an http URL",
			update: map[string]any{"slug": "webhook", "url": "file:///etc/passwd"},
			code:   "PROVIDER_INVALID_BASE_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errResp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			body := map[string]any{"sinks": []map[string]any{tc.update}}
			status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/event-sinks", token, body, &errResp)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", status, raw)
			}
			if errResp.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q; body: %s", errResp.Error.Code, tc.code, raw)
			}
		})
	}

	// And nothing was persisted by any of them (validation is all-or-nothing).
	var view eventSinksResp
	srv.AuthGET("/api/v1/settings/event-sinks", token, &view)
	if view.Sinks[0].Enabled || view.Sinks[0].HasSecret || view.Sinks[0].URL != "" {
		t.Fatalf("a refused save left state behind: %+v", view.Sinks[0])
	}
}

// TestEventSinkSettingsAdminOnly: a sink is an operator concern like every other
// Plugin setting, so a Member cannot see or change it.
func TestEventSinkSettingsAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	memberTok := srv.LoginAs("kid", "memberpass123")

	if status, _ := srv.AuthGET("/api/v1/settings/event-sinks", memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("member GET status = %d, want 403", status)
	}
	body := map[string]any{"sinks": []map[string]any{{"slug": "webhook", "enabled": false}}}
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/event-sinks", memberTok, body, nil)
	if status != http.StatusForbidden {
		t.Fatalf("member PUT status = %d, want 403; body: %s", status, raw)
	}
}

// --- delivery ---------------------------------------------------------------

// TestWebhookSinkReceivesOneSignedPostPerScan is the tracer: an Admin enters a URL
// and a secret, subscribes to scan.completed, runs a scan, and their own HTTP
// server receives exactly one signed document naming the Library that finished.
func TestWebhookSinkReceivesOneSignedPostPerScan(t *testing.T) {
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	configureSink(t, srv, token, map[string]any{
		"slug":    "webhook",
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
	verifySignature(t, "topsecret", post)

	if post.EventType != "scan.completed" {
		t.Fatalf("X-Obelo-Event = %q, want scan.completed", post.EventType)
	}
	if post.EventID == "" {
		t.Fatal("X-Obelo-Event-Id is empty — a receiver cannot deduplicate")
	}

	var ev struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		At      string `json:"at"`
		Library struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"library"`
		Scan *struct {
			TitlesFound int `json:"titlesFound"`
			FilesFound  int `json:"filesFound"`
		} `json:"scan"`
	}
	if err := json.Unmarshal(post.Body, &ev); err != nil {
		t.Fatalf("body is not a sink event: %v\nbody: %s", err, post.Body)
	}
	if ev.ID != post.EventID {
		t.Fatalf("body id %q and header id %q disagree", ev.ID, post.EventID)
	}
	if ev.Type != "scan.completed" {
		t.Fatalf("type = %q, want scan.completed", ev.Type)
	}
	if _, err := time.Parse(time.RFC3339, ev.At); err != nil {
		t.Fatalf("at = %q, not RFC 3339: %v", ev.At, err)
	}
	if ev.Library.ID != libID || ev.Library.Name != "Movies" || ev.Library.Kind != "movie" {
		t.Fatalf("library = %+v, want the scanned Library named and kinded", ev.Library)
	}
	if ev.Scan == nil {
		t.Fatalf("no scan block on a scan.completed event; body: %s", post.Body)
	}
	// Ids, names and kinds only — never a catalog row. Nothing about a path or a
	// root folder may ever reach a sink.
	if strings.Contains(string(post.Body), "rootFolders") || strings.Contains(string(post.Body), srv.DataDir) {
		t.Fatalf("the event carried catalog/filesystem detail: %s", post.Body)
	}
}

// TestUnsubscribedSinkReceivesNothing: a configured, enabled sink that did not
// subscribe to scan.completed receives nothing at all. scan.completed is the only
// event this slice derives, so "not in its subscribed list" is an empty list —
// which is also the state issue 06 will widen into a real choice.
func TestUnsubscribedSinkReceivesNothing(t *testing.T) {
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	configureSink(t, srv, token, map[string]any{
		"slug":    "webhook",
		"enabled": true,
		"secret":  "topsecret",
		"url":     receiver.srv.URL,
		"events":  []string{},
	})

	scanLib(t, srv, token, libID, "")

	// Generous: a delivery that was going to happen would have happened by now.
	time.Sleep(500 * time.Millisecond)
	if got := receiver.received(); len(got) != 0 {
		t.Fatalf("received %d documents for an unsubscribed sink, want 0", len(got))
	}
}

// TestDeadWebhookDoesNotSlowAScan: the promise that makes an Event sink safe to
// ship. The sink is pointed at a target that accepts the connection and NEVER
// answers; the scan must settle in nothing like the sink's own deadline, because
// delivery is off the publish path entirely.
//
// The bound is the assertion: the host gives one delivery 15 seconds
// (eventsink.DeliveryTimeout). A scan that waited on the sink could not possibly
// finish inside the four seconds asserted here.
func TestDeadWebhookDoesNotSlowAScan(t *testing.T) {
	hang := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	// Release the blocked handlers BEFORE shutting the server down: httptest.Close
	// waits for outstanding handlers.
	defer target.Close()
	defer close(hang)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	// Baseline: how long this scan takes with no sink configured at all.
	baselineStart := time.Now()
	scanLib(t, srv, token, libID, "")
	baseline := time.Since(baselineStart)

	configureSink(t, srv, token, map[string]any{
		"slug":    "webhook",
		"enabled": true,
		"secret":  "topsecret",
		"url":     target.URL,
		"events":  []string{"scan.completed"},
	})

	withSinkStart := time.Now()
	scanLib(t, srv, token, libID, "")
	withSink := time.Since(withSinkStart)

	if withSink > 4*time.Second {
		t.Fatalf("scan with a dead webhook took %v (baseline %v) — delivery is on the publish path",
			withSink, baseline)
	}
}

// TestQueuedEventsDoNotSurviveARestart documents the intended best-effort
// behavior end to end: the sink's target is down while the scan finishes, the
// server is shut down, and a fresh server over the SAME data directory delivers
// nothing from before. Sink delivery is in memory on purpose — a sink author who
// knows that builds something that tolerates a gap (PRD story 46).
func TestQueuedEventsDoNotSurviveARestart(t *testing.T) {
	dataDir := t.TempDir()
	receiver := newSinkReceiver(t)

	// First boot: the sink is configured to post somewhere that refuses the
	// connection, so the event is produced, attempted and lost.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening there any more

	first := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, first)
	libID := createMovieLibrary(t, first, token, t.TempDir())
	configureSink(t, first, token, map[string]any{
		"slug":    "webhook",
		"enabled": true,
		"secret":  "topsecret",
		"url":     deadURL,
		"events":  []string{"scan.completed"},
	})
	scanLib(t, first, token, libID, "")
	first.Close()

	// Second boot over the same install, now with a receiver that WOULD accept
	// anything replayed. The settings survived; the queued event did not.
	second := testharness.New(t, testharness.WithDataDir(dataDir))
	token2 := second.LoginAs("brandon", "hunter2hunter2")
	view := configureSink(t, second, token2, map[string]any{
		"slug": "webhook",
		"url":  receiver.srv.URL,
	})
	if !view.Sinks[0].Enabled || !view.Sinks[0].HasSecret {
		t.Fatalf("the sink's SETTINGS must survive a restart: %+v", view.Sinks[0])
	}

	time.Sleep(500 * time.Millisecond)
	if got := receiver.received(); len(got) != 0 {
		t.Fatalf("received %d documents after a restart, want 0 — delivery is best-effort", len(got))
	}
}
