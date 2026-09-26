// Package plugintest builds the suite's Installed plugin from source and places
// it on disk the way an Admin does today: a directory under <dataDir>/plugins/
// named by the manifest's id, holding a manifest.json and a plugin.wasm.
//
// # Why from source, every time
//
// A checked-in 3.4 MiB module would rot exactly the way
// internal/webui/dist/index.html did — silently, while every guard stayed green —
// and nobody reviews a binary diff. So the module is compiled by the ordinary
// command, from the source next to it, when a test first asks for it:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared
//
// That is the same command a Plugin author runs, with the same toolchain, which
// means this package also proves the authoring instructions work. It costs a few
// seconds the first time in a process and is cached by the Go build cache after.
//
// It needs no TinyGo. TinyGo produces a module 13× smaller and compiles it 14×
// faster (ADR-0058) and an author should use it; a test that required it would
// require it of every machine that runs the suite, for a property the suite does
// not measure.
package plugintest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

var (
	buildOnce sync.Once
	guestWasm []byte
	buildErr  error
)

// Guest returns the compiled test guest, building it on first use in this
// process. It is the module described by testdata/guest/main.go: an Event sink
// that signs one document per event and posts it, and — chosen by an obelo-mode=
// marker in the target URL the test configures — the panicking, hanging and
// allowlist-violating variants the failure tests need.
func Guest(t *testing.T) []byte {
	t.Helper()
	buildOnce.Do(build)
	if buildErr != nil {
		t.Fatalf("building the test guest: %v", buildErr)
	}
	return guestWasm
}

func build() {
	dir, err := guestDir()
	if err != nil {
		buildErr = err
		return
	}
	out, err := os.CreateTemp("", "obelo-test-guest-*.wasm")
	if err != nil {
		buildErr = err
		return
	}
	path := out.Name()
	_ = out.Close()
	defer os.Remove(path)

	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", path, ".")
	cmd.Dir = dir
	// GOFLAGS is cleared because the root module's flags (-mod=vendor, a -tags
	// list) have nothing to do with a separate module that imports only the
	// standard library, and inheriting them is how this build breaks on somebody
	// else's machine.
	//
	// GOWORK IS OFF FOR THE SAME REASON, and it is not optional
	// (.scratch/bundled-plugins issue 03). Since the repository gained a go.work
	// (ADR-0059 decision 9), a `go build` run anywhere under the repository is in
	// WORKSPACE mode, and this directory is not one of the workspace's modules —
	// so the build failed with "main module does not contain package ...", every
	// test here at once. Off is also the truthful setting: this guest stands in
	// for a plugin author's project, which is not inside this repository and has
	// no workspace around it.
	cmd.Env = append(os.Environ(),
		"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off",
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		buildErr = fmt.Errorf("go build in %s: %w\n%s", dir, err, combined)
		return
	}
	guestWasm, buildErr = os.ReadFile(path)
}

// guestDir finds testdata/guest beside this file, so a test can ask for the guest
// from any working directory.
func guestDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("plugintest: cannot locate its own source")
	}
	dir := filepath.Join(filepath.Dir(file), "testdata", "guest")
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return "", fmt.Errorf("plugintest: no guest source at %s: %w", dir, err)
	}
	return dir, nil
}

// SinkManifest is the manifest of a working Installed Event sink: the smallest
// document that is still a Plugin, plus whatever hosts the test means to allow.
func SinkManifest(id string, allowedHosts ...string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test Sink (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:           pluginapi.ExtensionEventSink,
			RequiresSecret: true,
		}},
		Network:     pluginapi.ManifestNetwork{Hosts: allowedHosts},
		Settings:    pluginapi.ManifestSettings{RequiresSecret: true},
		Description: "An Installed Event sink, built from source by the test suite.",
		DocsURL:     "https://example.test/obelo-test-sink",
	}
}

