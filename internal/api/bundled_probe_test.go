package api_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/bundled"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// "Test connection" works for every plugin this server ships
// (.scratch/bundled-plugins: issue 08; ADR-0059 decision 8).
//
// The PRD asks for that as a criterion and the suites written before this one
// prove it three providers at a time: issue 05 drove OMDb, TheTVDB and AniDB, and
// issue 06 drove MusicBrainz against both of its hosts. Those tests assert what
// each source is ASKED — the query shape, the login dance, the second host — and
// are the better tests for what they cover. What none of them covers is the whole
// set, which is the thing that quietly rots: the connectivity switch this feature
// replaced worked for the eight slugs its author knew about and answered "unknown
// provider" for everything else, and the failure mode of the thing that replaced
// it is a manifest that forgets its `probe` and a button that says "this provider
// declares no connection probe" to an operator who is trying to find out why their
// key does not work.
//
// So this one is deliberately shallow and complete: every bundled id, pointed at a
// stand-in, must reach it. It asserts nothing about what comes back, because what
// comes back is source-specific and the per-provider suites already own it.

// TestEveryBundledPluginsTestConnectionReachesItsSource: an Admin pressing Test
// connection on any of the seven shipped providers makes a real request to the
// host they configured — through the settings row, the resolver, the ABI, the
// guest and http_fetch — and never answers with the no-probe sentence.
func TestEveryBundledPluginsTestConnectionReachesItsSource(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, id := range shippedMetadataIDs {
		t.Run(id, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			// 404 for everything. A probe's VERDICT is source-specific and belongs to
			// the per-provider suites; what has to be true for all seven is that the
			// guest was asked something and asked it HERE.
			src := standIn(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
			})

			// The image host is pointed at the stand-in too. A provider with a second
			// host gets a second call, and MusicBrainz's default second host is the
			// real Cover Art Archive — a test suite must not be one packet away from
			// reaching it.
			_, detail := testProviderConnectionAtBothHosts(t, srv, token, id, "a-key", src.URL, src.URL)

			mu.Lock()
			asked := append([]string(nil), paths...)
			mu.Unlock()
			if len(asked) == 0 {
				t.Fatalf("Test connection on the bundled %s plugin made no request at all; "+
					"detail was %q", id, detail)
			}
			if strings.Contains(detail, "declares no connection probe") {
				t.Errorf("%s: %q — plugins/%s/manifest.json has lost its provides[].probe, so the "+
					"one outbound check an operator can press tells them nothing (ADR-0059 decision 8)",
					id, detail, id)
			}
			if strings.Contains(detail, "unknown provider") {
				t.Errorf("%s: %q — the shipped plugin is not in the catalog this handler reads", id, detail)
			}
		})
	}
}

// testProviderConnectionAtBothHosts is testProviderConnection with the second
// host named as well — what the dialog sends for a provider that has two URL
// fields (.scratch/bundled-plugins: issue 06).
func testProviderConnectionAtBothHosts(t *testing.T, srv *testharness.Server, token, slug, key, baseURL, imageBaseURL string) (bool, string) {
	t.Helper()
	var out struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	status, body := srv.JSON(http.MethodPost, providersPath+"/"+slug+"/test", token,
		map[string]any{"apiKey": key, "baseURL": baseURL, "imageBaseURL": imageBaseURL}, &out)
	if status != http.StatusOK {
		t.Fatalf("%s test = %d, want 200; body: %s", slug, status, body)
	}
	return out.OK, out.Detail
}

// probeIDCarriedAt is, for a provider whose manifest probe declares an External
// id (plugins/<id>/manifest.json's provides[].probe.externalIds), an exact check
// that the id reached the outgoing request — read from the provider's own
// request-building code below, never a loose substring match (the query string
// carries other digits too, e.g. anidb's constant protover=1, which a bare
// strings.Contains("1") would satisfy for free).
// TestEveryBundledPluginsTestConnectionReachesItsSource proves a request
// happens; it does not prove the id in the probe reached the wire, and 5 of
// these 7 providers would pass that test just as well with an id-less probe,
// because their outgoing request is built from the OTHER probe fields
// (title/year/kind), never from an id: omdb, tmdb and thetvdb resolve by title
// search, musicbrainz's probe is a release search by artist/album, and
// theaudiodb's is an artist-name lookup. anidb and fanarttv are the two whose
// probe is id-keyed — anidb.Provider.Lookup reads ref.ID(NamespaceAniDB) and
// puts it on the query string ("aid="), fanarttv's artistLookup reads
// ref.ID(NamespaceMusicBrainz) and puts it in the request path ("/music/<mbid>")
// — so those two are the ones this test can actually hold to carrying it.
var probeIDCarriedAt = map[string]func(r *http.Request) bool{
	"anidb": func(r *http.Request) bool {
		return r.URL.Query().Get("aid") == "1"
	},
	"fanarttv": func(r *http.Request) bool {
		return strings.HasSuffix(r.URL.Path, "/music/a74b1b7f-71a5-4011-9441-d0b5e4122711")
	},
}

