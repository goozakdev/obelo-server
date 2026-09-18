package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for the OPTIONAL catalog (.scratch/plugin-system issue 15): an
// operator points their server at a JSON index they trust and installs from it
// with one request.
//
// Everything here goes through the real endpoints and the real install path. The
// plugin is the reference one — the Discord sink, built from its own source by the
// suite — so "the Browse tab installs the Discord plugin from a catalog" is
// asserted with the actual module rather than with a stand-in.
//
// The behaviour worth defending, and the reason each test exists:
//
//   - WITH NO URL SET there is no catalog, no request and no tab. That is the
//     shipped state (ADR-0001: this project runs no catalog).
//   - AN UNREACHABLE CATALOG IS A 200 WITH A NOTE. The upload and paste-URL paths
//     have nothing to do with the catalog and must survive its outage, which they
//     cannot do if the screen's own GET is a 5xx.
//   - AN ENTRY IS A MANIFEST URL, so installing one is the ordinary URL install
//     and an entry pointing into this server's own network is refused with the
//     SAME SENTENCE a pasted address gets. There is no catalog-shaped hole in the
//     policy, because there is no catalog-shaped install path.

const (
	catalogPath = pluginsPath + "/catalog"
)

// --- wire shapes ----------------------------------------------------------------

type catalogEntryResp struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Publisher    string   `json:"publisher"`
	Provides     []string `json:"provides"`
	ManifestURL  string   `json:"manifestUrl"`
	SignatureURL string   `json:"signatureUrl"`
	Description  string   `json:"description"`
	DocsURL      string   `json:"docsUrl"`
}

type catalogResp struct {
	URL     string             `json:"url"`
	Entries []catalogEntryResp `json:"entries"`
	Error   string             `json:"error"`
}

// --- the index and the files it points at ---------------------------------------

