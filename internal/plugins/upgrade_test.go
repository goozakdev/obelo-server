package plugins_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// In-place upgrade of an Installed plugin (ADR-0069, .scratch/plugin-inplace-upgrade
// issue 03): the same id at a strictly higher semantic version, signed by the key the
// installed copy was first installed with, replaces the copy where it stands.

const upgradePublisher = "Example Publisher"

// variantModule is the suite's guest with a custom section naming tag: valid
// WebAssembly that behaves identically and is a different file.
func variantModule(t *testing.T, tag string) []byte {
	t.Helper()
	module := plugintest.Guest(t)
	section := append([]byte{0x00, byte(1 + len(tag)), byte(len(tag))}, tag...)
	return append(append(append([]byte{}, module[:8]...), section...), module[8:]...)
}

// upgradeArchive packs a sink plugin at a version, with a module distinguished by
// tag, signed by priv as publisher (unsigned when priv is nil).
func upgradeArchive(t *testing.T, id, version, tag string, priv ed25519.PrivateKey, publisher string,
	edit func(*pluginapi.Manifest)) []byte {
	t.Helper()
	m := plugintest.SinkManifest(id)
	m.Version = version
	if edit != nil {
		edit(&m)
	}
	manifest := plugintest.ManifestJSON(t, m)
	module := variantModule(t, tag)
	var doc []byte
	if priv != nil {
		sig, err := signing.Sign(priv, publisher, manifest, module)
		if err != nil {
			t.Fatal(err)
		}
		if doc, err = signing.Encode(sig); err != nil {
			t.Fatal(err)
		}
	}
	return plugintest.PackageZip(t, manifest, module, doc)
}

func mustInstall(t *testing.T, f *managerFixture, archive []byte) plugins.Installed {
	t.Helper()
	got, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload)
	if err != nil {
		t.Fatalf("installing: %v", err)
	}
	return got
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// snapshot is everything an upgrade must not change when it refuses: every name in
// the plugins directory (staging leftovers included), the plugin's files, its row,
// and the version the registry's Plugin carries.
type snapshot struct {
	dirNames []string
	files    map[string]string
	row      store.PluginRow
	loaded   string
	sinks    []string
}

func takeSnapshot(t *testing.T, f *managerFixture, id string) snapshot {
	t.Helper()
	s := snapshot{files: map[string]string{}}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s.dirNames = append(s.dirNames, e.Name())
	}
	files, _ := os.ReadDir(filepath.Join(f.dir, id))
	for _, e := range files {
		raw, err := os.ReadFile(filepath.Join(f.dir, id, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s.files[e.Name()] = string(raw)
	}
	rows, _ := f.store.Plugins()
	for _, r := range rows {
		if r.ID == id {
			s.row = r
		}
	}
	s.loaded = pluginNamed(t, f, id).Manifest().Version
	s.sinks = f.sinkSlugs()
	return s
}

func assertUnchanged(t *testing.T, f *managerFixture, id string, before snapshot) {
	t.Helper()
	after := takeSnapshot(t, f, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused upgrade changed something:\nbefore %+v\nafter  %+v", before, after)
	}
}

// newBlockingReceiver answers 204 after hit returns, so a test can hold a delivery
// open for as long as it likes.
func newBlockingReceiver(t *testing.T, hit func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func upgrade(f *managerFixture, archive []byte) (plugins.Installed, error) {
	return f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload)
}

// deliverThroughRegistry calls the plugin the REGISTRY holds for id, so a pass means
// the live registration answers.
func deliverThroughRegistry(t *testing.T, f *managerFixture, id string) error {
	t.Helper()
	reg := f.registrationFor(id)
	target := newReceiver(t)
	sink, err := reg.New(pluginapi.Settings{
		Enabled: true, Secret: "s", URL: target.srv.URL, URLEntered: true,
		Events: []string{pluginapi.EventScanCompleted},
	})
	if err != nil {
		t.Fatalf("building the sink: %v", err)
	}
	return sink.Deliver(context.Background(), scanEvent())
}

func TestAHigherVersionUnderTheRecordedKeyUpgradesInPlace(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	next := upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)
	got, err := upgrade(f, next)
	if err != nil {
		t.Fatalf("the upgrade was refused: %v", err)
	}
	if got.Version != "1.1.0" {
		t.Fatalf("the listed version is %q, want 1.1.0", got.Version)
	}
	if got.Upgrade == nil || got.Upgrade.From != "1.0.0" || got.Upgrade.To != "1.1.0" ||
		got.Upgrade.Publisher != "" || got.Upgrade.ClaimedPublisher != upgradePublisher || got.Upgrade.KeyID != signing.KeyID(pub) {
		t.Fatalf("upgrade summary = %+v, want 1.0.0 -> 1.1.0 with %q only CLAIMED (unpinned) and the key id", got.Upgrade, upgradePublisher)
	}
	if listed, _ := f.manager.List(context.Background()); len(listed) != 1 || listed[0].Upgrade != nil {
		t.Fatalf("a listing carries %+v, want one row with no upgrade summary", listed)
	}

	// The registry calls the NEW module: the loaded plugin is 1.1.0, the file on disk
	// is the new module, and the live registration answers.
	if v := pluginNamed(t, f, "up-sink").Manifest().Version; v != "1.1.0" {
		t.Fatalf("the loaded plugin is version %q, want 1.1.0", v)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.dir, "up-sink", plugins.DefaultModuleFile))
	if err != nil || string(onDisk) != string(variantModule(t, "v2")) {
		t.Fatalf("the module on disk is not the new one (err %v)", err)
	}
	if err := deliverThroughRegistry(t, f, "up-sink"); err != nil {
		t.Fatalf("the upgraded plugin does not answer through the registry: %v", err)
	}
	row := rowOf(t, f, "up-sink")
	if row.Version != "1.1.0" || row.SignerKey != signing.EncodeKey(pub) || row.SignerName != upgradePublisher {
		t.Fatalf("row = %+v, want the new version and the same signer", row)
	}
	if !contains(f.sinkSlugs(), builtinSlug) {
		t.Fatal("the upgrade lost the Built-ins")
	}
}

