package plugins_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/safefetch"
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

// fetchingURL is a stand-in's address that has the test guest fetch target once.
// The target rides in the operator's URL rather than in URL2 because URL2 is an
// address the operator typed too, and exempt as the base URL is.
func fetchingURL(base, target string) string {
	return standInURL(base, "fetch-once") + "&obelo-fetch=" + url.QueryEscape(target)
}

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
		Enabled:    true,
		Secret:     "a-client-name",
		URL:        standInURL(src.URL, "fetch-once"),
		URLEntered: true,
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
		Enabled:    true,
		Secret:     "a-client-name",
		URL:        fetchingURL(src.URL, "http://169.254.169.254:9001/httpapi?request=anime&aid=1"),
		URLEntered: true,
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

// countingStandIn is a loopback stand-in that counts the requests that reached it.
// A count above zero is a connection the address rule should have refused.
func countingStandIn(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(src.Close)
	return src, &hits
}

// onLocalhost is a loopback stand-in's URL spelled with the NAME localhost, so the
// host is not the operator's 127.0.0.1 and the transport resolves it itself.
func onLocalhost(t *testing.T, src *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(src.URL)
	if err != nil {
		t.Fatalf("the stand-in's URL is unparseable: %v", err)
	}
	return "http://localhost:" + u.Port() + "/"
}