// SubtitleManifest is the manifest of a working Installed Subtitle provider: the
// same module as SinkManifest names, declaring the other seam it fills. One
// module CAN fill both at once — the host looks up only the exports the provides
// list says to expect — and keeping them in one module is what keeps the suite to
// one wasm compile per process.
//
// It declares a default URL an operator is expected to override, exactly as
// OpenSubtitles does, and requires a secret for the same reason: a source with a
// free-tier quota is one nobody should be able to turn on by accident.
func SubtitleManifest(id string, allowedHosts ...string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test Subtitles (" + id + ")",
		Version:    "2.1.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:           pluginapi.ExtensionSubtitleProvider,
			Kinds:          []string{pluginapi.KindVideo},
			Capabilities:   []pluginapi.Capability{pluginapi.CapabilitySearch},
			RequiresSecret: true,
		}},
		Network: pluginapi.ManifestNetwork{Hosts: allowedHosts},
		Settings: pluginapi.ManifestSettings{
			RequiresSecret: true,
			DefaultURL:     "https://subs.example.test/v1",
		},
		Description: "An Installed Subtitle provider, built from source by the test suite.",
		DocsURL:     "https://example.test/obelo-test-subtitles",
	}
}

// MetadataProviderManifest is the manifest of an Installed Metadata provider
// (.scratch/plugin-system issue 11): the same document a sink ships, with a
// `metadata-provider` entry carrying the four facts that decide where it sits in
// the enrichment chain — which coarse kinds it serves, its default Role, its
// ADR-0027 Class, and the optional operations it implements.
//
// requiresSecret is TRUE, like every Built-in Supplement's, and that is not
// incidental: the builder composes a Supplement only for a provider whose key is
// present, and the Authoritative-provider list offers only a keyed Full provider,
// so a keyless Plugin is registered and configurable but never composed (the
// known limitation issue 04 named). A test that wants a Plugin in a chain keys it
// through the settings API, exactly as an Admin does.
func MetadataProviderManifest(id string, p pluginapi.ManifestProvides, allowedHosts ...string) pluginapi.Manifest {
	p.Kind = pluginapi.ExtensionMetadataProvider
	p.RequiresSecret = true
	return pluginapi.Manifest{
		ID:          id,
		Name:        "Test Source (" + id + ")",
		Version:     "1.0.0",
		APIVersion:  pluginapi.APIVersion,
		Provides:    []pluginapi.ManifestProvides{p},
		Network:     pluginapi.ManifestNetwork{Hosts: allowedHosts},
		Settings:    pluginapi.ManifestSettings{RequiresSecret: true},
		Description: "An Installed Metadata provider, built from source by the test suite.",
		DocsURL:     "https://example.test/obelo-test-source",
	}
}

// KeylessMetadataProviderManifest is MetadataProviderManifest for a source that
// declares it needs NO credential (.scratch/plugin-system issue 13).
//
// It exists because that Plugin could not work before issue 13: activation was
// inferred from key presence, so `requiresSecret: false` produced a source that was
// registered, configurable, offered in the Authoritative-provider dropdown and
// never composed. Every test in issue 11 declared a secret to get round it. The
// active fact closed the hole, and this is the manifest that proves it — a Plugin
// an Admin switches on and nothing else.
func KeylessMetadataProviderManifest(id string, p pluginapi.ManifestProvides, allowedHosts ...string) pluginapi.Manifest {
	m := MetadataProviderManifest(id, p, allowedHosts...)
	m.Provides[0].RequiresSecret = false
	m.Settings.RequiresSecret = false
	return m
}

// WebReferenceManifest is the manifest of an Installed Web reference provider
// serving video items. It declares no network hosts and no secret — the seam
// needs neither — and one setting of its own, `mode`, whose manifest default
// picks the part the guest plays: "https" (the well-behaved example), "http",
// "foreign" or "fetch" (see the guest's web_reference_links).
func WebReferenceManifest(id, mode string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test References (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:  pluginapi.ExtensionWebReferenceProvider,
			Kinds: []string{pluginapi.KindVideo},
		}},
		Settings: pluginapi.ManifestSettings{Fields: []pluginapi.SettingsField{{
			Key:     "mode",
			Type:    pluginapi.FieldEnum,
			Label:   "Mode",
			Options: []string{"https", "http", "foreign", "fetch"},
			Default: json.RawMessage(`"` + mode + `"`),
		}}},
		Description: "An Installed Web reference provider, built from source by the test suite.",
	}
}

// SignInFailsWithThePassword, passed as SignInManifest's accounts, is a
// directory whose guest logs the password it was handed and then fails the call
// with the password in its error.
const SignInFailsWithThePassword = "fail-with-the-password"