func TestAnUpgradeThatIsNotStrictlyHigherNamesBothVersionsAndChangesNothing(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	for _, tc := range []struct {
		name         string
		installed    string
		offered      string
		wantIn       []string
		installedRaw string
	}{
		{"the same version", "1.2.0", "1.2.0", []string{"1.2.0", "uninstall"}, ""},
		{"a lower version", "1.2.0", "1.1.9", []string{"1.2.0", "1.1.9", "uninstall"}, ""},
		{"a non-semver new version", "1.2.0", "latest", []string{"1.2.0", "latest", "semantic version"}, ""},
		{"a non-semver installed version", "nightly", "1.2.0", []string{"nightly", "1.2.0", "semantic version"}, ""},
		{"a leading-zero version", "1.2.0", "1.02.0", []string{"1.2.0", "1.02.0", "semantic version"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", tc.installed, "v1", priv, upgradePublisher, nil))
			before := takeSnapshot(t, f, "up-sink")

			_, err := upgrade(f, upgradeArchive(t, "up-sink", tc.offered, "v2", priv, upgradePublisher, nil))
			if refusalReason(err) != plugins.ReasonVersion {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonVersion)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message %q lacks %q", err, want)
				}
			}
			assertUnchanged(t, f, "up-sink", before)
		})
	}
}

func TestALowerVersionSignedByTheRightKeyStillRefuses(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "2.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.9.0", "v0", priv, upgradePublisher, nil))
	if refusalReason(err) != plugins.ReasonVersion {
		t.Fatalf("a downgrade under the right key gave %q (%v), want a version refusal", refusalReason(err), err)
	}
	assertUnchanged(t, f, "up-sink", before)
}

func TestAnUpgradeUnderAnotherKeyIsRefusedNamingBothPublishersAndKeys(t *testing.T) {
	plugins.Parallel(t)
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)

	for _, tc := range []struct {
		name      string
		priv      ed25519.PrivateKey
		publisher string
		offeredID string
	}{
		{"another publisher", otherPriv, "Somebody Else", signing.KeyID(otherPub)},
		{"the same publisher name with another key", otherPriv, upgradePublisher, signing.KeyID(otherPub)},
		{"no signature at all", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			before := takeSnapshot(t, f, "up-sink")

			_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", tc.priv, tc.publisher, nil))
			if refusalReason(err) != plugins.ReasonPublisher {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonPublisher)
			}
			msg := err.Error()
			for _, want := range []string{upgradePublisher, signing.KeyID(pub)} {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q does not name the installed %q", msg, want)
				}
			}
			if tc.priv != nil {
				for _, want := range []string{tc.publisher, tc.offeredID} {
					if !strings.Contains(msg, want) {
						t.Fatalf("message %q does not name the offered %q", msg, want)
					}
				}
			}
			assertUnchanged(t, f, "up-sink", before)
		})
	}
}

func TestACrossKeyRefusalSaysThereIsNoKeyRotation(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	_, otherPriv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", otherPriv, upgradePublisher, nil))
	if err == nil {
		t.Fatal("an upgrade under another key was accepted")
	}
	for _, want := range []string{"no key rotation", "uninstall"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err, want)
		}
	}
}

func TestAnUpgradeSignedByADifferentPinnedKeyIsRefused(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	if err := f.manager.PinPublisher("Other Publisher", signing.EncodeKey(otherPub)); err != nil {
		t.Fatal(err)
	}
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", otherPriv, "Other Publisher", nil))
	if refusalReason(err) != plugins.ReasonPublisher {
		t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonPublisher)
	}
	for _, want := range []string{upgradePublisher, signing.KeyID(pub), "Other Publisher", signing.KeyID(otherPub)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err, want)
		}
	}
	assertUnchanged(t, f, "up-sink", before)
}

func TestAnUpgradeOfAPluginWithNoRecordedKeyNeedsConfirmation(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *managerFixture)
	}{
		{"installed unsigned", func(t *testing.T, f *managerFixture) {
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", nil, "", nil))
		}},
		{"a row that predates the recorded key", func(t *testing.T, f *managerFixture) {
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			r := f.store.rows["up-sink"]
			r.SignerName, r.SignerKey, r.SignerKeyID = "", "", ""
			f.store.rows["up-sink"] = r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			tc.setup(t, f)
			before := takeSnapshot(t, f, "up-sink")

			// Signed by anyone, or by no one: either way nobody can say who the author is.
			for _, signer := range []ed25519.PrivateKey{priv, nil} {
				_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", signer, upgradePublisher, nil))
				if refusalReason(err) != plugins.ReasonNeedsConfirmation {
					t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonNeedsConfirmation)
				}
				if !strings.Contains(err.Error(), "author cannot be confirmed") {
					t.Fatalf("message %q lacks the author-cannot-be-confirmed wording", err)
				}
				assertUnchanged(t, f, "up-sink", before)
			}
		})
	}
}

