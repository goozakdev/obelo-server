package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/sdkguesttest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The SDK-built guest, across the real ABI (.scratch/bundled-plugins issue 03).
//
// Everything here is already proved for the SDK-FREE guest in metadata_test.go.
// It is proved again for a module built with pluginsdk because the two modules
// share nothing: the SDK writes its own obelo_alloc, its own packed-i64 returns,
// its own //go:wasmexport names and its own host-function wrappers, and a bug in
// any of them is a bug in every plugin the maintainer ships after issue 04.
//
// What is NOT re-proved here is the contract's behaviour — the chain, the
// acceptance test, the settings screen. Those are the host's and they do not care
// which toolchain built the guest.

// The strings the SDK guest writes. They live in the provider's source
// (pluginsdk/internal/testprovider), which this module may not import because
// `internal` there means internal to the SDK — so they are restated here exactly
// as internal/api restates the SDK-free guest's. Nothing else in this server
// writes them, so a record carrying one came from the guest.
const (
	sdkGuestOverview    = "Filled from inside the sandbox by a plugin built with the Obelo Go SDK."
	sdkGuestSource      = "sdkguest"
	sdkGuestKVKey       = "sdk-guest-marker"
	sdkGuestArtworkPath = "/art/poster.jpg"
)

// sdkFullVideoProvides is the manifest entry of a Full video source that can lead
// and that DECLARES the external-ref capability its provider does not implement —
// the pairing this issue has to prove degrades rather than breaks.
func sdkFullVideoProvides() pluginapi.ManifestProvides {
	return pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindVideo, pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			pluginapi.CapabilityArtworkCandidates,
			pluginapi.CapabilityExternalRef,
		},
	}
}

// sdkInstall places the SDK guest and its manifest and returns the Settings the
// host would resolve for it.
func sdkInstall(t *testing.T, dataDir, id, mode string, allowedHosts ...string) pluginapi.Settings {
	t.Helper()
	m := sdkguesttest.Manifest(id, mode, sdkFullVideoProvides(), allowedHosts...)
	sdkguesttest.Install(t, dataDir, m)
	return pluginapi.Settings{
		Enabled: true,
		Secret:  "an-operator-key",
		URL:     m.Settings.DefaultURL,
		URL2:    m.Settings.DefaultURL2,
	}
}

// TestASDKBuiltGuestAnswersTheThreeMandatoryCalls is the first claim: a provider
// written as three Go methods against pluginsdk.Host, compiled to wasm by the
// ordinary command, answers lookup, search and artwork candidates across the
// sandbox with the contract's own shapes.
func TestASDKBuiltGuestAnswersTheThreeMandatoryCalls(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "")

	set := load(t, dataDir, &logSink{})
	provider, d := providerFor(t, set, "sdk-source", s)

	if d.Role != pluginapi.RoleAuthoritative || d.Class != pluginapi.ClassFull {
		t.Fatalf("role/class = %q/%q, want authoritative/full", d.Role, d.Class)
	}

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("lookup outcome = %q, want matched", resp.Outcome)
	}
	if resp.Record.Name != "Dune" || resp.Record.Overview != sdkGuestOverview {
		t.Errorf("record = %q/%q, want the ref's title and the SDK guest's overview",
			resp.Record.Name, resp.Record.Overview)
	}
	if resp.Record.Source != sdkGuestSource || resp.Record.ExternalID != sdkGuestSource+"-movie" {
		t.Errorf("record identity = %q/%q", resp.Record.Source, resp.Record.ExternalID)
	}
	// The artwork is a URL on the image host the operator configured. The HOST
	// downloads it; a plugin returns URLs and never bytes.
	if len(resp.Record.Artwork) != 1 || resp.Record.Artwork[0].URL != sdkguesttest.ImageHost+sdkGuestArtworkPath {
		t.Errorf("artwork = %+v, want one poster on the configured image host", resp.Record.Artwork)
	}

	// A kind this source does not serve is a settled no-match, not a guess.
	none, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "sandwich", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup of an unserved kind: %v", err)
	}
	if none.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("unserved kind outcome = %q, want no-match", none.Outcome)
	}

	search, err := provider.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "Dune"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if search.Outcome != pluginapi.OutcomeMatched || len(search.Candidates) != 1 {
		t.Fatalf("search = %q with %d candidates, want matched with one", search.Outcome, len(search.Candidates))
	}
	if search.Candidates[0].Title != "Dune" || search.Candidates[0].Kind != "movie" {
		t.Errorf("candidate = %+v, want the query echoed with its kind", search.Candidates[0])
	}

	art, err := provider.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", ExternalIDs: map[string]string{pluginapi.NamespaceTMDB: "1"}}, Role: "poster",
	})
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if art.Outcome != pluginapi.OutcomeMatched || len(art.Candidates) != 1 {
		t.Fatalf("artwork candidates = %q with %d, want matched with one", art.Outcome, len(art.Candidates))
	}
	if art.Candidates[0].Width != 600 || art.Candidates[0].Height != 900 {
		t.Errorf("candidate dimensions = %dx%d, want 600x900", art.Candidates[0].Width, art.Candidates[0].Height)
	}
}

