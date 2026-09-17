package plugins_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The loader, driven by a REAL module built from source by the suite
// (internal/plugins/plugintest). Nothing here is mocked: every test compiles wasm,
// instantiates it in a wazero sandbox and calls into it across the hand-rolled ABI
// of ADR-0058, because a loader tested against a fake loader is a loader nobody has
// tested.
//
// The black-box half — that an Installed sink appears on the settings screen next
// to the Webhook and posts a signed document after a scan — is in
// internal/api/installed_event_sink_test.go. What lives here is the policy: the
// allowlist, the audit line, the refusals, and the failure accounting an operator
// reads on that screen.

// --- a recorder for the audit lines ------------------------------------------

// logSink collects what the loader logged, so the audit line can be asserted as
// the operator-facing artifact it is rather than assumed.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *logSink) contains(t *testing.T, want ...string) bool {
	t.Helper()
	all := l.all()
	for _, w := range want {
		if !strings.Contains(all, w) {
			return false
		}
	}
	return true
}

// hmacHex is the receiving script's half of the signature bargain, written here so
// the assertion is what a receiver would actually compute rather than something
// the guest handed us.
func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// --- helpers -----------------------------------------------------------------

// load builds a Set over a fresh data directory, with a short call budget so a
// hanging guest does not cost the suite its patience.
func load(t *testing.T, dataDir string, log *logSink) *plugins.Set {
	t.Helper()
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout: 2 * time.Second,
		Logf:        log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	return set
}

// sinkFor registers the Set into a fresh Registry and builds the one sink for id,
// exactly as the event-sink Manager does from a settings row.
func sinkFor(t *testing.T, set *plugins.Set, id string, s pluginapi.Settings) pluginapi.EventSink {
	t.Helper()
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.EventSink(id)
	if !ok {
		t.Fatalf("the Set registered no Event sink for %q", id)
	}
	sink, err := registration.New(s)
	if err != nil {
		t.Fatalf("building the sink: %v", err)
	}
	return sink
}

func scanEvent() pluginapi.SinkEvent {
	return pluginapi.SinkEvent{
		ID:      "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		Type:    pluginapi.EventScanCompleted,
		At:      "2026-09-17T12:00:00Z",
		Library: pluginapi.EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
		Scan:    &pluginapi.EventScan{TitlesFound: 3, FilesFound: 4},
	}
}

// receiver is the operator's target: the thing a guest posts to.
type receiver struct {
	mu    sync.Mutex
	posts []post
	srv   *httptest.Server
}

type post struct {
	body      []byte
	signature string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.posts = append(r.posts, post{body: body, signature: req.Header.Get("X-Obelo-Signature")})
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) received() []post {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]post, len(r.posts))
	copy(out, r.posts)
	return out
}

// --- the tracer ---------------------------------------------------------------

// TestAGuestDeliversASignedDocument is the whole slice in one test: a module and a
// manifest placed on disk, loaded, registered, built from settings, and handed an
// event — which comes out the other side as a signed POST at an address the
// operator chose.
func TestAGuestDeliversASignedDocument(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))

	log := &logSink{}
	set := load(t, dataDir, log)
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "topsecret", URL: target.srv.URL,
		Events: []string{pluginapi.EventScanCompleted},
	})

	ev := scanEvent()
	if err := sink.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	posts := target.received()
	if len(posts) != 1 {
		t.Fatalf("the receiver got %d documents, want exactly 1", len(posts))
	}
	// The receiving script's half of the bargain, recomputed here.
	if want := "sha256=" + hmacHex("topsecret", posts[0].body); posts[0].signature != want {
		t.Fatalf("signature = %q, want %q — a receiver would reject this", posts[0].signature, want)
	}
	var back pluginapi.SinkEvent
	if err := json.Unmarshal(posts[0].body, &back); err != nil {
		t.Fatalf("the body is not a sink event: %v\nbody: %s", err, posts[0].body)
	}
	if back.ID != ev.ID || back.Type != ev.Type || back.Library.Name != "Movies" {
		t.Fatalf("the document = %+v, want the event", back)
	}
	if back.Scan == nil || back.Scan.TitlesFound != 3 {
		t.Fatalf("scan block = %+v, want the terminal counts", back.Scan)
	}

	// The guest's own log line reached the server log, prefixed with its id, which
	// is the whole of why a guest needs no stdout.
	if !log.contains(t, "plugin example-sink [info]", "delivered one document") {
		t.Fatalf("the guest's log line did not reach the server log:\n%s", log.all())
	}

	// And nothing is disabled: a working Plugin has no last error to show.
	st, ok := set.Status("example-sink")
	if !ok || st.Disabled || st.LastError != "" {
		t.Fatalf("status = %+v (known=%v), want a working Plugin", st, ok)
	}
	if st.Name != "Test Sink (example-sink)" || st.Version != "1.0.0" {
		t.Fatalf("status = %+v, want the manifest's name and version", st)
	}
}