func TestContinuityDoesNotBypassThePinnedPolicy(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", nil, "", nil))
	if refusalReason(err) != plugins.ReasonSignature {
		t.Fatalf("reason = %q (%v), want the pinned-policy refusal %q", refusalReason(err), err, plugins.ReasonSignature)
	}
	if !strings.Contains(err.Error(), "carries no signature, and this server only installs plugins signed by a pinned publisher") {
		t.Fatalf("message %q is not the existing pinned-policy sentence", err)
	}
	assertUnchanged(t, f, "up-sink", before)
}

func TestAnUpgradeWhoseModuleDoesNotLoadLeavesTheOldVersionAnswering(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	// A module with no exports at all: it compiles and instantiates, and answers nothing.
	noExports := []byte("\x00asm\x01\x00\x00\x00")

	for _, tc := range []struct {
		name   string
		module []byte
	}{
		{"not WebAssembly", []byte("MZ\x00\x00 definitely a native binary")},
		{"lacking every claimed export", noExports},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			before := takeSnapshot(t, f, "up-sink")

			m := plugintest.SinkManifest("up-sink")
			m.Version = "1.1.0"
			manifest := plugintest.ManifestJSON(t, m)
			sig, err := signing.Sign(priv, upgradePublisher, manifest, tc.module)
			if err != nil {
				t.Fatal(err)
			}
			doc, _ := signing.Encode(sig)
			_, err = upgrade(f, plugintest.PackageZip(t, manifest, tc.module, doc))
			if refusalReason(err) != plugins.ReasonModule {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonModule)
			}
			assertUnchanged(t, f, "up-sink", before)
			if err := deliverThroughRegistry(t, f, "up-sink"); err != nil {
				t.Fatalf("the old version no longer answers: %v", err)
			}
		})
	}
}

func TestACallInFlightOnTheOldModuleCompletesDuringTheSwap(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	slow := newBlockingReceiver(t, func() { once.Do(func() { close(entered) }); <-release })
	sink, err := f.registrationFor("up-sink").New(pluginapi.Settings{
		Enabled: true, Secret: "s", URL: slow.URL, URLEntered: true, Events: []string{pluginapi.EventScanCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() { delivered <- sink.Deliver(context.Background(), scanEvent()) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery never reached the receiver")
	}

	upgraded := make(chan error, 1)
	go func() {
		_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
		upgraded <- err
	}()
	time.Sleep(200 * time.Millisecond) // let the upgrade reach the swap and wait on the old module
	close(release)

	for name, ch := range map[string]chan error{"the in-flight delivery": delivered, "the upgrade": upgraded} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s failed: %v", name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s never finished", name)
		}
	}
	if v := pluginNamed(t, f, "up-sink").Manifest().Version; v != "1.1.0" {
		t.Fatalf("the loaded plugin is %q after the swap, want 1.1.0", v)
	}
}

func TestOldFilesAreGoneAfterASwapAndAFailedSwapLeavesExactlyTheOldOnes(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	f.store.upgradeErr = errors.New("the disk is full")
	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err == nil {
		t.Fatal("an upgrade whose row could not be written reported success")
	}
	assertUnchanged(t, f, "up-sink", before)
	if err := deliverThroughRegistry(t, f, "up-sink"); err != nil {
		t.Fatalf("the old version no longer answers after a failed swap: %v", err)
	}

	f.store.upgradeErr = nil
	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "up-sink" {
		t.Fatalf("the plugins directory holds %v after a successful swap, want only up-sink (no old copy, no staging)", entries)
	}
	files, _ := os.ReadDir(filepath.Join(f.dir, "up-sink"))
	var names []string
	for _, e := range files {
		names = append(names, e.Name())
	}
	if want := []string{plugins.ManifestFile, plugins.DefaultModuleFile, pluginapi.SignatureFile}; !sameSet(names, want) {
		t.Fatalf("the plugin directory holds %v, want %v", names, want)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

func TestAnUpgradeThatWidensRemovesOrBreaksSettingsIsRefusedNamingTheDifference(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	withField := func(key string, typ pluginapi.SettingsFieldType) func(*pluginapi.Manifest) {
		return func(m *pluginapi.Manifest) {
			m.Settings.Fields = []pluginapi.SettingsField{{Key: key, Type: typ}}
		}
	}
	both := func(m *pluginapi.Manifest) {
		m.Provides = append(m.Provides, plugintest.SubtitleManifest("x").Provides...)
	}

	for _, tc := range []struct {
		name     string
		id       string
		old, new func(*pluginapi.Manifest)
		wantIn   string
	}{
		{name: "a new network host", id: "up-sink",
			new: func(m *pluginapi.Manifest) { m.Network.Hosts = []string{"api.example.test"} }, wantIn: "api.example.test"},
		{name: "a new extension point", id: "up-sink", new: both, wantIn: string(pluginapi.ExtensionSubtitleProvider)},
		{name: "a removed extension point", id: "up-sink", old: both, new: func(*pluginapi.Manifest) {}, wantIn: string(pluginapi.ExtensionSubtitleProvider)},
		{name: "a setting that changes type", id: "up-sink",
			old: withField("region", pluginapi.FieldString), new: withField("region", pluginapi.FieldInteger), wantIn: "region"},
		{name: "a setting that is removed", id: "up-sink",
			old: withField("region", pluginapi.FieldString), new: withField("other", pluginapi.FieldString), wantIn: "region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, tc.id, "1.0.0", "v1", priv, upgradePublisher, tc.old))
			before := takeSnapshot(t, f, tc.id)

			_, err := upgrade(f, upgradeArchive(t, tc.id, "1.1.0", "v2", priv, upgradePublisher, tc.new))
			if refusalReason(err) != plugins.ReasonNeedsConfirmation {
				t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonNeedsConfirmation)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("message %q does not name %q", err, tc.wantIn)
			}
			assertUnchanged(t, f, tc.id, before)
		})
	}
}

