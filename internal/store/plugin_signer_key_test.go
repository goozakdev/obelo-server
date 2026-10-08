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
