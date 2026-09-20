package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// An Installed plugin's manifest-declared settings (.scratch/plugin-system issue
// 13). Two properties, both of them schema facts and therefore cheapest to break
// by editing a WHERE clause:
//
//   - the rows are scoped per Plugin, like the key-value namespace beside them, and
//   - a save REPLACES the set, so a field a manifest stopped declaring stops
//     existing rather than lingering invisibly and still reaching the guest.
//
// And one that is not: uninstall takes them with it, which the schema's own
// comment promises and DeletePlugin's transaction is what keeps.

func TestPluginSettingsAreScopedAndReplacedWhole(t *testing.T) {
	db := openTemp(t)

	if got, err := db.PluginSettings("alpha"); err != nil || len(got) != 0 {
		t.Fatalf("a plugin with nothing saved = (%+v, %v), want empty", got, err)
	}

	if err := db.ReplacePluginSettings("alpha", []store.PluginSetting{
		{Key: "region", Value: `"eu"`},
		{Key: "token", Value: `"sekrit"`, Secret: true},
	}); err != nil {
		t.Fatalf("ReplacePluginSettings alpha: %v", err)
	}
	if err := db.ReplacePluginSettings("beta", []store.PluginSetting{
		{Key: "region", Value: `"apac"`},
	}); err != nil {
		t.Fatalf("ReplacePluginSettings beta: %v", err)
	}

	// The same key, two Plugins, two values, and the secret flag travels with the
	// row rather than being re-derived from a manifest the store never reads.
	alpha := settingsByKey(t, db, "alpha")
	if alpha["region"].Value != `"eu"` || alpha["token"].Value != `"sekrit"` || !alpha["token"].Secret {
		t.Fatalf("alpha = %+v, want its own two rows with the secret marked", alpha)
	}
	if beta := settingsByKey(t, db, "beta"); beta["region"].Value != `"apac"` || len(beta) != 1 {
		t.Fatalf("beta = %+v, want only its own row", beta)
	}

	// A save replaces the whole set: `token` is gone because this save did not
	// carry it, which is what makes a field the manifest dropped stop existing.
	if err := db.ReplacePluginSettings("alpha", []store.PluginSetting{
		{Key: "region", Value: `"us"`},
	}); err != nil {
		t.Fatalf("ReplacePluginSettings alpha (replace): %v", err)
	}
	alpha = settingsByKey(t, db, "alpha")
	if len(alpha) != 1 || alpha["region"].Value != `"us"` {
		t.Fatalf("after a replacing save alpha = %+v, want exactly the new set", alpha)
	}
	if settingsByKey(t, db, "beta")["region"].Value != `"apac"` {
		t.Fatal("writing one plugin's settings changed another's")
	}
}

func TestUninstallTakesTheDeclaredSettingsWithIt(t *testing.T) {
	db := openTemp(t)

	if err := db.InsertPlugin(store.PluginInsert{ID: "alpha", Name: "Alpha", APIVersion: 1}); err != nil {
		t.Fatalf("InsertPlugin: %v", err)
	}
	if err := db.ReplacePluginSettings("alpha", []store.PluginSetting{
		{Key: "token", Value: `"sekrit"`, Secret: true},
	}); err != nil {
		t.Fatalf("ReplacePluginSettings: %v", err)
	}
	if err := db.ReplacePluginSettings("beta", []store.PluginSetting{
		{Key: "token", Value: `"other"`, Secret: true},
	}); err != nil {
		t.Fatalf("ReplacePluginSettings beta: %v", err)
	}

	if err := db.DeletePlugin("alpha"); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}
	if got, _ := db.PluginSettings("alpha"); len(got) != 0 {
		t.Fatalf("uninstall left %+v behind — a stranded secret under a slug no plugin claims", got)
	}
	if got, _ := db.PluginSettings("beta"); len(got) != 1 {
		t.Fatalf("uninstalling one plugin took another's settings: %+v", got)
	}
}

func settingsByKey(t *testing.T, db *store.DB, pluginID string) map[string]store.PluginSetting {
	t.Helper()
	rows, err := db.PluginSettings(pluginID)
	if err != nil {
		t.Fatalf("PluginSettings %s: %v", pluginID, err)
	}
	out := make(map[string]store.PluginSetting, len(rows))
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}