func TestAnUpgradeThatAddsTheSocketGrantIsRefused(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	pack := func(version string, m pluginapi.Manifest) []byte {
		m.Version = version
		manifest := plugintest.ManifestJSON(t, m)
		module := variantModule(t, version)
		sig, err := signing.Sign(priv, upgradePublisher, manifest, module)
		if err != nil {
			t.Fatal(err)
		}
		doc, _ := signing.Encode(sig)
		return plugintest.PackageZip(t, manifest, module, doc)
	}
	mustInstall(t, f, pack("1.0.0", plugintest.SignInManifest("up-dir", "a:b")))
	before := takeSnapshot(t, f, "up-dir")

	_, err := upgrade(f, pack("1.1.0", plugintest.SocketSignInManifest("up-dir", "x")))
	if refusalReason(err) != plugins.ReasonNeedsConfirmation || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("a widened socket grant gave %q (%v), want a needs-confirmation refusal naming the socket", refusalReason(err), err)
	}
	assertUnchanged(t, f, "up-dir", before)
}

func TestSettingsAreKeptByteForByteAndANewKeyReadsItsDefault(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	fields := func(extra ...pluginapi.SettingsField) func(*pluginapi.Manifest) {
		return func(m *pluginapi.Manifest) {
			m.Settings.Fields = append([]pluginapi.SettingsField{
				{Key: "region", Type: pluginapi.FieldString},
				{Key: "token", Type: pluginapi.FieldSecret},
				{Key: "base", Type: pluginapi.FieldURL},
			}, extra...)
		}
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, fields()))
	saved := []store.PluginSetting{
		{Key: "region", Value: `"eu"`}, {Key: "token", Value: `"s3cret-value"`, Secret: true}, {Key: "base", Value: `"https://media.example.test"`},
	}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		fields(pluginapi.SettingsField{Key: "limit", Type: pluginapi.FieldInteger, Default: json.RawMessage("5")})))
	if err != nil {
		t.Fatalf("a compatible schema with one added key was refused: %v", err)
	}
	after, _ := f.store.PluginSettings("up-sink")
	if !reflect.DeepEqual(after, saved) {
		t.Fatalf("settings after = %+v, want exactly %+v", after, saved)
	}
	if got.Settings == nil || !got.Settings.Secrets["token"] {
		t.Fatalf("the secret is not on file after the upgrade: %+v", got.Settings)
	}
	if v, ok := got.Settings.Values["limit"]; !ok || v != float64(5) && v != int64(5) && v != 5 {
		t.Fatalf("the new key reads %v (present %v), want its default 5", v, ok)
	}
	if got.Settings.Values["region"] != "eu" {
		t.Fatalf("region reads %v, want eu", got.Settings.Values["region"])
	}
}

func TestAnApplyingUpgradeLeavesNoStalePinnedPublisher(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	first := mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	if first.Publisher == "" || first.KeyID == "" {
		t.Fatalf("setup: a pinned install should show its publisher, got %+v", first)
	}
	// The operator unpins; the next upgrade is verified only against the recorded key.
	if _, err := f.store.DeletePluginPublisher(upgradePublisher); err != nil {
		t.Fatal(err)
	}

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Publisher != "" || got.KeyID != "" {
		t.Fatalf("the view still shows publisher %q / key %q from the previous version", got.Publisher, got.KeyID)
	}
	if row := rowOf(t, f, "up-sink"); row.Publisher != "" || row.KeyID != "" || row.SignerKey != signing.EncodeKey(pub) {
		t.Fatalf("row = %+v, want the pinned columns cleared and the recorded key kept", row)
	}
}

func TestAnUpgradeFiresOnChangeWithThePluginIdExactlyOnce(t *testing.T) {
	plugins.Parallel(t)
	var mu sync.Mutex
	var heard []string
	f := newManagerFixtureWith(t, func(cfg *plugins.ManagerConfig) {
		cfg.OnChange = func(id string) { mu.Lock(); heard = append(heard, id); mu.Unlock() }
	})
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	mu.Lock()
	heard = nil
	mu.Unlock()

	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, id := range heard {
		if id == "up-sink" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("OnChange heard %v, want the plugin id exactly once", heard)
	}
}

// --- Bundled plugins --------------------------------------------------------------

// shippedSource is a BundledSource for a server that ships one plugin. A non-empty
// releaseKey makes it a release build, one that carries the Obelo key.
type shippedSource struct {
	id         string
	releaseKey ed25519.PublicKey
}