// TestANameThatRebindsIntoPrivateSpaceIsRefusedAtTheDial is DNS rebinding: an
// allowlisted name answers PUBLIC to the host's own address check and PRIVATE to
// the lookup the connection is made from. The first answer is the test's
// (ResolveAs), the second is the system resolver's answer for localhost, and the
// address rule has to hold against the second, because that is where the bytes go.
func TestANameThatRebindsIntoPrivateSpaceIsRefusedAtTheDial(t *testing.T) {
	plugins.ResolveAs(t, "203.0.113.7")
	op, _ := countingStandIn(t)
	inner, hits := countingStandIn(t)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir,
		plugintest.MetadataProviderManifest("rebind-probe", fullMusicProvides(), "localhost"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	provider, _ := providerFor(t, set, "rebind-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        fetchingURL(op.URL, onLocalhost(t, inner)),
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the private stand-in was reached %d times: the address check judged a lookup, "+
			"not the address dialed", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=rebind-probe", "host=localhost") {
		t.Fatalf("no audit line for the refused dial:\n%s", log.all())
	}
}

// TestARedirectIntoPrivateSpaceIsRefusedAtTheDial: the operator's own host is
// reached, and the redirect it answers with — into loopback, under another name —
// is not. The redirect policy refuses this by its own lookup too; the dial check
// is what still refuses it when that lookup is answered differently.
func TestARedirectIntoPrivateSpaceIsRefusedAtTheDial(t *testing.T) {
	inner, hits := countingStandIn(t)
	target := onLocalhost(t, inner)
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bounce" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(op.Close)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("bounce-probe", fullMusicProvides()))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	provider, _ := providerFor(t, set, "bounce-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        standInURL(op.URL, "fetch-once"),
		URL2:       op.URL + "/bounce",
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect into loopback was followed %d times", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
}

// TestTheDialCheckRefusesPrivateSpaceAndAllowsTheTailnet: the judgement made of
// the address actually connected to. 100.64.0.0/10 is shared address space and a
// Tailscale tailnet's, and it stays reachable.
func TestTheDialCheckRefusesPrivateSpaceAndAllowsTheTailnet(t *testing.T) {
	for _, addr := range []string{"203.0.113.7:443", "100.64.0.1:443", "100.127.255.254:80", "[2001:db8::1]:443"} {
		if err := plugins.CheckDialedAddress(addr); err != nil {
			t.Errorf("dial to %s refused: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:80", "10.0.0.1:80", "192.168.1.2:443", "169.254.169.254:80",
		"0.0.0.0:80", "[::]:80", "172.16.0.1:80", "172.31.255.254:443", "[fc00::1]:80", "[fd12:3456::1]:443",
		"[::1]:80", "[fe80::1%en0]:80", "[::ffff:127.0.0.1]:80", "not-an-address"} {
		if err := plugins.CheckDialedAddress(addr); err == nil {
			t.Errorf("dial to %s allowed", addr)
		}
	}
}

// proxyStandIn is a forward proxy on loopback, as an operator's HTTP_PROXY may
// well be: it answers every absolute-form request itself and records the host
// that request was for, which is all "forwarded" has to mean here.
func proxyStandIn(t *testing.T) (*http.Client, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var forwarded atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		forwarded.Store(r.URL.Host)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(proxy.Close)
	u, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("the proxy's URL is unparseable: %v", err)
	}
	// http.DefaultTransport reads HTTP_PROXY and HTTPS_PROXY once per process, so
	// the proxy is configured the way that reading configures it: Transport.Proxy.
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}, &hits, &forwarded
}

// TestAFetchGoesThroughAProxyOnAPrivateAddress: an operator's HTTP(S) proxy is
// theirs to put where they like, a LAN box included, so the dial to it is not
// judged by the private-address rule. The target is, by its lookup, and passes.
func TestAFetchGoesThroughAProxyOnAPrivateAddress(t *testing.T) {
	plugins.ResolveAs(t, "203.0.113.7")
	client, hits, forwarded := proxyStandIn(t)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir,
		plugintest.MetadataProviderManifest("proxy-probe", fullMusicProvides(), "api.example.test"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{HTTPClient: client})
	provider, _ := providerFor(t, set, "proxy-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        standInURL("http://operator.example.test", "fetch-once"),
		URL2:       "http://api.example.test/data",
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("the fetch through a loopback proxy did not arrive: %q\n%s", resp.Detail, log.all())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the proxy was reached %d times, want 1", n)
	}
	if got, _ := forwarded.Load().(string); got != "api.example.test" {
		t.Errorf("the proxy was asked for %q, want api.example.test", got)
	}
}

// TestAProxyDoesNotLaunderAPrivateTarget is the other half: the proxy's
// exemption is the proxy's, and a target that resolves into private space is
// refused before anything is sent to it.
func TestAProxyDoesNotLaunderAPrivateTarget(t *testing.T) {
	plugins.ResolveAs(t, "10.0.0.5")
	client, hits, _ := proxyStandIn(t)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir,
		plugintest.MetadataProviderManifest("proxy-private-probe", fullMusicProvides(), "internal.example.test"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{HTTPClient: client})
	provider, _ := providerFor(t, set, "proxy-private-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        fetchingURL("http://operator.example.test", "http://internal.example.test/data"),
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the proxy was asked %d times for a private target", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=proxy-private-probe", "host=internal.example.test") {
		t.Fatalf("no audit line for the private target:\n%s", log.all())
	}
}

// TestARedirectToTheOperatorsHostOnAnotherPortIsCheckedAtTheDial: the operator
// typed a host AND a port, and the exemption at the dial is exactly that
// address. Their host on another port — a redirect's Location can name any — is
// judged like every other dial.
func TestARedirectToTheOperatorsHostOnAnotherPortIsCheckedAtTheDial(t *testing.T) {
	inner, hits := countingStandIn(t)
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bounce" {
			http.Redirect(w, r, inner.URL+"/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(op.Close)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("port-bounce-probe", fullMusicProvides()))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	provider, _ := providerFor(t, set, "port-bounce-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        standInURL(op.URL, "fetch-once"),
		URL2:       op.URL + "/bounce",
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the operator's host on another port was reached %d times", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
}

// TestAGuestFetchToTheOperatorsHostOnAnotherPortIsRefused: the operator typed a
// host AND a port, and the exemption is exactly that address. The guest asking
// for their host on another port directly — not by redirect — is a manifest
// fetch like any other, and their host is loopback here, so it is refused.
func TestAGuestFetchToTheOperatorsHostOnAnotherPortIsRefused(t *testing.T) {
	inner, hits := countingStandIn(t)
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(op.Close)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("port-direct-probe", fullMusicProvides(), "127.0.0.1"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	provider, _ := providerFor(t, set, "port-direct-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        fetchingURL(op.URL, inner.URL+"/"),
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the operator's host on another port was reached %d times", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=port-direct-probe", "host=127.0.0.1", "reason=private-address") {
		t.Fatalf("no audit line for the private-address refusal:\n%s", log.all())
	}
}

// TestTheOperatorsConfiguredPortIsStillReached is the other half: the address
// the operator typed, private as it is, is still the guest's to reach.
func TestTheOperatorsConfiguredPortIsStillReached(t *testing.T) {
	op, hits := countingStandIn(t)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("port-own-probe", fullMusicProvides()))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	provider, _ := providerFor(t, set, "port-own-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        standInURL(op.URL, "fetch-once"),
		URL2:       op.URL + "/lookup",
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("detail = %q, want the operator's own address reached", resp.Detail)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the operator's address was reached %d times, want 1", n)
	}
}

// TestTheOperatorsSecondURLIsReachedAtItsOwnPort: the second URL (an image host,
// a CDN) is one the operator typed as surely as the base URL, so it is exempt at
// exactly its own host and port too — here the base URL's private box on another
// port. A third port on that box is neither, and the guest asking for it is
// refused.
func TestTheOperatorsSecondURLIsReachedAtItsOwnPort(t *testing.T) {
	op, _ := countingStandIn(t)
	images, imageHits := countingStandIn(t)
	third, thirdHits := countingStandIn(t)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("url2-probe", fullMusicProvides(), "127.0.0.1"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})

	reached, _ := providerFor(t, set, "url2-probe", pluginapi.Settings{
		Enabled:     true,
		Secret:      "a-key",
		URL:         standInURL(op.URL, "fetch-once"),
		URL2:        images.URL + "/art",
		URLEntered:  true,
		URL2Entered: true,
	})
	resp, err := reached.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("detail = %q, want the operator's second URL reached", resp.Detail)
	}
	if n := imageHits.Load(); n != 1 {
		t.Fatalf("the second URL was reached %d times, want 1", n)
	}

	refused, _ := providerFor(t, set, "url2-probe", pluginapi.Settings{
		Enabled:     true,
		Secret:      "a-key",
		URL:         fetchingURL(op.URL, third.URL+"/"),
		URL2:        images.URL + "/art",
		URLEntered:  true,
		URL2Entered: true,
	})
	resp, err = refused.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := thirdHits.Load(); n != 0 {
		t.Fatalf("a third port on the operator's box was reached %d times", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=url2-probe", "host=127.0.0.1", "reason=private-address") {
		t.Fatalf("no audit line for the private-address refusal:\n%s", log.all())
	}
}

// metadataAddressURL is the cloud-metadata address as a plugin's base URL, with the
// mode marker that has the test guest fetch that same address once.
const metadataAddressURL = "http://169.254.169.254/?obelo-mode=fetch-once&obelo-fetch=http%3A%2F%2F169.254.169.254%2F"

// TestADefaultURLOnTheMetadataAddressIsRefused: a plugin whose manifest DEFAULT is
// the cloud-metadata address, and whose Admin typed no URL, is the plugin author's
// choice of address and not the operator's, so it gets the private-address rule
// like any other target. The fetch goes through a proxy stand-in so that a
// fetch the rule let through is seen arriving rather than dialed for real.
func TestADefaultURLOnTheMetadataAddressIsRefused(t *testing.T) {
	client, hits, _ := proxyStandIn(t)
	manifest := plugintest.MetadataProviderManifest("default-metadata-probe", fullMusicProvides())
	manifest.Settings.DefaultURL = "http://169.254.169.254/"

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, manifest)
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{HTTPClient: client})
	provider, _ := providerFor(t, set, "default-metadata-probe", pluginapi.Settings{
		Enabled: true,
		Secret:  "a-key",
		URL:     metadataAddressURL,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the metadata address was fetched %d times from a default nobody typed", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}
	if !log.contains(t, "plugin audit", "plugin=default-metadata-probe", "host=169.254.169.254") {
		t.Fatalf("no audit line for the refused default:\n%s", log.all())
	}
}

// TestTheMetadataAddressEnteredByAnAdminIsStillReached is the other half: the same
// address TYPED by an Admin is the operator's choice, and reached as before.
func TestTheMetadataAddressEnteredByAnAdminIsStillReached(t *testing.T) {
	client, hits, forwarded := proxyStandIn(t)
	manifest := plugintest.MetadataProviderManifest("entered-metadata-probe", fullMusicProvides())
	manifest.Settings.DefaultURL = "http://169.254.169.254/"

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, manifest)
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{HTTPClient: client})
	provider, _ := providerFor(t, set, "entered-metadata-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        metadataAddressURL,
		URLEntered: true,
	})

	resp, err := provider.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("detail = %q, want the Admin's own address reached\n%s", resp.Detail, log.all())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the Admin's address was fetched %d times, want 1", n)
	}
	if got, _ := forwarded.Load().(string); got != "169.254.169.254" {
		t.Errorf("the fetch was for %q, want 169.254.169.254", got)
	}
}

