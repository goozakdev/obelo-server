package api_test

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Pasted-URL and catalog upgrades (ADR-0069, .scratch/plugin-inplace-upgrade issue 07).
// A catalog install IS from-url, so the first half drives that route and the second
// half the catalog listing's "Update available" hint.

// packageHost serves archives by file name and counts every request it gets.
type packageHost struct {
	*httptest.Server
	mu       sync.Mutex
	archives map[string][]byte
	hits     atomic.Int32
}

func newPackageHost(t *testing.T) *packageHost {
	t.Helper()
	h := &packageHost{archives: map[string][]byte{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		h.mu.Lock()
		body, ok := h.archives[path.Base(r.URL.Path)]
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *packageHost) serve(name string, archive []byte) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.archives[name] = archive
	return h.URL + "/" + name
}

func postFromURL(t *testing.T, srv *testharness.Server, token, url string) (int, []byte) {
	t.Helper()
	return srv.JSON(http.MethodPost, pluginsPath+"/from-url", token, map[string]any{"url": url}, nil)
}

func TestAPastedURLWithAHigherVersionUpgradesInPlaceAndRecordsTheNewSource(t *testing.T) {
	t.Parallel()
	host := newPackageHost(t)
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	v1 := host.serve("up-sink-1.zip", signedPackage(t, "up-sink", "1.0.0", priv))
	v2 := host.serve("up-sink-2.zip", signedPackage(t, "up-sink", "1.1.0", priv))

	if status, body := postFromURL(t, srv, token, v1); status != http.StatusCreated {
		t.Fatalf("first install status = %d, want 201; body: %s", status, body)
	}
	status, body := postFromURL(t, srv, token, v2)
	if status != http.StatusOK {
		t.Fatalf("upgrade status = %d, want 200; body: %s", status, body)
	}
	got := pluginNamed(t, readPlugins(t, srv, token), "up-sink")
	if got.Version != "1.1.0" || got.Source != v2 {
		t.Fatalf("plugin = version %q source %q, want 1.1.0 from %q", got.Version, got.Source, v2)
	}
}

func TestAPastedURLThatIsNotAnUpgradeGetsTheVersionMessageNeverAnOverwrite(t *testing.T) {
	t.Parallel()
	host := newPackageHost(t)
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	_, other, _ := signing.GenerateKey()
	first := host.serve("first.zip", signedPackage(t, "up-sink", "1.1.0", priv))
	if status, body := postFromURL(t, srv, token, first); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}

	for _, tc := range []struct {
		name     string
		archive  []byte
		wantCode string
		wantIn   []string
	}{
		{"the same version", signedPackage(t, "up-sink", "1.1.0", priv), "PLUGIN_UPGRADE_VERSION", []string{"not newer"}},
		{"a lower version", signedPackage(t, "up-sink", "1.0.0", priv), "PLUGIN_UPGRADE_VERSION", []string{"not newer"}},
		{"a different key", signedPackage(t, "up-sink", "1.2.0", other), "PLUGIN_UPGRADE_PUBLISHER", []string{"no key rotation", "key id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := host.serve("candidate.zip", tc.archive)
			status, body := postFromURL(t, srv, token, url)
			refusal := refusalOf(t, http.StatusConflict, status, body)
			if refusal.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %s (never PLUGIN_DUPLICATE); body: %s", refusal.Error.Code, tc.wantCode, body)
			}
			for _, in := range tc.wantIn {
				if !strings.Contains(refusal.Error.Message, in) {
					t.Fatalf("message = %q, want it to contain %q", refusal.Error.Message, in)
				}
			}
			if got := pluginNamed(t, readPlugins(t, srv, token), "up-sink"); got.Version != "1.1.0" || got.Source != first {
				t.Fatalf("a refused upgrade left %q from %q, want 1.1.0 from %q untouched", got.Version, got.Source, first)
			}
		})
	}
}

func TestAWideningPackageFromAURLReturnsThe202Preview(t *testing.T) {
	t.Parallel()
	host := newPackageHost(t)
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	v1 := host.serve("v1.zip", signedPackage(t, "up-sink", "1.0.0", priv))
	v2 := host.serve("v2.zip", wideningPackage(t, "up-sink", "1.1.0", priv))
	if status, body := postFromURL(t, srv, token, v1); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}

	status, body := postFromURL(t, srv, token, v2)
	if status != http.StatusAccepted {
		t.Fatalf("widening URL upgrade status = %d, want 202; body: %s", status, body)
	}
	if got := pluginNamed(t, readPlugins(t, srv, token), "up-sink"); got.Version != "1.0.0" || got.Source != v1 {
		t.Fatalf("staging changed the plugin to %q from %q, want 1.0.0 untouched", got.Version, got.Source)
	}
}

func TestAnUpgradeFetchStillRefusesAPrivateFirstHop(t *testing.T) {
	t.Parallel()
	host := newPackageHost(t)
	_, priv, _ := signing.GenerateKey()
	// Installed by upload, because the production policy refuses this fixture's
	// 127.0.0.1 address for any URL fetch.
	srv := testharness.New(t)
	token := adminToken(t, srv)
	if status, body := uploadPackage(t, srv, token, signedPackage(t, "up-sink", "1.0.0", priv)); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}
	url := host.serve("v2.zip", signedPackage(t, "up-sink", "1.1.0", priv))

	status, body := postFromURL(t, srv, token, url)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if refusal.Error.Code != "PLUGIN_SOURCE_REFUSED" || !strings.Contains(refusal.Error.Message, "upload the package instead") {
		t.Fatalf("got %s %q, want the pasted-address refusal", refusal.Error.Code, refusal.Error.Message)
	}
	if n := host.hits.Load(); n != 0 {
		t.Fatalf("the refused upgrade fetch reached the host %d times, want 0", n)
	}
}

