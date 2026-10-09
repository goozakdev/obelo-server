package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The trust-on-first-install record (ADR-0069) is separate from the pinned
// publisher/key_id columns, and a row that never had one reads back as no key.

func TestPluginSignerKeyIsRecordedApartFromThePinnedColumns(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertPlugin(store.PluginInsert{ID: "p", Source: plugins.SourceUpload, Origin: plugins.OriginAdmin}); err != nil {
		t.Fatal(err)
	}
	read := func() store.PluginRow {
		rows, err := db.Plugins()
		if err != nil || len(rows) != 1 {
			t.Fatalf("Plugins() = %v, %v", rows, err)
		}
		return rows[0]
	}
	if r := read(); r.SignerName != "" || r.SignerKey != "" || r.SignerKeyID != "" {
		t.Fatalf("a fresh row has a recorded key: %+v", r)
	}
	if err := db.SetPluginSignerKey("p", "Pub", "a2V5", "kid"); err != nil {
		t.Fatal(err)
	}
	r := read()
	if r.SignerName != "Pub" || r.SignerKey != "a2V5" || r.SignerKeyID != "kid" {
		t.Fatalf("recorded %+v, want Pub / a2V5 / kid", r)
	}
	if r.Publisher != "" || r.KeyID != "" {
		t.Fatalf("the pinned columns were fed: %q / %q", r.Publisher, r.KeyID)
	}
}

func TestMigrationLeavesExistingPluginRowsWithNoKey(t *testing.T) {
	db := existingDB(t)
	mustExec(t, db, `INSERT INTO plugins (id, origin) VALUES ('old', 'admin')`)
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Plugins()
	if err != nil || len(rows) != 1 {
		t.Fatalf("Plugins() = %v, %v", rows, err)
	}
	if r := rows[0]; r.SignerName != "" || r.SignerKey != "" || r.SignerKeyID != "" {
		t.Fatalf("a pre-existing row gained a key: %+v", r)
	}
}

func TestUpgradePluginReplacesTheRowsFactsAndSignerAndNothingElse(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertPlugin(store.PluginInsert{ID: "p", Name: "Old", Version: "1.0.0", APIVersion: 1,
		Provides: []string{"event-sink"}, Source: plugins.SourceUpload, Origin: plugins.OriginBundled}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPluginSigner("p", "Pinned", "kid-old"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPluginSignerKey("p", "Pub", "a2V5", "kid-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetPluginEnabled("p", false); err != nil {
		t.Fatal(err)
	}
	before, _ := db.Plugins()

	if err := db.UpgradePlugin(store.PluginUpgrade{ID: "p", Name: "New", Version: "1.1.0", APIVersion: 1,
		Provides: []string{"event-sink", "subtitle-provider"}, Source: "https://x.test/p.zip", Origin: plugins.OriginAdmin,
		SignerName: "Pub", SignerKey: "a2V5", SignerKeyID: "kid-old"}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Plugins()
	if err != nil || len(rows) != 1 {
		t.Fatalf("Plugins() = %v, %v", rows, err)
	}
	r := rows[0]
	if r.Name != "New" || r.Version != "1.1.0" || len(r.Provides) != 2 || r.Source != "https://x.test/p.zip" || r.Origin != plugins.OriginAdmin {
		t.Fatalf("the facts were not replaced: %+v", r)
	}
	if r.Publisher != "" || r.KeyID != "" {
		t.Fatalf("the previous version's pinned publisher survived: %q / %q", r.Publisher, r.KeyID)
	}
	if r.SignerKey != "a2V5" || r.SignerName != "Pub" {
		t.Fatalf("the signer was not recorded: %+v", r)
	}
	if r.Enabled || r.InstalledAt != before[0].InstalledAt {
		t.Fatalf("the enable switch or install time changed: %+v", r)
	}
	if err := db.UpgradePlugin(store.PluginUpgrade{ID: "nobody"}); err == nil {
		t.Fatal("upgrading a row that does not exist reported success")
	}
}

func TestUpdatePluginManifestSetsTheSourceBackToTheShippedMarker(t *testing.T) {
	db := openTemp(t)
	if err := db.InsertPlugin(store.PluginInsert{ID: "p", Version: "2.0.0", Source: plugins.SourceUpload, Origin: plugins.OriginBundled}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePluginManifest(store.PluginInsert{ID: "p", Version: "3.0.0", Source: "shipped with obelo", Origin: plugins.OriginBundled}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.Plugins()
	if len(rows) != 1 || rows[0].Source != "shipped with obelo" || rows[0].Version != "3.0.0" {
		t.Fatalf("row = %+v, want source back to the shipped marker", rows)
	}
}