// TestASDKBuiltGuestReadsTheSettingsTheHostResolved: settings_get, through the
// SDK's Host.Settings, inside one call. The guest writes what it saw into the
// record's overview, which is somewhere a test can read it.
func TestASDKBuiltGuestReadsTheSettingsTheHostResolved(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "settings")
	s.Language = "de-DE"
	limit := 250
	s.RateLimitMillis = &limit

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	var got struct {
		Enabled         bool   `json:"enabled"`
		HasSecret       bool   `json:"hasSecret"`
		URL             string `json:"url"`
		URL2            string `json:"url2"`
		Language        string `json:"language"`
		RateLimitMillis *int   `json:"rateLimitMillis"`
	}
	if err := json.Unmarshal([]byte(resp.Record.Overview), &got); err != nil {
		t.Fatalf("the guest did not echo its settings as JSON: %v\noverview: %s", err, resp.Record.Overview)
	}
	if !got.Enabled || !got.HasSecret {
		t.Errorf("the guest saw enabled=%v hasSecret=%v, want both true", got.Enabled, got.HasSecret)
	}
	if got.URL != s.URL || got.URL2 != s.URL2 {
		t.Errorf("the guest saw %q/%q, want the resolved URLs %q/%q", got.URL, got.URL2, s.URL, s.URL2)
	}
	if got.Language != "de-DE" {
		t.Errorf("the guest saw language %q, want de-DE", got.Language)
	}
	// The pointer is the point: absent and zero are different settings, and the
	// operator's 250 has to arrive as 250 rather than as a missing key.
	if got.RateLimitMillis == nil || *got.RateLimitMillis != 250 {
		t.Errorf("the guest saw rateLimitMillis %v, want 250", got.RateLimitMillis)
	}
}

// TestASDKBuiltGuestRoundTripsItsOwnKeyValueNamespace: kv_set and kv_get through
// the SDK's wrappers, landing in the store under the plugin's own id.
func TestASDKBuiltGuestRoundTripsItsOwnKeyValueNamespace(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "kv")

	kv := newMemKV()
	set := loadWithKV(t, dataDir, &logSink{}, kv)
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "album", Title: "Kid A", Artist: "Radiohead"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (%s), want matched", resp.Outcome, resp.Detail)
	}
	if resp.Record.Overview != s.Secret {
		t.Errorf("the guest read back %q, want the value it just wrote (%q)", resp.Record.Overview, s.Secret)
	}
	// The namespace is the host's doing, from the manifest on disk: the guest
	// never spells its own id and has no field to put one in.
	value, found, _ := kv.PluginKV("sdk-source", sdkGuestKVKey)
	if !found || string(value) != s.Secret {
		t.Errorf("the store holds %q/%v under the plugin's id, want the secret", value, found)
	}
}

// TestASDKBuiltGuestFetchesThroughTheHost: the only way out of the sandbox,
// reached through pluginsdk.Host.Fetch and the SDK's GetJSON helper.
//
// The target is the OPERATOR's own URL, which the host allows beside the manifest
// allowlist — an author cannot know which mirror an operator points at — so a
// local httptest server stands in for the source without the manifest naming it.
func TestASDKBuiltGuestFetchesThroughTheHost(t *testing.T) {
	var seen []string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "fetch")
	s.URL = source.URL + "?obelo-mode=fetch"

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "sdk-source", s)

	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (%s), want matched", resp.Outcome, resp.Detail)
	}
	if resp.Record.Overview != "from the source itself" {
		t.Errorf("overview = %q, want the body the source served", resp.Record.Overview)
	}
	if len(seen) != 1 || seen[0] != "/record" {
		t.Errorf("the source saw %v, want exactly one GET of /record", seen)
	}
}