func (s shippedSource) Has(id string) bool                   { return id == s.id }
func (s shippedSource) Name(string) string                   { return s.id }
func (s shippedSource) Assert(context.Context, string) error { return errors.New("not used") }
func (s shippedSource) ReleaseKey() (name, key string, release bool) {
	if s.releaseKey == nil {
		return "", "", false
	}
	return "Obelo", signing.EncodeKey(s.releaseKey), true
}

// bundledFixture is a Manager over a server that ships "up-sink", with a row in the
// state a boot leaves a Bundled plugin: bundled origin, installed from the shipped copy,
// carrying recorded as its signer when recorded is true.
func bundledFixture(t *testing.T, release, recorded bool) (f *managerFixture, obeloPub ed25519.PublicKey, obeloPriv ed25519.PrivateKey) {
	t.Helper()
	obeloPub, obeloPriv = newKey(t)
	src := shippedSource{id: "up-sink"}
	if release {
		src.releaseKey = obeloPub
	}
	f = newManagerFixtureWith(t, func(cfg *plugins.ManagerConfig) { cfg.Bundled = src })
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", nil, "", nil))
	r := f.store.rows["up-sink"]
	r.Origin, r.Source = plugins.OriginBundled, "shipped with obelo"
	if recorded {
		r.SignerName, r.SignerKey, r.SignerKeyID = "Obelo", signing.EncodeKey(obeloPub), signing.KeyID(obeloPub)
	}
	f.store.rows["up-sink"] = r
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{{Key: "region", Value: `"eu"`}}); err != nil {
		t.Fatal(err)
	}
	return f, obeloPub, obeloPriv
}

func TestABundledPluginIsUpgradedByAnObeloSignedPackageAndStaysBundled(t *testing.T) {
	plugins.Parallel(t)
	f, obeloPub, obeloPriv := bundledFixture(t, true, true)
	before, _ := f.store.PluginSettings("up-sink")

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", obeloPriv, "Obelo", nil))
	if err != nil {
		t.Fatalf("an Obelo-signed upgrade of a Bundled plugin was refused: %v", err)
	}
	row := rowOf(t, f, "up-sink")
	if row.Origin != plugins.OriginBundled || got.Origin != plugins.OriginBundled {
		t.Fatalf("origin = %q (view %q), want it to stay bundled", row.Origin, got.Origin)
	}
	if row.Version != "1.1.0" || row.SignerKey != signing.EncodeKey(obeloPub) || row.Source != plugins.SourceUpload {
		t.Fatalf("row = %+v, want 1.1.0, the Obelo key and the upload as its source", row)
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(before, after) {
		t.Fatalf("settings changed: %+v -> %+v", before, after)
	}
}

func TestABundledPluginRefusesAnythingNotSignedByTheObeloKey(t *testing.T) {
	plugins.Parallel(t)
	for _, recorded := range []bool{true, false} {
		name := "a row with the Obelo key recorded"
		if !recorded {
			name = "a row from a release that recorded no key"
		}
		t.Run(name, func(t *testing.T) {
			f, obeloPub, _ := bundledFixture(t, true, recorded)
			_, otherPriv := newKey(t)
			before := takeSnapshot(t, f, "up-sink")

			for label, signer := range map[string]ed25519.PrivateKey{"unsigned": nil, "third-party": otherPriv} {
				_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", signer, "Third Party", nil))
				if refusalReason(err) != plugins.ReasonPublisher {
					t.Fatalf("%s: reason = %q (%v), want %q", label, refusalReason(err), err, plugins.ReasonPublisher)
				}
				for _, want := range []string{"Obelo", signing.KeyID(obeloPub)} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s: message %q does not name the Obelo publisher (%q)", label, err, want)
					}
				}
				assertUnchanged(t, f, "up-sink", before)
			}
		})
	}
}

func TestAPreFeatureBundledRowAcceptsOnlyAnObeloSignedUpload(t *testing.T) {
	plugins.Parallel(t)
	f, _, obeloPriv := bundledFixture(t, true, false)

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", obeloPriv, "Obelo", nil))
	if err != nil {
		t.Fatalf("an Obelo-signed upload over a pre-feature Bundled row was refused: %v", err)
	}
	if row := rowOf(t, f, "up-sink"); got.Version != "1.1.0" || row.Origin != plugins.OriginBundled || row.SignerName != "Obelo" {
		t.Fatalf("row = %+v, want 1.1.0, still bundled, signed by Obelo", row)
	}
}

func TestADevBuildBundledPluginWithNoKeyNeedsConfirmationAndIsNeverAppliedInOneStep(t *testing.T) {
	plugins.Parallel(t)
	f, _, _ := bundledFixture(t, false, false)
	_, priv := newKey(t)
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	if refusalReason(err) != plugins.ReasonNeedsConfirmation {
		t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonNeedsConfirmation)
	}
	assertUnchanged(t, f, "up-sink", before)
}

func TestOnADevBuildAnAppliedUpgradeOfABundledRowBecomesAdminOwned(t *testing.T) {
	plugins.Parallel(t)
	// A dev build over a database that remembers a signer: the recorded key lets the
	// upgrade through, but only a build that knows the Obelo key may keep "bundled".
	f, _, _ := bundledFixture(t, false, false)
	pub, priv := newKey(t)
	r := f.store.rows["up-sink"]
	r.SignerName, r.SignerKey, r.SignerKeyID = upgradePublisher, signing.EncodeKey(pub), signing.KeyID(pub)
	f.store.rows["up-sink"] = r

	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, f, "up-sink"); row.Origin != plugins.OriginAdmin {
		t.Fatalf("origin = %q, want admin so the next boot leaves it alone", row.Origin)
	}
}

