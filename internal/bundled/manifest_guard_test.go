package bundled

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
//
// -update REFUSES to write THIS golden when any shipped id's version is
// unchanged from it — either the working copy or the one committed at git
// HEAD — but its hash differs (D002): that combination is exactly a
// same-version content change, and writing even the other, legitimate rows
// would launder it through the tool meant to catch it. Each guard decides
// for its own golden only: the source guard, run in the same `go test`
// invocation, may still write its golden when it has nothing to refuse, so
// a refused run can leave the two goldens disagreeing on a bumped version
// until the refusal is fixed and -update rerun. Bump the version
// first, then rerun -update. A brand-new shipped id (absent from both
// goldens) is written; an id no longer shipped is dropped. The refusal check
// needs git; the plain (non -update) run above does not.

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
//
// -update follows the same same-version-different-hash refusal (D002); see
// updateGuardGolden.
func TestBundledManifestsMatchTheirGolden(t *testing.T) {
	checkShippedMatchesPluginsDir(t)

	src := currentManifestGuardEntries(t)

	if *updateManifestGuardGolden {
		old := readOptionalGuardGolden(t, manifestGuardGoldenPath)
		head := readHeadGuardGolden(t, filepath.Join("..", ".."), manifestGuardGoldenPath)
		updateGuardGolden(t, manifestGuardGoldenPath, src, old, head, writeManifestGuardGolden)
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

// readOptionalGuardGolden reads a golden file for -update's refusal check
// (D002): unlike readManifestGuardGolden/readSourceGuardGolden, a MISSING file
// is not an error here — it means every id is new — but a present-and-invalid
// file still fails loudly.
func readOptionalGuardGolden(t *testing.T, path string) map[string]manifestGuardEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]manifestGuardEntry{}
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]manifestGuardEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return m
}

// readHeadGuardGolden reads path as committed at git HEAD (D002): the working
// golden alone cannot catch a bump/un-bump round trip or a hand-deleted row
// (issue 11), since both erase the working golden's memory of the old row
// before -update ever runs. A path absent at HEAD entirely (a golden that has
// never been committed) is an empty map, matching readOptionalGuardGolden's
// "missing file" semantics for the working golden. Any other git failure —
// no git binary, cwd not inside a repository — FAILS the -update run: the
// refusal check cannot run without it, even though the plain (non -update)
// test path above never invokes git at all.
func readHeadGuardGolden(t *testing.T, repoDir, path string) map[string]manifestGuardEntry {
	t.Helper()
	cmd := buildHeadGoldenCmd(repoDir, path)
	out, err := cmd.Output()
	stderr := ""
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr = string(exitErr.Stderr)
	}
	m, fatal := classifyHeadGuardGolden(path, out, err, stderr)
	if fatal != "" {
		t.Fatalf("%s", fatal)
	}
	return m
}

// buildHeadGoldenCmd builds the `git show HEAD:internal/bundled/<path>`
// command readHeadGuardGolden runs, pulled out so TestBuildHeadGoldenCmdPinsLocale
// can inspect cmd.Env directly: dropping LC_ALL=C (see the comment below) has
// no observable effect without a non-English git installed, so asserting on
// the built *exec.Cmd is the only seam that catches the regression in CI.
func buildHeadGoldenCmd(repoDir, path string) *exec.Cmd {
	cmd := exec.Command("git", "show", "HEAD:internal/bundled/"+path)
	cmd.Dir = repoDir
	// LC_ALL=C: classifyHeadGuardGolden's stderr match below is English text
	// git emits; without pinning the locale a non-English git could produce
	// different stderr and be misclassified as a fatal error.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd
}

// TestBuildHeadGoldenCmdPinsLocale is the seam for readHeadGuardGolden's
// LC_ALL=C pin (see buildHeadGoldenCmd's doc comment): dropping it from the
// real runner produces no observable stdout/stderr difference on an English
// git, so nothing else in this package would notice the regression.
func TestBuildHeadGoldenCmdPinsLocale(t *testing.T) {
	cmd := buildHeadGoldenCmd("/some/repo", "testdata/x.json")
	if cmd.Dir != "/some/repo" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/some/repo")
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "LC_ALL=C" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cmd.Env = %v, want it to contain %q", cmd.Env, "LC_ALL=C")
	}
}

