package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestTailnetSettingsAbsentRow: nothing about remote access may require a row to
// exist. A migrated-but-never-seeded database — every install between the
// migration running and the first boot's seed, and any narrow test that skipped
// it — must read as the shipped default: off, and flagged as never written so the
// first-boot seed still fires.
func TestTailnetSettingsAbsentRow(t *testing.T) {
	db := openTemp(t)

	empty, err := db.TailnetSettingsEmpty()
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if !empty {
		t.Error("a freshly migrated DB reports tailnet settings already written")
	}
	got, err := db.TailnetSettings()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Enabled || got.Hostname != "" || got.ControlURL != "" || got.HTTPSEnabled {
		t.Errorf("settings with no row = %+v, want the all-off zero value", got)
	}
}

// TestTailnetSettingsRoundTrip: the full write, and the narrow enabled-only write
// that Connect/Disconnect use — which must not disturb the other three columns,
// because a connect that clobbered the hostname an operator had just saved would
// be a bug nobody would suspect the connect button of.
func TestTailnetSettingsRoundTrip(t *testing.T) {
	db := openTemp(t)

	if err := db.SetTailnetSettings(store.TailnetSettingsUpsert{
		Enabled:      false,
		Hostname:     "attic",
		ControlURL:   "https://headscale.example.com",
		HTTPSEnabled: true,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	empty, err := db.TailnetSettingsEmpty()
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if empty {
		t.Error("settings still report empty after a write; the first-boot seed would re-fire every boot")
	}

	got, err := db.TailnetSettings()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Enabled || got.Hostname != "attic" || got.ControlURL != "https://headscale.example.com" || !got.HTTPSEnabled {
		t.Fatalf("round-tripped = %+v, want what was written", got)
	}

	if err := db.SetTailnetEnabled(true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err = db.TailnetSettings()
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.Enabled || got.Hostname != "attic" || got.ControlURL != "https://headscale.example.com" || !got.HTTPSEnabled {
		t.Errorf("after SetTailnetEnabled = %+v, want only enabled changed", got)
	}

	// A cleared control URL reads back the same as one that was never set: both
	// mean Tailscale's own coordination server.
	if err := db.SetTailnetSettings(store.TailnetSettingsUpsert{Hostname: "attic"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err = db.TailnetSettings(); err != nil || got.ControlURL != "" {
		t.Errorf("cleared control URL = %q (err %v), want \"\"", got.ControlURL, err)
	}
}

// TestSetTailnetEnabledCreatesTheRow: connecting on an install whose settings
// were never seeded must still record the desire, or a restart would silently
// drop the operator back out of their own Tailnet.
func TestSetTailnetEnabledCreatesTheRow(t *testing.T) {
	db := openTemp(t)

	if err := db.SetTailnetEnabled(true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err := db.TailnetSettings()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Enabled {
		t.Error("enabling on an unseeded install did not persist")
	}
}