func TestABuiltInIdStillRefusesAsADuplicate(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	_, err := upgrade(f, upgradeArchive(t, builtinSlug, "9.9.9", "v1", priv, upgradePublisher, nil))
	if refusalReason(err) != plugins.ReasonDuplicate {
		t.Fatalf("an upload over a Built-in id gave %q (%v), want %q", refusalReason(err), err, plugins.ReasonDuplicate)
	}
}

// TestVersionsAreOrderedBySemverPrecedence pins semver.org's ordering, including the
// prerelease rules a plain numeric compare would get wrong.
func TestVersionsAreOrderedBySemverPrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		installed, offered string
		upgrades           bool
	}{
		{"1.9.0", "1.10.0", true}, // numeric, not lexical
		{"1.0.0", "2.0.0", true},
		{"1.0.0-rc.1", "1.0.0", true}, // a release is above its prerelease
		{"1.0.0", "1.0.0-rc.1", false},
		{"1.0.0-alpha", "1.0.0-alpha.1", true},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", true}, // numbers below words
		{"1.0.0-rc.2", "1.0.0-rc.10", true},
		{"1.0.0+build.1", "1.0.0+build.2", false}, // build metadata is ignored
		{"1.0.0", "1.0.0", false},
		{"1.2.0", "v1.3.0", false}, // no "v" prefix
		{"1.2", "1.3", false},      // two components are not semver
		{"1.2.0", "1.2.1.4", false},
	} {
		err := plugins.CheckUpgradeVersion("x", tc.installed, tc.offered)
		if (err == nil) != tc.upgrades {
			t.Errorf("%s -> %s: err = %v, want upgrade %v", tc.installed, tc.offered, err, tc.upgrades)
		}
	}
}

// On an unpinned server a signer's NAME is only what its document says, so a refusal
// between two signers who use the same name must not present either as verified.
func TestAnUnpinnedKeyMismatchWordsBothSidesAsClaims(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", otherPriv, upgradePublisher, nil))
	if err == nil {
		t.Fatal("an upgrade under another key was accepted")
	}
	msg := err.Error()
	for _, want := range []string{
		"installed under key id " + signing.KeyID(pub) + ` (claims "` + upgradePublisher + `")`,
		"signed with key id " + signing.KeyID(otherPub) + ` (claims "` + upgradePublisher + `")`,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "signed by") {
		t.Fatalf("message %q presents an unverified name as proven", msg)
	}
}

// --- crash recovery ----------------------------------------------------------------
//
// An upgrade moves the old files aside, renames the new ones in, writes the row and
// then deletes the aside copy. These tests build, by hand, the directories a kill at
// each point would leave, and hand them to the boot-time sweep.

func copyDirForTest(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func manifestVersionOnDisk(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m pluginapi.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Version
}

func recoverIn(t *testing.T, f *managerFixture) []string {
	t.Helper()
	var lines []string
	plugins.RecoverUpgrades(f.dir, f.store, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	return lines
}

func assertNoAside(t *testing.T, f *managerFixture) {
	t.Helper()
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upgrade-") {
			t.Fatalf("a leftover %s survived the sweep", e.Name())
		}
	}
}

func TestBootRestoresTheOldFilesWhenAnUpgradeWasKilledBetweenTheRenames(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	// Killed after the old directory moved aside and before the new one arrived.
	if err := os.Rename(filepath.Join(f.dir, "up-sink"), filepath.Join(f.dir, ".upgrade-up-sink-4242-1")); err != nil {
		t.Fatal(err)
	}

	lines := recoverIn(t, f)

	if v := manifestVersionOnDisk(t, filepath.Join(f.dir, "up-sink")); v != "1.0.0" {
		t.Fatalf("the plugin directory is version %q after recovery, want the old 1.0.0", v)
	}
	assertNoAside(t, f)
	if len(lines) == 0 {
		t.Fatal("recovery said nothing")
	}
}

func TestBootDeletesALeftoverAsideCopyOfACompletedUpgrade(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	copyDirForTest(t, filepath.Join(f.dir, "up-sink"), filepath.Join(f.dir, ".upgrade-up-sink-4242-1"))
	before := takeSnapshot(t, f, "up-sink")

	recoverIn(t, f)

	assertNoAside(t, f)
	after := takeSnapshot(t, f, "up-sink")
	before.dirNames = after.dirNames // the aside name is the only difference allowed
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("recovery changed the live plugin:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestBootFinishesAnUpgradeWhoseNewFilesVerifyUnderTheRecordedKey(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	aside := filepath.Join(f.dir, ".upgrade-up-sink-4242-1")
	copyDirForTest(t, filepath.Join(f.dir, "up-sink"), aside)
	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
		t.Fatal(err)
	}
	// Killed after the swap and before the row was written: the row still says 1.0.0.
	r := f.store.rows["up-sink"]
	r.Version = "1.0.0"
	f.store.rows["up-sink"] = r

	lines := recoverIn(t, f)

	if got := rowOf(t, f, "up-sink").Version; got != "1.1.0" {
		t.Fatalf("row version = %q after recovery, want it brought up to the verified files (1.1.0)", got)
	}
	if v := manifestVersionOnDisk(t, filepath.Join(f.dir, "up-sink")); v != "1.1.0" {
		t.Fatalf("disk version = %q, want 1.1.0 kept", v)
	}
	assertNoAside(t, f)
	if !strings.Contains(strings.Join(lines, "\n"), "database says 1.0.0") {
		t.Fatalf("the disagreement was not logged: %v", lines)
	}
}

