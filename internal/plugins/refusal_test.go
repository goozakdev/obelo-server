package plugins_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A guest that RAN TO COMPLETION and cleanly answered an error
// (.scratch/bundled-plugins, decision of 2026-09-18 on issue 08).
//
// Every other way a call can go wrong says the INSTANCE is unusable: a trap, a
// deadline kill, an instantiate failure, a response that is not the contract's
// shape. A `0` with a sentence in last_error() says something else entirely — the
// module was entered, it worked, it decided it could not answer this call, and it
// came back to say why. For a Metadata provider that is a claim about the SOURCE:
// a rejected key, a document it cannot parse. The item is parked (ADR-0048,
// non-transient) and the Plugin is left alone.
//
// The rule is per Extension point, which is why every test here comes in pairs:
// the same clean error that costs a provider nothing still costs a sink a strike,
// because a sink has no item to park and no second channel to say it down.
//
// The guest mode is `obelo-mode=refuse`, and the number in its sentence is the
// count of calls THAT INSTANCE has answered. It lives in the guest's linear
// memory, so a rebuilt instance starts again at 1 — which is how a test here
// proves an instance was kept rather than asserting it about a field it cannot
// see.

// refusalCount pulls the guest's own per-instance counter out of the sentence it
// answered with. A parse failure is the test's problem, not the Plugin's, so it
// fails loudly rather than returning zero.
func refusalCount(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		t.Fatal("the call answered; this guest refuses every call")
	}
	const marker = "refusal "
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		t.Fatalf("error = %q, want the guest's own sentence with its instance counter in it", err)
	}
	var n int
	if _, scanErr := fmt.Sscanf(err.Error()[i+len(marker):], "%d", &n); scanErr != nil {
		t.Fatalf("error = %q: cannot read the counter: %v", err, scanErr)
	}
	return n
}

// refusingProvider installs a metadata guest whose every lookup is a clean error
// and hands back the provider and the Set it lives in.
func refusingProvider(t *testing.T, log *logSink) (pluginapi.MetadataProvider, *plugins.Set) {
	t.Helper()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("refusing-source", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{})
	provider, _ := providerFor(t, set, "refusing-source",
		settingsFor("refuse", "http://unused.example.test", "http://unused.example.test"))
	return provider, set
}

// TestAMetadataGuestThatAnswersAnErrorIsNotStruck is the decision itself, and the
// number of lookups is deliberately well past the threshold: the old rule stopped
// the Plugin at three, so five that all reach the guest is the difference.
//
// Three things are asserted and all three matter. The caller still gets the error
// every time — the item must still park, exactly as the Built-in parked it. The
// Plugin is still enabled with no consecutive failures against it. And the guest's
// own counter runs 1, 2, 3, 4, 5, which is only possible if ONE instance answered
// all five: a rebuilt instance would have answered "refusal 1" each time.
func TestAMetadataGuestThatAnswersAnErrorIsNotStruck(t *testing.T) {
	const lookups = 5
	if lookups <= plugins.DefaultFailureThreshold {
		t.Fatalf("this test needs more than %d lookups to say anything", plugins.DefaultFailureThreshold)
	}
	log := &logSink{}
	provider, set := refusingProvider(t, log)

	for i := 1; i <= lookups; i++ {
		_, err := provider.Lookup(context.Background(), albumRef())
		if err == nil {
			t.Fatalf("lookup %d answered; this guest refuses every call", i)
		}
		// The guest's own words reach the caller unchanged, prefixed by the ABI's
		// sentence. That is what parks the item and what the operator reads.
		if !strings.Contains(err.Error(), "the guest refused the call") ||
			!strings.Contains(err.Error(), "status 401") {
			t.Fatalf("lookup %d error = %q, want the guest's refusal with its own detail", i, err)
		}
		if got := refusalCount(t, err); got != i {
			t.Fatalf("lookup %d was the guest's refusal number %d; a kept instance counts %d — "+
				"the host rebuilt the instance", i, got, i)
		}
	}

	s := set.Statuses()[0]
	if s.Disabled {
		t.Fatalf("status = %+v, want an ENABLED Plugin after %d clean errors: a rejected key is not "+
			"a broken module, and taking the provider off the server is the regression this fixed", s, lookups)
	}
	if strings.Contains(log.all(), "consecutive failures") {
		t.Errorf("a clean error was counted as a failure:\n%s", log.all())
	}
}