// TestASDKBuiltGuestPacesFetchesByTheOperatorsInterval proves pluginsdk.Pacer
// actually paces INSIDE a real guest: pacing depends on the guest's clock and
// sleep being real, not the wazero defaults (a fake clock that advances 1 ms per
// read and a Nanosleep that returns immediately), so this is the one test in the
// suite that catches a host that forgot to enable them.
//
// The guest's own PacedHost wrapping (pluginsdk/testdata/guest/main.go) already
// paces every Fetch; this only has to make several fetches through one instance
// and watch the source's own clock, which the sandbox cannot fake.
func TestASDKBuiltGuestPacesFetchesByTheOperatorsInterval(t *testing.T) {
	const interval = 300 * time.Millisecond
	var requestTimes struct {
		times []time.Time
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestTimes.times = append(requestTimes.times, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "fetch")
	s.URL = source.URL + "?obelo-mode=fetch"
	limit := int(interval / time.Millisecond)
	s.RateLimitMillis = &limit

	set := load(t, dataDir, &logSink{})
	provider, _ := providerFor(t, set, "sdk-source", s)

	// Three calls through the SAME provider, hence the same guest instance
	// (ADR-0058 decision 7), so the pacer's state in the guest's linear memory
	// persists across them exactly as it would across three real lookups.
	const calls = 3
	for i := 0; i < calls; i++ {
		resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
		})
		if err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
		if resp.Outcome != pluginapi.OutcomeMatched {
			t.Fatalf("Lookup %d outcome = %q, want matched", i, resp.Outcome)
		}
	}
	if len(requestTimes.times) != calls {
		t.Fatalf("the source saw %d requests, want %d", len(requestTimes.times), calls)
	}

	// 20 ms of slack for scheduling jitter; the interval itself is 300 ms, so a
	// paced guest clears this by more than an order of magnitude and an unpaced
	// one (the wazero default) misses it by close to the whole interval.
	//
	// The upper bound proves the OTHER direction: a Pacer that overshoots its own
	// interval — say, one that measured its sleep against a clock the sandbox
	// never advanced past its last read — would pass the lower bound above by
	// waiting forever and never be caught by it alone.
	const tolerance = 20 * time.Millisecond
	const upperBound = interval + 200*time.Millisecond
	for i := 1; i < len(requestTimes.times); i++ {
		gap := requestTimes.times[i].Sub(requestTimes.times[i-1])
		if gap < interval-tolerance {
			t.Errorf("gap between request %d and %d = %v, want >= %v", i-1, i, gap, interval-tolerance)
		}
		if gap > upperBound {
			t.Errorf("gap between request %d and %d = %v, want < %v", i-1, i, gap, upperBound)
		}
	}
}

// TestAGuestThatIgnoresCtxIsStillCappedAtItsCallDeadline is ADR-0059 decision
// 6's HOST backstop, with the SDK's ctx deadline out of the picture entirely.
//
// The "blind-sleep" mode calls time.Sleep(3s) directly and never looks at a ctx —
// not even the one the SDK now builds — so the only thing that can keep this call
// from taking the full 3 seconds is wazero's own Nanosleep, capped at what is
// left of the call's deadline (internal/plugins/guest.go). Before that cap
// existed this test failed at very close to 3 seconds: wazero's
// WithCloseOnContextDone only unwinds a guest that RE-ENTERS wasm, not one
// blocked inside a real time.Sleep, so the deadline kill happened late rather
// than not at all.
//
// It also asserts the SHAPE of what comes back: the backstop still ends the
// call with a deadline kill (unlike the SDK-side seam the pacing test below
// proves), and that costs a strike — FailureThreshold 1 here so ONE kill is
// enough to
// observe it — because a guest that ignored ctx and had to be capped is not the
// same fact as one that answered cleanly.
func TestAGuestThatIgnoresCtxIsStillCappedAtItsCallDeadline(t *testing.T) {
	const budget = 1 * time.Second
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "blind-sleep")

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:        2 * time.Second,
		MetadataCallBudget: budget,
		FailureThreshold:   1,
		Logf:               log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider, _ := providerFor(t, set, "sdk-source", s)

	start := time.Now()
	_, lookupErr := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	elapsed := time.Since(start)

	const tolerance = 300 * time.Millisecond
	if elapsed >= budget+tolerance {
		t.Errorf("Lookup took %v against a %v budget, want < %v — the guest's Nanosleep "+
			"was not capped at the call deadline", elapsed, budget, budget+tolerance)
	}
	if lookupErr == nil {
		t.Fatal("Lookup returned nil for a guest capped mid-sleep, want a deadline kill")
	}
	if !strings.Contains(lookupErr.Error(), "deadline") {
		t.Errorf("Lookup error = %q, want it to name a deadline kill", lookupErr.Error())
	}
	if !set.Plugins()[0].Disabled() {
		t.Error("the plugin was not disabled after one deadline kill (FailureThreshold 1) — the kill did not count as a strike")
	}
}