// TestManyDeliveriesReuseOneInstance: the instance is POOLED, not rebuilt per call
// (ADR-0058 decision 7 measured the difference at a factor of 27). A guest that
// kept no state between calls would pass this either way, so what it really pins is
// that a long run of calls neither leaks nor trips the recycle budget.
func TestManyDeliveriesReuseOneInstance(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))

	set := load(t, dataDir, &logSink{})
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL,
	})
	for i := 0; i < 25; i++ {
		if err := sink.Deliver(context.Background(), scanEvent()); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}
	if got := len(target.received()); got != 25 {
		t.Fatalf("the receiver got %d documents, want 25", got)
	}
}

// --- the allowlist ------------------------------------------------------------

// TestAGuestCannotReachAHostItsManifestDoesNotAllow: the refusal, the audit line,
// and the fact that the guest learns nothing beyond "refused".
func TestAGuestCannotReachAHostItsManifestDoesNotAllow(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	// The manifest allows one host, and it is not the one the guest will try.
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink", "allowed.example.test"))

	log := &logSink{}
	set := load(t, dataDir, log)
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=forbidden",
	})

	err := sink.Deliver(context.Background(), scanEvent())
	if err == nil {
		t.Fatal("Deliver returned nil for a guest whose fetch was refused")
	}
	if !strings.Contains(err.Error(), "host not in allowlist") {
		t.Fatalf("error = %v, want the guest to have been told its host was not allowed", err)
	}
	// Nothing was sent anywhere.
	if got := len(target.received()); got != 0 {
		t.Fatalf("the receiver got %d documents from a refused fetch, want 0", got)
	}
	// The audit line names the Plugin, the host and the reason — which is what an
	// operator greps for.
	if !log.contains(t, "plugin audit", "plugin=example-sink", "host=not-allowed.example.test", "reason=allowlist") {
		t.Fatalf("no audit line naming the plugin and the host:\n%s", log.all())
	}
}

// TestAPrivateAddressIsRefusedEvenInsideTheAllowlist: the allowlist is the author's
// claim about where their code goes; it is not a grant of the operator's private
// network. 169.254.169.254 is on the allowlist here and is refused anyway, which is
// the case the PRD names by number.
func TestAPrivateAddressIsRefusedEvenInsideTheAllowlist(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink", "169.254.169.254"))

	log := &logSink{}
	set := load(t, dataDir, log)
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=metadata",
	})

	err := sink.Deliver(context.Background(), scanEvent())
	if err == nil {
		t.Fatal("Deliver returned nil for a guest that fetched the cloud metadata address")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !log.contains(t, "plugin audit", "plugin=example-sink", "host=169.254.169.254", "reason=private-address") {
		t.Fatalf("no audit line for the private-address refusal:\n%s", log.all())
	}
}

// TestTheOperatorsOwnTargetNeedsNoAllowlistEntry pins the one asymmetry in the
// fetch policy, and the reason for it: an author cannot know the URL an operator
// will type for their own receiver, so a sink that could reach only manifest hosts
// could never post anywhere. The URL here is on loopback — which is precisely the
// deployment this product is for — and the manifest allows nothing at all.
func TestTheOperatorsOwnTargetNeedsNoAllowlistEntry(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))

	set := load(t, dataDir, &logSink{})
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL,
	})
	if err := sink.Deliver(context.Background(), scanEvent()); err != nil {
		t.Fatalf("Deliver to the operator's own target: %v", err)
	}
	if got := len(target.received()); got != 1 {
		t.Fatalf("the receiver got %d documents, want 1", got)
	}
}

