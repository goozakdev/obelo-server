package plugins_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/useragent"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The three host fetch rules of ADR-0059 decisions 6 and 7
// (.scratch/bundled-plugins issue 02), driven by the real module the suite builds
// from source:
//
//  1. every fetch carries the HOST's User-Agent, exactly once, and a guest's own
//     is dropped;
//  2. a Metadata provider call has its own budget — thirty seconds by default,
//     raisable by a manifest up to a host cap — rather than the sink's ten;
//  3. a fetch ALWAYS returns before the call deadline, so a slow upstream is a
//     fetch error the guest answers "unavailable" to and NOT a deadline kill that
//     costs the Plugin a strike.
//
// Rule 3 is the one with teeth. Before it, a source slower than the call budget
// killed the guest mid-request, the kill was counted, and three slow lookups
// disabled a whole provider — ADR-0048's "a transient failure is retried, not
// parked", violated for a source instead of an item. What must still be counted
// is a guest that SPINS, and that is tested here beside it.
//
// # About the durations
//
// The acceptance criteria are written in real seconds: a stand-in answering after
// 4 s, a 30-second budget, a 10-second one. Everything but the defaults is scaled
// down by the same factor here (the stand-in waits 600 ms, the short budget is
// 1.8 s with a 300 ms grace), which preserves every ratio the criteria turn on —
// two fetches fit, the third does not — and costs the suite seconds rather than
// minutes. The DEFAULTS themselves are asserted, unscaled, in
// TestTheHostFetchDefaults below, and the long-budget case runs against the real
// default budget with no Options at all.

// guestAgent is the identity the test guest asks the host to send, copied from
// its source the way internal/api copies guestOverview: a stand-in that ever sees
// it is a stand-in looking at a bug.
const guestAgent = "guest-agent/9.9 ( nobody@example.test )"

// --- a stand-in for a slow, large or talkative source -------------------------

// standIn is an upstream that answers after a delay and records what it was told.
//
// It abandons its answer the moment the CALLER gives up (r.Context().Done()),
// which is not politeness: httptest.Server.Close waits for requests in flight, so
// a handler that slept through its full delay would make every timeout test cost
// the delay rather than the deadline.
//
// A request path of "/<n>" asks for an n-byte body, which is how the byte-cap
// tests ask for two mebibytes and then three.
type standIn struct {
	*httptest.Server

	mu       sync.Mutex
	requests int
	agents   [][]string
	guestHdr []string
}