// TestASDKBuiltGuestAnswersUnavailableInsteadOfBeingKilledByItsOwnPacing is
// ADR-0059 decision 6's SDK-side ctx deadline, proved by the same shape as the
// test above — a RateLimitMillis pace that cannot fit inside the call budget —
// but this time the guest's own Pacer honours ctx, so it notices first and
// answers "unavailable" rather than being killed.
//
// A ctx-blind guest (the test above) would still be saved from taking longer
// than its budget by the host's own cap; what this test proves is the seam above that
// backstop — a well-behaved plugin gets an ANSWER, not a strike, and the instance
// is kept.
func TestASDKBuiltGuestAnswersUnavailableInsteadOfBeingKilledByItsOwnPacing(t *testing.T) {
	// Bigger than DefaultFetchGrace (1 s): a budget that left no fetch window at
	// all would fail the FIRST Lookup for a reason that has nothing to do with
	// pacing.
	const budget = 3 * time.Second
	// A ctx-timed-out Wait never marks the pacer's last-fetch time (Pacer.Wait
	// returns before that line), so each failed attempt below still leaves the
	// NEXT one racing against the first call's timestamp — and real time keeps
	// passing while this loop runs. The interval has to clear everything four
	// attempts at roughly one call-window (~2.25 s here) apiece could burn
	// through, or a later attempt's wait would already be satisfied by elapsed
	// wall time alone and it would answer matched rather than unavailable.
	const rateLimit = 60 * time.Second

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "fetch")
	s.URL = source.URL + "?obelo-mode=fetch"
	limit := int(rateLimit / time.Millisecond)
	s.RateLimitMillis = &limit

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:        2 * time.Second,
		MetadataCallBudget: budget,
		Logf:               log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider, _ := providerFor(t, set, "sdk-source", s)

	// The first Lookup reaches the source normally — nothing to pace against yet
	// — and marks the pacer's last-fetch time, which is all this test needs from
	// it.
	first, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil || first.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("first Lookup = %v/%q (%s), want a clean match to seed the pacer", err, first.Outcome, first.Detail)
	}

	const tolerance = 300 * time.Millisecond
	for i := 0; i < 4; i++ {
		start := time.Now()
		resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Lookup %d: %v (want a clean answer, not a call error)", i, err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("Lookup %d outcome = %q, want unavailable — the pace could not fit the budget", i, resp.Outcome)
		}
		if elapsed >= budget+tolerance {
			t.Errorf("Lookup %d took %v against a %v budget, want < %v", i, elapsed, budget, budget+tolerance)
		}
	}

	if set.Plugins()[0].Disabled() {
		t.Errorf("the plugin was disabled; an unavailable answer must not count as a failure")
	}
}