// SignInHangs, passed as SignInManifest's accounts, is a directory whose guest
// never answers: only the host's deadline ends its call.
const SignInHangs = "hang"

// The directories below, passed as SignInManifest's accounts, each put the
// password they were handed somewhere the host keeps or logs text, and then
// reject the login. SignInFetchesWithThePassword is a prefix: the guest fetches
// the URL that follows it with the password appended.
const (
	SignInFetchesWithThePassword = "fetch-with-the-password:"
	SignInFetchesThePasswordHost = "fetch-the-password-host"
	SignInStoresThePassword      = "store-the-password"
	SignInLogsThePassword        = "log-the-password"
)

// SignInManifest is the manifest of an Installed Sign-in provider implementing
// the password flow. Its "directory" is one declared setting, `accounts`, whose
// manifest default is the accounts argument: `;`-separated entries of
// `login:password:subject:name:groups` (see the guest's sign_in_password). It
// declares whatever hosts the test means to allow, usually none — the guest checks
// what it was handed and nothing else.
func SignInManifest(id, accounts string, allowedHosts ...string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test Directory (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:         pluginapi.ExtensionSignInProvider,
			Capabilities: []pluginapi.Capability{pluginapi.CapabilityPasswordSignIn},
		}},
		Network: pluginapi.ManifestNetwork{Hosts: allowedHosts},
		Settings: pluginapi.ManifestSettings{Fields: []pluginapi.SettingsField{{
			Key:     "accounts",
			Type:    pluginapi.FieldString,
			Label:   "Accounts",
			Default: json.RawMessage(strconv.Quote(accounts)),
		}}},
		Description: "An Installed Sign-in provider, built from source by the test suite.",
	}
}

// LyricManifest is the manifest of an Installed Lyric provider serving music
// whose source is sourceURL: the guest POSTs each request to <sourceURL>/lyrics
// and answers whatever comes back (see the guest's lyric_provider_lyrics). The
// source is the manifest's default URL, which is the host a call may reach, so
// it declares no network hosts of its own and no secret.
func LyricManifest(id, sourceURL string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test Lyrics (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:  pluginapi.ExtensionLyricProvider,
			Kinds: []string{pluginapi.KindMusic},
		}},
		Settings:    pluginapi.ManifestSettings{DefaultURL: sourceURL},
		Description: "An Installed Lyric provider, built from source by the test suite.",
	}
}

// MarkerManifest is the manifest of an Installed Marker provider serving video
// whose source is sourceURL: the guest POSTs each request to <sourceURL>/markers
// and answers whatever comes back (see the guest's marker_provider_markers). The
// source is the manifest's default URL, which is the host a call may reach, so it
// declares no network hosts of its own and no secret.
func MarkerManifest(id, sourceURL string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test Markers (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:  pluginapi.ExtensionMarkerProvider,
			Kinds: []string{pluginapi.KindVideo},
		}},
		Settings:    pluginapi.ManifestSettings{DefaultURL: sourceURL},
		Description: "An Installed Marker provider, built from source by the test suite.",
	}
}

// EverySettingsFieldType is one declared settings field of every type the contract
// defines, in AllSettingsFieldTypes order, with the labels, help, defaults, options
// and bounds a real manifest would carry.
//
// It is the fixture for "a manifest declares one field of each type": one place
// that has to grow when a field type is added, and one place a form, a validator
// and a guest are all exercised against.
func EverySettingsFieldType() []pluginapi.SettingsField {
	min, max := 1, 10
	return []pluginapi.SettingsField{
		{
			Key:   "account",
			Type:  pluginapi.FieldString,
			Label: "Account name",
			Help:  "The name this source knows you by.",
		},
		{
			Key:      "token",
			Type:     pluginapi.FieldSecret,
			Label:    "Access token",
			Help:     "Never shown again once saved.",
			Required: true,
		},
		{
			Key:     "endpoint",
			Type:    pluginapi.FieldURL,
			Label:   "Mirror",
			Help:    "An absolute http(s) address.",
			Default: json.RawMessage(`"https://mirror.example.test"`),
		},
		{
			Key:     "adult",
			Type:    pluginapi.FieldBool,
			Label:   "Include adult titles",
			Default: json.RawMessage(`false`),
		},
		{
			Key:      "region",
			Type:     pluginapi.FieldEnum,
			Label:    "Region",
			Options:  []string{"eu", "us", "apac"},
			Required: true,
		},
		{
			Key:     "formats",
			Type:    pluginapi.FieldMultiSelect,
			Label:   "Formats",
			Options: []string{"srt", "ass", "vtt"},
		},
		{
			Key:   "retries",
			Type:  pluginapi.FieldInteger,
			Label: "Retries",
			Help:  "How many times to try again before giving up.",
			Min:   &min,
			Max:   &max,
		},
	}
}

