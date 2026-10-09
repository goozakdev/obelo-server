package plugins_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
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

// Preview and confirm (ADR-0069, .scratch/plugin-inplace-upgrade issue 06): an upgrade
// that widens, loses a setting, removes an extension point or cannot name its author is
// staged and previewed; an Admin confirms it, or it expires.

// fakeClock is the Manager's clock in a test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func previewFixture(t *testing.T) (*managerFixture, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	f := newManagerFixtureWith(t, func(c *plugins.ManagerConfig) { c.Now = clk.Now })
	return f, clk
}

// stagedBy uploads an archive that must come back as a preview and returns it.
func staged(t *testing.T, f *managerFixture, archive []byte) *plugins.StagedUpgrade {
	t.Helper()
	got, err := upgrade(f, archive)
	if err != nil {
		t.Fatalf("the upgrade was refused: %v", err)
	}
	if got.Staged == nil {
		t.Fatalf("the upgrade was applied (version %q) where a preview was expected", got.Version)
	}
	if got.Upgrade != nil {
		t.Fatalf("a preview also carries an applied-upgrade summary: %+v", got.Upgrade)
	}
	return got.Staged
}

// applyConfirmed upgrades in one step or, when a preview comes back, confirms it.
func applyConfirmed(t *testing.T, f *managerFixture, archive []byte) plugins.Installed {
	t.Helper()
	got, err := upgrade(f, archive)
	if err != nil {
		t.Fatalf("the upgrade was refused: %v", err)
	}
	if got.Staged == nil {
		return got
	}
	done, err := f.manager.ConfirmUpgrade(context.Background(), got.ID, got.Staged.Staged)
	if err != nil {
		t.Fatalf("confirming: %v", err)
	}
	return done
}

func withBoth(m *pluginapi.Manifest) {
	m.Provides = append(m.Provides, plugintest.SubtitleManifest("x").Provides...)
}

func TestAWideningUpgradeIsStagedAndNamedAndChangesNothingUntilConfirmed(t *testing.T) {
	plugins.Parallel(t)
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
	sink := func(edit func(*pluginapi.Manifest)) func(string) []byte {
		return func(v string) []byte {
			m := plugintest.SinkManifest("up-sink")
			if edit != nil {
				edit(&m)
			}
			return pack(v, m)
		}
	}
	for _, tc := range []struct {
		name  string
		first func(string) []byte
		next  func(string) []byte
		check func(t *testing.T, p plugins.UpgradePreview)
	}{
		{"a new host", sink(nil), sink(func(m *pluginapi.Manifest) { m.Network.Hosts = []string{"api.example.test"} }),
			func(t *testing.T, p plugins.UpgradePreview) {
				if !reflect.DeepEqual(p.HostsAdded, []string{"api.example.test"}) {
					t.Fatalf("hostsAdded = %v", p.HostsAdded)
				}
			}},
		{"a new extension point", sink(nil), sink(withBoth),
			func(t *testing.T, p plugins.UpgradePreview) {
				if !reflect.DeepEqual(p.ExtensionPointsAdded, []string{string(pluginapi.ExtensionSubtitleProvider)}) {
					t.Fatalf("extensionPointsAdded = %v", p.ExtensionPointsAdded)
				}
			}},
		{"the socket grant", func(v string) []byte { return pack(v, plugintest.SignInManifest("up-sink", "a:b")) },
			func(v string) []byte { return pack(v, plugintest.SocketSignInManifest("up-sink", "x")) },
			func(t *testing.T, p plugins.UpgradePreview) {
				if !p.SocketGrantAdded {
					t.Fatalf("socketGrantAdded = false in %+v", p)
				}
			}},
		{"a removed extension point with nothing depending on it", sink(withBoth), sink(nil),
			func(t *testing.T, p plugins.UpgradePreview) {
				if !reflect.DeepEqual(p.ExtensionPointsRemoved, []string{string(pluginapi.ExtensionSubtitleProvider)}) {
					t.Fatalf("extensionPointsRemoved = %v", p.ExtensionPointsRemoved)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := previewFixture(t)
			mustInstall(t, f, tc.first("1.0.0"))
			before := takeSnapshot(t, f, "up-sink")

			s := staged(t, f, tc.next("1.1.0"))

			tc.check(t, s.Preview)
			if s.Staged == "" || s.Preview.From != "1.0.0" || s.Preview.To != "1.1.0" || s.Preview.AuthorUnconfirmed {
				t.Fatalf("staged = %+v, want a token and 1.0.0 -> 1.1.0 with the author confirmed", s)
			}
			assertUnchanged(t, f, "up-sink", before)
		})
	}
}

func TestPreviewListsDroppedAndDeletedSettingsByKeyAndReasonNeverByValue(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	secrets := func(keys ...string) []pluginapi.SettingsField {
		var out []pluginapi.SettingsField
		for _, k := range keys {
			out = append(out, pluginapi.SettingsField{Key: k, Type: pluginapi.FieldSecret})
		}
		return out
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher,
		withFields(append(secrets("gone", "port"), str("region"))...)))
	saved := []store.PluginSetting{
		{Key: "gone", Value: secretValue, Secret: true}, {Key: "port", Value: secretValue, Secret: true}, {Key: "region", Value: `"eu"`},
	}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}

	s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(pluginapi.SettingsField{Key: "port", Type: pluginapi.FieldInteger}, str("region"))))

	if !reflect.DeepEqual(s.Preview.SettingsDeleted, []string{"gone"}) {
		t.Fatalf("settingsDeleted = %v, want [gone]", s.Preview.SettingsDeleted)
	}
	if len(s.Preview.SettingsDropped) != 1 || s.Preview.SettingsDropped[0].Key != "port" || s.Preview.SettingsDropped[0].Reason == "" {
		t.Fatalf("settingsDropped = %+v, want port with a reason", s.Preview.SettingsDropped)
	}
	body, _ := json.Marshal(s)
	if strings.Contains(string(body), "s3cret-value-do-not-leak") {
		t.Fatalf("the preview carries a secret's value: %s", body)
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("staging changed the stored settings: %+v", after)
	}
}