// discordSource serves the reference plugin the way an author publishes it: a
// manifest and a module in one directory, under the names they take on disk. It
// is exactly the layout docs/plugins/authoring.md tells authors to publish, which
// is what makes it also the layout a catalog can list.
//
// opts.signature, when set, is served beside them as plugin.sig.json.
func discordSource(t *testing.T, signature []byte) *httptest.Server {
	t.Helper()
	manifest := discordtest.ManifestJSON(t)
	module := discordtest.Module(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path.Base(r.URL.Path) {
		case plugins.ManifestFile:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifest)
		case plugins.DefaultModuleFile:
			w.Header().Set("Content-Type", "application/wasm")
			_, _ = w.Write(module)
		case pluginapi.SignatureFile:
			if len(signature) == 0 {
				// The ordinary case: an unsigned plugin's source answers 404 here, and
				// a 404 is not an error on the install path.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(signature)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// catalogServing is an index listing whatever entries it is given.
func catalogServing(t *testing.T, entries ...pluginapi.CatalogEntry) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(pluginapi.CatalogIndex{
		Version: pluginapi.CatalogVersion,
		Entries: entries,
	})
	if err != nil {
		t.Fatalf("encoding the test catalog: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func discordEntry(manifestURL string) pluginapi.CatalogEntry {
	return pluginapi.CatalogEntry{
		ID:          "discord",
		Name:        "Discord",
		Version:     "0.1.0",
		Publisher:   "Example Publisher",
		Provides:    []pluginapi.ExtensionPoint{pluginapi.ExtensionEventSink},
		ManifestURL: manifestURL,
		Description: "Posts a message to a Discord channel when something finishes.",
	}
}

func readCatalog(t *testing.T, srv *testharness.Server, token string) catalogResp {
	t.Helper()
	var resp catalogResp
	status, body := srv.AuthGET(catalogPath, token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET catalog status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func setCatalog(t *testing.T, srv *testharness.Server, token, url string) catalogResp {
	t.Helper()
	var resp catalogResp
	status, body := srv.JSON(http.MethodPut, catalogPath, token, map[string]any{"url": url}, &resp)
	if status != http.StatusOK {
		t.Fatalf("PUT catalog status = %d, want 200; body: %s", status, body)
	}
	return resp
}

// --- the acceptance criterion, whole --------------------------------------------

// TestACatalogListsThePluginAndInstallsIt. An Admin sets an address, sees what it
// offers, and installs the reference plugin from it — through the same endpoint a
// pasted URL goes through, with the module fetched from beside the manifest.
func TestACatalogListsThePluginAndInstallsIt(t *testing.T) {
	source := discordSource(t, nil)
	index := catalogServing(t, discordEntry(source.URL+"/"+plugins.ManifestFile))

	// The fixture is served from 127.0.0.1, which the INSTALL path refuses by
	// default because a plugin is code. The catalog INDEX is data and is fetched
	// under the ordinary policy, so only the install half needs this.
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)

	// Nothing set: no catalog, and nothing fetched.
	if got := readCatalog(t, srv, token); got.URL != "" || len(got.Entries) != 0 || got.Error != "" {
		t.Fatalf("a server with no catalog answered %+v, want an empty one with no note", got)
	}

	view := setCatalog(t, srv, token, index.URL)
	if view.URL != index.URL {
		t.Fatalf("url = %q, want the address that was just saved", view.URL)
	}
	if len(view.Entries) != 1 {
		t.Fatalf("the catalog listed %d entries, want 1: %+v", len(view.Entries), view.Entries)
	}
	entry := view.Entries[0]
	if entry.ID != "discord" || entry.Name != "Discord" || entry.Version != "0.1.0" {
		t.Fatalf("entry = %+v, want the index's own name and version", entry)
	}
	if len(entry.Provides) != 1 || entry.Provides[0] != string(pluginapi.ExtensionEventSink) {
		t.Fatalf("provides = %v, want [event-sink]", entry.Provides)
	}
	if entry.Publisher != "Example Publisher" {
		t.Fatalf("publisher = %q, want the index's claim", entry.Publisher)
	}

	// Installing an entry IS installing its manifest URL. No catalog-specific
	// endpoint exists and none should.
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": entry.ManifestURL}, nil)
	if status != http.StatusCreated {
		t.Fatalf("installing the catalog entry: status = %d, want 201; body: %s", status, body)
	}

	installed := pluginNamed(t, readPlugins(t, srv, token), "discord")
	if installed.DisabledByFailure || installed.LastError != "" {
		t.Fatalf("the plugin installed from a catalog reads as %+v, want it working", installed)
	}
	// The provenance recorded is the ADDRESS, which is what the catalog chose and
	// what an Admin would go back to.
	if installed.Source != entry.ManifestURL {
		t.Fatalf("source = %q, want the manifest URL the catalog gave", installed.Source)
	}
	// And the facts on the row come from the MANIFEST, not from the index: the
	// catalog's claims are display and the manifest is the authority.
	if installed.Name != discordtest.Manifest(t).Name {
		t.Fatalf("name = %q, want the manifest's %q", installed.Name, discordtest.Manifest(t).Name)
	}
}

// TestAnEntryResolvingToAPrivateAddressIsRefusedLikeAPastedOne. The catalog does
// not soften the install policy by a single sentence, and this is the test that
// says so: same code, same message, with the ONLY difference being that a
// catalog chose the address instead of a person.
func TestAnEntryResolvingToAPrivateAddressIsRefused(t *testing.T) {
	source := discordSource(t, nil)
	index := catalogServing(t, discordEntry(source.URL+"/"+plugins.ManifestFile))

	// Deliberately WITHOUT the private-source option: this is the production
	// policy. The catalog index itself is still read, because an index is data and
	// an operator's own index on their own LAN is a case worth supporting.
	srv := testharness.New(t)
	token := adminToken(t, srv)

	view := setCatalog(t, srv, token, index.URL)
	if len(view.Entries) != 1 {
		t.Fatalf("the catalog was not read: %+v", view)
	}
	// The entry is LISTED even though it cannot be installed. Hiding it would leave
	// an operator comparing their catalog against their screen and finding a plugin
	// missing with nothing said about why.
	entry := view.Entries[0]

	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": entry.ManifestURL}, nil)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if refusal.Error.Code != "PLUGIN_SOURCE_REFUSED" {
		t.Fatalf("code = %q, want PLUGIN_SOURCE_REFUSED; body: %s", refusal.Error.Code, body)
	}
	if !strings.Contains(refusal.Error.Message, "upload the file instead") {
		t.Fatalf("message = %q, want the same sentence a pasted URL gets", refusal.Error.Message)
	}
	if got := readPlugins(t, srv, token); len(got.Plugins) != 0 {
		t.Fatalf("a refused catalog install left %+v behind", got.Plugins)
	}
}

// TestAnUnreachableCatalogDegradesQuietly. The one behaviour that decides whether
// this feature is worth having: somebody else's outage must not cost an operator
// the two install paths that have nothing to do with it.
func TestAnUnreachableCatalogDegradesQuietly(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	deadURL := dead.URL + "/index.json"
	dead.Close() // nothing is listening at all now

	srv := testharness.New(t)
	token := adminToken(t, srv)

	// Saving the address is not an error: the address of a catalog that happens to
	// be down is not a wrong address.
	view := setCatalog(t, srv, token, deadURL)
	if view.URL != deadURL {
		t.Fatalf("url = %q, want the address to have been saved anyway", view.URL)
	}
	if len(view.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", view.Entries)
	}
	if view.Error == "" {
		t.Fatal("an unreachable catalog came back with no note at all, so a screen would show an empty tab and no reason")
	}

	// A 200 with a note, not a 5xx — checked again on the ordinary GET, which is
	// the request the screen makes on every load.
	got := readCatalog(t, srv, token)
	if got.Error == "" || len(got.Entries) != 0 {
		t.Fatalf("GET catalog = %+v, want an empty list and a note", got)
	}

	// And an upload still works, which is the whole point.
	status, body := uploadPlugin(t, srv, token, discordtest.ManifestJSON(t), discordtest.Module(t))
	if status != http.StatusCreated {
		t.Fatalf("an upload while the catalog was down: status = %d, want 201; body: %s", status, body)
	}
	if _, ok := pluginIn(readPlugins(t, srv, token), "discord"); !ok {
		t.Fatal("the plugin uploaded while the catalog was down is not installed")
	}
}

// TestACatalogThatIsNotACatalogIsANoteToo — the other three ways an index can
// disappoint, all of them the operator's address being wrong rather than this
// server failing.
func TestACatalogThatIsNotACatalogIsANoteToo(t *testing.T) {
	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not an index</html>"))
	}))
	t.Cleanup(notJSON.Close)
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(missing.Close)

	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, tc := range []struct {
		name, url, wantIn string
	}{
		{"not an index", notJSON.URL + "/index.json", "did not answer with a plugin index"},
		{"nothing there", missing.URL + "/index.json", "answered 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := setCatalog(t, srv, token, tc.url)
			if !strings.Contains(view.Error, tc.wantIn) {
				t.Fatalf("note = %q, want it to contain %q", view.Error, tc.wantIn)
			}
			if len(view.Entries) != 0 {
				t.Fatalf("entries = %+v, want none", view.Entries)
			}
		})
	}

	// An address that is not an address IS refused, though: nothing could ever make
	// it work, so saving it would be recording a mistake.
	status, body := srv.JSON(http.MethodPut, catalogPath, token, map[string]any{"url": "not-a-url"}, nil)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if refusal.Error.Code != "PLUGIN_SOURCE_REFUSED" {
		t.Fatalf("code = %q, want PLUGIN_SOURCE_REFUSED; body: %s", refusal.Error.Code, body)
	}
}