// TestASDKBuiltGuestAnswersUnavailableWhenTheCallersOwnDeadlineIsShorterThanTheBudget
// is ADR-0059 decision 6: Settings.CallRemainingMillis carries the time
// REMAINING until the deadline callGuestUnder is actually enforcing — min(the
// caller's own ctx deadline, the seam's nominal budget) — not that nominal
// budget itself.
//
// The metadata budget here is 30 s, but the CALLER's own ctx (an operator's
// "Test connection", say) allows only 1.5 s. A guest told the nominal 30 s would
// build a ctx a margin short of 30 s, never notice its Pacer.Wait cannot fit
// before the HOST's real 1.5 s deadline arrives, and be deadline-killed — a
// strike — instead of answering. Told the truth, it notices at ~1.5 s and
// answers "unavailable" cleanly, well inside the caller's own deadline.
func TestASDKBuiltGuestAnswersUnavailableWhenTheCallersOwnDeadlineIsShorterThanTheBudget(t *testing.T) {
	const callerDeadline = 1500 * time.Millisecond
	const metadataBudget = 30 * time.Second
	const rateLimit = 5 * time.Second

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "fetch")
	s.URL = source.URL + "?obelo-mode=fetch"
	limit := int(rateLimit / time.Millisecond)
	s.RateLimitMillis = &limit

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:        2 * time.Second,
		MetadataCallBudget: metadataBudget,
		Logf:               log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider, _ := providerFor(t, set, "sdk-source", s)

	// Seed the pacer with a plain, unbounded call.
	first, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil || first.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("first Lookup = %v/%q (%s), want a clean match to seed the pacer", err, first.Outcome, first.Detail)
	}

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	resp, err := provider.Lookup(ctx, pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Lookup: %v (want a clean answer, not a call error — a deadline kill means the guest "+
			"was told the wrong budget)", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable — the pace could not fit the caller's own deadline", resp.Outcome)
	}
	if elapsed >= callerDeadline {
		t.Errorf("Lookup took %v, want less than the caller's own %v deadline", elapsed, callerDeadline)
	}

	if set.Plugins()[0].Disabled() {
		t.Errorf("the plugin was disabled; an unavailable answer must not count as a failure")
	}
}

// TestASDKBuiltGuestSubtitleSeamAnswersUnavailableWhenTheCallersOwnDeadlineIsShorterThanTheBudget
// is the test above's shape at the Subtitle provider seam: the same
// ADR-0059 decision 6 fix has to hold for every seam that stamps
// CallRemainingMillis, not only metadata_lookup — the bug this proves against
// (OpenSubtitles' "Test connection" against a 10 s ctx and the host's own
// larger nominal budget) is a Subtitle provider call, not a metadata one.
func TestASDKBuiltGuestSubtitleSeamAnswersUnavailableWhenTheCallersOwnDeadlineIsShorterThanTheBudget(t *testing.T) {
	const callerDeadline = 1500 * time.Millisecond
	const subtitleBudget = 30 * time.Second
	const rateLimit = 5 * time.Second

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"id":"1","language":"en"}]}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	m := sdkguesttest.SubtitleManifest("sdk-subs")
	sdkguesttest.Install(t, dataDir, m)

	limit := int(rateLimit / time.Millisecond)
	s := pluginapi.Settings{
		Enabled:         true,
		Secret:          "an-operator-key",
		URL:             source.URL,
		RateLimitMillis: &limit,
	}

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout: subtitleBudget, // the subtitle seam's own budget IS CallTimeout
		Logf:        log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider := subtitleProviderFor(t, set, "sdk-subs", s)

	// Seed the pacer with a plain, unbounded search.
	first, err := provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	})
	if err != nil || first.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("first SearchSubtitles = %v/%q (%s), want a clean match to seed the pacer", err, first.Outcome, first.Detail)
	}

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	resp, err := provider.SearchSubtitles(ctx, pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SearchSubtitles: %v (want a clean answer, not a call error — a deadline kill means the "+
			"guest was told the wrong budget)", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable — the pace could not fit the caller's own deadline", resp.Outcome)
	}
	if elapsed >= callerDeadline {
		t.Errorf("SearchSubtitles took %v, want less than the caller's own %v deadline", elapsed, callerDeadline)
	}

	if set.Plugins()[0].Disabled() {
		t.Errorf("the plugin was disabled; an unavailable answer must not count as a failure")
	}
}

