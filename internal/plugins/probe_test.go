package plugins_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The install-time probe (ADR-0069 Q6): a module is compiled, STARTED once and checked
// for every export its claims need before an install or an upgrade touches anything.

func wasmSection(id byte, content []byte) []byte {
	return append([]byte{id, byte(len(content))}, content...)
}

func wasmName(s string) []byte { return append([]byte{byte(len(s))}, s...) }

// bareModule is the smallest module the loader accepts: memory, obelo_alloc,
// obelo_free and _initialize, and NO contract call. trapInit makes _initialize execute
// `unreachable`.
func bareModule(trapInit bool) []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, wasmSection(1, []byte{3, 0x60, 1, 0x7f, 1, 0x7f, 0x60, 1, 0x7f, 0, 0x60, 0, 0})...)
	out = append(out, wasmSection(3, []byte{3, 0, 1, 2})...)
	out = append(out, wasmSection(5, []byte{1, 0, 1})...)
	ex := []byte{4}
	ex = append(append(ex, wasmName("memory")...), 2, 0)
	ex = append(append(ex, wasmName("obelo_alloc")...), 0, 0)
	ex = append(append(ex, wasmName("obelo_free")...), 0, 1)
	ex = append(append(ex, wasmName("_initialize")...), 0, 2)
	out = append(out, wasmSection(7, ex)...)
	init := []byte{3, 0, 0x01, 0x0b} // nop
	if trapInit {
		init = []byte{3, 0, 0x00, 0x0b} // unreachable
	}
	code := []byte{3, 4, 0, 0x41, 0x10, 0x0b, 2, 0, 0x0b}
	code = append(code, init...)
	return append(out, wasmSection(10, code)...)
}

func bareArchive(t *testing.T, id, version string, module []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	m := plugintest.SinkManifest(id)
	m.Version = version
	manifest := plugintest.ManifestJSON(t, m)
	var doc []byte
	if priv != nil {
		sig, err := signing.Sign(priv, upgradePublisher, manifest, module)
		if err != nil {
			t.Fatal(err)
		}
		if doc, err = signing.Encode(sig); err != nil {
			t.Fatal(err)
		}
	}
	return plugintest.PackageZip(t, manifest, module, doc)
}

var probeCases = []struct {
	name   string
	module []byte
	wantIn []string
}{
	{"a sink with no deliver export", bareModule(false), []string{`"deliver"`, "event-sink"}},
	{"a module whose _initialize traps", bareModule(true), []string{"could not be started"}},
}

func TestAFreshInstallRefusesAModuleThatDoesNotStartOrLacksAClaimedExport(t *testing.T) {
	plugins.Parallel(t)
	for _, tc := range probeCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			_, err := f.manager.InstallPackage(context.Background(), bareArchive(t, "probe-sink", "1.0.0", tc.module, nil), plugins.SourceUpload)
			if refusalReason(err) != plugins.ReasonModule {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonModule)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message %q lacks %q", err, want)
				}
			}
			assertPluginsDirEmpty(t, f.dir)
			if rows, _ := f.store.Plugins(); len(rows) != 0 {
				t.Fatalf("a refused install left %d rows", len(rows))
			}
		})
	}
}

func TestAnUpgradeRefusesAModuleThatDoesNotStartOrLacksAClaimedExportAndTouchesNothing(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	for _, tc := range probeCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "probe-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			before := takeSnapshot(t, f, "probe-sink")

			_, err := upgrade(f, bareArchive(t, "probe-sink", "1.1.0", tc.module, priv))
			if refusalReason(err) != plugins.ReasonModule {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonModule)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message %q lacks %q", err, want)
				}
			}
			assertUnchanged(t, f, "probe-sink", before)
			if err := deliverThroughRegistry(t, f, "probe-sink"); err != nil {
				t.Fatalf("the old version no longer answers: %v", err)
			}
		})
	}
}

// --- the probe grants nothing ------------------------------------------------------

