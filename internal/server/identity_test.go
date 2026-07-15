package server

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity's whole value is that it survives — an id that changes across
// boots is worse than no id, because a client would keep rebinding to a "new"
// server and silently drop its token.

func TestLoadOrCreateIdentityIsStableAcrossBoots(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateIdentity(dir, "Living Room")
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if first.ID == "" {
		t.Fatal("first boot minted an empty id")
	}

	second, err := LoadOrCreateIdentity(dir, "Living Room")
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("id changed across boots: %q -> %q", first.ID, second.ID)
	}
}

func TestLoadOrCreateIdentityDistinctPerDataDir(t *testing.T) {
	// Resetting the data dir is the documented cheapest way back to a known state
	// (test-harness.md), and it has no Users, Devices, or tokens to honor — so it
	// SHOULD read as a different server.
	a, err := LoadOrCreateIdentity(t.TempDir(), "")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := LoadOrCreateIdentity(t.TempDir(), "")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("two data dirs produced the same identity")
	}
}

func TestRenamingDoesNotChangeID(t *testing.T) {
	// The id and the name are separate fields precisely so renaming never orphans
	// a Device token. If this ever fails, that guarantee is gone.
	dir := t.TempDir()
	before, err := LoadOrCreateIdentity(dir, "Old Name")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	after, err := LoadOrCreateIdentity(dir, "New Name")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("rename changed the id: %q -> %q", before.ID, after.ID)
	}
	if after.Name != "New Name" {
		t.Fatalf("name = %q, want %q", after.Name, "New Name")
	}
}

func TestIdentityNameFallsBackToSomethingPrintable(t *testing.T) {
	// A nameless server is a blank row in a picker.
	id, err := LoadOrCreateIdentity(t.TempDir(), "   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Name == "" {
		t.Fatal("blank configured name produced an empty display name")
	}
}

func TestLoadOrCreateIdentityRemintsOnCorruptFile(t *testing.T) {
	// An empty identity file is corruption, not a valid identity. Advertising ""
	// would make every such server look like the same server.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identityFile), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	id, err := LoadOrCreateIdentity(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.ID == "" {
		t.Fatal("corrupt file was not re-minted")
	}
}

func TestLoadOrCreateIdentityPersistsToDataDir(t *testing.T) {
	// It lives beside the SQLite file on purpose: readable before the DB is open,
	// and inside the DataDir durability boundary so a reset mints a new one.
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, identityFile)); err != nil {
		t.Fatalf("identity not persisted in the data dir: %v", err)
	}
}

func TestMetadataAdvertisesIdentity(t *testing.T) {
	meta := NewMetadata(fakeUsers{n: 1}, Identity{ID: "srv-1", Name: "Living Room"})
	info, err := meta.Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Identity.ID != "srv-1" || info.Identity.Name != "Living Room" {
		t.Fatalf("handshake identity = %+v, want srv-1/Living Room", info.Identity)
	}
	// The advertiser reads from here too, so both claims come from one source.
	if meta.Identity().ID != "srv-1" {
		t.Fatalf("Metadata.Identity() = %+v", meta.Identity())
	}
}

type fakeUsers struct{ n int }

func (f fakeUsers) CountUsers() (int, error) { return f.n, nil }