// TestADefaultURLThatRebindsIntoPrivateSpaceIsRefusedAtTheDial: a default URL
// gets the dial check too, not only the lookup — here an allowlisted name that
// answers public to the lookup and loopback to the dial. Typed by an Admin, the
// same URL is the operator's and is reached.
func TestADefaultURLThatRebindsIntoPrivateSpaceIsRefusedAtTheDial(t *testing.T) {
	plugins.ResolveAs(t, "203.0.113.7")
	src, hits := countingStandIn(t)
	base := onLocalhost(t, src)
	self := standInURL(strings.TrimSuffix(base, "/"), "fetch-once") + "&obelo-fetch=" + url.QueryEscape(base)
	manifest := plugintest.MetadataProviderManifest("default-rebind-probe", fullMusicProvides(), "localhost")
	manifest.Settings.DefaultURL = self

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, manifest)
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})

	defaulted, _ := providerFor(t, set, "default-rebind-probe", pluginapi.Settings{
		Enabled: true,
		Secret:  "a-key",
		URL:     self,
	})
	resp, err := defaulted.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a default that rebinds into loopback was reached %d times", n)
	}
	if !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("detail = %q, want a refusal", resp.Detail)
	}

	entered, _ := providerFor(t, set, "default-rebind-probe", pluginapi.Settings{
		Enabled:    true,
		Secret:     "a-key",
		URL:        self,
		URLEntered: true,
	})
	resp, err = entered.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("detail = %q, want the Admin's own URL reached\n%s", resp.Detail, log.all())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the Admin's own URL was reached %d times, want 1", n)
	}
}

