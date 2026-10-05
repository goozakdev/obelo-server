package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// Replacing a Bundled plugin in place must not leave the previous build's module
// or a stale signature beside the new manifest.
func TestInstallFilesReplaceInPlaceSweepsStaleFiles(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	if err := InstallFiles(dir, "demo", []byte(`{"module":"old.wasm"}`), []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "plugin.sig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallFiles(dir, "demo", []byte(`{"module":"new.wasm"}`), []byte("new")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != 2 || !got["new.wasm"] || !got[ManifestFile] {
		t.Fatalf("plugin dir holds %v, want only new.wasm and %s", got, ManifestFile)
	}
}
