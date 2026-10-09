package plugins_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Settings migration on upgrade (ADR-0069, .scratch/plugin-inplace-upgrade issue 04).

const secretValue = `"s3cret-value-do-not-leak"`

func withFields(fields ...pluginapi.SettingsField) func(*pluginapi.Manifest) {
	return func(m *pluginapi.Manifest) { m.Settings.Fields = fields }
}

func str(key string) pluginapi.SettingsField {
	return pluginapi.SettingsField{Key: key, Type: pluginapi.FieldString}
}

func TestAnAdditiveUpgradeReportsWhatItKeptAndAddedAndLeavesTheRowsAlone(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	base := []pluginapi.SettingsField{str("region"), {Key: "token", Type: pluginapi.FieldSecret}, {Key: "base", Type: pluginapi.FieldURL}}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(base...)))
	saved := []store.PluginSetting{
		{Key: "base", Value: `"https://media.example.test"`}, {Key: "region", Value: `"eu"`}, {Key: "token", Value: secretValue, Secret: true},
	}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}

	next := append(append([]pluginapi.SettingsField(nil), base...),
		pluginapi.SettingsField{Key: "limit", Type: pluginapi.FieldInteger, Default: json.RawMessage("5")},
		pluginapi.SettingsField{Key: "audience", Type: pluginapi.FieldString, Required: true})
	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withFields(next...)))
	if err != nil {
		t.Fatalf("an additive upgrade was refused: %v", err)
	}
	after, _ := f.store.PluginSettings("up-sink")
	if !reflect.DeepEqual(after, saved) {
		t.Fatalf("stored settings changed: %+v, want byte-identical %+v", after, saved)
	}
	want := plugins.SettingsReport{
		Kept: []string{"base", "region", "token"}, Added: []string{"audience", "limit"},
		Dropped: []plugins.SettingDropped{}, Deleted: []string{}, NeedsValue: []string{"audience"},
	}
	if got.Upgrade == nil || !reflect.DeepEqual(got.Upgrade.Settings, want) {
		t.Fatalf("settings report = %+v, want %+v", got.Upgrade, want)
	}
	if v := got.Settings.Values["limit"]; v != float64(5) && v != int64(5) && v != 5 {
		t.Fatalf("the new key reads %v, want its default 5", v)
	}
}

func TestMigrateSettingsReportsATypeChangeAsDroppedAndDoesNotCoerceIt(t *testing.T) {
	plugins.Parallel(t)
	old := []pluginapi.SettingsField{str("port"), str("region")}
	next := []pluginapi.SettingsField{{Key: "port", Type: pluginapi.FieldInteger}, str("region")}
	stored := []store.PluginSetting{{Key: "port", Value: `"8080"`}, {Key: "region", Value: `"eu"`}}

	rows, rep := plugins.MigrateSettings(old, next, stored)

	if want := []plugins.SettingDropped{{Key: "port", Reason: "type changed from string to integer"}}; !reflect.DeepEqual(rep.Dropped, want) {
		t.Fatalf("dropped = %+v, want %+v", rep.Dropped, want)
	}
	if want := []store.PluginSetting{{Key: "region", Value: `"eu"`}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, want only the unchanged key %+v (no coercion of the retyped one)", rows, want)
	}
	if !reflect.DeepEqual(rep.Kept, []string{"region"}) || len(rep.Deleted) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestMigrateSettingsReportsARemovedKeyAsDeletedAndOmitsItsRow(t *testing.T) {
	plugins.Parallel(t)
	old := []pluginapi.SettingsField{str("region"), str("gone")}
	next := []pluginapi.SettingsField{str("region")}
	stored := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}

	rows, rep := plugins.MigrateSettings(old, next, stored)

	if !reflect.DeepEqual(rep.Deleted, []string{"gone"}) || len(rep.Dropped) != 0 {
		t.Fatalf("report = %+v, want gone deleted", rep)
	}
	if want := []store.PluginSetting{{Key: "region", Value: `"eu"`}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

func TestMigrateSettingsReportsNothingLostForAKeyThatWasNeverStored(t *testing.T) {
	plugins.Parallel(t)
	old := []pluginapi.SettingsField{str("a"), str("b")}
	next := []pluginapi.SettingsField{{Key: "a", Type: pluginapi.FieldInteger}}
	rows, rep := plugins.MigrateSettings(old, next, nil)
	if len(rows) != 0 || len(rep.Dropped) != 0 || len(rep.Deleted) != 0 || len(rep.Kept) != 0 {
		t.Fatalf("rows %+v report %+v, want nothing reported for values nobody stored", rows, rep)
	}
}

func TestAnAppliedUpgradeDeletesARemovedKeysRowAndDropsARetypedOne(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	f.manager.SetAllowSettingLossForTest(true)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"), str("port"))))
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{
		{Key: "gone", Value: `"x"`}, {Key: "port", Value: `"80"`}, {Key: "region", Value: `"eu"`},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(str("region"), pluginapi.SettingsField{Key: "port", Type: pluginapi.FieldInteger})))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.PluginSettings("up-sink")
	if want := []store.PluginSetting{{Key: "region", Value: `"eu"`}}; !reflect.DeepEqual(after, want) {
		t.Fatalf("settings after = %+v, want %+v", after, want)
	}
	rep := got.Upgrade.Settings
	if !reflect.DeepEqual(rep.Deleted, []string{"gone"}) ||
		!reflect.DeepEqual(rep.Dropped, []plugins.SettingDropped{{Key: "port", Reason: "type changed from string to integer"}}) {
		t.Fatalf("report = %+v", rep)
	}
}

func TestWithoutConfirmationALossyUpgradeIsStillRefusedAndKeepsEverySetting(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	saved := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}
	_, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withFields(str("region"))))
	if refusalReason(err) != plugins.ReasonNeedsConfirmation {
		t.Fatalf("reason = %q (%v), want needs-confirmation", refusalReason(err), err)
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("a refused upgrade changed settings: %+v", after)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *lockedBuf) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sb.WriteString(strings.TrimSpace(fmt.Sprintf(format, args...)) + "\n")
}