// TestACleanMetadataErrorSurfacesAsLastErrorAndIsRetiredByASuccess is the
// judgment call the decision left open, pinned in both directions.
//
// It SURFACES: `lastError` is the only place an Admin is told anything about a
// Plugin, and "status 401" is far more actionable than a silently parked movie.
// It is RETIRED by the next call that works, which a recorded failure's sentence
// is not: a failure is news about the Plugin and stays readable, a refusal is
// news about one call and a source that answered properly has settled it.
func TestACleanMetadataErrorSurfacesAsLastErrorAndIsRetiredByASuccess(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("moody-source", fullMusicProvides()))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})

	// The mode rides on the URL, so the two Settings are two different providers
	// over ONE Plugin — which is also the shape the enrichment Catalog builds.
	refusing, _ := providerFor(t, set, "moody-source",
		settingsFor("refuse", "http://unused.example.test", "http://unused.example.test"))
	working, _ := providerFor(t, set, "moody-source",
		pluginapi.Settings{Enabled: true, Secret: "a-key", URL: "http://unused.example.test/v1"})

	if _, err := refusing.Lookup(context.Background(), albumRef()); err == nil {
		t.Fatal("the refusing lookup answered")
	}
	s := set.Statuses()[0]
	if s.Disabled {
		t.Fatalf("status = %+v, want it still enabled", s)
	}
	if !strings.Contains(s.LastError, "401") {
		t.Fatalf("lastError = %q, want the guest's own sentence so an Admin can tell a rejected "+
			"credential from a broken module", s.LastError)
	}

	resp, err := working.Lookup(context.Background(), albumRef())
	if err != nil || resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("the working lookup = (%q, %v), want a match", resp.Outcome, err)
	}
	if s := set.Statuses()[0]; s.LastError != "" {
		t.Errorf("lastError = %q after a call that worked, want it retired", s.LastError)
	}
}

// TestACleanMetadataErrorLeavesTheFailureStreakAlone is the OTHER judgment call,
// and the conservative reading of it: a guest that ran and answered is neither a
// failure of the code (so it must not count) nor evidence that the code works (so
// it must not forgive a run of traps). Two deadline kills, one clean error, one
// more kill — and the third kill is still the third, so the Plugin stops.
//
// The alternative — a refusal clearing the streak — is what would let a Plugin
// that traps two calls out of three never reach the threshold at all.
func TestACleanMetadataErrorLeavesTheFailureStreakAlone(t *testing.T) {
	log := &logSink{}
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("half-broken", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{MetadataCallBudget: 400 * time.Millisecond})

	spinning, _ := providerFor(t, set, "half-broken",
		settingsFor("spin", "http://unused.example.test", "http://unused.example.test"))
	refusing, _ := providerFor(t, set, "half-broken",
		settingsFor("refuse", "http://unused.example.test", "http://unused.example.test"))

	for i := 1; i <= 2; i++ {
		if _, err := spinning.Lookup(context.Background(), albumRef()); err == nil {
			t.Fatalf("kill %d: the spinning guest answered", i)
		}
	}
	if s := set.Statuses()[0]; s.Disabled {
		t.Fatalf("status = %+v, want it still enabled after two of three kills", s)
	}

	if _, err := refusing.Lookup(context.Background(), albumRef()); err == nil {
		t.Fatal("the refusing lookup answered")
	}
	if s := set.Statuses()[0]; s.Disabled {
		t.Fatalf("status = %+v: a clean error DISABLED the Plugin", s)
	}

	if _, err := spinning.Lookup(context.Background(), albumRef()); err == nil {
		t.Fatal("kill 3: the spinning guest answered")
	}
	s := set.Statuses()[0]
	if !s.Disabled {
		t.Fatalf("status = %+v, want it disabled: a refusal between two kills is not a success "+
			"and must not forgive the streak", s)
	}
	if !log.contains(t, "is disabled after 3 consecutive failures") {
		t.Errorf("the operator was never told why:\n%s", log.all())
	}
}