// callingModule is a module whose _initialize calls the host function importName with
// the JSON payload, then (optionally) exports deliver. A refused host function answers
// an error value, not a trap, so the module still starts.
func callingModule(importName, payload string, withDeliver bool) []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	// types: 0 (i32)->i32, 1 (i32)->(), 2 ()->(), 3 (i32,i32)->i64
	out = append(out, wasmSection(1, []byte{4, 0x60, 1, 0x7f, 1, 0x7f, 0x60, 1, 0x7f, 0, 0x60, 0, 0, 0x60, 2, 0x7f, 0x7f, 1, 0x7e})...)
	imp := []byte{1}
	imp = append(imp, wasmName("obelo")...)
	imp = append(imp, wasmName(importName)...)
	imp = append(imp, 0x00, 3)
	out = append(out, wasmSection(2, imp)...)
	funcs, nExports := []byte{3, 0, 1, 2}, byte(4)
	if withDeliver {
		funcs, nExports = []byte{4, 0, 1, 2, 3}, 5
	}
	out = append(out, wasmSection(3, funcs)...)
	out = append(out, wasmSection(5, []byte{1, 0, 1})...)
	ex := []byte{nExports}
	ex = append(append(ex, wasmName("memory")...), 2, 0)
	ex = append(append(ex, wasmName("obelo_alloc")...), 0, 1)
	ex = append(append(ex, wasmName("obelo_free")...), 0, 2)
	ex = append(append(ex, wasmName("_initialize")...), 0, 3)
	if withDeliver {
		ex = append(append(ex, wasmName("deliver")...), 0, 4)
	}
	out = append(out, wasmSection(7, ex)...)
	// _initialize: call the import with (ptr 1024, len) and drop the result.
	body := []byte{0, 0x41, 0x80, 0x08, 0x41, byte(len(payload)), 0x10, 0x00, 0x1a, 0x0b}
	code := []byte{3, 4, 0, 0x41, 0x10, 0x0b, 2, 0, 0x0b, byte(len(body))}
	code = append(code, body...)
	if withDeliver {
		code[0] = 4
		code = append(code, 4, 0, 0x42, 0x00, 0x0b)
	}
	out = append(out, wasmSection(10, code)...)
	data := append([]byte{1, 0, 0x41, 0x80, 0x08, 0x0b, byte(len(payload))}, payload...)
	return append(out, wasmSection(11, data)...)
}

const kvPayload = `{"key":"pwned","value":"eA=="}`

type recordingKV struct {
	mu   sync.Mutex
	rows map[string]string
}

func (k *recordingKV) PluginKV(id, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.rows[id+"/"+key]
	return []byte(v), ok, nil
}

func (k *recordingKV) SetPluginKV(id, key string, v []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows[id+"/"+key] = string(v)
	return nil
}

func (k *recordingKV) DeletePluginKV(id, key string) error { return nil }

func (k *recordingKV) written() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.rows)
}

func kvFixture(t *testing.T) (*managerFixture, *recordingKV) {
	t.Helper()
	kv := &recordingKV{rows: map[string]string{}}
	return newManagerFixtureWith(t, func(c *plugins.ManagerConfig) { c.Loader.KV = kv }), kv
}

func TestTheProbeWritesNoKeyValueRowsForARefusedFreshInstall(t *testing.T) {
	plugins.Parallel(t)
	for name, module := range map[string][]byte{
		"a missing export": callingModule("kv_set", kvPayload, false),
	} {
		t.Run(name, func(t *testing.T) {
			f, kv := kvFixture(t)
			_, err := f.manager.InstallPackage(context.Background(), bareArchive(t, "kv-sink", "1.0.0", module, nil), plugins.SourceUpload)
			if refusalReason(err) != plugins.ReasonModule {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonModule)
			}
			if n := kv.written(); n != 0 {
				t.Fatalf("the probe of a refused install wrote %d key-value rows", n)
			}
		})
	}
}

