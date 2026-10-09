package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"), str("port"))))
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{
		{Key: "gone", Value: `"x"`}, {Key: "port", Value: `"80"`}, {Key: "region", Value: `"eu"`},
	}); err != nil {
		t.Fatal(err)
	}

	got := applyConfirmed(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(str("region"), pluginapi.SettingsField{Key: "port", Type: pluginapi.FieldInteger})))
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

func TestWithoutConfirmationALossyUpgradeIsOnlyStagedAndKeepsEverySetting(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	saved := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}
	s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withFields(str("region"))))
	if !reflect.DeepEqual(s.Preview.SettingsDeleted, []string{"gone"}) {
		t.Fatalf("preview = %+v, want gone listed as deleted", s.Preview)
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("a staged upgrade changed settings: %+v", after)
	}
	if v := rowOf(t, f, "up-sink").Version; v != "1.0.0" {
		t.Fatalf("row version = %q, want it untouched until confirmed", v)
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
	got := applyConfirmed(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(secrets("kept-key")[0], pluginapi.SettingsField{Key: "retyped-key", Type: pluginapi.FieldString})))
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
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"))))
	saved := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", saved); err != nil {
		t.Fatal(err)
	}
	f.store.upgradeErr = errors.New("the disk is full")
	s := staged(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, withFields(str("region"))))
	if _, err := f.manager.ConfirmUpgrade(context.Background(), "up-sink", s.Staged); err == nil {
		t.Fatal("a failed row update reported success")
	}
	if after, _ := f.store.PluginSettings("up-sink"); !reflect.DeepEqual(after, saved) {
		t.Fatalf("settings = %+v, want %+v", after, saved)
	}
}

// --- Carry-overs from issue 04, closed by issue 06 -----------------------------

func TestBootFinishingAnInterruptedUpgradeMigratesSettingsLikeANormalOne(t *testing.T) {
	plugins.Parallel(t)
	f := newManagerFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(str("region"), str("gone"), str("port"))))
	original := []store.PluginSetting{{Key: "gone", Value: `"x"`}, {Key: "port", Value: `"80"`}, {Key: "region", Value: `"eu"`}}
	if err := f.store.ReplacePluginSettings("up-sink", original); err != nil {
		t.Fatal(err)
	}
	copyDirForTest(t, filepath.Join(f.dir, "up-sink"), filepath.Join(f.dir, ".upgrade-up-sink-4242-1"))
	applyConfirmed(t, f, upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(str("region"), pluginapi.SettingsField{Key: "port", Type: pluginapi.FieldInteger})))
	want, _ := f.store.PluginSettings("up-sink")
	// Fault injection: the process died after the files were swapped and before the
	// row and the settings were written.
	r := f.store.rows["up-sink"]
	r.Version = "1.0.0"
	f.store.rows["up-sink"] = r
	if err := f.store.ReplacePluginSettings("up-sink", original); err != nil {
		t.Fatal(err)
	}

	recoverIn(t, f)

	if got := rowOf(t, f, "up-sink").Version; got != "1.1.0" {
		t.Fatalf("row version = %q, want the recovery to finish the upgrade (1.1.0)", got)
	}
	got, _ := f.store.PluginSettings("up-sink")
	if !reflect.DeepEqual(got, want) || len(got) != 1 || got[0].Key != "region" {
		t.Fatalf("settings after recovery = %+v, want exactly what a normal upgrade left: %+v", got, want)
	}
}

func TestMigrateSettingsNeitherRevivesNorSilentlyDiscardsAnOrphanTheNewVersionRedeclares(t *testing.T) {
	plugins.Parallel(t)
	old := []pluginapi.SettingsField{str("region")}
	next := []pluginapi.SettingsField{str("region"), {Key: "token", Type: pluginapi.FieldString, Required: true}}
	// "token" was stored by a version older than the installed one and is stale.
	stored := []store.PluginSetting{{Key: "region", Value: `"eu"`}, {Key: "token", Value: `"stale"`}}

	rows, rep := plugins.MigrateSettings(old, next, stored)

	if want := []store.PluginSetting{{Key: "region", Value: `"eu"`}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, want the stale value gone: %+v", rows, want)
	}
	if len(rep.Dropped) != 1 || rep.Dropped[0].Key != "token" || rep.Dropped[0].Reason == "" {
		t.Fatalf("dropped = %+v, want the stale token reported with a reason", rep.Dropped)
	}
	if !reflect.DeepEqual(rep.NeedsValue, []string{"token"}) {
		t.Fatalf("needsValue = %v, want token (required, no default, value discarded)", rep.NeedsValue)
	}
}

func TestMigrateSettingsReportsANarrowedEnumOrBoundAsDroppedNotKept(t *testing.T) {
	plugins.Parallel(t)
	one, ten := 1, 10
	three := 3
	old := []pluginapi.SettingsField{
		{Key: "tier", Type: pluginapi.FieldEnum, Options: []string{"a", "b", "c"}},
		{Key: "tier-ok", Type: pluginapi.FieldEnum, Options: []string{"a", "b", "c"}},
		{Key: "limit", Type: pluginapi.FieldInteger, Min: &one, Max: &ten},
		{Key: "limit-ok", Type: pluginapi.FieldInteger, Min: &one, Max: &ten},
		{Key: "tags", Type: pluginapi.FieldMultiSelect, Options: []string{"a", "b", "c"}},
	}
	next := []pluginapi.SettingsField{
		{Key: "tier", Type: pluginapi.FieldEnum, Options: []string{"a", "b"}},
		{Key: "tier-ok", Type: pluginapi.FieldEnum, Options: []string{"a", "b"}},
		{Key: "limit", Type: pluginapi.FieldInteger, Min: &one, Max: &three},
		{Key: "limit-ok", Type: pluginapi.FieldInteger, Min: &one, Max: &three},
		{Key: "tags", Type: pluginapi.FieldMultiSelect, Options: []string{"a", "b"}},
	}
	stored := []store.PluginSetting{
		{Key: "tier", Value: `"c"`}, {Key: "tier-ok", Value: `"a"`},
		{Key: "limit", Value: `8`}, {Key: "limit-ok", Value: `2`},
		{Key: "tags", Value: `["a","c"]`},
	}

	rows, rep := plugins.MigrateSettings(old, next, stored)

	if got := droppedKeysForTest(rep.Dropped); !reflect.DeepEqual(got, []string{"limit", "tags", "tier"}) {
		t.Fatalf("dropped = %v, want limit, tags and tier (their values no longer fit)", got)
	}
	if !reflect.DeepEqual(rep.Kept, []string{"limit-ok", "tier-ok"}) {
		t.Fatalf("kept = %v, want only the values that still fit", rep.Kept)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want only the two that still fit", rows)
	}
}

func droppedKeysForTest(d []plugins.SettingDropped) []string {
	out := make([]string, 0, len(d))
	for _, x := range d {
		out = append(out, x.Key)
	}
	return out
}
