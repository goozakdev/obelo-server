package pluginsdk_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/internal/testprovider"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The SDK's central claim, as a test (.scratch/bundled-plugins issue 03).
//
// testprovider.Provider is compiled into a WebAssembly module and driven through
// wazero by internal/plugins/sdk_guest_test.go and internal/api. HERE the SAME
// type is driven natively, in milliseconds, with no toolchain and no sandbox,
// because everything it can do goes through a pluginsdk.Host and sdktest.Host is
// one.
//
// That is the shape issues 04–07 port the seven bundled providers under: their
// existing tests keep their assertions and swap an httptest.Server for a
// sdktest.Host, and the module they ship is built from the same code.

// sdkTestSettings is the settings a host would resolve for this provider, with
// the mode marker that chooses which part it plays.
func sdkTestSettings(mode string) pluginapi.Settings {
	return pluginapi.Settings{
		Enabled:  true,
		Secret:   "an-operator-key",
		URL:      "https://source.example.test/v1?obelo-mode=" + mode,
		URL2:     "https://images.example.test",
		Language: "en-US",
	}
}

// TestTheSameProviderAnswersNativelyAgainstAnInMemoryHost is the three mandatory
// calls, with no network at all.
func TestTheSameProviderAnswersNativelyAgainstAnInMemoryHost(t *testing.T) {
	host := sdktest.New(sdktest.WithSettings(sdkTestSettings("")))
	p := testprovider.New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if resp.Record.Name != "Dune" || resp.Record.Overview != testprovider.Overview {
		t.Errorf("record = %q/%q", resp.Record.Name, resp.Record.Overview)
	}
	if len(resp.Record.Artwork) != 1 || resp.Record.Artwork[0].URL != "https://images.example.test"+testprovider.ArtworkPath {
		t.Errorf("artwork = %+v, want one poster on the configured image host", resp.Record.Artwork)
	}

	none, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "sandwich"},
	})
	if err != nil {
		t.Fatalf("Lookup of an unserved kind: %v", err)
	}
	if none.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("unserved kind = %q, want no-match", none.Outcome)
	}

	search, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "album", Query: "Kid A"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(search.Candidates) != 1 || search.Candidates[0].Title != "Kid A" {
		t.Errorf("candidates = %+v", search.Candidates)
	}

	art, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie"}, Role: "poster",
	})
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(art.Candidates) != 1 || art.Candidates[0].Width != 600 {
		t.Errorf("artwork candidates = %+v", art.Candidates)
	}

	// Nothing went out: the provider answered from what the Host told it.
	if len(host.Requests()) != 0 {
		t.Errorf("the provider fetched %v for a lookup it could answer without one", host.Paths())
	}
}

// sdk-sample:begin native-test

// TestAnInMemoryHostAnswersAFetchFromAnHTTPHandler is the port's ergonomics,
// asserted: an httptest handler becomes the source without a listener, and what
// the provider asked for is readable afterwards.
func TestAnInMemoryHostAnswersAFetchFromAnHTTPHandler(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(sdkTestSettings("fetch")),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Nothing") != "" {
				t.Errorf("unexpected header on %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"overview":"from the source itself"}`))
		}),
	)
	p := testprovider.New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Record.Overview != "from the source itself" {
		t.Fatalf("overview = %q, want the body the handler served", resp.Record.Overview)
	}
	if paths := host.Paths(); len(paths) != 1 || paths[0] != "/v1"+testprovider.FetchPath {
		t.Errorf("the provider asked for %v, want one GET of /v1%s", paths, testprovider.FetchPath)
	}
}

// sdk-sample:end native-test

// TestAnInMemoryHostCanRefuseLikeTheRealOne: a provider has to survive the host
// saying no, and proving that should not take a sandbox.
func TestAnInMemoryHostCanRefuseLikeTheRealOne(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(sdkTestSettings("fetch")),
		sdktest.WithAllowedHosts("somewhere-else.example.test"),
	)
	p := testprovider.New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", Title: "Dune"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v — a refusal is an answer, not a failure of the call", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
	}
	if resp.Detail == "" {
		t.Error("the provider reported no detail for a refusal an operator would want to read")
	}
	// And it said so in the log, which is where an operator finds it.
	if logs := host.Logs(); len(logs) != 1 || logs[0].Level != pluginsdk.LevelError {
		t.Errorf("logs = %+v, want one error line", logs)
	}
}

// TestAnInMemoryHostRoundTripsTheKeyValueNamespaceAndTheSettings covers the two
// host functions that are not a fetch.
func TestAnInMemoryHostRoundTripsTheKeyValueNamespaceAndTheSettings(t *testing.T) {
	host := sdktest.New(sdktest.WithSettings(sdkTestSettings("kv")))
	p := testprovider.New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "album", Title: "Kid A"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Record.Overview != "an-operator-key" {
		t.Errorf("the provider read back %q, want what it wrote", resp.Record.Overview)
	}
	if keys := host.KVKeys(); len(keys) != 1 || keys[0] != testprovider.KVKey {
		t.Errorf("kv keys = %v, want just the one the provider wrote", keys)
	}

	// The settings the host resolved, echoed by the provider and decoded here.
	s := sdkTestSettings("settings")
	ms := 1000
	s.RateLimitMillis = &ms
	host.SetSettings(s)
	resp, err = p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "album", Title: "Kid A"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	var echoed testprovider.EchoedSettings
	if err := json.Unmarshal([]byte(resp.Record.Overview), &echoed); err != nil {
		t.Fatalf("the provider did not echo its settings as JSON: %v", err)
	}
	if want := (testprovider.Echo(s)); echoed.URL != want.URL || echoed.Language != want.Language ||
		echoed.RateLimitMillis == nil || *echoed.RateLimitMillis != ms {
		t.Errorf("echoed = %+v, want %+v", echoed, want)
	}
	if !echoed.HasSecret {
		t.Error("the provider could not see the secret the host resolved for the call")
	}
}

// TestAnUnroutedFetchSaysSoRatherThanPanicking: a test that forgot to wire a route
// should fail with a sentence.
func TestAnUnroutedFetchSaysSoRatherThanPanicking(t *testing.T) {
	host := sdktest.New(sdktest.WithSettings(sdkTestSettings("fetch")))
	_, err := host.Fetch(context.Background(), pluginapi.FetchRequest{URL: "https://nowhere.example.test/x"})
	if err == nil {
		t.Fatal("an unrouted fetch answered without complaint")
	}
}
