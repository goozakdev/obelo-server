package bundled

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The manifest guard (D002, .claude/scratch/issue-08-followups): a Bundled
// plugin's manifest CONTENT is pinned to its VERSION, so a manifest that changes
// WITHOUT its version bumping fails here instead of shipping silently.
// internal/bundled/assert.go replaces an installed copy only when the shipped
// version is newer; it has no way to see a same-version content change at all.
// The guard also pins the exact set of Bundled plugins: every `plugins/*/`
// directory must be named in shipped(), and every id in shipped() must have a
// directory.
//
// The golden (testdata/manifest_guard_golden.json, D004) is one canonical hash per
// shipped id, of its manifest with "version" removed — so the hash is blind to the
// one field this guard exists to correlate against, and to whitespace/key order,
// which mean nothing about the manifest's content. Regenerate it after a
// DELIBERATE manifest change with:
//
//	go test ./internal/bundled/ -run TestBundledManifestsMatchTheirGolden -update

// updateManifestGuardGolden is the regeneration seam (D005): -update writes the
// current source manifests' versions+hashes to the golden and passes, rather than
// checking them against what is there.
var updateManifestGuardGolden = flag.Bool("update", false,
	"regenerate internal/bundled/testdata/manifest_guard_golden.json from plugins/*/manifest.json")

const manifestGuardGoldenPath = "testdata/manifest_guard_golden.json"

// manifestGuardEntry is one shipped plugin's golden row.
type manifestGuardEntry struct {
	Version string `json:"version"`
	Hash    string `json:"hash"`
}

// TestBundledManifestsMatchTheirGolden is the guard itself.
//
// Rules, per shipped id:
//   - same version as the golden, different hash → FAIL: the manifest's CONTENT
//     changed without its version bumping.
//   - a different version than the golden (whatever the hash) → FAIL: the golden
//     always mirrors what ships, so a real version bump needs the golden updated too.
//   - shipped but absent from the golden, or in the golden but not shipped → FAIL.
func TestBundledManifestsMatchTheirGolden(t *testing.T) {
	checkShippedMatchesPluginsDir(t)

	src := currentManifestGuardEntries(t)

	if *updateManifestGuardGolden {
		writeManifestGuardGolden(t, src)
		return
	}

	golden := readManifestGuardGolden(t)
	for id, cur := range src {
		want, ok := golden[id]
		if !ok {
			t.Errorf("%s is a shipped plugin but has no entry in %s — run `go test ./internal/bundled/ "+
				"-run TestBundledManifestsMatchTheirGolden -update`", id, manifestGuardGoldenPath)
			continue
		}
		if cur.Version != want.Version {
			t.Errorf("%s's manifest version is %q, the golden says %q — the golden mirrors what "+
				"ships; run `go test ./internal/bundled/ -run TestBundledManifestsMatchTheirGolden -update` "+
				"to update it", id, cur.Version, want.Version)
			continue
		}
		if cur.Hash != want.Hash {
			t.Errorf("%s's manifest changed; bump its \"version\" in plugins/%s/manifest.json, "+
				"run make plugins, then update %s", id, id, manifestGuardGoldenPath)
		}
	}
	for id := range golden {
		if _, ok := src[id]; !ok {
			t.Errorf("%s has an entry in %s but is not a shipped plugin — remove it", id, manifestGuardGoldenPath)
		}
	}
}

// checkShippedMatchesPluginsDir cross-checks shipped() against the directories
// actually on disk under plugins/, each holding a manifest.json: a directory
// shipped() does not list is not a Bundled plugin by this guard's rule ("add it
// to shipped() and the golden, or it is not a Bundled plugin"), and an id in
// shipped() with no such directory is equally wrong.
//
// A symlink is followed, not skipped: os.ReadDir's DirEntry reports the LINK's
// own type, which is never a directory even when it points at one, so `!e.IsDir()`
// alone would let a symlinked plugin dir through this guard unseen. A
// manifest.json stat error other than "not exist" (e.g. permissions) is not
// silently treated as absent — it FAILS, because "the guard could not tell" is
// not the same claim as "there is no manifest here". A DANGLING manifest.json
// symlink is not distinguishable from that case: os.Stat on it returns ENOENT,
// so it stats as not-exist and is skipped exactly like a genuinely absent
// manifest.
func checkShippedMatchesPluginsDir(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatalf("reading plugins/: %v", err)
	}
	onDisk := make(map[string]bool)
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join("..", "..", "plugins", e.Name()))
			isDir = err == nil && info.IsDir()
		}
		if !isDir {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", "plugins", e.Name(), "manifest.json")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("stat plugins/%s/manifest.json: %v", e.Name(), err)
		}
		onDisk[e.Name()] = true
	}
	want := make(map[string]bool, len(shipped()))
	for _, id := range shipped() {
		want[id] = true
		if !onDisk[id] {
			t.Errorf("shipped() lists %q but plugins/%s/manifest.json does not exist", id, id)
		}
	}
	for id := range onDisk {
		if !want[id] {
			t.Errorf("plugins/%s/manifest.json exists but %q is not in shipped() — "+
				"add it to shipped() and the golden, or it is not a Bundled plugin", id, id)
		}
	}
}

// currentManifestGuardEntries reads the SOURCE manifest for every shipped id
// (plugins/<id>/manifest.json, reachable from this package by a relative path
// that survives `go test ./...` from any directory) and canonicalizes each: parse
// as JSON, drop "version", re-encode — whitespace and key order (encoding/json
// sorts map keys) never move the hash.
func currentManifestGuardEntries(t *testing.T) map[string]manifestGuardEntry {
	t.Helper()
	out := make(map[string]manifestGuardEntry, len(shipped()))
	for _, id := range shipped() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", id, "manifest.json"))
		if err != nil {
			t.Fatalf("reading plugins/%s/manifest.json: %v", id, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("plugins/%s/manifest.json is not valid JSON: %v", id, err)
		}
		version, _ := doc["version"].(string)
		delete(doc, "version")
		canon, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("canonicalizing plugins/%s/manifest.json: %v", id, err)
		}
		sum := sha256.Sum256(canon)
		out[id] = manifestGuardEntry{Version: version, Hash: hex.EncodeToString(sum[:])}
	}
	return out
}

func readManifestGuardGolden(t *testing.T) map[string]manifestGuardEntry {
	t.Helper()
	raw, err := os.ReadFile(manifestGuardGoldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update to create it)", manifestGuardGoldenPath, err)
	}
	var m map[string]manifestGuardEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", manifestGuardGoldenPath, err)
	}
	return m
}

func writeManifestGuardGolden(t *testing.T, entries map[string]manifestGuardEntry) {
	t.Helper()
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", manifestGuardGoldenPath, err)
	}
	if err := os.WriteFile(manifestGuardGoldenPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", manifestGuardGoldenPath, err)
	}
}
