package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// OMDb, TheTVDB and AniDB as Bundled plugins (.scratch/bundled-plugins: issue 05).
//
// Everything here drives the REAL WebAssembly modules through wazero, which is
// what this package is for: the ported unit suites under plugins/<id>/<id>/ prove
// the parsing record for record, and only these prove that the module loads, the
// exports are spelled right, the settings the Admin saved reach the guest, and the
// JSON survives the boundary in both directions.
//
// The three bundled video supplements replaced three Built-ins, so the thing most
// worth asserting is what did NOT change: the shipped order, the chain positions
// that order produces, and the connection probe an Admin presses.

// bundledVideoIDs are the four video providers this server ships as plugins, in
// the order internal/bundled carries them — which IS the catalog order (ADR-0059
// decision 3).
var bundledVideoIDs = []string{"tmdb", "omdb", "thetvdb", "anidb"}

// TestTheShippedVideoPluginsInstallAndLeadInShippedOrder: a fresh server comes up
// with all four video plugins installed as bundled, and the providers screen lists
// them ahead of the Built-ins in the shipped order.
//
// The ORDER is the assertion with teeth. Three things read it — the settings
// screen, the fill-only Supplement composition behind the Authoritative provider,
// and "the first authoritative-role Full provider of a kind is that kind's default
// lead" — so tmdb leading and anidb NOT leading are both facts about this list and
// about nothing else. Installed plugins otherwise load in alphabetical directory
// order, which would have made anidb the default video lead.
func TestTheShippedVideoPluginsInstallAndLeadInShippedOrder(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)

	rows := readPlugins(t, srv, token)
	for _, id := range bundledVideoIDs {
		got := pluginNamed(t, rows, id)
		if got.Origin != plugins.OriginBundled {
			t.Errorf("%s: origin = %q, want %q", id, got.Origin, plugins.OriginBundled)
		}
		if !got.Enabled || got.DisabledByFailure || got.LastError != "" {
			t.Errorf("the shipped %s plugin did not come up: %+v", id, got)
		}
		if got.APIVersion != 1 || got.Version == "" {
			t.Errorf("%s: version/apiVersion = %q/%d, want the manifest's", id, got.Version, got.APIVersion)
		}
	}

	// The providers screen: the four bundled video plugins first, in shipped order,
	// then whatever Built-ins are still compiled in.
	var got []string
	for _, p := range readMetadataProviders(t, srv, token) {
		got = append(got, p.Slug)
	}
	if len(got) < len(bundledVideoIDs) {
		t.Fatalf("the providers screen lists %v, want at least the four bundled video plugins", got)
	}
	for i, want := range bundledVideoIDs {
		if got[i] != want {
			t.Fatalf("the providers screen lists %v; position %d is %q, want %q — registration "+
				"order decides the default video lead and the supplement composition",
				got, i, got[i], want)
		}
	}
}

// standIn is a plain-http server with the handler a test gives it, plus the
// explicit port it ended up on.
func standIn(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("the stand-in's URL is unparseable: %v", err)
	}
	if u.Scheme != "http" || u.Port() == "" {
		t.Fatalf("the stand-in is %q, want plain http on an explicit port", srv.URL)
	}
	return srv
}

// testProviderConnection presses "Test connection" for one provider with the
// credential and base URL an Admin typed, and returns what the screen would show.
func testProviderConnection(t *testing.T, srv *testharness.Server, token, slug, key, baseURL string) (bool, string) {
	t.Helper()
	var out struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	status, body := srv.JSON(http.MethodPost, providersPath+"/"+slug+"/test", token,
		map[string]any{"apiKey": key, "baseURL": baseURL}, &out)
	if status != http.StatusOK {
		t.Fatalf("%s test = %d, want 200; body: %s", slug, status, body)
	}
	return out.OK, out.Detail
}

