package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

func TestAllPluginSettingsGroupsEveryPluginsRowsByKey(t *testing.T) {
	db := openTemp(t)

	got, err := db.AllPluginSettings()
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing saved = (%+v, %v), want empty", got, err)
	}

	alpha := []store.PluginSetting{
		{Key: "region", Value: `"eu"`},
		{Key: "token", Value: `"sekrit"`, Secret: true},
	}
	beta := []store.PluginSetting{{Key: "region", Value: `"apac"`}}
	if err := db.ReplacePluginSettings("alpha", alpha); err != nil {
		t.Fatalf("ReplacePluginSettings alpha: %v", err)
	}
	if err := db.ReplacePluginSettings("beta", beta); err != nil {
		t.Fatalf("ReplacePluginSettings beta: %v", err)
	}

	got, err = db.AllPluginSettings()
	if err != nil {
		t.Fatalf("AllPluginSettings: %v", err)
	}
	want := map[string][]store.PluginSetting{"alpha": alpha, "beta": beta}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllPluginSettings = %+v, want %+v", got, want)
	}
}