func TestTheProbeWritesNoKeyValueRowsForAnAcceptedInstall(t *testing.T) {
	plugins.Parallel(t)
	f, kv := kvFixture(t)
	if _, err := f.manager.InstallPackage(context.Background(), bareArchive(t, "kv-sink", "1.0.0", callingModule("kv_set", kvPayload, true), nil), plugins.SourceUpload); err != nil {
		t.Fatalf("a module that merely tries kv_set must still install: %v", err)
	}
	if n := kv.written(); n != 0 {
		t.Fatalf("installing wrote %d key-value rows before the plugin was ever called", n)
	}
}

func TestTheProbeOfAStagedAndCancelledUpgradeWritesNoKeyValueRows(t *testing.T) {
	plugins.Parallel(t)
	f, kv := kvFixture(t)
	mustInstall(t, f, upgradeArchive(t, "kv-up", "1.0.0", "v1", nil, "", nil))
	m := plugintest.SinkManifest("kv-up")
	m.Version = "1.1.0"
	m.Network.Hosts = []string{"widened.example.test"} // widens, so it is staged for confirmation
	module := callingModule("kv_set", kvPayload, true)
	got, err := upgrade(f, plugintest.PackageZip(t, plugintest.ManifestJSON(t, m), module, nil))
	if err != nil || got.Staged == nil {
		t.Fatalf("setup: want a staged upgrade, got %+v / %v", got.Staged, err)
	}
	if n := kv.written(); n != 0 {
		t.Fatalf("staging wrote %d key-value rows before the Admin confirmed", n)
	}
	if err := f.manager.CancelUpgrade(context.Background(), "kv-up", got.Staged.Staged); err != nil {
		t.Fatal(err)
	}
	if n := kv.written(); n != 0 {
		t.Fatalf("a cancelled upgrade left %d key-value rows", n)
	}
}

func TestTheProbeRefusesHTTPFetch(t *testing.T) {
	plugins.Parallel(t)
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	var mu sync.Mutex
	var lines []string
	f := newManagerFixtureWith(t, func(c *plugins.ManagerConfig) {
		c.Loader.Logf = func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
		}
	})
	payload := fmt.Sprintf(`{"url":%q}`, target.URL+"/")
	if _, err := f.manager.InstallPackage(context.Background(), bareArchive(t, "fetch-sink", "1.0.0", callingModule("http_fetch", payload, true), nil), plugins.SourceUpload); err != nil {
		t.Fatalf("a module that merely tries http_fetch must still install: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the probe reached the network %d times", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(lines, "\n"), "reason=no-network-call") {
		t.Fatalf("the fetch was not refused as a call with no network: %v", lines)
	}
}

// --- the probe's budget ------------------------------------------------------------

func TestTheProbeBudgetIsTheLargestTheManifestCouldGetAtRuntime(t *testing.T) {
	t.Parallel()
	load := func(m pluginapi.Manifest) *plugins.Plugin {
		dataDir := t.TempDir()
		plugintest.Install(t, dataDir, m)
		set, err := plugins.Load(context.Background(), dataDir+"/"+plugins.DirName, plugins.Options{Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close(context.Background()) })
		return set.Plugins()[0]
	}
	sink := load(plugintest.SinkManifest("budget-sink"))
	if got := plugins.ProbeBudget(sink); got != plugins.DefaultCallTimeout {
		t.Fatalf("a sink's probe budget = %s, want the call timeout %s", got, plugins.DefaultCallTimeout)
	}
	plain := load(plugintest.MetadataProviderManifest("budget-meta", pluginapi.ManifestProvides{}))
	if got := plugins.ProbeBudget(plain); got != plugins.DefaultMetadataCallBudget {
		t.Fatalf("a metadata provider's probe budget = %s, want its default %s", got, plugins.DefaultMetadataCallBudget)
	}
	slow := load(plugintest.MetadataProviderManifest("budget-slow", pluginapi.ManifestProvides{CallBudgetMillis: 90000}))
	if got := plugins.ProbeBudget(slow); got != 90*time.Second {
		t.Fatalf("a metadata provider declaring 90s has a probe budget of %s, want 90s", got)
	}
}