// TestAnAddressATestNamesIsReachedFromADefaultURL: Options.ExemptFetchAddrs, which
// only a test can set, has the fetch policy treat exactly the host and port it
// names as the operator's, so a suite's loopback stand-in can serve a manifest
// default. Without it the same default is refused, and with it the same host on
// another port still is.
func TestAnAddressATestNamesIsReachedFromADefaultURL(t *testing.T) {
	src, hits := countingStandIn(t)
	other, otherHits := countingStandIn(t)
	self := fetchingURL(src.URL, src.URL+"/")
	manifest := plugintest.MetadataProviderManifest("named-exempt-probe", fullMusicProvides(), "127.0.0.1")
	manifest.Settings.DefaultURL = self

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, manifest)
	log := &logSink{}
	plain := loadWith(t, dataDir, log, plugins.Options{})
	named := loadWith(t, dataDir, log, plugins.Options{ExemptFetchAddrs: []string{src.Listener.Addr().String()}})

	refused, _ := providerFor(t, plain, "named-exempt-probe", pluginapi.Settings{Enabled: true, Secret: "a-key", URL: self})
	resp, err := refused.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := hits.Load(); n != 0 || !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("with no address named, the loopback default was reached %d times (detail %q)", n, resp.Detail)
	}

	reached, _ := providerFor(t, named, "named-exempt-probe", pluginapi.Settings{Enabled: true, Secret: "a-key", URL: self})
	resp, err = reached.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
		t.Fatalf("detail = %q, want the named address reached\n%s", resp.Detail, log.all())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the named address was reached %d times, want 1", n)
	}

	elsewhere, _ := providerFor(t, named, "named-exempt-probe", pluginapi.Settings{Enabled: true, Secret: "a-key", URL: fetchingURL(src.URL, other.URL+"/")})
	resp, err = elsewhere.Lookup(context.Background(), albumRef())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if n := otherHits.Load(); n != 0 || !strings.Contains(resp.Detail, "refused") {
		t.Fatalf("another port on the named host was reached %d times (detail %q)", n, resp.Detail)
	}
}

// operatorDial is the operator client dialCheckedClients builds, and a context
// exempting exactly op's host and port, as a fetch to the address the operator
// typed carries it.
func operatorDial(t *testing.T, op *httptest.Server) (*http.Client, context.Context) {
	t.Helper()
	u, err := url.Parse(op.URL)
	if err != nil {
		t.Fatalf("the stand-in's URL is unparseable: %v", err)
	}
	_, operator := plugins.DialCheckedClients(&http.Client{})
	return operator, plugins.ExemptDialTo(context.Background(), u.Hostname(), u.Port())
}