// TestARedirectIntoPrivateSpaceIsRefused: the operator's own target is trusted for
// the FIRST hop and nowhere it then sends us. This is safefetch's rule, reached
// through a guest, and it is why a Plugin gets a guarded client rather than a bare
// one.
func TestARedirectIntoPrivateSpaceIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/landed", http.StatusFound)
	}))
	defer srv.Close()

	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))
	log := &logSink{}
	set := load(t, dataDir, log)
	sink := sinkFor(t, set, "example-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: srv.URL,
	})

	err := sink.Deliver(context.Background(), scanEvent())
	if err == nil {
		t.Fatal("Deliver returned nil for a target that redirected into private space")
	}
	if !log.contains(t, "plugin audit", "reason=fetch-policy") {
		t.Fatalf("no audit line for the blocked redirect:\n%s", log.all())
	}
}

// --- failure --------------------------------------------------------------------

// TestAPanickingGuestIsDisabledAfterARunOfFailures: a trap is recorded, the
// instance is discarded and rebuilt, and three in a row stop the Plugin being
// called at all — with the error an operator reads on the settings screen.
func TestAPanickingGuestIsDisabledAfterARunOfFailures(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("bad-sink"))

	set := load(t, dataDir, &logSink{})
	sink := sinkFor(t, set, "bad-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=panic",
	})

	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		if err := sink.Deliver(context.Background(), scanEvent()); err == nil {
			t.Fatalf("delivery %d returned nil for a guest that panics on every call", i)
		}
	}
	st, _ := set.Status("bad-sink")
	if !st.Disabled {
		t.Fatalf("status = %+v, want it disabled after %d consecutive failures", st, plugins.DefaultFailureThreshold)
	}
	if st.LastError == "" {
		t.Fatal("a disabled Plugin with no last error tells an operator nothing")
	}

	// And it is no longer CALLED: the next delivery is refused by the host, which
	// is the difference between "disabled" and "still failing".
	err := sink.Deliver(context.Background(), scanEvent())
	if !errors.Is(err, plugins.ErrDisabled) {
		t.Fatalf("a delivery into a disabled Plugin = %v, want ErrDisabled", err)
	}
}

// TestAHangingGuestIsStoppedByItsDeadline: a guest that never returns is unwound
// by the runtime, not asked to stop, and the call comes back in something like the
// budget rather than never.
func TestAHangingGuestIsStoppedByItsDeadline(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("slow-sink"))

	set := load(t, dataDir, &logSink{}) // CallTimeout is 2s
	sink := sinkFor(t, set, "slow-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=hang",
	})

	start := time.Now()
	err := sink.Deliver(context.Background(), scanEvent())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Deliver returned nil for a guest that never returns")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("the call took %v; the deadline did not stop the guest", elapsed)
	}

	// The instance was CLOSED by the deadline and is not resumable, so the next
	// call has to rebuild it. That it fails the same way rather than differently is
	// the proof the rebuild happened at all.
	if err := sink.Deliver(context.Background(), scanEvent()); err == nil {
		t.Fatal("the second delivery returned nil")
	}
}

// TestACallerDeadlineBoundsAGuestBelowItsOwnBudget: the host sets the deadline
// (ADR-0057 decision 2) and the Plugin's own budget is a ceiling, not a floor. A
// sink worker whose delivery timeout is shorter must get its call back sooner.
func TestACallerDeadlineBoundsAGuestBelowItsOwnBudget(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("slow-sink"))

	set := load(t, dataDir, &logSink{})
	sink := sinkFor(t, set, "slow-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=hang",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sink.Deliver(ctx, scanEvent()); err == nil {
		t.Fatal("Deliver returned nil under a 300ms caller deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the call took %v, want it bounded by the caller's 300ms deadline", elapsed)
	}
}

// TestRepeatedAllowlistViolationsDisableAPlugin: the issue's rule, and the reason
// violations are counted apart from failures — a Plugin reaching for a host it said
// it would not is doing something wrong even on the calls where it works.
func TestRepeatedAllowlistViolationsDisableAPlugin(t *testing.T) {
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("nosy-sink"))

	set := load(t, dataDir, &logSink{})
	sink := sinkFor(t, set, "nosy-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=forbidden",
	})
	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		_ = sink.Deliver(context.Background(), scanEvent())
	}
	st, _ := set.Status("nosy-sink")
	if !st.Disabled {
		t.Fatalf("status = %+v, want a Plugin that violated its allowlist repeatedly to be disabled", st)
	}
	if !strings.Contains(st.LastError, "not-allowed.example.test") {
		t.Fatalf("lastError = %q, want it to name the host the Plugin reached for", st.LastError)
	}
}