func (b *lockedBuf) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.sb.String() }

func TestASecretsValueNeverAppearsInTheSummaryOrTheLog(t *testing.T) {
	plugins.Parallel(t)
	var logs lockedBuf
	f := newManagerFixtureWith(t, func(c *plugins.ManagerConfig) { c.Logf = logs.logf })
	f.manager.SetAllowSettingLossForTest(true)
	_, priv := newKey(t)
	secrets := func(keys ...string) []pluginapi.SettingsField {
		var out []pluginapi.SettingsField
		for _, k := range keys {
			out = append(out, pluginapi.SettingsField{Key: k, Type: pluginapi.FieldSecret})
		}
		return out
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(secrets("kept-key", "retyped-key", "removed-key")...)))
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{
		{Key: "kept-key", Value: secretValue, Secret: true},
		{Key: "removed-key", Value: secretValue, Secret: true},
		{Key: "retyped-key", Value: secretValue, Secret: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(secrets("kept-key")[0], pluginapi.SettingsField{Key: "retyped-key", Type: pluginapi.FieldString})))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(got)
	for name, text := range map[string]string{"response": string(body), "log": logs.String()} {
		if strings.Contains(text, "s3cret-value-do-not-leak") {
			t.Fatalf("the %s carries a secret's value: %s", name, text)
		}
	}
	if !strings.Contains(string(body), "removed-key") || !strings.Contains(string(body), "retyped-key") {
		t.Fatalf("the response does not name the lost keys: %s", body)
	}
}

func TestAFailedModuleCheckAfterTheSettingsDiffLeavesEverySettingRowUnchanged(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	f.manager.SetAllowSettingLossForTest(true)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	saved := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}
	before := takeSnapshot(t, f, "up-sink")

	m := plugintest.SinkManifest("up-sink")
	m.Version = "1.1.0"
	m.Settings.Fields = []pluginapi.SettingsField{str("region")}
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
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("settings = %+v, want %+v", after, saved)
	}
}

func TestAFailedRowUpdateLeavesEverySettingRowUnchanged(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	f.manager.SetAllowSettingLossForTest(true)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	saved := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}
	f.store.upgradeErr = errors.New("the disk is full")
	if _, err := upgrade(f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withFields(str("region")))); err == nil {
		t.Fatal("a failed row update reported success")
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("settings = %+v, want %+v", after, saved)
	}
}