func TestAnUpgradeWithOnlyHostsRemovedAppliesInOneStep(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher,
		func(m *pluginapi.Manifest) { m.Network.Hosts = []string{"api.example.test"} }))

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
	if err != nil || got.Staged != nil || got.Upgrade == nil || got.Version != "1.1.0" {
		t.Fatalf("removing a host alone should apply at once: %+v / %v", got, err)
	}
}

func TestAnUnconfirmedAuthorIsPreviewedAndARecordedKeyThatVerifiesAppliesInOneStep(t *testing.T) {
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
			f, _ := previewFixture(t)
			tc.setup(t, f)
			before := takeSnapshot(t, f, "up-sink")
			for _, signer := range []ed25519.PrivateKey{priv, nil} {
				s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", signer, upgradePublisher, nil))
				if !s.Preview.AuthorUnconfirmed {
					t.Fatalf("authorUnconfirmed = false in %+v", s.Preview)
				}
				assertUnchanged(t, f, "up-sink", before)
			}
		})
	}

	t.Run("a recorded key that verifies", func(t *testing.T) {
		f, _ := previewFixture(t)
		mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
		got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
		if err != nil || got.Staged != nil || got.Upgrade == nil || got.Version != "1.1.0" {
			t.Fatalf("a verified, non-widening upgrade should apply in one step: %+v / %v", got, err)
		}
	})
}

func TestAnUnconfirmedUpgradeThatIsConfirmedRecordsTheNewPackagesKey(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	pub, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", nil, "", nil))

	applyConfirmed(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))

	if row := rowOf(t, f, "up-sink"); row.Version != "1.1.0" || row.SignerKey != signing.EncodeKey(pub) {
		t.Fatalf("row = %+v, want 1.1.0 and the key it arrived signed with", row)
	}
}

func TestRemovingAnExtensionPointWithDependentStateIsRefusedAndNeverStaged(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withOnlineSource))
	f.store.grants["up-sink"] = 2

	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))

	if refusalReason(err) != plugins.ReasonDependents {
		t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonDependents)
	}
	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", "anything"); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("something was staged: %v", err)
	}
}

func TestAStagedPackageIsNotCallableNotListedAndLeavesNoFiles(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, func(m *pluginapi.Manifest) {
		m.Network.Hosts = []string{"api.example.test"}
	}))

	list, err := f.manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == "up-sink" && (p.Version != "1.0.0" || p.Staged != nil || p.Upgrade != nil) {
			t.Fatalf("the listing shows %+v, want the installed 1.0.0 and nothing staged", p)
		}
	}
	if err := deliverThroughRegistry(t, f, "up-sink"); err != nil {
		t.Fatalf("the installed version stopped answering: %v", err)
	}
	if got := pluginNamed(t, f, "up-sink").Manifest(); len(got.Network.Hosts) != 0 || got.Version != "1.0.0" {
		t.Fatalf("the live plugin is %+v, want the old manifest", got)
	}
	assertUnchanged(t, f, "up-sink", before)
}

func widening(m *pluginapi.Manifest) { m.Network.Hosts = []string{"api.example.test"} }

