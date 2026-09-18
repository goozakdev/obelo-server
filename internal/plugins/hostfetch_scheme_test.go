package plugins_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// What the host's fetch URL check makes of a target that is not https on port 443
// (.scratch/bundled-plugins: issue 05).
//
// It has a name now because a shipped plugin needs it: AniDB's published endpoint
// is `http://api.anidb.net:9001/httpapi` — PLAIN HTTP, on an EXPLICIT PORT — and
// the AniDB plugin's manifest allowlists `api.anidb.net`, which carries neither.
// If the check silently required https, or matched the allowlist against
// "api.anidb.net:9001", the provider would be refused on every call for a reason
// nothing in the manifest could express.
//
// It does neither, and these two tests are what say so. The rule is one sentence
// in each direction: a legitimate source on a legitimate port is reached, and the
// allowlist is still not a grant of the operator's private network.

// standInURL is a stand-in's address with the mode marker the test guest reads,
// which is how a test reaches inside the sandbox.
func standInURL(base, mode string) string { return base + "/?obelo-mode=" + mode }

// TestAPlainHTTPTargetOnAnExplicitPortIsFetched: the guest asks for a plain-http
// URL carrying an explicit, non-default port and a path, and the host fetches it.
//
// The stand-in is an httptest server, so its address is loopback on a port the OS
// chose — which is exactly the shape under test, and which the host permits here
// because it is the URL the OPERATOR configured (the documented asymmetry in
// hostfuncs.go).
func TestAPlainHTTPTargetOnAnExplicitPortIsFetched(t *testing.T) {
	var got struct {
		scheme string
		host   string
		path   string
		query  string
		hits   int
	}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.hits++
		got.scheme = "http" // httptest.NewServer is plain http by construction
		got.host = r.Host
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<anime id="1"><titles><title type="main">Cowboy Bebop</title></titles></anime>`))
	}))
	t.Cleanup(src.Close)

	base, err := url.Parse(src.URL)
	if err != nil {
		t.Fatalf("the stand-in's URL is unparseable: %v", err)
	}
	if base.Port() == "" {
		t.Fatalf("the stand-in has no explicit port (%q); this test proves nothing without one", src.URL)
	}

	dataDir := t.TempDir()
	// The manifest allowlists NOTHING, so the only host this guest may reach is the
	// one the operator configured — which is the stand-in, on its explicit port.
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("port-probe", fullMusicProvides()))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})

	provider, _ := providerFor(t, set, "port-probe", pluginapi.Settings{
		Enabled: true,
		Secret:  "a-client-name",
		URL:     standInURL(src.URL, "fetch-once"),
		// What the guest actually fetches: AniDB's own URL SHAPE, pointed at the
		// stand-in — plain http, explicit port, a path and a query.
		URL2: src.URL + "/httpapi?request=anime&client=a-client-name&clientver=1&protover=1&aid=1",
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("the host would not fetch a plain-http URL on an explicit port: %q", resp.Detail)
	}
	if got.hits != 1 {
		t.Fatalf("the stand-in was hit %d times, want 1", got.hits)
	}
	if got.scheme != "http" {
		t.Errorf("scheme = %q, want http", got.scheme)
	}
	if got.host != base.Host {
		t.Errorf("Host header = %q, want %q — the explicit port must survive the host's check",
			got.host, base.Host)
	}
	if got.path != "/httpapi" {
		t.Errorf("path = %q, want /httpapi", got.path)
	}
	if !strings.Contains(got.query, "aid=1") {
		t.Errorf("query = %q, want the guest's own query untouched", got.query)
	}
}

// TestAPrivateAddressOnAnExplicitPortIsStillRefused is the mirror image, and it
// is the half that must NOT bend: an explicit port does not buy a manifest host
// anything. 169.254.169.254:9001 is on this manifest's allowlist and is refused
// anyway, with the audit line an operator greps for.
//
// internal/plugins already proves this for an Event sink on the default port
// (TestAPrivateAddressIsRefusedEvenInsideTheAllowlist). This is the Metadata
// provider's, on a port, because "the endpoint is on :9001" is the sentence a
// plugin author would reach for if there were a hole here.
func TestAPrivateAddressOnAnExplicitPortIsStillRefused(t *testing.T) {
	dataDir := t.TempDir()
	// A stand-in for the OPERATOR's URL, so the guest is reachable at all; the
	// cloud-metadata address is what the manifest allowlists and what it fetches.
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(src.Close)

	plugintest.Install(t, dataDir,
		plugintest.MetadataProviderManifest("metadata-probe", fullMusicProvides(), "169.254.169.254"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})

	provider, _ := providerFor(t, set, "metadata-probe", pluginapi.Settings{
		Enabled: true,
		Secret:  "a-client-name",
		URL:     standInURL(src.URL, "fetch-once"),
		URL2:    "http://169.254.169.254:9001/httpapi?request=anime&aid=1",
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal — an allowlist entry is not a grant of the "+
			"operator's private network, whatever port it names", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=metadata-probe", "host=169.254.169.254", "reason=private-address") {
		t.Fatalf("no audit line for the private-address refusal:\n%s", log.all())
	}
}