// TestASpinningMetadataGuestIsStillKilledCountedAndDisabled keeps the other half
// of the rule honest next to the change. Nothing about a deadline kill moved: the
// instance is dropped, the failure is counted, three of them stop the Plugin.
//
// (hostfetch_test.go asserts the same thing from the fetch rules' side. It is
// repeated here because this file is where somebody widening the refusal rule
// will be reading, and the cost of the duplicate is one wall-clock second.)
func TestASpinningMetadataGuestIsStillKilledCountedAndDisabled(t *testing.T) {
	log := &logSink{}
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("still-spinning", fullMusicProvides()))
	set := loadWith(t, dataDir, log, plugins.Options{MetadataCallBudget: 300 * time.Millisecond})
	provider, _ := providerFor(t, set, "still-spinning",
		settingsFor("spin", "http://unused.example.test", "http://unused.example.test"))

	for i := 1; i <= plugins.DefaultFailureThreshold; i++ {
		if _, err := provider.Lookup(context.Background(), albumRef()); err == nil {
			t.Fatalf("lookup %d answered; a guest that never returns must be killed", i)
		}
	}
	s := set.Statuses()[0]
	if !s.Disabled {
		t.Fatalf("status = %+v, want it disabled after %d deadline kills", s, plugins.DefaultFailureThreshold)
	}
	if !log.contains(t, "is disabled after 3 consecutive failures") {
		t.Errorf("the operator was never told why:\n%s", log.all())
	}
}

// TestASinkGuestThatAnswersAnErrorIsStillStruck is the rule's boundary, and the
// reason it is drawn at the Extension point rather than at the ABI.
//
// A provider's clean error has somewhere to go: the item parks and the operator
// finds it on the attention list. A sink's has nowhere — there is no item, and
// the only record of a receiver that can never be written to is the Plugin's own
// status. So a sink's refusal stays a strike, the instance is still dropped, and
// three of them still stop the Plugin. UNCHANGED, and the per-instance counter
// reading 1 every time is the proof that the instance really was rebuilt.
func TestASinkGuestThatAnswersAnErrorIsStillStruck(t *testing.T) {
	log := &logSink{}
	dataDir := t.TempDir()
	target := newReceiver(t)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("refusing-sink"))

	set := load(t, dataDir, log)
	sink := sinkFor(t, set, "refusing-sink", pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL + "/?obelo-mode=refuse",
	})

	for i := 1; i <= plugins.DefaultFailureThreshold; i++ {
		err := sink.Deliver(context.Background(), scanEvent())
		if err == nil {
			t.Fatalf("delivery %d returned nil for a guest that refuses every call", i)
		}
		if got := refusalCount(t, err); got != 1 {
			t.Fatalf("delivery %d was the guest's refusal number %d, want 1: a sink's refusal "+
				"still drops the instance", i, got)
		}
	}

	st, _ := set.Status("refusing-sink")
	if !st.Disabled {
		t.Fatalf("status = %+v, want it disabled after %d clean errors — a sink's rules did not change",
			st, plugins.DefaultFailureThreshold)
	}
	if !strings.Contains(st.LastError, "cannot deliver") {
		t.Errorf("lastError = %q, want the guest's own sentence", st.LastError)
	}
	if err := sink.Deliver(context.Background(), scanEvent()); !errors.Is(err, plugins.ErrDisabled) {
		t.Fatalf("a delivery into a disabled Plugin = %v, want ErrDisabled", err)
	}
}