func TestAStagedUpgradeConfirmsBeforeItExpiresAndNotAfter(t *testing.T) {
	plugins.Parallel(t)
	_, priv := newKey(t)
	for _, tc := range []struct {
		name    string
		wait    time.Duration
		applied bool
	}{
		{"just before ten minutes", 10*time.Minute - time.Second, true},
		{"after ten minutes", 10*time.Minute + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, clk := previewFixture(t)
			mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
			s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening))
			if want := clk.Now().Add(10 * time.Minute); !s.ExpiresAt.Equal(want) {
				t.Fatalf("expiresAt = %v, want %v", s.ExpiresAt, want)
			}
			before := takeSnapshot(t, f, "up-sink")
			clk.Advance(tc.wait)

			got, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged)

			if tc.applied {
				if err != nil || got.Version != "1.1.0" || got.Upgrade == nil {
					t.Fatalf("confirm before expiry: %+v / %v", got, err)
				}
				return
			}
			if refusalReason(err) != plugins.ReasonStaged || !strings.Contains(err.Error(), "expired") {
				t.Fatalf("confirm after expiry: %q (%v), want a staged refusal saying expired", refusalReason(err), err)
			}
			assertUnchanged(t, f, "up-sink", before)
		})
	}
}

func TestConfirmRechecksEverythingAtApplyTime(t *testing.T) {
	plugins.Parallel(t)
	pub, priv := newKey(t)
	_, other := newKey(t)
	otherPub := other.Public().(ed25519.PublicKey)

	setup := func(t *testing.T, edit func(*pluginapi.Manifest)) (*managerFixture, *plugins.StagedUpgrade) {
		f, _ := previewFixture(t)
		mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, edit))
		return f, staged(t, f, upgradeArchive(t, "up-sink", "1.2.0", "v3", priv, upgradePublisher, widening))
	}
	refused := func(t *testing.T, f *managerFixture, s *plugins.StagedUpgrade, reason string) {
		t.Helper()
		before := takeSnapshot(t, f, "up-sink")
		_, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged)
		if refusalReason(err) != reason {
			t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, reason)
		}
		assertUnchanged(t, f, "up-sink", before)
	}

	t.Run("the installed version changed in between", func(t *testing.T) {
		f, s := setup(t, nil)
		if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil)); err != nil {
			t.Fatal(err)
		}
		refused(t, f, s, plugins.ReasonStaged)
	})
	t.Run("a different key was pinned for the publisher meanwhile", func(t *testing.T) {
		f, s := setup(t, nil)
		if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(otherPub)); err != nil {
			t.Fatal(err)
		}
		refused(t, f, s, plugins.ReasonSignature)
	})
	t.Run("the recorded key no longer matches", func(t *testing.T) {
		f, s := setup(t, nil)
		r := f.store.rows["up-sink"]
		r.SignerKey, r.SignerKeyID = signing.EncodeKey(otherPub), signing.KeyID(otherPub)
		f.store.rows["up-sink"] = r
		refused(t, f, s, plugins.ReasonPublisher)
	})
	t.Run("dependent state appeared meanwhile", func(t *testing.T) {
		f, _ := previewFixture(t)
		mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withOnlineSource))
		s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil))
		f.store.grants["up-sink"] = 3
		refused(t, f, s, plugins.ReasonDependents)
	})
	t.Run("what would be lost changed meanwhile", func(t *testing.T) {
		f, _ := previewFixture(t)
		mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
		s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
			func(m *pluginapi.Manifest) { widening(m); withFields(str("region"))(m) }))
		if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{{Key: "region", Value: `"eu"`}, {Key: "gone", Value: `"x"`}}); err != nil {
			t.Fatal(err)
		}
		refused(t, f, s, plugins.ReasonStaged)
	})
	t.Run("the pinned key that matches lets it through", func(t *testing.T) {
		f, s := setup(t, nil)
		if err := f.manager.PinPublisher(upgradePublisher, signing.EncodeKey(pub)); err != nil {
			t.Fatal(err)
		}
		if got, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); err != nil || got.Version != "1.2.0" {
			t.Fatalf("confirm under a matching pin: %+v / %v", got, err)
		}
	})
	t.Run("with no pins the recorded key alone governs", func(t *testing.T) {
		f, s := setup(t, nil)
		if got, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); err != nil || got.Version != "1.2.0" {
			t.Fatalf("confirm with nothing pinned: %+v / %v", got, err)
		}
	})
	t.Run("a wrong token", func(t *testing.T) {
		f, s := setup(t, nil)
		_, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged+"x")
		if refusalReason(err) != plugins.ReasonStaged {
			t.Fatalf("reason = %q (%v)", refusalReason(err), err)
		}
		// And the right one still works afterwards.
		if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); err != nil {
			t.Fatalf("a wrong guess destroyed the staged upgrade: %v", err)
		}
	})
}

