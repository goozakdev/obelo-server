package bundled

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The source guard (D003, D007, .claude/scratch/release-gate-loose-ends): the
// manifest guard above pins a shipped plugin's manifest CONTENT to its VERSION,
// but a plugin's SOURCE — plugins/<id>/*.go, go.mod, go.sum, sub-packages, anything
// else that ships — has nothing pinning it at all. A source change with no version
// bump is invisible here and to assert.go, which only replaces an installed copy
// when the shipped version is NEWER: same version, different code, ships silently.
//
// The golden (testdata/source_guard_golden.json) is one hash per shipped id, over
// an ALLOWLIST of what a plugin actually ships: non-test *.go files, go.mod and
// go.sum, anywhere under plugins/<id>/ except testdata/ (fixtures, never ship).
// manifest.json is excluded — it is already pinned by the manifest guard, and its
// version is what this golden is pinned to, not part of what it hashes. Everything
// else on disk (host junk like .DS_Store, editor swap files, scratch output) is
// not in the allowlist and is simply not hashed. Regenerate this golden alone,
// after a DELIBERATE source change, with:
//
//	go test ./internal/bundled/ -run TestBundledSourceMatchesTheirGolden -update
const sourceGuardGoldenPath = "testdata/source_guard_golden.json"

// TestBundledSourceMatchesTheirGolden is the source guard. Same three rules as
// TestBundledManifestsMatchTheirGolden, over a source hash instead of a manifest
// hash: same version, different hash → FAIL; different version → FAIL (the golden
// mirrors what ships); shipped but absent from the golden, or the reverse → FAIL.
func TestBundledSourceMatchesTheirGolden(t *testing.T) {
	src := currentSourceGuardEntries(t)

	if *updateManifestGuardGolden {
		writeSourceGuardGolden(t, src)
		return
	}

	golden := readSourceGuardGolden(t)
	for id, cur := range src {
		want, ok := golden[id]
		if !ok {
			t.Errorf("%s is a shipped plugin but has no entry in %s — run `go test ./internal/bundled/ "+
				"-run TestBundledSourceMatchesTheirGolden -update`", id, sourceGuardGoldenPath)
			continue
		}
		if cur.Version != want.Version {
			t.Errorf("%s's manifest version is %q, the golden says %q — the golden mirrors what "+
				"ships; run `go test ./internal/bundled/ -run TestBundledSourceMatchesTheirGolden "+
				"-update` to update it", id, cur.Version, want.Version)
			continue
		}
		if cur.Hash != want.Hash {
			t.Errorf("%s's source changed; bump its \"version\" in plugins/%s/manifest.json, "+
				"run make plugins, then update %s", id, id, sourceGuardGoldenPath)
		}
	}
	for id := range golden {
		if _, ok := src[id]; !ok {
			t.Errorf("%s has an entry in %s but is not a shipped plugin — remove it", id, sourceGuardGoldenPath)
		}
	}
}

// currentSourceGuardEntries pairs each shipped id's manifest version (read the
// same way as the manifest guard, so the two goldens always agree on what
// "version" means) with a hash of its allowlisted source files.
func currentSourceGuardEntries(t *testing.T) map[string]manifestGuardEntry {
	t.Helper()
	versions := currentManifestGuardEntries(t)
	out := make(map[string]manifestGuardEntry, len(shipped()))
	for _, id := range shipped() {
		out[id] = manifestGuardEntry{Version: versions[id].Version, Hash: sourceHash(t, id)}
	}
	return out
}

// isAllowlistedSourceFile reports whether name is one the source guard hashes:
// a non-test .go file, go.mod, or go.sum. Anything not on this list (host junk
// like .DS_Store, editor swap files, scratch output, manifest.json) is simply
// not part of what a plugin ships, and is not hashed.
func isAllowlistedSourceFile(name string) bool {
	if name == "go.mod" || name == "go.sum" {
		return true
	}
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// sourceHash hashes the allowlisted files under plugins/<id>/ (see
// isAllowlistedSourceFile), skipping testdata/: sorted relative slash paths,
// each path, its byte length, and its content fed to the hash in that order,
// so the result depends on nothing but what is actually on disk — no mtimes, no
// directory-listing order, no git — and no two distinct file sets can produce
// the same byte stream by shifting a boundary (the decimal length between path
// and content pins where each file's bytes end).
//
// A file that matches the allowlist by name but is not a regular file (e.g. a
// symlink) fails the test loudly rather than being silently hashed or skipped:
// this guard's whole point is to know what ships, and "what does opening this
// path actually give a user" is not something a directory listing can answer.
// A hashed .go file containing "//go:embed" also fails loudly: an embedded file
// ships as part of the binary without ever matching the allowlist by name, so
// the allowlist would need to learn about it explicitly.
func sourceHash(t *testing.T, id string) string {
	t.Helper()
	root := filepath.Join("..", "..", "plugins", id)
	contents := make(map[string][]byte)
	var relPaths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !isAllowlistedSourceFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s matches the source allowlist by name but is not a regular file (mode %s)", path, info.Mode())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(d.Name(), ".go") && strings.Contains(string(raw), "//go:embed") {
			return fmt.Errorf("%s contains \"//go:embed\" — the source allowlist must learn about embedded files "+
				"before this plugin can ship one", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		relPaths = append(relPaths, rel)
		contents[rel] = raw
		return nil
	})
	if err != nil {
		t.Fatalf("walking plugins/%s: %v", id, err)
	}
	sort.Strings(relPaths)
	h := sha256.New()
	for _, rel := range relPaths {
		content := contents[rel]
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write([]byte(fmt.Sprintf("%d", len(content))))
		h.Write([]byte{0})
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readSourceGuardGolden(t *testing.T) map[string]manifestGuardEntry {
	t.Helper()
	raw, err := os.ReadFile(sourceGuardGoldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update to create it)", sourceGuardGoldenPath, err)
	}
	var m map[string]manifestGuardEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", sourceGuardGoldenPath, err)
	}
	return m
}

func writeSourceGuardGolden(t *testing.T, entries map[string]manifestGuardEntry) {
	t.Helper()
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", sourceGuardGoldenPath, err)
	}
	if err := os.WriteFile(sourceGuardGoldenPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", sourceGuardGoldenPath, err)
	}
}