// classifyHeadGuardGolden is readHeadGuardGolden's decision, pulled out as a
// pure function so it is testable without a real git failure: given the raw
// result of running `git show HEAD:internal/bundled/<path>` (its stdout,
// its error, and — when err is a non-nil *exec.ExitError — its stderr),
// return either the parsed golden or, when fatal is non-empty, the message
// readHeadGuardGolden should t.Fatalf with. A path absent at HEAD entirely
// (git's "does not exist in" / "exists on disk, but not in" stderr) is an
// empty map, matching readOptionalGuardGolden's "missing file" semantics for
// the working golden; any other git failure, or output that is not valid
// JSON, is fatal.
func classifyHeadGuardGolden(path string, out []byte, err error, stderr string) (m map[string]manifestGuardEntry, fatal string) {
	if err != nil {
		if strings.Contains(stderr, "does not exist in") || strings.Contains(stderr, "exists on disk, but not in") {
			return map[string]manifestGuardEntry{}, ""
		}
		return nil, fmt.Sprintf("-update's refusal check needs git to read %s as committed at HEAD, and it failed: %v (%s)", path, err, strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Sprintf("HEAD:internal/bundled/%s is not valid JSON: %v", path, err)
	}
	return m, ""
}

// updateGuardGolden implements the -update refusal shared by both guards
// (D002): a row whose version matches either the working golden (old) or the
// golden as committed at git HEAD (head) but whose hash differs is a
// same-version content change. Any such row anywhere in src makes -update
// write NOTHING to that golden — it ends byte-identical to before the run —
// because writing even the other, legitimate rows would launder the flagged
// change through the same run. Each refused id gets exactly one t.Errorf,
// naming every golden it matched (working, HEAD, or both) rather than one
// line per matched golden. When nothing is refused, every row in src is
// written (a brand-new id, a genuine version bump, or an unchanged row); an
// id present in old but no longer in src (no longer shipped) is dropped by
// never being carried into out.
func updateGuardGolden(t *testing.T, path string, src, old, head map[string]manifestGuardEntry, write func(*testing.T, map[string]manifestGuardEntry)) {
	t.Helper()
	refused := refusedGuardRows(src, old, head)
	for _, r := range refused {
		var matched []string
		if r.matchedOld {
			matched = append(matched, fmt.Sprintf("working golden %s", path))
		}
		if r.matchedHead {
			matched = append(matched, fmt.Sprintf("golden at git HEAD %s", path))
		}
		t.Errorf("%s's content changed without a version bump (matches %s); nothing was written to %s — "+
			"bump \"version\" in plugins/%s/manifest.json, then rerun with -update",
			r.id, strings.Join(matched, " and "), path, r.id)
	}
	if len(refused) > 0 {
		return
	}
	out := make(map[string]manifestGuardEntry, len(src))
	for id, cur := range src {
		out[id] = cur
	}
	write(t, out)
}

// refusedGuardRow is one id refusedGuardRows flagged, naming which golden(s)
// (the working copy, the one at git HEAD, or both) it matched.
type refusedGuardRow struct {
	id                      string
	matchedOld, matchedHead bool
}

// refusedGuardRows is updateGuardGolden's refusal decision (D002), pulled out
// as a pure function so it is testable without a real *testing.T failing:
// an id in src whose version equals its version in old (the working golden)
// or head (the golden as committed at git HEAD) but whose hash differs is a
// same-version content change, and is refused — once per id in the returned
// slice, with matchedOld/matchedHead saying which golden(s) it hit, so the
// caller can name them in one message. An id present in old or head but no
// longer in src (no longer shipped) is not examined here at all: it is
// simply dropped by never being carried into updateGuardGolden's write, not
// refused. The result is sorted by id for deterministic output.
func refusedGuardRows(src, old, head map[string]manifestGuardEntry) []refusedGuardRow {
	ids := make([]string, 0, len(src))
	for id := range src {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []refusedGuardRow
	for _, id := range ids {
		cur := src[id]
		var r refusedGuardRow
		if prev, ok := old[id]; ok && prev.Version == cur.Version && prev.Hash != cur.Hash {
			r.matchedOld = true
		}
		if prev, ok := head[id]; ok && prev.Version == cur.Version && prev.Hash != cur.Hash {
			r.matchedHead = true
		}
		if r.matchedOld || r.matchedHead {
			r.id = id
			out = append(out, r)
		}
	}
	return out
}