// --- the export mapping ------------------------------------------------------------

// Every extension point and every capability maps to the exports its adapter calls. A
// dropped mapping fails here.
func TestRequiredExportsFollowTheManifest(t *testing.T) {
	t.Parallel()
	prov := func(kind pluginapi.ExtensionPoint, caps ...pluginapi.Capability) pluginapi.Manifest {
		return pluginapi.Manifest{Provides: []pluginapi.ManifestProvides{{Kind: kind, Capabilities: caps}}}
	}
	for _, tc := range []struct {
		name string
		m    pluginapi.Manifest
		want []string
	}{
		{"event sink", prov(pluginapi.ExtensionEventSink), []string{"deliver"}},
		{"subtitle provider", prov(pluginapi.ExtensionSubtitleProvider), []string{"obelo_subtitle_search", "obelo_subtitle_download"}},
		{"web reference", prov(pluginapi.ExtensionWebReferenceProvider), []string{"web_reference_links"}},
		{"lyrics", prov(pluginapi.ExtensionLyricProvider), []string{"lyric_provider_lyrics"}},
		{"markers", prov(pluginapi.ExtensionMarkerProvider), []string{"marker_provider_markers"}},
		{"online source", prov(pluginapi.ExtensionOnlineSourceProvider),
			[]string{"online_source_rows", "online_source_row", "online_source_search", "online_source_resolve"}},
		{"metadata alone", prov(pluginapi.ExtensionMetadataProvider), []string{"metadata_lookup"}},
		{"metadata search", prov(pluginapi.ExtensionMetadataProvider, pluginapi.CapabilitySearch),
			[]string{"metadata_lookup", "metadata_search"}},
		{"metadata artwork", prov(pluginapi.ExtensionMetadataProvider, pluginapi.CapabilityArtworkCandidates),
			[]string{"metadata_lookup", "metadata_artwork_candidates"}},
		{"metadata episodes", prov(pluginapi.ExtensionMetadataProvider, pluginapi.CapabilityEpisodeList),
			[]string{"metadata_lookup", "metadata_series_seasons", "metadata_season_episodes"}},
		{"metadata albums", prov(pluginapi.ExtensionMetadataProvider, pluginapi.CapabilityAlbumTracklist),
			[]string{"metadata_lookup", "metadata_album_tracklist", "metadata_release_editions"}},
		{"metadata external ref", prov(pluginapi.ExtensionMetadataProvider, pluginapi.CapabilityExternalRef),
			[]string{"metadata_lookup", "metadata_external_ref"}},
		// search is a capability of a metadata provider only; a subtitle provider's search
		// is its extension point's mandatory call, already listed.
		{"subtitle search capability adds nothing", prov(pluginapi.ExtensionSubtitleProvider, pluginapi.CapabilitySearch),
			[]string{"obelo_subtitle_search", "obelo_subtitle_download"}},
		{"sign-in password", prov(pluginapi.ExtensionSignInProvider, pluginapi.CapabilityPasswordSignIn), []string{"sign_in_password"}},
		{"sign-in redirect", prov(pluginapi.ExtensionSignInProvider, pluginapi.CapabilityRedirectSignIn),
			[]string{"sign_in_authorize_url", "sign_in_exchange"}},
		{"sign-in lookup and refresh", prov(pluginapi.ExtensionSignInProvider, pluginapi.CapabilitySignInLookup, pluginapi.CapabilitySignInRefresh),
			[]string{"sign_in_lookup", "sign_in_refresh"}},
		{"two entries share an export once", pluginapi.Manifest{Provides: []pluginapi.ManifestProvides{
			{Kind: pluginapi.ExtensionEventSink}, {Kind: pluginapi.ExtensionEventSink}}}, []string{"deliver"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plugins.RequiredExportNames(tc.m)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("needs %v, want %v", got, tc.want)
			}
		})
	}
}
