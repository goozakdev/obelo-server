package store_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
)

// An upgrade's settings migration is written in the row's own transaction
// (ADR-0069, .scratch/plugin-inplace-upgrade issue 04): all of it or none of it, and
// the plugin's key-value data is never touched.

func insertUpgradable(t *testing.T, db *store.DB) []store.PluginSetting {
	t.Helper()
	if err := db.InsertPlugin(store.PluginInsert{ID: "p", Name: "Old", Version: "1.0.0", APIVersion: 1,
		Provides: []string{"event-sink"}, Source: plugins.SourceUpload, Origin: plugins.OriginAdmin}); err != nil {
		t.Fatal(err)
	}
	saved := []store.PluginSetting{
		{Key: "a", Value: `"1"`}, {Key: "b", Value: `"2"`, Secret: true}, {Key: "c", Value: `"3"`},
	}
	if err := db.ReplacePluginSettings("p", saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestUpgradePluginReplacesTheSettingsWithTheMigratedSetAndKeepsKeyValueData(t *testing.T) {
	db := openTemp(t)
	insertUpgradable(t, db)
	if err := db.SetPluginKV("p", "cursor", []byte("page-7")); err != nil {
		t.Fatal(err)
	}

	kept := []store.PluginSetting{{Key: "b", Value: `"2"`, Secret: true}}
	if err := db.UpgradePlugin(store.PluginUpgrade{ID: "p", Name: "New", Version: "1.1.0", APIVersion: 1,
		Source: plugins.SourceUpload, Origin: plugins.OriginAdmin, ReplaceSettings: true, Settings: kept}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.PluginSettings("p")
	if !reflect.DeepEqual(got, kept) {
		t.Fatalf("settings = %+v, want %+v", got, kept)
	}
	v, found, err := db.PluginKV("p", "cursor")
	if err != nil || !found || !bytes.Equal(v, []byte("page-7")) {
		t.Fatalf("kv after the upgrade = %q found=%v err=%v, want the value written before", v, found, err)
	}
}

func TestUpgradePluginWithoutReplaceSettingsLeavesTheSettingsAlone(t *testing.T) {
	db := openTemp(t)
	saved := insertUpgradable(t, db)
	if err := db.UpgradePlugin(store.PluginUpgrade{ID: "p", Name: "New", Version: "1.1.0", APIVersion: 1,
		Source: plugins.SourceUpload, Origin: plugins.OriginAdmin}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.PluginSettings("p"); !reflect.DeepEqual(got, saved) {
		t.Fatalf("settings = %+v, want %+v", got, saved)
	}
}

func TestAFailedSettingsWriteRollsBackTheRowAndEverySetting(t *testing.T) {
	db := openTemp(t)
	saved := insertUpgradable(t, db)

	// Two rows under one key: the second insert violates the primary key AFTER the old
	// rows were deleted and the plugin row updated.
	err := db.UpgradePlugin(store.PluginUpgrade{ID: "p", Name: "New", Version: "1.1.0", APIVersion: 1,
		Source: plugins.SourceUpload, Origin: plugins.OriginAdmin, ReplaceSettings: true,
		Settings: []store.PluginSetting{{Key: "a", Value: `"x"`}, {Key: "a", Value: `"y"`}}})
	if err == nil {
		t.Fatal("a failed settings write reported success")
	}
	if got, _ := db.PluginSettings("p"); !reflect.DeepEqual(got, saved) {
		t.Fatalf("settings = %+v, want every row as it was: %+v", got, saved)
	}
	rows, _ := db.Plugins()
	if len(rows) != 1 || rows[0].Version != "1.0.0" || rows[0].Name != "Old" {
		t.Fatalf("the row changed although the upgrade failed: %+v", rows)
	}
}