// get is one GET of target on c under ctx, its body drained.
func get(ctx context.Context, c *http.Client, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// bouncer is the operator's box: it answers /bounce with a redirect to target,
// and counts every request it is sent.
func bouncer(t *testing.T, target string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(op.Close)
	return op, &hits
}

// TestARedirectThatRebindsOffTheOperatorsHostIsRefusedAtTheDial: the operator
// client's redirect hop is dialed under the check, with nothing in front of it.
// Its redirect policy here is a lookup check of its own that the name passes —
// public to the lookup, loopback to the dial — so only the dial is left to refuse.
func TestARedirectThatRebindsOffTheOperatorsHostIsRefusedAtTheDial(t *testing.T) {
	plugins.ResolveAs(t, "203.0.113.7")
	inner, hits := countingStandIn(t)
	op, opHits := bouncer(t, onLocalhost(t, inner))
	operator, ctx := operatorDial(t, op)
	operator.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		addrs, err := plugins.LookupIPAddr(req.Context(), req.URL.Hostname())
		if err != nil {
			return err
		}
		for _, a := range addrs {
			if safefetch.IsInternalIP(a.IP) {
				return safefetch.ErrRedirectBlocked
			}
		}
		return nil
	}

	err := get(ctx, operator, op.URL+"/bounce")
	if n := opHits.Load(); n != 1 {
		t.Fatalf("the operator's own address was reached %d times, want 1", n)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect that rebinds into loopback was dialed unchecked %d times", n)
	}
	if err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("err = %v, want the dial check's refusal", err)
	}
}

// TestAFollowedRedirectToTheOperatorsHostOnAnotherPortIsRefusedAtTheDial: the
// dial exemption is the operator's host AND port. A redirect policy that lets
// every hop through leaves only the dial to judge their host on another port,
// and it is judged like any other address.
func TestAFollowedRedirectToTheOperatorsHostOnAnotherPortIsRefusedAtTheDial(t *testing.T) {
	inner, hits := countingStandIn(t)
	op, opHits := bouncer(t, inner.URL+"/")
	operator, ctx := operatorDial(t, op)
	operator.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }

	err := get(ctx, operator, op.URL+"/bounce")
	if n := opHits.Load(); n != 1 {
		t.Fatalf("the operator's own address was reached %d times, want 1", n)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the operator's host on another port was dialed unchecked %d times", n)
	}
	if err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("err = %v, want the dial check's refusal", err)
	}
}

// TestTheOperatorClientReusesNoConnection: a connection the operator client
// dialed unchecked is never handed to another request. Two fetches to the
// operator's address open two connections, and a request with no exemption to
// the same private address is dialed afresh, under the check, and refused.
func TestTheOperatorClientReusesNoConnection(t *testing.T) {
	var conns, hits atomic.Int32
	op := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	op.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	op.Start()
	t.Cleanup(op.Close)
	operator, ctx := operatorDial(t, op)

	for i := 0; i < 2; i++ {
		if err := get(ctx, operator, op.URL+"/"); err != nil {
			t.Fatalf("fetch %d to the operator's address: %v", i+1, err)
		}
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("two fetches opened %d connections, want 2: a connection was reused", n)
	}
	err := get(context.Background(), operator, op.URL+"/")
	if n := hits.Load(); n != 2 {
		t.Fatalf("the private address was reached %d times, want 2: a request with no exemption "+
			"was sent on a connection dialed unchecked", n)
	}
	if err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("err = %v, want the dial check's refusal", err)
	}
}

// TestAnEnteredURLInAnotherCaseOrWithATrailingDotIsStillTheOperators: a host
// name is compared as normalizeHost spells it, at the fetch and at the dial, so
// the Admin's URL typed in capitals or with a DNS trailing dot is still their
// address and is reached.
func TestAnEnteredURLInAnotherCaseOrWithATrailingDotIsStillTheOperators(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("spelling-probe", fullMusicProvides()))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})

	for _, host := range []string{"LOCALHOST", "localhost."} {
		op, hits := countingStandIn(t)
		u, err := url.Parse(op.URL)
		if err != nil {
			t.Fatalf("the stand-in's URL is unparseable: %v", err)
		}
		base := "http://" + host + ":" + u.Port()
		provider, _ := providerFor(t, set, "spelling-probe", pluginapi.Settings{
			Enabled:    true,
			Secret:     "a-key",
			URL:        fetchingURL(base, base+"/lookup"),
			URLEntered: true,
		})
		resp, err := provider.Lookup(context.Background(), albumRef())
		if err != nil {
			t.Fatalf("%s: Lookup: %v", host, err)
		}
		if strings.Contains(resp.Detail, "refused") || strings.Contains(resp.Detail, "failed") {
			t.Fatalf("%s: detail = %q, want the Admin's own URL reached\n%s", host, resp.Detail, log.all())
		}
		if n := hits.Load(); n != 1 {
			t.Fatalf("%s: the Admin's own URL was reached %d times, want 1", host, n)
		}
	}
}