// TestProbeIDCarriedAtMatchesTheManifests: probeIDCarriedAt names EXACTLY the
// Bundled plugins whose manifest probe declares an externalIds — the set above is
// hand-maintained, and nothing else fails when a NEW Bundled provider ships an
// id-keyed probe and is not added to the table (it would simply never be checked).
// Read through internal/bundled.Manifests() — the embedded copies every other
// test in this package already depends on being present — rather than the source
// plugins/*/manifest.json, since a black-box test in this package has no relative
// path back to the repo root that survives `go test ./...` from any directory.
func TestProbeIDCarriedAtMatchesTheManifests(t *testing.T) {
	manifests, err := bundled.Manifests()
	if err != nil {
		t.Fatalf("reading the embedded manifests: %v (run make plugins)", err)
	}
	declared := map[string]bool{}
	for _, m := range manifests {
		for _, p := range m.Provides {
			if p.Probe != nil && len(p.Probe.ExternalIDs) > 0 {
				declared[m.ID] = true
			}
		}
	}
	for id := range declared {
		if _, ok := probeIDCarriedAt[id]; !ok {
			t.Errorf("%s's manifest probe declares an externalIds, but probeIDCarriedAt has no "+
				"entry for it — add one so this suite actually checks its id reaches the wire", id)
		}
	}
	for id := range probeIDCarriedAt {
		if !declared[id] {
			t.Errorf("probeIDCarriedAt has an entry for %s, but its manifest probe declares no "+
				"externalIds — remove the entry (or the table checks an id that was never there)", id)
		}
	}
}

// TestBundledProbeCarriesItsExternalID: for the providers whose manifest probe
// declares an id, that id reaches the outgoing request — not just a request.
func TestBundledProbeCarriesItsExternalID(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for id, carries := range probeIDCarriedAt {
		t.Run(id, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			found := false
			src := standIn(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.RequestURI())
				if carries(r) {
					found = true
				}
				mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
			})
			testProviderConnectionAtBothHosts(t, srv, token, id, "a-key", src.URL, src.URL)

			mu.Lock()
			got := append([]string(nil), requests...)
			ok := found
			mu.Unlock()
			if !ok {
				t.Errorf("%s: no outgoing request carried the probe's external id; requests: %v", id, got)
			}
		})
	}
}

// shippedPluginIDs is every Bundled plugin, in shipped order. It is restated here
// rather than read from internal/bundled on purpose: this is a black-box suite,
// and a test that asks the code under test what it ships would pass on a server
// that shipped nothing.
var shippedPluginIDs = append(append([]string(nil), shippedMetadataIDs...), "opensubtitles")

// shippedMetadataIDs is the Bundled plugins that are Metadata providers — every
// shipped plugin but OpenSubtitles, which is a Subtitle provider and is tested
// through the subtitle-providers screen (bundled_opensubtitles_test.go).
var shippedMetadataIDs = []string{"tmdb", "omdb", "thetvdb", "anidb", "musicbrainz", "fanarttv", "theaudiodb"}

// TestTheShippedSetIsEightPluginsInOrder: the list above is what a fresh server
// really has, so nothing else in this file can be quietly testing a subset.
func TestTheShippedSetIsEightPluginsInOrder(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	rows := readPlugins(t, srv, token)
	got := make([]string, 0, len(rows.Plugins))
	for _, p := range rows.Plugins {
		got = append(got, p.ID)
	}
	if len(got) != len(shippedPluginIDs) {
		t.Fatalf("installed plugins = %v, want the eight bundled ones", got)
	}
	// The PLUGINS screen sorts by id; the PROVIDERS screen is the one that carries
	// the shipped order, and that is what decides which source leads a kind.
	for _, id := range shippedPluginIDs {
		pluginNamed(t, rows, id)
	}

	var providers struct {
		Providers []struct {
			Slug string `json:"slug"`
		} `json:"providers"`
	}
	status, body := srv.JSON(http.MethodGet, providersPath, token, nil, &providers)
	if status != http.StatusOK {
		t.Fatalf("providers = %d; body: %s", status, body)
	}
	order := make([]string, 0, len(providers.Providers))
	for _, p := range providers.Providers {
		order = append(order, p.Slug)
	}
	if len(order) != len(shippedMetadataIDs) {
		t.Fatalf("providers = %v, want exactly the seven bundled ones — no Built-in metadata "+
			"provider is compiled into this server any more (.scratch/bundled-plugins: issue 08)", order)
	}
	for i, want := range shippedMetadataIDs {
		if order[i] != want {
			t.Fatalf("providers = %v, want %v — registration order decides the default lead of "+
				"each kind and the music chain's two image slots", order, shippedMetadataIDs)
		}
	}
}