func TestBootUndoesAnUpgradeWhoseNewFilesDoNotVerifyUnderTheRecordedKey(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	live := filepath.Join(f.dir, "up-sink")
	aside := filepath.Join(f.dir, ".upgrade-up-sink-4242-1")
	copyDirForTest(t, live, aside)
	// The new files on disk are unsigned: whoever wrote them is not the recorded key.
	m := plugintest.SinkManifest("up-sink")
	m.Version = "1.1.0"
	if err := os.WriteFile(filepath.Join(live, plugins.ManifestFile), plugintest.ManifestJSON(t, m), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, pluginapi.SignatureFile)); err != nil {
		t.Fatal(err)
	}

	lines := recoverIn(t, f)

	if v := manifestVersionOnDisk(t, live); v != "1.0.0" {
		t.Fatalf("disk version = %q after recovery, want the old 1.0.0 restored", v)
	}
	if got := rowOf(t, f, "up-sink").Version; got != "1.0.0" {
		t.Fatalf("row version = %q, want it untouched", got)
	}
	assertNoAside(t, f)
	if !strings.Contains(strings.Join(lines, "\n"), "undone") {
		t.Fatalf("the undo was not logged: %v", lines)
	}
}

func TestEachUpgradesAsideNameIsUniqueToThisProcess(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	var asides []string
	f.manager.SetUpgradeHookForTest(func(aside string) { asides = append(asides, filepath.Base(aside)) })
	for _, v := range []string{"1.1.0", "1.2.0"} {
		if _, err := upgrade(f, upgradeArchive(t, "up-sink", v, "v"+v, priv, upgradePublisher, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if len(asides) != 2 || asides[0] == asides[1] || !strings.Contains(asides[0], fmt.Sprint(os.Getpid())) {
		t.Fatalf("aside names = %v, want two distinct names carrying the process id", asides)
	}
}

func TestAPinnedUpgradeReportsTheVerifiedPublisher(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Upgrade.Publisher != upgradePublisher || got.Upgrade.ClaimedPublisher != "" || got.Upgrade.KeyID != signing.KeyID(pub) {
		t.Fatalf("summary = %+v, want the pinned publisher as verified", got.Upgrade)
	}
}

// --- recovery must not promote files nobody vouches for ---------------------------

func TestBootDoesNotRestoreAnAsideThatIsNotTheRowsVersionOrDoesNotVerify(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	_, other := newKey(t)
	for _, tc := range []struct {
		name    string
		version string
		signer  ed25519.PrivateKey
	}{
		{"an unsigned planted version", "9.9.9", nil},
		{"the right version signed by another key", "1.0.0", other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			// The live directory is gone (killed between the renames) and what sits in the
			// aside slot is not what the row describes.
			if err := os.RemoveAll(filepath.Join(f.dir, "up-sink")); err != nil {
				t.Fatal(err)
			}
			planted := filepath.Join(f.dir, ".upgrade-up-sink-4242-1")
			writeArchive(t, planted, upgradeArchive(t, "up-sink", tc.version, "vx", tc.signer, upgradePublisher, nil))

			lines := recoverIn(t, f)

			if _, err := os.Stat(filepath.Join(f.dir, "up-sink")); err == nil {
				t.Fatal("unvouched files were promoted to the plugin directory")
			}
			if _, err := os.Stat(planted); err != nil {
				t.Fatalf("the planted directory was removed instead of left in place: %v", err)
			}
			if !strings.Contains(strings.Join(lines, "\n"), "LEFT IN PLACE") {
				t.Fatalf("nothing loud was logged: %v", lines)
			}
		})
	}
}

func TestBootDoesNotPromoteAnAsideWithNoRow(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	planted := filepath.Join(f.dir, ".upgrade-ghost-4242-1")
	writeArchive(t, planted, upgradeArchive(t, "ghost", "1.0.0", "v1", priv, upgradePublisher, nil))

	recoverIn(t, f)

	if _, err := os.Stat(filepath.Join(f.dir, "ghost")); err == nil {
		t.Fatal("an aside with no row was promoted")
	}
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("the aside was removed: %v", err)
	}
}

func TestBootNeverDeletesTheLiveDirectoryForAnUncheckedAside(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	live := filepath.Join(f.dir, "up-sink")
	// A live directory that does not verify, beside an aside that is not the row's version.
	if err := os.Remove(filepath.Join(live, pluginapi.SignatureFile)); err != nil {
		t.Fatal(err)
	}
	m := plugintest.SinkManifest("up-sink")
	m.Version = "1.1.0"
	if err := os.WriteFile(filepath.Join(live, plugins.ManifestFile), plugintest.ManifestJSON(t, m), 0o644); err != nil {
		t.Fatal(err)
	}
	writeArchive(t, filepath.Join(f.dir, ".upgrade-up-sink-4242-1"), upgradeArchive(t, "up-sink", "9.9.9", "vx", nil, "", nil))

	recoverIn(t, f)

	if v := manifestVersionOnDisk(t, live); v != "1.1.0" {
		t.Fatalf("the live directory was replaced (version %q) by an unchecked aside", v)
	}
}

func TestARecoveryFinishedUpgradeIsWrittenLikeANormalOne(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	pub, priv := newKey(t)
	if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	copyDirForTest(t, filepath.Join(f.dir, "up-sink"), filepath.Join(f.dir, ".upgrade-up-sink-4242-1"))
	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
		t.Fatal(err)
	}
	want := rowOf(t, f, "up-sink")
	r := want
	r.Version, r.Publisher, r.KeyID, r.Source = "1.0.0", "", "", "stale"
	f.store.rows["up-sink"] = r

	recoverIn(t, f)

	got := rowOf(t, f, "up-sink")
	if got.Version != "1.1.0" || got.Publisher != want.Publisher || got.KeyID != want.KeyID || got.Source != want.Source {
		t.Fatalf("recovered row = %+v, want it written as a normal upgrade wrote %+v", got, want)
	}
	if got.Publisher == "" {
		t.Fatal("setup: the pinned publisher should have been recorded")
	}
}

