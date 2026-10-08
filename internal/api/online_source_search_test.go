package api_test

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the source page's search and its failure states (ADR-0068,
// issue 05), through the same Installed Plugin the other Online source tests use.

type onlineSearchResp struct {
	Items []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnailUrl"`
	} `json:"items"`
}

func searchPath(slug, q string) string {
	return onlineBase + "/" + slug + "/search?q=" + url.QueryEscape(q)
}

// TestSearchReturnsPluginResultsAndTheSameQueryTwiceMakesTwoPluginCalls: search is a
// live read, never cached, and its thumbnails load through the proxy.
func TestSearchReturnsPluginResultsAndTheSameQueryTwiceMakesTwoPluginCalls(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	var out onlineSearchResp
	for i := 0; i < 2; i++ {
		out = onlineSearchResp{}
		if st, body := srv.AuthGET(searchPath(onlineSlug, "  cats "), admin, &out); st != http.StatusOK {
			t.Fatalf("search %d = %d; body: %s", i, st, body)
		}
	}
	if len(out.Items) != 1 || out.Items[0].ID != "hit-cats" || out.Items[0].Title != "Found cats" {
		t.Fatalf("items = %+v, want the Plugin's one hit for the trimmed query", out.Items)
	}
	if want := []string{"cats", "cats"}; !reflect.DeepEqual(src.queries, want) {
		t.Fatalf("Plugin saw queries %v, want %v (two calls, trimmed)", src.queries, want)
	}
	if !strings.HasSuffix(out.Items[0].ThumbnailURL, "/onlineSources/tube/items/hit-cats/thumbnail") {
		t.Fatalf("thumbnailUrl = %q, want the Server's own proxy path", out.Items[0].ThumbnailURL)
	}
	if st, _ := srv.AuthGET(out.Items[0].ThumbnailURL, admin, nil); st != http.StatusOK {
		t.Fatalf("a search result's thumbnail = %d, want 200", st)
	}
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/search", admin, &out); st != http.StatusOK || len(out.Items) != 0 {
		t.Fatalf("search with no query = %d %+v, want 200 and no items", st, out.Items)
	}
}

// TestSearchGoesThroughTheSameCapsAndMalformedDropsAsRows: the same judgment, so a
// bad entry costs one item, not the answer.
func TestSearchGoesThroughTheSameCapsAndMalformedDropsAsRows(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	src.search = func(string) []map[string]any {
		items := []map[string]any{
			pagedItem(media, "ok"),
			{"id": "bad/id", "title": "No", "thumbnailUrl": media.srv.URL + "/thumb/x.png", "durationMs": 1},
			{"id": "plain", "title": "No", "thumbnailUrl": "http://insecure.example/x.png", "durationMs": 1},
			{"id": "nonnumeric", "title": "No", "thumbnailUrl": media.srv.URL + "/thumb/x.png", "durationMs": "soon"},
		}
		for i := 0; i < 150; i++ {
			items = append(items, pagedItem(media, "fill"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))))
		}
		return items
	}
	var out onlineSearchResp
	if st, body := srv.AuthGET(searchPath(onlineSlug, "x"), admin, &out); st != http.StatusOK {
		t.Fatalf("search = %d; body: %s", st, body)
	}
	if len(out.Items) != 100 || out.Items[0].ID != "ok" {
		t.Fatalf("got %d items starting %q, want the cap of 100 starting at ok", len(out.Items), out.Items[0].ID)
	}
	for _, it := range out.Items {
		if it.ID == "bad/id" || it.ID == "plain" || it.ID == "nonnumeric" {
			t.Fatalf("malformed item %q survived", it.ID)
		}
	}
}