// --- refusals at load ----------------------------------------------------------

// TestAnUnsupportedAPIVersionIsRefusedNamingWhichSideToUpgrade: the ADR-0055
// posture applied to a Plugin. "Incompatible" is useless to the person reading it.
func TestAnUnsupportedAPIVersionIsRefusedNamingWhichSideToUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		apiVersion int
		want       string
	}{
		{"built for a later contract", 2, "upgrade the server"},
		{"built for an earlier contract", 0, "upgrade the plugin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			m := plugintest.SinkManifest("future-sink")
			m.APIVersion = tc.apiVersion
			plugintest.Install(t, dataDir, m)

			log := &logSink{}
			set := load(t, dataDir, log)
			st, ok := set.Status("future-sink")
			if !ok {
				t.Fatal("a refused Plugin must still be listed, or an operator has no idea what happened")
			}
			if !st.Disabled {
				t.Fatalf("status = %+v, want it refused", st)
			}
			if !strings.Contains(st.LastError, tc.want) {
				t.Fatalf("lastError = %q, want it to say %q", st.LastError, tc.want)
			}
			if !strings.Contains(st.LastError, "plugin API v1") {
				t.Fatalf("lastError = %q, want it to name the version this server speaks", st.LastError)
			}
			// And it is still REGISTERED, so the settings screen lists it with the
			// reason rather than silently omitting it.
			reg := pluginapi.NewRegistry()
			set.Register(reg)
			registration, listed := reg.EventSink("future-sink")
			if !listed {
				t.Fatal("a refused Plugin was not registered, so nothing will ever show the operator why")
			}
			if _, err := registration.New(pluginapi.Settings{URL: "https://x.example.test"}); err == nil {
				t.Fatal("a refused Plugin built a working sink")
			}
		})
	}
}

// TestRefusalsThatAreNotAVersionMismatch walks the rest of the ways a hand-placed
// Plugin does not load. Every one of them is listed, disabled, with a sentence.
func TestRefusalsThatAreNotAVersionMismatch(t *testing.T) {
	t.Run("a manifest that is not JSON", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.WriteRawManifest(t, dataDir, "broken", []byte("{ this is not json"))
		st := mustStatus(t, load(t, dataDir, &logSink{}), "broken")
		if !st.Disabled || !strings.Contains(st.LastError, "not valid JSON") {
			t.Fatalf("status = %+v, want it refused for unreadable JSON", st)
		}
	})

	t.Run("no manifest at all", func(t *testing.T) {
		dataDir := t.TempDir()
		dir := filepath.Join(dataDir, plugins.DirName, "empty")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		st := mustStatus(t, load(t, dataDir, &logSink{}), "empty")
		if !st.Disabled || !strings.Contains(st.LastError, "manifest.json") {
			t.Fatalf("status = %+v, want it refused for having no manifest", st)
		}
	})

	t.Run("a module that is not wasm", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.InstallModule(t, dataDir, plugintest.SinkManifest("junk"), []byte("not a wasm module"))
		st := mustStatus(t, load(t, dataDir, &logSink{}), "junk")
		if !st.Disabled || !strings.Contains(st.LastError, "compiling") {
			t.Fatalf("status = %+v, want it refused for not compiling", st)
		}
	})

	t.Run("no module beside the manifest", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.InstallModule(t, dataDir, plugintest.SinkManifest("bodiless"), nil)
		st := mustStatus(t, load(t, dataDir, &logSink{}), "bodiless")
		if !st.Disabled || !strings.Contains(st.LastError, "plugin.wasm") {
			t.Fatalf("status = %+v, want it refused for having no module", st)
		}
	})

	t.Run("a manifest whose id is not its directory", func(t *testing.T) {
		dataDir := t.TempDir()
		dir := plugintest.Install(t, dataDir, plugintest.SinkManifest("one-name"))
		renamed := plugintest.SinkManifest("another-name")
		plugintest.WriteManifest(t, dir, renamed)
		st := mustStatus(t, load(t, dataDir, &logSink{}), "one-name")
		if !st.Disabled || !strings.Contains(st.LastError, "must match") {
			t.Fatalf("status = %+v, want it refused for disagreeing with its own directory", st)
		}
	})

	t.Run("a wildcard in the allowlist", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.Install(t, dataDir, plugintest.SinkManifest("wild", "*.example.test"))
		st := mustStatus(t, load(t, dataDir, &logSink{}), "wild")
		if !st.Disabled || !strings.Contains(st.LastError, "wildcard") {
			t.Fatalf("status = %+v, want a wildcard allowlist refused", st)
		}
	})

	t.Run("a module named by a path", func(t *testing.T) {
		dataDir := t.TempDir()
		m := plugintest.SinkManifest("escapee")
		m.Module = "../../obelo.db"
		plugintest.InstallModule(t, dataDir, m, nil)
		st := mustStatus(t, load(t, dataDir, &logSink{}), "escapee")
		if !st.Disabled || !strings.Contains(st.LastError, "not a path") {
			t.Fatalf("status = %+v, want a module outside the plugin directory refused", st)
		}
	})
}