// TestASubtitleCallQueuedBehindAnotherIsToldTheRemainingTimeAfterTheWait is the
// lock-contention shape of the test above: a SECOND SearchSubtitles call is
// queued behind a FIRST one still running on the SAME Plugin instance (one
// instance, one callMu, ADR-0058 decision 7), so the remaining time the host
// stamps on the second call's Settings has to be read AFTER callGuestUnder has
// taken that lock and built its own bounded ctx, not before — the seam's
// nominal budget computed ahead of the queue is a number the wait has already
// made false.
//
// The first call is unbounded and, seeded by pacing, sleeps ~5s inside callMu,
// holding it the whole time. The second call's own ctx allows 8s from the
// moment it is ISSUED, so by the time it clears the queue only ~3s of it is
// left. Told that truth, its own pacer notices its wait cannot fit and answers
// unavailable, cleanly, well inside its 8s. Told the stale ~8s the queue has
// already spent, it does not notice in time and is deadline-killed instead — a
// strike, with FailureThreshold 1 here so ONE kill is enough to observe it.
func TestASubtitleCallQueuedBehindAnotherIsToldTheRemainingTimeAfterTheWait(t *testing.T) {
	const callerDeadline = 8 * time.Second
	const subtitleBudget = 30 * time.Second
	const rateLimit = 5 * time.Second

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"id":"1","language":"en"}]}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	m := sdkguesttest.SubtitleManifest("sdk-subs-queue")
	sdkguesttest.Install(t, dataDir, m)

	limit := int(rateLimit / time.Millisecond)
	s := pluginapi.Settings{
		Enabled:         true,
		Secret:          "an-operator-key",
		URL:             source.URL,
		RateLimitMillis: &limit,
	}

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:      subtitleBudget, // the subtitle seam's own budget IS CallTimeout
		FailureThreshold: 1,
		Logf:             log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider := subtitleProviderFor(t, set, "sdk-subs-queue", s)

	// Seed the pacer with a plain, unbounded search.
	if _, err := provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	}); err != nil {
		t.Fatalf("seeding SearchSubtitles: %v", err)
	}

	// The FIRST call: unbounded, and the pace since the seed above makes it
	// sleep ~5s inside callMu, holding it the whole time.
	holding := make(chan struct{})
	go func() {
		close(holding)
		_, _ = provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
			Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
		})
	}()
	<-holding
	// A short, deterministic head start: an idle lock is taken in microseconds,
	// so this is enough for the goroutine above to be the one holding callMu
	// (about to for ~5s), without meaningfully shortening the queue this test
	// measures.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	resp, err := provider.SearchSubtitles(ctx, pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SearchSubtitles: %v (want a clean answer, not a call error — a deadline kill means "+
			"the guest was told the seam's nominal budget instead of what the queue left it)", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable — the pace could not fit what was left of the caller's "+
			"own %v deadline after queueing", resp.Outcome, callerDeadline)
	}
	if elapsed >= callerDeadline {
		t.Errorf("SearchSubtitles took %v, want less than the caller's own %v deadline", elapsed, callerDeadline)
	}
	if set.Plugins()[0].Disabled() {
		t.Error("the plugin was disabled after one queued call — a deadline kill means it was told the " +
			"stale, pre-queue remaining time rather than what the wait actually left it")
	}
}

// TestASinkDeliveryQueuedBehindAnotherIsToldTheRemainingTimeAfterTheWait is the
// Event sink seam's version of the test above: the same lock-contention shape,
// proving the fix at the seam whose request carries Settings directly (sink.go)
// rather than through settings_get, and against an SDK-built sink
// (testprovider.Sink) so the mutation of pluginsdk/sink/exports_wasm.go's
// CallContext argument to a constant nil — dropping Settings.CallRemainingMillis
// entirely — makes it fail: with no deadline at all the guest's own pace no
// longer notices anything, and only the host's real deadline can still stop it,
// which is a kill either way.
func TestASinkDeliveryQueuedBehindAnotherIsToldTheRemainingTimeAfterTheWait(t *testing.T) {
	const callerDeadline = 8 * time.Second
	const callTimeout = 30 * time.Second
	const rateLimit = 5 * time.Second

	dataDir := t.TempDir()
	m := sdkguesttest.SinkManifest("sdk-sink-queue")
	sdkguesttest.Install(t, dataDir, m)

	limit := int(rateLimit / time.Millisecond)
	s := pluginapi.Settings{Enabled: true, Secret: "an-operator-key", RateLimitMillis: &limit}

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:      callTimeout,
		FailureThreshold: 1,
		Logf:             log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	sink := sinkFor(t, set, "sdk-sink-queue", s)

	// Seed the pacer with a plain, unbounded delivery.
	if err := sink.Deliver(context.Background(), scanEvent()); err != nil {
		t.Fatalf("seeding Deliver: %v", err)
	}

	// The FIRST delivery: unbounded, and the pace since the seed above makes it
	// sleep ~5s inside callMu, holding it the whole time.
	holding := make(chan struct{})
	go func() {
		close(holding)
		_ = sink.Deliver(context.Background(), scanEvent())
	}()
	<-holding
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	err = sink.Deliver(ctx, scanEvent())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Deliver returned nil — the pace could not fit what was left of the caller's own " +
			"deadline after queueing, so this delivery must report it undelivered")
	}
	if elapsed >= callerDeadline {
		t.Errorf("Deliver took %v, want less than the caller's own %v deadline", elapsed, callerDeadline)
	}
	if set.Plugins()[0].Disabled() {
		t.Error("the plugin was disabled after one queued delivery — a deadline kill means it was told " +
			"the stale, pre-queue remaining time rather than what the wait actually left it")
	}
}