// TestTheSearchEndpointIs404ForAnUngrantedMemberAndARemoteCaller: this slice's own
// endpoint follows the access rule of the rest of an Online source, the Plugin is
// never called for a refused caller, and the granted Member and the Admin get 200.
func TestTheSearchEndpointIs404ForAnUngrantedMemberAndARemoteCaller(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	path := searchPath(onlineSlug, "cats")

	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	granted := srv.LoginAs("kid", "memberpass123")
	srv.CreateUser(admin, "other", "memberpass123", "member")
	ungranted := srv.LoginAs("other", "memberpass123")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)

	var remoteOut struct {
		ID string `json:"id"`
	}
	if st, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "peer", "role": "remote"}, &remoteOut); st != http.StatusCreated {
		t.Fatalf("creating the remote User = %d; body: %s", st, body)
	}
	remote := srv.IssueTokenForUser(remoteOut.ID, "peer-server-id")

	for name, token := range map[string]string{"admin": admin, "granted member": granted} {
		if st, body := srv.AuthGET(path, token, nil); st != http.StatusOK {
			t.Errorf("%s: GET %s = %d, want 200; body: %s", name, path, st, body)
		}
	}
	calls := src.calls()
	for name, token := range map[string]string{"ungranted member": ungranted, "remote": remote} {
		if st, _ := srv.AuthGET(path, token, nil); st != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404", name, path, st)
		}
	}
	if st, _ := srv.AuthGET(path, "", nil); st != http.StatusUnauthorized {
		t.Errorf("no token: GET %s = %d, want 401", path, st)
	}
	if st, _ := srv.AuthGET(searchPath("nosuchsource", "cats"), admin, nil); st != http.StatusNotFound {
		t.Errorf("an unknown source = %d, want 404", st)
	}
	if n := src.calls(); n != calls {
		t.Fatalf("a refused caller caused %d Plugin calls", n-calls)
	}
}

// TestAFailingPluginAnswersASourceUnavailableForRowsAndSearch: the 502 the page turns
// into "{source} isn't responding", not a 500 and not an empty 200.
func TestAFailingPluginAnswersASourceUnavailableForRowsAndSearch(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	src.mu.Lock()
	src.down = func(path string) bool { return path == "/rows" || path == "/search" }
	src.mu.Unlock()
	for _, path := range []string{onlineBase + "/" + onlineSlug + "/rows", searchPath(onlineSlug, "cats")} {
		var env errorEnvelope
		if st, body := srv.AuthGET(path, admin, &env); st != http.StatusBadGateway || env.Error.Code != "SOURCE_UNAVAILABLE" {
			t.Fatalf("GET %s = %d %+v, want 502 SOURCE_UNAVAILABLE; body: %s", path, st, env.Error, body)
		}
	}
}

// TestAFailingOrSlowSourceKeepsItsTileButADisabledPluginLosesIt: a source that keeps
// failing is still a tile (the page says it isn't responding); only a Plugin the
// Admin switches off, or one this server stopped, is hidden from Users, and it is
// explained on the Plugins screen and back when re-enabled.
func TestAFailingOrSlowSourceKeepsItsTileButADisabledPluginLosesIt(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	kid := srv.LoginAs("kid", "memberpass123")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	if got := tileIDs(t, srv, kid); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles before = %v, want the granted source", got)
	}

	// Failing: far more failed calls than the server's strike threshold.
	src.mu.Lock()
	src.down = func(string) bool { return true }
	src.mu.Unlock()
	for i := 0; i < 8; i++ {
		path := onlineBase + "/" + onlineSlug + "/rows"
		if i%2 == 1 {
			path = searchPath(onlineSlug, "cats")
		}
		if st, _ := srv.AuthGET(path, kid, nil); st != http.StatusBadGateway {
			t.Fatalf("failing call %d = %d, want 502", i, st)
		}
	}
	if got := tileIDs(t, srv, kid); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles after a failing source = %v, want the tile kept", got)
	}
	if p := pluginNamed(t, readPlugins(t, srv, admin), onlineSlug); p.DisabledByFailure || !p.Enabled {
		t.Fatalf("a merely failing source was stopped by the server: %+v", p)
	}
	src.mu.Lock()
	src.down = nil
	src.mu.Unlock()
	if st, body := srv.AuthGET(searchPath(onlineSlug, "cats"), kid, nil); st != http.StatusOK {
		t.Fatalf("search after the source recovered = %d; body: %s", st, body)
	}

	// Admin switches the Plugin off: the tile goes, the Plugins screen says why.
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/disable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("disable = %d; body: %s", st, body)
	}
	if got := tileIDs(t, srv, kid); len(got) != 0 {
		t.Fatalf("tiles with the Plugin switched off = %v, want none", got)
	}
	if st, _ := srv.AuthGET(searchPath(onlineSlug, "cats"), kid, nil); st != http.StatusNotFound {
		t.Fatalf("search on a switched-off source = %d, want 404", st)
	}
	if p := pluginNamed(t, readPlugins(t, srv, admin), onlineSlug); p.Enabled {
		t.Fatalf("the Plugins screen shows %+v for a switched-off Plugin, want it listed as off", p)
	}

	// Re-enabled: the tile is back, still granted.
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/enable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("enable = %d; body: %s", st, body)
	}
	if got := tileIDs(t, srv, kid); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles after re-enabling = %v, want the tile restored", got)
	}
}