// writeArchive unpacks a package archive into dir, as the files an upgrade would have
// left there.
func writeArchive(t *testing.T, dir string, archive []byte) {
	t.Helper()
	pkg, err := plugins.UnpackPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{plugins.ManifestFile: pkg.Manifest, plugins.DefaultModuleFile: pkg.Module}
	if len(pkg.Signature) > 0 {
		files[pluginapi.SignatureFile] = pkg.Signature
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// --- Removed extension points other records depend on (issue 05) ---------------

func addProvides(m *pluginapi.Manifest, from pluginapi.Manifest) {
	m.Provides = append(m.Provides, from.Provides...)
}

func withSignIn(m *pluginapi.Manifest) { addProvides(m, plugintest.SignInManifest("x", "")) }
func withOnlineSource(m *pluginapi.Manifest) {
	addProvides(m, plugintest.OnlineSourceManifest("x", "X", "http://x.example.test"))
}
func withLyric(m *pluginapi.Manifest) {
	addProvides(m, plugintest.LyricManifest("x", "http://x.example.test"))
}

func refusalOf(t *testing.T, err error) *plugins.Refusal {
	t.Helper()
	var r *plugins.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *plugins.Refusal", err)
	}
	return r
}

func TestRemovingASignInProviderWithIdentitiesIsRefusedWithExactCounts(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	f := newManagerFixture(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withSignIn))
	f.store.identities["up-sink"] = 3
	f.store.casualties["up-sink"] = []store.SignInCasualty{{ID: "u1", Username: "ada"}, {ID: "u2", Username: "bea"}}
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	r := refusalOf(t, err)
	if r.Reason != plugins.ReasonDependents {
		t.Fatalf("reason = %q (%v), want %q", r.Reason, err, plugins.ReasonDependents)
	}
	for _, want := range []string{"sign-in", "3 identities", "2 users", "uninstall up-sink to remove it"} {
		if !strings.Contains(strings.ToLower(r.Message), want) {
			t.Fatalf("message %q lacks %q", r.Message, want)
		}
	}
	if r.Details["identities"] != 3 || r.Details["users"] != 2 {
		t.Fatalf("details = %v, want identities 3 and users 2", r.Details)
	}
	assertUnchanged(t, f, "up-sink", before)
	if f.store.identities["up-sink"] != 3 || len(f.store.casualties["up-sink"]) != 2 {
		t.Fatal("identities or users changed")
	}
}

func TestRemovingAnOnlineSourceProviderWithGrantsIsRefusedWithTheGrantCount(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	f := newManagerFixture(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withOnlineSource))
	f.store.grants["up-sink"] = 4
	before := takeSnapshot(t, f, "up-sink")

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	r := refusalOf(t, err)
	if r.Reason != plugins.ReasonDependents {
		t.Fatalf("reason = %q (%v), want %q", r.Reason, err, plugins.ReasonDependents)
	}
	for _, want := range []string{"online-source", "4 grants", "uninstall up-sink to remove it"} {
		if !strings.Contains(strings.ToLower(r.Message), want) {
			t.Fatalf("message %q lacks %q", r.Message, want)
		}
	}
	if r.Details["grants"] != 4 {
		t.Fatalf("details = %v, want grants 4", r.Details)
	}
	assertUnchanged(t, f, "up-sink", before)
	if f.store.grants["up-sink"] != 4 {
		t.Fatal("grants changed")
	}
}

func TestRemovingThoseExtensionPointsWithNoDependentStatePassesTheDependentsCheck(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	for name, edit := range map[string]func(*pluginapi.Manifest){
		"sign-in": withSignIn, "online source": withOnlineSource, "lyric (no dependent-state concept)": withLyric,
	} {
		t.Run(name, func(t *testing.T) {
			f := newManagerFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, edit))
			_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
			// Issue 03's removal refusal stays until issue 06's preview: the upgrade is
			// still refused, but as needing confirmation, not as depended upon.
			if got := refusalReason(err); got != plugins.ReasonNeedsConfirmation {
				t.Fatalf("reason = %q (%v), want %q", got, err, plugins.ReasonNeedsConfirmation)
			}
		})
	}
}

func TestAnExtensionPointThatStaysIsNotCountedAsDropped(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	f := newManagerFixture(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withOnlineSource))
	f.store.grants["up-sink"] = 4
	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withOnlineSource))
	if refusalReason(err) == plugins.ReasonDependents {
		t.Fatalf("a kept extension point was refused as dropped: %v", err)
	}
}