// --- the catalog's "Update available" hint ---------------------------------------

func catalogEntryFor(id, version, packageURL string) pluginapi.CatalogEntry {
	return pluginapi.CatalogEntry{ID: id, Name: id, Version: version, PackageURL: packageURL}
}

func installUploaded(t *testing.T, srv *testharness.Server, token, id, version string, priv ed25519.PrivateKey) {
	t.Helper()
	if status, body := uploadPackage(t, srv, token, signedPackage(t, id, version, priv)); status != http.StatusCreated {
		t.Fatalf("installing %s %s: status = %d; body: %s", id, version, status, body)
	}
}

func TestACatalogEntryWithAHigherSemverIsFlaggedAndNothingElseIs(t *testing.T) {
	t.Parallel()
	packages := newPackageHost(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	installUploaded(t, srv, token, "up-newer", "1.0.0", priv)
	installUploaded(t, srv, token, "up-same", "1.0.0", priv)
	installUploaded(t, srv, token, "up-lower", "2.0.0", priv)
	installUploaded(t, srv, token, "up-wordy", "nightly", priv)
	installUploaded(t, srv, token, "up-entry-wordy", "1.0.0", priv)

	index := catalogServing(t,
		catalogEntryFor("up-newer", "1.10.0", packages.URL+"/a.zip"),
		catalogEntryFor("up-same", "1.0.0", packages.URL+"/b.zip"),
		catalogEntryFor("up-lower", "1.9.9", packages.URL+"/c.zip"),
		catalogEntryFor("up-wordy", "2.0.0", packages.URL+"/d.zip"),
		catalogEntryFor("up-entry-wordy", "latest", packages.URL+"/e.zip"),
		catalogEntryFor("up-absent", "9.0.0", packages.URL+"/f.zip"),
		catalogEntryFor("up-noversion", "", packages.URL+"/g.zip"),
	)
	view := setCatalog(t, srv, token, index.URL)

	byID := map[string]catalogEntryResp{}
	for _, e := range view.Entries {
		byID[e.ID] = e
	}
	if len(byID) != 7 {
		t.Fatalf("the catalog listed %d entries, want 7: %+v", len(byID), view.Entries)
	}
	if e := byID["up-newer"]; !e.UpdateAvailable || e.InstalledVersion != "1.0.0" {
		t.Fatalf("up-newer = %+v, want updateAvailable with installedVersion 1.0.0", e)
	}
	for _, id := range []string{"up-same", "up-lower", "up-wordy", "up-entry-wordy", "up-absent", "up-noversion"} {
		if e := byID[id]; e.UpdateAvailable || e.InstalledVersion != "" {
			t.Fatalf("%s = %+v, want no flag", id, e)
		}
	}
}

func TestListingACatalogFetchesNoPackageAndInstallsNothing(t *testing.T) {
	t.Parallel()
	packages := newPackageHost(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	installUploaded(t, srv, token, "up-sink", "1.0.0", priv)
	packages.serve("up.zip", signedPackage(t, "up-sink", "2.0.0", priv))

	index := catalogServing(t,
		catalogEntryFor("up-sink", "2.0.0", packages.URL+"/up.zip"),
		catalogEntryFor("fresh", "1.0.0", packages.URL+"/fresh.zip"))
	view := setCatalog(t, srv, token, index.URL)
	_ = readCatalog(t, srv, token)

	if len(view.Entries) != 2 || !view.Entries[0].UpdateAvailable {
		t.Fatalf("entries = %+v, want the first flagged", view.Entries)
	}
	if n := packages.hits.Load(); n != 0 {
		t.Fatalf("listing the catalog requested a package URL %d times, want 0", n)
	}
	list := readPlugins(t, srv, token)
	if got := pluginNamed(t, list, "up-sink"); got.Version != "1.0.0" {
		t.Fatalf("listing upgraded up-sink to %q", got.Version)
	}
	if got := notShipped(list.Plugins); len(got) != 1 {
		t.Fatalf("listing left %+v installed, want only up-sink", got)
	}
}

func TestAFlaggedCatalogEntrySignedByAnotherKeyStillRefusesOnInstall(t *testing.T) {
	t.Parallel()
	packages := newPackageHost(t)
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	_, other, _ := signing.GenerateKey()
	installUploaded(t, srv, token, "up-sink", "1.0.0", priv)
	url := packages.serve("up.zip", signedPackage(t, "up-sink", "2.0.0", other))

	index := catalogServing(t, catalogEntryFor("up-sink", "2.0.0", url))
	view := setCatalog(t, srv, token, index.URL)
	if !view.Entries[0].UpdateAvailable {
		t.Fatalf("entry = %+v, want it flagged: the flag is a hint", view.Entries[0])
	}
	status, body := postFromURL(t, srv, token, url)
	if refusal := refusalOf(t, http.StatusConflict, status, body); refusal.Error.Code != "PLUGIN_UPGRADE_PUBLISHER" {
		t.Fatalf("code = %q, want PLUGIN_UPGRADE_PUBLISHER", refusal.Error.Code)
	}
}