// Install places a manifest and the compiled guest under <dataDir>/plugins/<id>/,
// exactly as an Admin does by hand in this slice. It returns the Plugin directory.
func Install(t *testing.T, dataDir string, m pluginapi.Manifest) string {
	t.Helper()
	return InstallModule(t, dataDir, m, Guest(t))
}

// InstallModule is Install with a module of the caller's choosing, for the tests
// that need something that is not a working guest — bytes that are not wasm at
// all, or no module beside the manifest.
func InstallModule(t *testing.T, dataDir string, m pluginapi.Manifest, wasm []byte) string {
	t.Helper()
	dir := filepath.Join(dataDir, plugins.DirName, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the plugin directory: %v", err)
	}
	WriteManifest(t, dir, m)
	if wasm != nil {
		name := m.Module
		if name == "" {
			name = plugins.DefaultModuleFile
		}
		if err := os.WriteFile(filepath.Join(dir, name), wasm, 0o644); err != nil {
			t.Fatalf("writing the module: %v", err)
		}
	}
	return dir
}

// WriteManifest writes a manifest into an existing Plugin directory. Separate
// from Install so a test can write a manifest that no Go value can express — one
// that is not JSON, or one whose apiVersion this build has never heard of.
func WriteManifest(t *testing.T, dir string, m pluginapi.Manifest) {
	t.Helper()
	raw, err := marshalManifest(m)
	if err != nil {
		t.Fatalf("encoding the manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), raw, 0o644); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}
}

// WriteRawManifest writes bytes as the manifest, whatever they are.
func WriteRawManifest(t *testing.T, dataDir, id string, raw []byte) string {
	t.Helper()
	dir := filepath.Join(dataDir, plugins.DirName, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the plugin directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), raw, 0o644); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}
	return dir
}

// ManifestJSON is a manifest as the bytes an author would ship — the same
// encoding Install writes to disk.
//
// It exists for the install tests (.scratch/plugin-system issue 10), which hand a
// manifest to an HTTP endpoint rather than placing it in a directory, and it is
// deliberately the SAME encoder: an upload and a hand-placed file have to be the
// same document, or the two paths are not exercising one loader.
func ManifestJSON(t *testing.T, m pluginapi.Manifest) []byte {
	t.Helper()
	raw, err := marshalManifest(m)
	if err != nil {
		t.Fatalf("encoding the manifest: %v", err)
	}
	return raw
}

// marshalManifest writes the manifest indented, because in this slice these files
// are placed and edited by hand and a one-line JSON document is not something an
// operator can work with.
func marshalManifest(m pluginapi.Manifest) ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// RedirectSignInManifest is the manifest of an Installed Sign-in provider
// implementing the redirect flow as a plain OAuth2 provider: it declares no
// idToken, so the host has nothing to verify. Its authorize URL is the declared
// `authorize` setting, whose default is the authorize argument, with the host's
// state, nonce, challenge and callback on it. Its exchange reads the code as
// `subject|username|groups` (groups `,`-separated) and accepts that identity; a
// code of `reject` is refused, and one prefixed `token|` also answers an ID token
// (see the guest's sign_in_exchange). Its `client_secret` setting is a secret the
// guest only ever repeats: see SignInRedirectFailsWithTheSecrets.
func RedirectSignInManifest(id, authorize string) pluginapi.Manifest {
	return pluginapi.Manifest{
		ID:         id,
		Name:       "Test OAuth (" + id + ")",
		Version:    "1.0.0",
		APIVersion: pluginapi.APIVersion,
		Provides: []pluginapi.ManifestProvides{{
			Kind:         pluginapi.ExtensionSignInProvider,
			Capabilities: []pluginapi.Capability{pluginapi.CapabilityRedirectSignIn},
		}},
		Settings: pluginapi.ManifestSettings{Fields: []pluginapi.SettingsField{{
			Key:     "authorize",
			Type:    pluginapi.FieldURL,
			Label:   "Authorize URL",
			Default: json.RawMessage(strconv.Quote(authorize)),
		}, {
			Key:   "client_secret",
			Type:  pluginapi.FieldSecret,
			Label: "Client secret",
		}}},
		Description: "An Installed OAuth2 Sign-in provider, built from source by the test suite.",
	}
}