// TestAQueuedCallWithAnAlreadyExpiredCallerDeadlineIsNeverInvoked is ADR-0059
// decision 6: a call whose own caller ctx has already run out while it sat
// waiting for callMu must never reach the guest at all — not be invoked and
// deadline-killed, and not be counted as a failure. The lock-contention shape
// is TestASubtitleCallQueuedBehindAnotherIsToldTheRemainingTimeAfterTheWait's;
// what is new is the caller's own deadline (3s) being SHORTER than the ~5s the
// first call holds callMu for, so by the time this call finally gets the lock
// its own ctx is already done, rather than merely short once it starts.
//
// The source server's request count is a sanity check only — it distinguishes
// "the guest ran" from "the guest never ran", nothing more. What actually proves
// the Plugin's instance (and its Pacer, which lives inside it) was kept rather
// than closed and dropped is call C below: it must still be paced against A's
// own last fetch, which only a surviving Pacer remembers.
func TestAQueuedCallWithAnAlreadyExpiredCallerDeadlineIsNeverInvoked(t *testing.T) {
	const callerDeadline = 3 * time.Second
	const subtitleBudget = 30 * time.Second
	const rateLimit = 5 * time.Second

	var hits int32
	var fetchMu sync.Mutex
	var fetchTimes []time.Time
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fetchMu.Lock()
		fetchTimes = append(fetchTimes, time.Now())
		fetchMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"id":"1","language":"en"}]}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	m := sdkguesttest.SubtitleManifest("sdk-subs-expired")
	sdkguesttest.Install(t, dataDir, m)

	limit := int(rateLimit / time.Millisecond)
	s := pluginapi.Settings{
		Enabled:         true,
		Secret:          "an-operator-key",
		URL:             source.URL,
		RateLimitMillis: &limit,
	}

	log := &logSink{}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), plugins.Options{
		CallTimeout:      subtitleBudget,
		FailureThreshold: 1,
		Logf:             log.logf,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	provider := subtitleProviderFor(t, set, "sdk-subs-expired", s)

	// Seed the pacer with a plain, unbounded search.
	if _, err := provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	}); err != nil {
		t.Fatalf("seeding SearchSubtitles: %v", err)
	}

	// The FIRST call: unbounded, and the pace since the seed above makes it sleep
	// ~5s inside callMu, holding it the whole time. aDone closes once it has
	// released the lock, so the assertion below can tell "queued behind it" from
	// "raced it".
	aDone := make(chan struct{})
	holding := make(chan struct{})
	go func() {
		close(holding)
		_, _ = provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
			Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
		})
		close(aDone)
	}()
	<-holding
	// A short, deterministic head start: an idle lock is taken in microseconds, so
	// this is enough for the goroutine above to be the one holding callMu, without
	// meaningfully shortening the queue this test measures.
	time.Sleep(100 * time.Millisecond)
	hitsBeforeB := atomic.LoadInt32(&hits)

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	_, err = provider.SearchSubtitles(ctx, pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	})
	elapsed := time.Since(start)
	select {
	case <-aDone:
	default:
		t.Fatal("the queued call returned before the one holding callMu released it — it raced the lock " +
			"instead of queuing behind it, so this run proves nothing")
	}
	if err == nil {
		t.Fatal("SearchSubtitles returned nil for a call whose own 3s deadline had already passed while it " +
			"queued behind the ~5s holder")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded — the caller's own timeout, not a trap "+
			"or a runtime deadline kill", err)
	}
	if elapsed < callerDeadline {
		t.Errorf("SearchSubtitles took only %v, want at least the caller's own %v deadline — it must have "+
			"waited for callMu, not returned immediately", elapsed, callerDeadline)
	}
	hitsAfterB := atomic.LoadInt32(&hits)
	if hitsAfterB != hitsBeforeB+1 {
		t.Errorf("the source saw %d requests while this call was queued and %d after it returned, want "+
			"exactly one more (the holder's own, not this call's) — sanity check only, see call C below "+
			"for the actual proof that the guest was never invoked for B",
			hitsBeforeB, hitsAfterB)
	}
	if set.Plugins()[0].Disabled() {
		t.Error("the plugin was disabled after one call queued past its own deadline — a caller's timeout " +
			"is not the plugin's fault and must not count as a failure")
	}

	// Call C, immediately after B returns: a call on the same Plugin with a
	// generous deadline. FailureThreshold is 1, so it also re-proves no strike
	// was recorded for B — any strike would already have disabled the Plugin and
	// this call would fail outright. What it adds is proof the INSTANCE, and the
	// Pacer living inside it, survived B: C's own fetch must still be paced at
	// least rateLimit after A's last fetch. A dropped-and-rebuilt instance gets a
	// fresh Pacer with no memory of A, and would fetch at once instead.
	fetchMu.Lock()
	aFetch := fetchTimes[len(fetchTimes)-1]
	countBeforeC := len(fetchTimes)
	fetchMu.Unlock()

	if _, err := provider.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Dune"}, Language: "en",
	}); err != nil {
		t.Fatalf("call C after the queued one failed: %v", err)
	}

	fetchMu.Lock()
	defer fetchMu.Unlock()
	if len(fetchTimes) != countBeforeC+1 {
		t.Fatalf("call C did not reach the source (fetch count %d, want %d) — it must fetch, not just answer "+
			"from a nil/empty result", len(fetchTimes), countBeforeC+1)
	}
	cFetch := fetchTimes[len(fetchTimes)-1]
	if gap := cFetch.Sub(aFetch); gap < rateLimit-time.Second {
		t.Errorf("call C's fetch arrived only %v after A's last fetch, want at least the %v operator "+
			"interval — the instance (and its Pacer) must have been kept across the call queued past its "+
			"own deadline, not closed and dropped", gap, rateLimit)
	}
}