func TestADifferentAdminThanTheStagerCanConfirmAndBothAreRecorded(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	got, err := f.manager.InstallPackage(plugins.WithActor(context.Background(), "ada"),
		upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening), plugins.SourceUpload)
	if err != nil || got.Staged == nil {
		t.Fatalf("staging: %+v / %v", got, err)
	}

	done, err := f.manager.ConfirmUpgrade(plugins.WithActor(context.Background(), "bea"), "up-sink", got.Staged.Staged)

	if err != nil || done.Upgrade == nil {
		t.Fatalf("a second admin could not confirm: %+v / %v", done, err)
	}
	if done.Upgrade.StagedBy != "ada" || done.Upgrade.ConfirmedBy != "bea" {
		t.Fatalf("recorded stager %q and confirmer %q, want ada and bea", done.Upgrade.StagedBy, done.Upgrade.ConfirmedBy)
	}
}

func TestAModuleThatFailsThePreSwapCheckIsRefusedAtUploadAndNeverStaged(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	m := plugintest.SinkManifest("up-sink")
	m.Version = "1.1.0"
	widening(&m)
	manifest := plugintest.ManifestJSON(t, m)
	module := []byte("MZ\x00\x00 definitely a native binary")
	sig, err := signing.Sign(priv, upgradePublisher, manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := signing.Encode(sig)
	_, err = upgrade(f, plugintest.PackageZip(t, manifest, module, doc))

	if refusalReason(err) != plugins.ReasonModule {
		t.Fatalf("reason = %q (%v), want %q", refusalReason(err), err, plugins.ReasonModule)
	}
	assertUnchanged(t, f, "up-sink", before)
	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", "x"); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("something was staged: %v", err)
	}
}

func TestConfirmAppliesExactlyTheStagedBytes(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	archive := upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening)
	pkg, err := plugins.UnpackPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	s := staged(t, f, archive)

	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string][]byte{plugins.ManifestFile: pkg.Manifest, plugins.DefaultModuleFile: pkg.Module, pluginapi.SignatureFile: pkg.Signature} {
		got, err := os.ReadFile(filepath.Join(f.dir, "up-sink", name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s on disk differs from the staged bytes (%v)", name, err)
		}
	}
}

func TestCancelRemovesTheStagedPackageAndANewUploadReplacesIt(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))
	before := takeSnapshot(t, f, "up-sink")

	first := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening))
	if err := f.manager.CancelUpgrade(context.Background(), "up-sink", first.Staged+"x"); refusalReason(err) != plugins.ReasonUnknown {
		t.Fatalf("cancelling with a wrong token: %q (%v), want unknown", refusalReason(err), err)
	}
	if err := f.manager.CancelUpgrade(context.Background(), "up-sink", first.Staged); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", first.Staged); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("a cancelled upgrade confirmed: %v", err)
	}
	assertUnchanged(t, f, "up-sink", before)

	a := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening))
	b := staged(t, f, upgradeArchive(t, "up-sink", "1.2.0", "v3", priv, upgradePublisher, widening))
	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", a.Staged); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("the replaced staged upgrade confirmed: %v", err)
	}
	if got, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", b.Staged); err != nil || got.Version != "1.2.0" {
		t.Fatalf("the replacing upload did not confirm: %+v / %v", got, err)
	}
}

func TestUninstallingAPluginDropsItsStagedUpgrade(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	archive := upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil)
	mustInstall(t, f, archive)
	s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening))
	if err := f.manager.Uninstall(context.Background(), "up-sink"); err != nil {
		t.Fatal(err)
	}
	mustInstall(t, f, archive)

	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("an upgrade staged for the previous install confirmed: %v", err)
	}
}

func TestAnAppliedConfirmedLossyUpgradeMigratesTheSettingsAndTheOrphanIsReported(t *testing.T) {
	plugins.Parallel(t)
	f, _ := previewFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	// "token" is a stale row from an older version, and the new version declares it.
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{
		{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}, {Key: "token", Value: `"stale"`}}); err != nil {
		t.Fatal(err)
	}

	s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(str("region"), pluginapi.SettingsField{Key: "token", Type: pluginapi.FieldString, Required: true})))
	if got := droppedKeysForTest(s.Preview.SettingsDropped); !reflect.DeepEqual(got, []string{"token"}) {
		t.Fatalf("the stale re-declared row is not in the preview: %+v", s.Preview)
	}
	done, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged)
	if err != nil {
		t.Fatal(err)
	}

	after, _ := f.store.PluginSettings("up-sink")
	if want := []store.PluginSetting{{Key: "region", Value: `"eu"`}}; !reflect.DeepEqual(after, want) {
		t.Fatalf("settings = %+v, want %+v", after, want)
	}
	if !reflect.DeepEqual(done.Upgrade.Settings.NeedsValue, []string{"token"}) {
		t.Fatalf("needsValue = %v, want token", done.Upgrade.Settings.NeedsValue)
	}
}
