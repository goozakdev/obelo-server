package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The plugin-scoped key-value namespace (ADR-0058 decision 5, .scratch/
// plugin-system issue 11). Two properties, and both are the point of the table's
// primary key rather than of anything a caller does:
//
//   - two Plugins writing the SAME key do not see each other's value, and
//   - uninstall drops one Plugin's namespace and touches nobody else's.
//
// They are tested here, at the store, as well as through two real guests in
// internal/plugins, because the isolation is a schema fact and a schema fact is
// cheapest to break by editing a WHERE clause.

func TestPluginKVIsScopedPerPlugin(t *testing.T) {
	db := openTemp(t)

	// An unwritten key is ABSENT, which is not the same as empty.
	if v, found, err := db.PluginKV("alpha", "cursor"); err != nil || found || v != nil {
		t.Fatalf("unwritten key = (%q, found=%v, err=%v), want absent", v, found, err)
	}

	if err := db.SetPluginKV("alpha", "cursor", []byte("page-1")); err != nil {
		t.Fatalf("SetPluginKV alpha: %v", err)
	}
	if err := db.SetPluginKV("beta", "cursor", []byte("page-99")); err != nil {
		t.Fatalf("SetPluginKV beta: %v", err)
	}

	// The same key, two Plugins, two values. Neither can see the other's.
	if v, found, err := db.PluginKV("alpha", "cursor"); err != nil || !found || string(v) != "page-1" {
		t.Fatalf("alpha/cursor = (%q, found=%v, err=%v), want page-1", v, found, err)
	}
	if v, found, err := db.PluginKV("beta", "cursor"); err != nil || !found || string(v) != "page-99" {
		t.Fatalf("beta/cursor = (%q, found=%v, err=%v), want page-99", v, found, err)
	}

	// A write replaces rather than duplicating.
	if err := db.SetPluginKV("alpha", "cursor", []byte("page-2")); err != nil {
		t.Fatalf("SetPluginKV alpha (replace): %v", err)
	}
	if v, _, _ := db.PluginKV("alpha", "cursor"); string(v) != "page-2" {
		t.Fatalf("after replacing, alpha/cursor = %q, want page-2", v)
	}
	if v, _, _ := db.PluginKV("beta", "cursor"); string(v) != "page-99" {
		t.Fatalf("beta's value changed when alpha wrote: %q", v)
	}

	// An EMPTY value is a value: found is true and the bytes are empty. A guest
	// caching "I asked and there was nothing" depends on this.
	if err := db.SetPluginKV("alpha", "empty", nil); err != nil {
		t.Fatalf("SetPluginKV empty: %v", err)
	}
	if v, found, err := db.PluginKV("alpha", "empty"); err != nil || !found || len(v) != 0 {
		t.Fatalf("alpha/empty = (%q, found=%v, err=%v), want a present empty value", v, found, err)
	}

	// Delete removes one key and nothing else, and deleting an absent key is fine.
	if err := db.DeletePluginKV("alpha", "empty"); err != nil {
		t.Fatalf("DeletePluginKV: %v", err)
	}
	if err := db.DeletePluginKV("alpha", "never-written"); err != nil {
		t.Fatalf("deleting an absent key is an error: %v", err)
	}
	if _, found, _ := db.PluginKV("alpha", "empty"); found {
		t.Fatal("alpha/empty survived its delete")
	}
	if v, _, _ := db.PluginKV("alpha", "cursor"); string(v) != "page-2" {
		t.Fatalf("deleting one key took another with it: cursor = %q", v)
	}
}

// TestUninstallDropsOnlyThatPluginsNamespace is the method issue 10's uninstall
// calls. Everything the Plugin stored goes; nobody else's does.
func TestUninstallDropsOnlyThatPluginsNamespace(t *testing.T) {
	db := openTemp(t)
	for _, k := range []string{"a", "b", "c"} {
		if err := db.SetPluginKV("alpha", k, []byte(k)); err != nil {
			t.Fatalf("seeding alpha/%s: %v", k, err)
		}
		if err := db.SetPluginKV("beta", k, []byte(k)); err != nil {
			t.Fatalf("seeding beta/%s: %v", k, err)
		}
	}

	if err := db.DeletePluginNamespace("alpha"); err != nil {
		t.Fatalf("DeletePluginNamespace: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, found, _ := db.PluginKV("alpha", k); found {
			t.Errorf("alpha/%s survived the namespace drop", k)
		}
		if _, found, _ := db.PluginKV("beta", k); !found {
			t.Errorf("beta/%s was dropped with alpha's namespace", k)
		}
	}

	// Dropping a namespace that never existed is not an error: uninstall is
	// idempotent, and a Plugin that never wrote anything must still uninstall.
	if err := db.DeletePluginNamespace("gamma"); err != nil {
		t.Fatalf("dropping an empty namespace: %v", err)
	}

	// The id is never optional. A method that accepted "" would drop nothing or
	// everything depending on the WHERE clause, and both are wrong.
	if err := db.DeletePluginNamespace(""); err == nil {
		t.Error("DeletePluginNamespace(\"\") was accepted")
	}
	if err := db.SetPluginKV("", "k", []byte("v")); err == nil {
		t.Error("SetPluginKV with no plugin id was accepted")
	}
	if _, _, err := db.PluginKV("", "k"); err == nil {
		t.Error("PluginKV with no plugin id was accepted")
	}
}

// TestDeletePluginTakesTheNamespaceWithIt is .scratch/plugin-system issue 16, at
// the layer where the promise is kept: UNINSTALL is DeletePlugin, and until this
// it deleted the declared settings, the sink row and the plugins row while
// leaving the guest's own rows behind. A Plugin reinstalled under the same id
// then read the previous install's cursors back through kv_get as if it had
// written them.
//
// The two Plugins share a key on purpose: dropping one namespace has to stay the
// one-line property the primary key gives, not a delete that guessed at a prefix.
func TestDeletePluginTakesTheNamespaceWithIt(t *testing.T) {
	db := openTemp(t)
	seedTwoPlugins(t, db)

	if err := db.DeletePlugin("alpha"); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}

	if v, found, err := db.PluginKV("alpha", "shared-key"); err != nil || found {
		t.Errorf("after uninstalling alpha its key reads (%q, found=%v, err=%v), want absent", v, found, err)
	}
	if v, found, err := db.PluginKV("beta", "shared-key"); err != nil || !found || string(v) != "beta-cursor" {
		t.Errorf("beta's value under the same key = (%q, found=%v, err=%v), want beta-cursor", v, found, err)
	}

	// The other three deletes are exactly as they were.
	if got, err := db.PluginSettings("alpha"); err != nil || len(got) != 0 {
		t.Errorf("alpha's declared settings = (%+v, %v), want none", got, err)
	}
	if got, err := db.PluginSettings("beta"); err != nil || len(got) != 1 {
		t.Errorf("beta's declared settings = (%+v, %v), want its one row", got, err)
	}
	rows, err := db.Plugins()
	if err != nil {
		t.Fatalf("Plugins: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "beta" {
		t.Errorf("the plugins table holds %+v, want beta alone", rows)
	}
	if n := countSinks(t, db); n != 1 {
		t.Errorf("event_sinks holds %d rows, want beta's alone", n)
	}
}

// TestAFailedUninstallLeavesEverything: the four deletes are ONE transaction, so
// a Plugin is uninstalled or it is not. Nothing in this server can make the
// plugins delete fail on demand, so the test takes the table away — the same
// shape of failure (a statement that cannot run) and the only one a store test
// can stage honestly.
func TestAFailedUninstallLeavesEverything(t *testing.T) {
	db := openTemp(t)
	seedTwoPlugins(t, db)
	mustExec(t, db, `DROP TABLE plugins`)

	if err := db.DeletePlugin("alpha"); err == nil {
		t.Fatal("DeletePlugin reported success with its last statement unable to run")
	}
	// The three deletes that DID run were rolled back with it.
	if v, found, _ := db.PluginKV("alpha", "shared-key"); !found || string(v) != "alpha-cursor" {
		t.Errorf("alpha's namespace = (%q, found=%v) after a failed uninstall, want it untouched", v, found)
	}
	if got, _ := db.PluginSettings("alpha"); len(got) != 1 {
		t.Errorf("alpha's declared settings = %+v after a failed uninstall, want its one row", got)
	}
	if n := countSinks(t, db); n != 2 {
		t.Errorf("event_sinks holds %d rows after a failed uninstall, want both", n)
	}
}

// seedTwoPlugins installs two Plugins with a row apiece in every table an
// uninstall touches, and the SAME key in both namespaces.
func seedTwoPlugins(t *testing.T, db *store.DB) {
	t.Helper()
	for _, id := range []string{"alpha", "beta"} {
		if err := db.InsertPlugin(store.PluginInsert{
			ID: id, Name: "Test " + id, Version: "1.0.0", APIVersion: 1,
			Provides: []string{"event-sink"}, Source: "upload", Origin: plugins.OriginAdmin,
		}); err != nil {
			t.Fatalf("InsertPlugin %s: %v", id, err)
		}
		if err := db.SetPluginKV(id, "shared-key", []byte(id+"-cursor")); err != nil {
			t.Fatalf("seeding %s's namespace: %v", id, err)
		}
		if err := db.ReplacePluginSettings(id, []store.PluginSetting{
			{Key: "region", Value: `"eu"`},
		}); err != nil {
			t.Fatalf("seeding %s's declared settings: %v", id, err)
		}
		mustExec(t, db,
			`INSERT INTO event_sinks (slug, enabled, secret, url, events)
			      VALUES (?, 1, 'topsecret', 'https://sink.example.test/hook', 'scan.completed')`, id)
	}
}

func countSinks(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM event_sinks`).Scan(&n); err != nil {
		t.Fatalf("counting event sinks: %v", err)
	}
	return n
}