// TestASDKBuiltGuestAnswersUnavailableForACapabilityItDoesNotImplement is the
// degradation ADR-0057 decision 3 asks for, at the SDK's seam rather than the
// host's.
//
// The manifest DECLARES external-ref, so the host makes the call — it consults
// the declaration, not the module. The served provider implements no
// ExternalRefParser, so the SDK's dispatcher answers "unavailable", which is the
// same known state an undeclared capability produces and is NOT an error: nothing
// is counted against the plugin and the host is free to read the paste itself.
func TestASDKBuiltGuestAnswersUnavailableForACapabilityItDoesNotImplement(t *testing.T) {
	dataDir := t.TempDir()
	s := sdkInstall(t, dataDir, "sdk-source", "")

	log := &logSink{}
	set := load(t, dataDir, log)
	provider, d := providerFor(t, set, "sdk-source", s)

	if !d.HasCapability(pluginapi.CapabilityExternalRef) {
		t.Fatalf("the manifest did not declare external-ref, so this test proves nothing")
	}
	parser, ok := provider.(pluginapi.ExternalRefParser)
	if !ok {
		t.Fatalf("an Installed provider must implement every optional interface unconditionally")
	}
	resp, err := parser.ParseExternalRef(context.Background(), pluginapi.ExternalRefRequest{
		Kind: "movie", Pasted: "https://sdk-source.example.test/movie/1",
	})
	if err != nil {
		t.Fatalf("ParseExternalRef: %v — an unimplemented capability is a known state, not a failure", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
	if !strings.Contains(resp.Detail, "external-ref") {
		t.Errorf("detail = %q, want it to name the capability the manifest over-claimed", resp.Detail)
	}

	// The same for an optional call whose capability was never declared at all:
	// the host would not make it, but nothing about the module breaks if it does.
	lister, ok := provider.(pluginapi.EpisodeLister)
	if !ok {
		t.Fatalf("an Installed provider must implement every optional interface unconditionally")
	}
	seasons, err := lister.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "1"})
	if err != nil {
		t.Fatalf("SeriesSeasons: %v", err)
	}
	if seasons.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("seasons outcome = %q, want unavailable", seasons.Outcome)
	}
}