// TestASlowSourceThatTimesOutKeepsItsTile: calls that outrun the Plugin's budget
// answer 502 every time and never stop the Plugin, so the tile stays.
func TestASlowSourceThatTimesOutKeepsItsTile(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t, testharness.WithPluginCallTimeout(300*time.Millisecond))
	src.hang.Store(true)
	for i := 0; i < 5; i++ {
		if st, _ := srv.AuthGET(searchPath(onlineSlug, "cats"), admin, nil); st != http.StatusBadGateway {
			t.Fatalf("slow search %d = %d, want 502", i, st)
		}
	}
	if got := tileIDs(t, srv, admin); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles after a slow source = %v, want the tile kept", got)
	}
	if p := pluginNamed(t, readPlugins(t, srv, admin), onlineSlug); p.DisabledByFailure || !p.Enabled {
		t.Fatalf("a merely slow source was stopped by the server: %+v", p)
	}
}

// TestAPluginThisServerStoppedLosesItsTileWithTheReasonOnThePluginsScreen: a guest
// that traps (here, on one query) on a run of calls is stopped by the server
// (ADR-0001); its tile goes and the Plugins screen carries the reason; Re-enable
// restores it.
func TestAPluginThisServerStoppedLosesItsTileWithTheReasonOnThePluginsScreen(t *testing.T) {
	t.Parallel()
	srv, admin, _, _ := onlineServer(t)
	if got := tileIDs(t, srv, admin); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles before = %v, want the source", got)
	}
	for i := 0; i < 3; i++ {
		srv.AuthGET(searchPath(onlineSlug, "obelo-trap"), admin, nil)
	}
	if got := tileIDs(t, srv, admin); len(got) != 0 {
		t.Fatalf("tiles for a stopped Plugin = %v, want none", got)
	}
	p := pluginNamed(t, readPlugins(t, srv, admin), onlineSlug)
	if !p.DisabledByFailure || p.LastError == "" {
		t.Fatalf("the Plugins screen shows %+v for a stopped Plugin, want the reason", p)
	}
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/reenable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("reenable = %d; body: %s", st, body)
	}
	if got := tileIDs(t, srv, admin); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("tiles after Re-enable = %v, want the tile restored", got)
	}
}

// TestGlobalSearchHasNoOnlineItems: the query that finds an Online item on its
// source page finds nothing in the Server's global search, and no Plugin is asked.
func TestGlobalSearchHasNoOnlineItems(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	if st, body := srv.AuthGET(searchPath(onlineSlug, "talk"), admin, nil); st != http.StatusOK {
		t.Fatalf("source search = %d; body: %s", st, body)
	}
	calls := src.calls()
	st, body := srv.AuthGET("/api/v1/search?q=talk", admin, nil)
	if st != http.StatusOK {
		t.Fatalf("global search = %d; body: %s", st, body)
	}
	for _, leak := range []string{"hit-talk", "Found talk", "A talk", onlineSlug, "onlineSources"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("global search body mentions %q: %s", leak, body)
		}
	}
	if n := src.calls(); n != calls {
		t.Fatalf("global search caused %d Plugin calls, want 0", n-calls)
	}
}
