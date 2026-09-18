package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// MusicBrainz as a Bundled plugin, end to end (.scratch/bundled-plugins issue 06).
//
// Every link in what these two tests drive lives in a different package: the real
// musicbrainz WebAssembly module, inside wazero, fetching a stand-in through the
// host's guarded fetcher; the SDK classifying what comes back; the guest answering
// the contract; the host adapter turning that answer into an enrichment outcome;
// and the pass deciding what to write. Only a black-box test crosses all of them.

// --- a source too slow to answer inside its budget -----------------------------

// A SLOW SOURCE RETRIES THE ITEMS AND DOES NOT STRIKE THE PLUGIN.
//
// This is ADR-0059 decision 6 and ADR-0048 for the case the 503 ladder does not
// cover: the source is not refusing, it is simply not answering in time. The host
// cuts the fetch when the call's budget runs out and hands the guest
// `FetchResponse{Error: …}`; the guest must read that as `unavailable` and NOT as a
// Go error, because a Go error from a guest is a STRIKE and three consecutive ones
// disable the plugin. A music library on a bad afternoon would otherwise take
// MusicBrainz off the server.
//
// DURATIONS ARE SCALED, and the ratio is what the assertion is about. The issue
// states the case as "a stand-in that answers every request after 12 seconds",
// against the 30-second budget a Metadata call gets by default — a source taking
// more of the call than the call has. This plugin asks for 90 seconds (it paces
// itself at one request a second and one album search legitimately makes a dozen),
// so the faithful figure here would be a stand-in slower than that, and the test
// would take minutes to say one thing. It is run at 1/150th instead: a 600ms budget
// leaving a 100ms grace, and a source that answers after 700ms. The RELATIONSHIP is
// identical — the source outlives the room the budget leaves — and the shipped
// numbers are asserted literally in internal/plugins:TestTheHostFetchDefaults and
// in plugins/musicbrainz/musicbrainz (callBudget beside the manifest's figure).
func TestASlowMusicSourceRetriesTheItemsAndDoesNotStrikeThePlugin(t *testing.T) {
	requireFixtures(t)

	const answerAfter = 700 * time.Millisecond
	var calls int32
	var mu sync.Mutex
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(answerAfter)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[],"release-groups":[],"recordings":[]}`))
	}))
	defer slow.Close()

	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithPluginMetadataCallBudget(600*time.Millisecond, 100*time.Millisecond),
	)
	token := adminToken(t, srv)
	// Point the SHIPPED plugin at the stand-in, both hosts. The operator's own base
	// URL is reachable beside the manifest allowlist, which is what lets a bundled
	// plugin be driven against a loopback server at all. Pacing OFF, because this
	// test is about the budget and one request a second would be a second assertion.
	putProviders(t, srv, token, map[string]any{
		"musicBrainzRateLimitMs": 0,
		"providers": []map[string]any{
			{"slug": "musicbrainz", "enabled": true, "baseURL": slow.URL, "imageBaseURL": slow.URL},
		},
	}, http.StatusOK)

	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")

	res := enrichLib(t, srv, token, libID, "full")
	if res.Total == 0 {
		t.Skip("no titles in the music fixture")
	}
	if res.Failed != 0 {
		t.Fatalf("%d of %d items were PARKED as failed by a source that did not answer in "+
			"time — that says nothing about the item (ADR-0048)", res.Failed, res.Total)
	}
	if res.Retrying != res.Total {
		t.Fatalf("retrying = %d of %d, want every item scheduled for a retry", res.Retrying, res.Total)
	}

	// THE NUMBER THAT MATTERS IS THREE: the failure threshold. The pass has to have
	// made at least that many guest calls for "the plugin survived" to mean anything.
	mu.Lock()
	got := calls
	mu.Unlock()
	if got < 3 {
		t.Fatalf("the stand-in saw %d requests; fewer than the failure threshold, so this "+
			"test cannot tell a surviving plugin from an untested one", got)
	}

	// And the plugin came through it: no strike, no recorded error, still leading.
	row := pluginNamed(t, readPlugins(t, srv, token), "musicbrainz")
	if row.DisabledByFailure {
		t.Fatalf("the MusicBrainz plugin was DISABLED by a slow source: %+v — %d consecutive "+
			"unavailable answers must cost it nothing (ADR-0059 decision 6)", row, got)
	}
	if row.LastError != "" {
		t.Errorf("the plugin recorded %q; a source the guest ANSWERED for is not the plugin failing",
			row.LastError)
	}
	if !row.Enabled {
		t.Errorf("the plugin is no longer enabled: %+v", row)
	}
	if got := musicLead(getProviders(t, srv, token)); got != "musicbrainz" {
		t.Errorf("after the outage the music lead is %q, want musicbrainz", got)
	}
}

// musicLead is the slug of the first authoritative music provider on the providers
// screen — the rule that makes registration order the lead (ADR-0027, ADR-0059
// decision 3).
func musicLead(v providersView) string {
	for _, p := range v.Providers {
		if p.Role != "authoritative" {
			continue
		}
		for _, k := range p.Kinds {
			if k == "music" {
				return p.Slug
			}
		}
	}
	return ""
}

// --- "Test connection" reaches BOTH hosts --------------------------------------

// A WRONG COVER ART HOST FAILS THE CONNECTION TEST, AND THE SENTENCE NAMES IT.
//
// This is the operator-visible half of issue 06. The Cover Art Archive used to be a
// provider row with its own "Test connection" button; folding it into MusicBrainz
// as a second URL would have taken that away, leaving a dialog with two URL fields
// and a verdict about one of them — so a mistyped cover host would pass the test
// and then fail silently on every album cover the pass downloaded.
//
// The rule (enrich.TestConnection): a Descriptor that declares a second URL and the
// artwork-candidates capability gets a SECOND call, artwork-candidates for the
// record the probe just resolved, because that is the one contract call whose whole
// subject is images.
func TestTestConnectionReachesBothMusicHosts(t *testing.T) {
	// The web service answers the probe's album lookup with a release-group, which
	// is what gives the image call a record to ask about.
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/release-group"):
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg-ok-computer","title":"OK Computer","first-release-date":"1997-05-21"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ws.Close()

	var caaCalls int32
	var mu sync.Mutex
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		caaCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"image":"https://caa/full.jpg","front":true}]}`))
	}))
	defer caa.Close()

	srv := testharness.New(t, testharness.WithMusicBrainzEnabled(true))
	token := adminToken(t, srv)

	t.Run("both hosts reachable passes, and the cover host was really called", func(t *testing.T) {
		ok, detail := testProvider(t, srv, token, "musicbrainz", map[string]any{
			"baseURL": ws.URL, "imageBaseURL": caa.URL,
		})
		if !ok {
			t.Fatalf("test connection failed with both hosts up: %q", detail)
		}
		mu.Lock()
		got := caaCalls
		mu.Unlock()
		if got == 0 {
			t.Fatal("the cover-art host was never called, so the test proves nothing about it — " +
				"which is exactly the gap folding the `coverart` row into this provider opened")
		}
	})

	t.Run("a wrong cover-art host fails, naming it", func(t *testing.T) {
		// A host this server will not talk to at all: the refusal is the host's own,
		// and it is what an operator typing an unreachable name produces.
		const wrong = "http://not-a-real-cover-host.invalid"
		ok, detail := testProvider(t, srv, token, "musicbrainz", map[string]any{
			"baseURL": ws.URL, "imageBaseURL": wrong,
		})
		if ok {
			t.Fatalf("test connection PASSED with a wrong cover-art host (%q) — the operator is "+
				"told their configuration is good and then gets no album covers", wrong)
		}
		if !strings.Contains(detail, "not-a-real-cover-host.invalid") {
			t.Errorf("the failure says %q; it must name the cover-art host, because a dialog "+
				"with two URL fields and one verdict cannot be acted on", detail)
		}
	})

	t.Run("a wrong web service fails without blaming the cover host", func(t *testing.T) {
		ok, detail := testProvider(t, srv, token, "musicbrainz", map[string]any{
			"baseURL": "http://not-a-real-music-host.invalid", "imageBaseURL": caa.URL,
		})
		if ok {
			t.Fatal("test connection passed with an unreachable web service")
		}
		if strings.Contains(detail, "image host") {
			t.Errorf("the failure says %q — the API host is what failed, and blaming the "+
				"image host sends the operator to the wrong field", detail)
		}
	})
}

// testProvider posts the connection probe with edited (unsaved) credentials, which
// is what the dialog does, and returns the verdict.
func testProvider(t *testing.T, srv *testharness.Server, token, slug string, body map[string]any) (bool, string) {
	t.Helper()
	var resp struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	status, raw := srv.JSON(http.MethodPost,
		"/api/v1/settings/metadata-providers/"+slug+"/test", token, body, &resp)
	if status != http.StatusOK {
		t.Fatalf("POST test status = %d; body: %s", status, raw)
	}
	return resp.OK, resp.Detail
}