// SignInRedirectFailsWithTheSecrets, as the code a redirect provider built from
// RedirectSignInManifest exchanges or as the last path segment of its
// `authorize` setting, is a provider that logs what the call handed it and the
// `client_secret` setting, marked "the guest was handed", and then fails the
// call with the same in its error.
const SignInRedirectFailsWithTheSecrets = "fail-with-the-secrets"

// SignInRedirectDiscovers, as the prefix of the code a redirect provider built
// from RedirectSignInManifest exchanges, is an OpenID Connect provider's
// exchange: it reads the discovery document of its `issuer` setting (which the
// test declares) and posts to the token endpoint that document names, failing
// with what the host answered unless both are 200, and then reads the rest of
// the code as usual.
const SignInRedirectDiscovers = "discover|"

// SignInRedirectFetchesABlockedHost, as the code a redirect provider built from
// RedirectSignInManifest exchanges or as the last path segment of its
// `authorize` setting, is a provider that fetches a host no manifest lists and
// then answers as usual: the authorize URL, or a refused exchange.
const SignInRedirectFetchesABlockedHost = "fetch-a-blocked-host"

// The re-check directories a `lookup` setting names (see LookupSignInManifest):
// SignInLookupFails fails every re-check, and SignInLookupFetchesABlockedHost
// fetches a host no manifest lists on every re-check, and fails.
const (
	SignInLookupFails               = "fail"
	SignInLookupFetchesABlockedHost = "fetch-a-blocked-host"
)

// lookupField is the declared `lookup` setting a re-check is answered from:
// `;`-separated `subject:status:groups` entries (groups `,`-separated), a
// subject it does not list being gone.
func lookupField(lookup string) pluginapi.SettingsField {
	return pluginapi.SettingsField{
		Key:     "lookup",
		Type:    pluginapi.FieldString,
		Label:   "Lookup",
		Default: json.RawMessage(strconv.Quote(lookup)),
	}
}

// LookupSignInManifest is SignInManifest also declaring sign-in-lookup, whose
// re-checks are answered from the `lookup` setting (see lookupField).
func LookupSignInManifest(id, accounts, lookup string) pluginapi.Manifest {
	m := SignInManifest(id, accounts)
	m.Provides[0].Capabilities = append(m.Provides[0].Capabilities, pluginapi.CapabilitySignInLookup)
	m.Settings.Fields = append(m.Settings.Fields, lookupField(lookup))
	return m
}

// RefreshingRedirectSignInManifest is RedirectSignInManifest also declaring
// sign-in-refresh. A code prefixed `refresh|` hands back the refresh token
// `rt|<subject>`, and a refresh is answered from the `lookup` setting (see
// lookupField).
func RefreshingRedirectSignInManifest(id, authorize, lookup string) pluginapi.Manifest {
	m := RedirectSignInManifest(id, authorize)
	m.Provides[0].Capabilities = append(m.Provides[0].Capabilities, pluginapi.CapabilitySignInRefresh)
	m.Settings.Fields = append(m.Settings.Fields, lookupField(lookup))
	return m
}

// SignInSocketScript, as the prefix of a Sign-in provider's `accounts` value, is
// a directory that runs the `|`-separated socket steps after it and answers
// accepted, subject "socket", with one group per step saying what the host
// answered (see the guest's runSocketScript for the steps and the outcomes).
const SignInSocketScript = "socket:"

// SocketSignInManifest is SignInManifest asking for the socket grant, whose
// directory runs script (see SignInSocketScript).
func SocketSignInManifest(id, script string) pluginapi.Manifest {
	m := SignInManifest(id, SignInSocketScript+script)
	m.Provides[0].Socket = true
	return m
}