// TestClearingTheCatalogTurnsItOff, which is how an operator gets the screen they
// had before they opted in.
func TestClearingTheCatalogTurnsItOff(t *testing.T) {
	source := discordSource(t, nil)
	index := catalogServing(t, discordEntry(source.URL+"/"+plugins.ManifestFile))

	srv := testharness.New(t)
	token := adminToken(t, srv)

	if got := setCatalog(t, srv, token, index.URL); len(got.Entries) != 1 {
		t.Fatalf("the catalog was not read: %+v", got)
	}
	cleared := setCatalog(t, srv, token, "")
	if cleared.URL != "" || len(cleared.Entries) != 0 || cleared.Error != "" {
		t.Fatalf("after clearing, the catalog reads as %+v, want empty with no note", cleared)
	}
	// "Cleared" and "never set" are the same state, deliberately: they mean the
	// same thing to an operator and to the screen.
	if got := readCatalog(t, srv, token); got.URL != "" {
		t.Fatalf("url = %q after clearing, want empty", got.URL)
	}
}

// TestAnEntryWithNoManifestURLIsNotOffered. Every other claim in an entry is
// display and is left alone; a row with no address is the one a server could do
// nothing with at all.
func TestAnEntryWithNoManifestURLIsNotOffered(t *testing.T) {
	index := catalogServing(t,
		pluginapi.CatalogEntry{ID: "broken", Name: "Broken"},
		discordEntry("https://plugins.example.test/discord/"+plugins.ManifestFile),
	)
	srv := testharness.New(t)
	token := adminToken(t, srv)

	view := setCatalog(t, srv, token, index.URL)
	if len(view.Entries) != 1 || view.Entries[0].ID != "discord" {
		t.Fatalf("entries = %+v, want only the one with an address", view.Entries)
	}
}

// TestTheCatalogRoutesAreAdminOnly, like every other /settings route.
func TestTheCatalogRoutesAreAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")

	if status, _ := srv.AuthGET(catalogPath, member, nil); status != http.StatusForbidden {
		t.Fatalf("a Member GETting the catalog got %d, want 403", status)
	}
	status, _ := srv.JSON(http.MethodPut, catalogPath, member, map[string]any{"url": ""}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a Member setting the catalog got %d, want 403", status)
	}
}

// pluginIn is hasPlugin with the row, for the tests that want to look at it.
func pluginIn(resp pluginsResp, id string) (installedPluginResp, bool) {
	for _, p := range resp.Plugins {
		if p.ID == id {
			return p, true
		}
	}
	return installedPluginResp{}, false
}