// TestTheBundledAniDBPluginReachesAPlainHTTPStandInOnAnExplicitPort is the whole
// AniDB chain in one call, and the URL shape is the point.
//
// AniDB publishes `http://api.anidb.net:9001/httpapi` — PLAIN HTTP, on an EXPLICIT
// PORT — which every other shipped source's https-on-443 would have let this
// server get away with never handling. It is handled: the host's fetch check
// admits http beside https, and it matches the allowlist and the operator's own
// target on the HOSTNAME with the port stripped, so `api.anidb.net` in a manifest
// covers `:9001`. (internal/plugins proves both halves of that rule directly,
// including that an explicit port buys a manifest host no access to a private
// address.)
//
// What runs here is the real module: the Admin's client name and base URL are
// resolved into Settings, handed across the ABI, turned into an http_fetch by the
// guest, answered by the stand-in, parsed as XML INSIDE WASM, and returned as a
// record — which is the fact ADR-0059 decision 10 rests on, since encoding/xml was
// the reason TinyGo was rejected.
func TestTheBundledAniDBPluginReachesAPlainHTTPStandInOnAnExplicitPort(t *testing.T) {
	var asked url.Values
	src := standIn(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query()
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<anime id="1">
  <titles><title xml:lang="en" type="official">Cowboy Bebop</title></titles>
  <description>A ragtag crew of bounty hunters.</description>
  <picture>12345.jpg</picture>
  <startdate>1998-04-03</startdate>
</anime>`))
	})

	srv := testharness.New(t)
	token := adminToken(t, srv)

	ok, detail := testProviderConnection(t, srv, token, "anidb", "obelo-client", src.URL+"/httpapi")
	if !ok {
		t.Fatalf("the AniDB probe failed against a healthy plain-http stand-in: %q", detail)
	}
	// The manifest's own probe reached the guest: aid 1, with the client name the
	// Admin typed and the version/protover AniDB requires on every request.
	if asked.Get("aid") != "1" {
		t.Errorf("aid = %q, want 1 — the manifest's probe is what the host looks up "+
			"(ADR-0059 decision 8)", asked.Get("aid"))
	}
	if asked.Get("client") != "obelo-client" {
		t.Errorf("client = %q, want the registered client name the Admin typed", asked.Get("client"))
	}
	if asked.Get("request") != "anime" || asked.Get("protover") != "1" {
		t.Errorf("request/protover = %q/%q, want anime/1", asked.Get("request"), asked.Get("protover"))
	}
}

// TestTheBundledOMDbAndTheTVDBPluginsProbeThroughTheSandbox: the two connectivity
// probes that used to live in internal/enrich, at the level where they now mean
// something.
//
// They were white-box tests of a Go client against an httptest server. Both
// clients are guests now, so the same assertion moved here — where the probe
// really does cross the settings row, the resolver, the ABI, the guest and
// http_fetch — and the parsing half moved to plugins/<id>/<id>/. TheTVDB's case
// carries its login dance with it: the stand-in mints a bearer token on /login and
// refuses any data request that does not present one, so a probe that answers ok
// is a probe that logged in.
func TestTheBundledOMDbAndTheTVDBPluginsProbeThroughTheSandbox(t *testing.T) {
	t.Run("omdb", func(t *testing.T) {
		var asked url.Values
		src := standIn(t, func(w http.ResponseWriter, r *http.Request) {
			asked = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Title":"Inception","Rated":"PG-13","Genre":"Action, Sci-Fi",` +
				`"Plot":"A thief who steals corporate secrets.","Response":"True"}`))
		})
		srv := testharness.New(t)
		token := adminToken(t, srv)

		ok, detail := testProviderConnection(t, srv, token, "omdb", "a-key", src.URL)
		if !ok {
			t.Fatalf("the OMDb probe failed against a healthy stub: %q", detail)
		}
		// The manifest's probe is Inception 2010, resolved by title+year because the
		// probe carries no IMDb id.
		if asked.Get("t") != "Inception" || asked.Get("y") != "2010" {
			t.Errorf("t/y = %q/%q, want Inception/2010", asked.Get("t"), asked.Get("y"))
		}
		if asked.Get("apikey") != "a-key" {
			t.Errorf("apikey = %q, want the key the Admin typed", asked.Get("apikey"))
		}
	})

	t.Run("thetvdb", func(t *testing.T) {
		var logins int
		var dataReqs []string
		src := standIn(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login" {
				logins++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok"}}`))
				return
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			dataReqs = append(dataReqs, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/search":
				_, _ = w.Write([]byte(`{"data":[{"tvdb_id":"81189","name":"Breaking Bad"}]}`))
			case strings.HasPrefix(r.URL.Path, "/series/"):
				_, _ = w.Write([]byte(`{"data":{"name":"Breaking Bad","overview":"A chemistry teacher.",` +
					`"image":"https://art/bb.jpg","genres":[{"name":"Drama"}]}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		srv := testharness.New(t)
		token := adminToken(t, srv)

		ok, detail := testProviderConnection(t, srv, token, "thetvdb", "a-key", src.URL)
		if !ok {
			t.Fatalf("the TheTVDB probe failed against a healthy stub: %q", detail)
		}
		if logins != 1 {
			t.Errorf("logins = %d, want exactly 1 — the bearer token is minted on first use "+
				"and lives in the guest instance", logins)
		}
		// The probe is "Breaking Bad" with no id, so it resolves by name first.
		if len(dataReqs) != 2 || dataReqs[0] != "/search" || dataReqs[1] != "/series/81189" {
			t.Errorf("data reqs = %v, want /search then /series/81189", dataReqs)
		}
	})
}

// TestTheVideoChainFillsAMovieFromTheBundledOMDbPlugin is acceptance criterion 1
// written out: TWO real bundled guests in one chain, the authoritative one
// answering a record with gaps and the supplement filling exactly those gaps and
// nothing else.
//
// It is the composition that matters here, not the parsing. TMDB's stand-in
// deliberately answers a movie with no overview, no certification and no genres —
// the shape that made OMDb worth having — and OMDb's stand-in has all three. What
// the Title ends up with is the whole assertion: OMDb's three fields, and TMDB's
// name and runtime untouched.
func TestTheVideoChainFillsAMovieFromTheBundledOMDbPlugin(t *testing.T) {
	requireFixtures(t)

	tmdb := standIn(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/"):
			_, _ = w.Write([]byte(`{"results":[{"id":603}]}`))
		default:
			// A record with GAPS: no overview, no release_dates certification, no
			// genres. This is the case the supplement exists for.
			_, _ = w.Write([]byte(`{"id":603,"title":"The Matrix","runtime":136,` +
				`"release_date":"1999-03-30","production_companies":[{"name":"Warner Bros."}]}`))
		}
	})
	omdb := standIn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Title":"The Matrix","Rated":"R","Genre":"Action, Sci-Fi",` +
			`"Plot":"A hacker learns the truth about his reality.","Response":"True"}`))
	})

	srv := testharness.New(t)
	token := adminToken(t, srv)
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "tmdb", "enabled": true, "apiKey": "tmdb-key", "baseURL": tmdb.URL},
		{"slug": "omdb", "enabled": true, "apiKey": "omdb-key", "baseURL": omdb.URL},
	}}, http.StatusOK)

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	if res := enrichLib(t, srv, token, libID, "full"); res.Total == 0 {
		t.Skip("no titles in the movie fixture")
	}

	titles := listAllTitles(t, srv, token, libID).Titles
	if len(titles) == 0 {
		t.Skip("no titles in the movie fixture")
	}
	d := getEnrichedDetail(t, srv, token, titles[0].ID)

	// The three fields the SUPPLEMENT filled. Each was empty in TMDB's answer, so
	// each of these is OMDb's guest having been consulted, reached and believed.
	if d.Overview != "A hacker learns the truth about his reality." {
		t.Errorf("overview = %q, want OMDb's plot — the supplement did not fill the gap", d.Overview)
	}
	if d.ContentRating != "R" {
		t.Errorf("content rating = %q, want OMDb's R", d.ContentRating)
	}
	if len(d.Genres) != 2 || d.Genres[0] != "Action" || d.Genres[1] != "Sci-Fi" {
		t.Errorf("genres = %v, want OMDb's [Action Sci-Fi]", d.Genres)
	}
	// And what the AUTHORITATIVE source said is untouched: a fill-only supplement
	// never overwrites a field the lead already answered.
	if d.RuntimeMinutes != 136 {
		t.Errorf("runtime = %d, want TMDB's 136 (the supplement must not touch it)", d.RuntimeMinutes)
	}
	if d.Studio != "Warner Bros." {
		t.Errorf("studio = %q, want TMDB's (the supplement must not touch it)", d.Studio)
	}
}