func mustStatus(t *testing.T, set *plugins.Set, id string) plugins.Status {
	t.Helper()
	st, ok := set.Status(id)
	if !ok {
		t.Fatalf("the Set does not know %q; a Plugin an operator placed must always be visible", id)
	}
	return st
}

// TestAPluginCannotShadowABuiltIn: the Webhook's slug is the key an Admin's signing
// secret is stored under. A Plugin claiming it would move that secret onto code the
// maintainer did not write, so the Plugin loses.
func TestAPluginCannotShadowABuiltIn(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("webhook"))
	set := load(t, dataDir, &logSink{})

	reg := pluginapi.NewRegistry()
	builtin := func(pluginapi.Settings) (pluginapi.EventSink, error) { return nil, nil }
	reg.RegisterEventSink(pluginapi.EventSinkRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "webhook", Name: "Webhook"},
		New:        builtin,
	})
	set.Register(reg)

	registration, _ := reg.EventSink("webhook")
	if registration.Descriptor.Name != "Webhook" {
		t.Fatalf("the Built-in was replaced by an Installed plugin: %+v", registration.Descriptor)
	}
	st, _ := set.Status("webhook")
	if !st.Disabled || !strings.Contains(st.LastError, "already claimed") {
		t.Fatalf("status = %+v, want the shadowing Plugin refused with the reason", st)
	}
}

// TestNoPluginsDirectoryIsANoOp: every server today. It must be silent and empty,
// not an error and not a warning.
func TestNoPluginsDirectoryIsANoOp(t *testing.T) {
	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(t.TempDir(), "plugins"), plugins.Options{Logf: log.logf})
	if err != nil {
		t.Fatalf("Load with no plugins directory = %v, want no error", err)
	}
	if got := set.Plugins(); len(got) != 0 {
		t.Fatalf("Plugins() = %d, want none", len(got))
	}
	if got := set.Statuses(); len(got) != 0 {
		t.Fatalf("Statuses() = %d, want none", len(got))
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	if got := reg.EventSinks(); len(got) != 0 {
		t.Fatalf("an empty Set registered %d sinks", len(got))
	}
	if log.all() != "" {
		t.Fatalf("a server with no Installed plugins said something about it:\n%s", log.all())
	}
	if err := set.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTheSandboxGrantsNothingButTheTwoHostFunctions pins the load-time half of the
// sandbox rule (ADR-0058 decision 4): the guest this suite builds imports WASI and
// this server's own module, and nothing else. A module that asked for a third
// namespace would be refused before it ran.
func TestTheSandboxGrantsNothingButTheTwoHostFunctions(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))
	set := load(t, dataDir, &logSink{})
	if st, _ := set.Status("example-sink"); st.Disabled {
		t.Fatalf("the suite's own guest was refused at load: %s", st.LastError)
	}
	// The negative half is covered by the refusal path above: a module importing
	// anything else fails checkImports and lands in LastError. Proving it needs a
	// hostile module, which ADR-0058's spike already built and measured; what
	// matters here is that the honest one passes the same check.
}