func newStandIn(t *testing.T, delay time.Duration) *standIn {
	t.Helper()
	s := &standIn{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.agents = append(s.agents, r.Header.Values("User-Agent"))
		s.guestHdr = append(s.guestHdr, r.Header.Get("X-Guest-Header"))
		s.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		n := 0
		if raw := strings.TrimPrefix(r.URL.Path, "/"); raw != "" {
			n, _ = strconv.Atoi(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		if n > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(n))
			_, _ = w.Write(make([]byte, n))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) seen() (count int, agents [][]string, guestHdr []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, append([][]string(nil), s.agents...), append([]string(nil), s.guestHdr...)
}

// --- helpers -----------------------------------------------------------------

// The Set builder these tests use is subtitle_test.go's `loadWith`, because the
// budgets ARE the subject here and every one of them is an Option.

// fetchingProvider installs one Metadata provider whose guest runs mode against
// url2, and builds it. The operator's URL carries the mode, which is the one
// thing a test can set that reaches inside the sandbox; url2 is what the guest
// fetches.
func fetchingProvider(t *testing.T, id, mode, base, url2 string, opts plugins.Options, log *logSink) pluginapi.MetadataProvider {
	t.Helper()
	dataDir := t.TempDir()
	m := plugintest.MetadataProviderManifest(id, fullMusicProvides())
	plugintest.Install(t, dataDir, m)
	set := loadWith(t, dataDir, log, opts)
	provider, _ := providerFor(t, set, id, settingsFor(mode, base, url2))
	return provider
}

// settingsFor is the Settings the host resolves for a provider under test: the
// mode in the operator's URL, what to fetch in URL2.
//
// base is the stand-in's own address and not a decoration. The manifests here
// allowlist NOTHING, so the only host these guests may reach is the one the
// OPERATOR configured (ADR-0058 decision 5) — which is exactly the asymmetry a
// loopback stand-in needs, since an allowlisted 127.0.0.1 would be refused by the
// address rule instead.
func settingsFor(mode, base, url2 string) pluginapi.Settings {
	return pluginapi.Settings{
		Enabled: true,
		Secret:  "a-key",
		URL:     base + "/?obelo-mode=" + mode,
		URL2:    url2,
	}
}

// albumRef is a ref every kind-agnostic mode of the guest answers.
func albumRef() pluginapi.LookupRequest {
	return pluginapi.LookupRequest{Ref: pluginapi.MediaRef{Kind: "album", Title: "Doolittle"}}
}

// --- the defaults -------------------------------------------------------------

// TestTheHostFetchDefaults: the numbers ADR-0059 decision 6 names, asserted where
// they are written rather than only where they are scaled down.
//
// The tests below run with budgets of a second or two so the suite does not spend
// minutes proving a ratio. This is the test that says what the SHIPPED numbers
// are, so a future edit that quietly halves the metadata budget fails here
// instead of passing everywhere.
func TestTheHostFetchDefaults(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"metadata call budget", plugins.DefaultMetadataCallBudget, 30 * time.Second},
		{"host call-budget cap", plugins.DefaultMaxCallBudget, 2 * time.Minute},
		{"fetch grace", plugins.DefaultFetchGrace, time.Second},
		{"fetch byte cap", int64(plugins.DefaultMaxFetchBytes), int64(1 << 20)},
		{"host fetch byte cap", int64(plugins.DefaultMaxFetchBytesCap), int64(8 << 20)},
		{"sink/subtitle call timeout", plugins.DefaultCallTimeout, 10 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// --- rule 2: the metadata budget ----------------------------------------------

// TestAMetadataLookupHasRoomForSeveralSlowFetches: three fetches that each take
// 600 ms complete inside the DEFAULT metadata budget and the lookup answers
// matched — while CallTimeout, the sink's budget, is set to 200 ms.
//
// That second half is what makes this more than a slow test. If a Metadata
// provider call still ran under CallTimeout the guest would be killed before its
// first fetch came back, so the assertion is not merely "1.8 s fits in 30 s" but
// "a provider call does not take the sink's ten seconds — it has its own".
//
// (Scaled: the criterion says three fetches of 4 s inside the 30 s budget. The
// budget here IS the default; only the stand-in is faster.)
func TestAMetadataLookupHasRoomForSeveralSlowFetches(t *testing.T) {
	src := newStandIn(t, 600*time.Millisecond)
	log := &logSink{}
	provider := fetchingProvider(t, "slow-source", "fetch-slow", src.URL, src.URL,
		plugins.Options{CallTimeout: 200 * time.Millisecond}, log)

	started := time.Now()
	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("lookup: %v — a provider call must not run under the sink's CallTimeout", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q (%s), want matched", resp.Outcome, resp.Detail)
	}
	if elapsed := time.Since(started); elapsed < 1500*time.Millisecond {
		t.Errorf("the lookup answered in %s; three 600 ms fetches cannot have happened", elapsed)
	}
	if n, _, _ := src.seen(); n != 3 {
		t.Errorf("the stand-in served %d requests, want 3", n)
	}
}

// TestASlowSourceIsAFetchErrorAndNotADeadlineKill: the same guest under a budget
// too short for its third fetch gets a fetch ERROR on that third fetch, answers
// `unavailable`, and leaves the Plugin enabled with nothing against it.
//
// This is ADR-0059 decision 6's whole point. The item takes ADR-0048's backoff;
// the SOURCE is not parked for something the far end did.
//
// (Scaled from "three fetches of 4 s under a 10 s budget": 600 ms fetches under a
// 1.8 s budget with a 300 ms grace, so the fetch deadline lands at 1.5 s — after
// the second fetch and before the third, exactly as 9 s does in the criterion.)
func TestASlowSourceIsAFetchErrorAndNotADeadlineKill(t *testing.T) {
	src := newStandIn(t, 600*time.Millisecond)
	log := &logSink{}

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("slow-source", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{
		MetadataCallBudget: 1800 * time.Millisecond,
		FetchGrace:         300 * time.Millisecond,
	})
	provider, _ := providerFor(t, set, "slow-source", settingsFor("fetch-slow", src.URL, src.URL))

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("lookup: %v — a slow upstream must reach the guest as a fetch error, not kill it", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q (%s), want unavailable", resp.Outcome, resp.Detail)
	}
	if !strings.HasPrefix(resp.Detail, "fetch 3 failed:") {
		t.Errorf("detail = %q, want the THIRD fetch to be the one that ran out of budget", resp.Detail)
	}
	if !strings.Contains(resp.Detail, "deadline") {
		t.Errorf("detail = %q, want an error naming the deadline to the guest", resp.Detail)
	}

	// The Plugin is untouched: no failure, no last error, still enabled. A
	// deadline kill would have left all three the other way round.
	statuses := set.Statuses()
	if len(statuses) != 1 {
		t.Fatalf("Statuses() = %d rows, want 1", len(statuses))
	}
	if statuses[0].Disabled || statuses[0].LastError != "" {
		t.Errorf("status = %+v, want an enabled Plugin with nothing recorded against it", statuses[0])
	}
}

// TestASpentBudgetRefusesTheFetchUnsent: when the remaining budget minus the
// grace is already non-positive, the fetch answers the same error WITHOUT a
// request — and three such calls still do not disable the Plugin, though three
// counted failures would.
//
// The budget is 400 ms and the grace 500 ms, so there is never a moment at which
// a fetch may start. That is the degenerate end of the same rule and it is the
// cheapest possible proof that a slow source costs the Plugin nothing.
func TestASpentBudgetRefusesTheFetchUnsent(t *testing.T) {
	src := newStandIn(t, time.Hour) // never answers; nothing should ever ask it
	log := &logSink{}

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("spent-budget", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{
		MetadataCallBudget: 400 * time.Millisecond,
		FetchGrace:         500 * time.Millisecond,
	})
	provider, _ := providerFor(t, set, "spent-budget", settingsFor("fetch-slow", src.URL, src.URL))

	for i := 1; i <= 3; i++ {
		resp, err := provider.Lookup(context.Background(), albumRef())
		if err != nil {
			t.Fatalf("lookup %d: %v — the guest must be left to answer", i, err)
		}
		if resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Fatalf("lookup %d outcome = %q (%s), want unavailable", i, resp.Outcome, resp.Detail)
		}
		if !strings.HasPrefix(resp.Detail, "fetch 1 failed:") || !strings.Contains(resp.Detail, "deadline") {
			t.Errorf("lookup %d detail = %q, want the FIRST fetch refused with the deadline's sentence", i, resp.Detail)
		}
	}
	if n, _, _ := src.seen(); n != 0 {
		t.Errorf("the stand-in served %d requests; a spent budget must send none", n)
	}
	if s := set.Statuses()[0]; s.Disabled || s.LastError != "" {
		t.Errorf("status after three slow lookups = %+v, want an enabled Plugin — this is the ADR-0048 violation", s)
	}
}

// TestASpinningGuestIsStillKilledAndCounted: the other half of the rule. Now that
// a slow SOURCE never produces a deadline kill, a deadline kill means exactly one
// thing — the guest itself did not return — and that is still counted, still
// disables the Plugin after three, and still says so on the settings screen.
func TestASpinningGuestIsStillKilledAndCounted(t *testing.T) {
	log := &logSink{}
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("spinner", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{MetadataCallBudget: 400 * time.Millisecond})
	provider, _ := providerFor(t, set, "spinner", settingsFor("spin", "http://unused.example.test", "http://unused.example.test"))

	for i := 1; i <= 3; i++ {
		if _, err := provider.Lookup(context.Background(), albumRef()); err == nil {
			t.Fatalf("lookup %d answered; a guest that never returns must be killed", i)
		}
	}
	s := set.Statuses()[0]
	if !s.Disabled {
		t.Errorf("status = %+v, want a Plugin disabled after three deadline kills", s)
	}
	if !log.contains(t, "is disabled after 3 consecutive failures") {
		t.Errorf("the operator was never told why:\n%s", log.all())
	}
}

// --- rule 2b: what a manifest may raise ---------------------------------------

// TestAManifestRaisesItsBudgetAndItsByteCapUpToTheHostCap: a manifest's
// callBudgetMillis and maxFetchBytes are honoured up to the host's caps and
// CLAMPED above them, with a line at load naming both numbers — never refused.
func TestAManifestRaisesItsBudgetAndItsByteCapUpToTheHostCap(t *testing.T) {
	t.Run("a modest budget is honoured as declared", func(t *testing.T) {
		// 700 ms, well under the cap: the guest must be killed at 700 ms and not at
		// the 30-second default, which is how we know the declaration was read.
		log := &logSink{}
		dataDir := t.TempDir()
		m := plugintest.MetadataProviderManifest("modest-budget", fullMusicProvides())
		m.Provides[0].CallBudgetMillis = 700
		plugintest.Install(t, dataDir, m)
		set := loadWith(t, dataDir, log, plugins.Options{})
		provider, _ := providerFor(t, set, "modest-budget", settingsFor("spin", "http://unused.example.test", "http://unused.example.test"))

		started := time.Now()
		if _, err := provider.Lookup(context.Background(), albumRef()); err == nil {
			t.Fatal("the spinning guest answered")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the guest ran for %s; the manifest asked for 700 ms", elapsed)
		}
	})

	t.Run("a greedy budget is clamped and said out loud", func(t *testing.T) {
		log := &logSink{}
		dataDir := t.TempDir()
		m := plugintest.MetadataProviderManifest("greedy-budget", fullMusicProvides())
		m.Provides[0].CallBudgetMillis = 600000 // ten minutes
		m.Provides[0].MaxFetchBytes = 64 << 20  // sixty-four mebibytes
		plugintest.Install(t, dataDir, m)
		// Host caps small enough that the clamp is observable in the wall clock.
		set := loadWith(t, dataDir, log, plugins.Options{MaxCallBudget: 500 * time.Millisecond})
		provider, _ := providerFor(t, set, "greedy-budget", settingsFor("spin", "http://unused.example.test", "http://unused.example.test"))

		if !log.contains(t, "greedy-budget asks for a 10m0s call budget", "clamped") {
			t.Errorf("the clamp was silent:\n%s", log.all())
		}
		if !log.contains(t, "greedy-budget asks for a 67108864-byte fetch limit", "8388608") {
			t.Errorf("the byte clamp was silent:\n%s", log.all())
		}
		if set.Statuses()[0].Disabled {
			t.Fatal("an over-cap manifest was REFUSED; ADR-0059 says it is clamped")
		}

		started := time.Now()
		if _, err := provider.Lookup(context.Background(), albumRef()); err == nil {
			t.Fatal("the spinning guest answered")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the guest ran for %s; the cap is 500 ms, so the ten minutes it asked for were not honoured", elapsed)
		}
	})

	t.Run("a raised byte cap takes bodies up to it and refuses one over it, whole", func(t *testing.T) {
		src := newStandIn(t, 0)
		log := &logSink{}
		dataDir := t.TempDir()
		m := plugintest.MetadataProviderManifest("big-bodies", fullMusicProvides())
		m.Provides[0].MaxFetchBytes = 2 << 20 // two mebibytes, under the 8 MiB host cap
		plugintest.Install(t, dataDir, m)
		set := loadWith(t, dataDir, log, plugins.Options{})

		// Two mebibytes: the default 1 MiB limit would have refused this.
		fits, _ := providerFor(t, set, "big-bodies", settingsFor("fetch-once", src.URL, src.URL+"/2097152"))
		resp, err := fits.Lookup(context.Background(), albumRef())
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if resp.Detail != "bytes=2097152" {
			t.Errorf("a 2 MiB body came back as %q, want the whole thing", resp.Detail)
		}

		// Three: over the manifest's own limit, refused WHOLE rather than
		// truncated — a shortened document is one a guest parses as complete.
		over, _ := providerFor(t, set, "big-bodies", settingsFor("fetch-once", src.URL, src.URL+"/3145728"))
		resp, err = over.Lookup(context.Background(), albumRef())
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if !strings.HasPrefix(resp.Detail, "refused:") || !strings.Contains(resp.Detail, "larger than a plugin may receive") {
			t.Errorf("a 3 MiB body came back as %q, want a refusal", resp.Detail)
		}
		if !log.contains(t, "reason=oversize") {
			t.Errorf("an oversize fetch was not audited:\n%s", log.all())
		}
	})
}

// --- rule 1: the User-Agent ----------------------------------------------------

// TestEveryFetchCarriesTheHostsUserAgentExactlyOnce: the identity on the wire is
// the host's, it carries the build version and the Plugin's id and version, and
// there is exactly one of it whether or not the guest sent its own.
//
// The line this replaced did `Set` a hardcoded "obelo/1.0 (self-hosted; plugin
// x)" and then `Add`ed every guest header, so a Plugin that identified itself
// produced TWO User-Agent headers and a version that stopped existing at 0.1.0.
// MusicBrainz requires one specific shape and throttles anything else.
func TestEveryFetchCarriesTheHostsUserAgentExactlyOnce(t *testing.T) {
	src := newStandIn(t, 0)
	log := &logSink{}
	provider := fetchingProvider(t, "agent-source", "fetch-once", src.URL, src.URL, plugins.Options{}, log)

	for i := 0; i < 2; i++ {
		if _, err := provider.Lookup(context.Background(), albumRef()); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}

	count, agents, guestHdr := src.seen()
	if count != 2 {
		t.Fatalf("the stand-in served %d requests, want 2", count)
	}
	want := useragent.ForPlugin("agent-source", "1.0.0")
	for i, got := range agents {
		if len(got) != 1 {
			t.Fatalf("request %d carried %d User-Agent headers (%q), want exactly one", i, len(got), got)
		}
		if got[0] != want {
			t.Errorf("request %d agent = %q, want %q", i, got[0], want)
		}
		if got[0] == guestAgent || strings.Contains(got[0], "guest-agent") {
			t.Errorf("request %d carried the GUEST's agent: %q", i, got[0])
		}
		if !strings.Contains(got[0], server.Version) {
			t.Errorf("request %d agent = %q, want the build version %q", i, got[0], server.Version)
		}
		if !strings.Contains(got[0], "plugin/agent-source/1.0.0") {
			t.Errorf("request %d agent = %q, want the plugin id and version as a comment", i, got[0])
		}
	}
	// Every OTHER header the guest sent still travels: the agent is the one the
	// host owns, not the whole request.
	for i, got := range guestHdr {
		if got != "yes" {
			t.Errorf("request %d lost the guest's own header (X-Guest-Header = %q)", i, got)
		}
	}

	// The drop is a debug note to the AUTHOR, once per Plugin — not an audit line,
	// and not one per fetch: a provider that sets an agent sets it every time.
	if n := strings.Count(log.all(), "sent its own User-Agent"); n != 1 {
		t.Errorf("the dropped agent was logged %d times, want exactly 1:\n%s", n, log.all())
	}
	if strings.Contains(log.all(), "plugin audit: refused a fetch: plugin=agent-source") {
		t.Errorf("dropping a header was audited as a refusal:\n%s", log.all())
	}
}

// TestAGuestThatSendsNoAgentGetsTheHostsToo: the ordinary case, so the assertion
// above cannot be satisfied by a host that only acts when a guest misbehaves.
func TestAGuestThatSendsNoAgentGetsTheHostsToo(t *testing.T) {
	src := newStandIn(t, 0)
	log := &logSink{}
	provider := fetchingProvider(t, "quiet-source", "fetch-slow", src.URL, src.URL, plugins.Options{}, log)

	if _, err := provider.Lookup(context.Background(), albumRef()); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	count, agents, _ := src.seen()
	if count != 3 {
		t.Fatalf("the stand-in served %d requests, want 3", count)
	}
	want := useragent.ForPlugin("quiet-source", "1.0.0")
	for i, got := range agents {
		if len(got) != 1 || got[0] != want {
			t.Errorf("request %d agent = %q, want exactly one %q", i, got, want)
		}
	}
	if strings.Contains(log.all(), "sent its own User-Agent") {
		t.Errorf("a guest that sent no agent was told it had:\n%s", log.all())
	}
}
