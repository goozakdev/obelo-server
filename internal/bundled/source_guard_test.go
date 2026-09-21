package bundled

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
//
// -update refuses the same same-version-different-hash row as the manifest
// guard (D002; see updateGuardGolden) — bump the version first.
//
// The hash also covers the in-repo packages a plugin's build reaches outside
// its own plugins/<id>/ (D003, sharedSourceRoots below): assert.go only
// replaces an installed plugin when the shipped version is newer, so an SDK
// fix with no plugin version bump would otherwise ship to nobody. Every
// hashed .go file's imports (parsed with go/parser, ImportsOnly — build tags
// are not evaluated, so a build-tagged file is still checked) are checked:
// an in-repo import outside the plugin's own dir and outside
// sharedSourceRoots fails, naming the import and the pinned list, so the root
// list cannot go stale silently — including an import made BY a shared root
// itself.
//
// A symlinked directory anywhere under a hashed tree fails loudly instead of
// being silently skipped: filepath.WalkDir does not follow it, so a source
// file inside would ship unseen by this guard.
const sourceGuardGoldenPath = "testdata/source_guard_golden.json"

// inRepoModulePrefix is this repo's module path, the prefix of every in-repo
// import.
const inRepoModulePrefix = "github.com/goozakdev/obelo-server"

// sharedSourceRoots (D003) are the in-repo packages, outside a plugin's own
// plugins/<id>/, that a Bundled plugin's build actually reaches. Determined
// from source: every plugin's non-test .go files import pluginapi/v1,
// pluginsdk, and pluginsdk/metadata or pluginsdk/subtitle; those four
// packages import only each other (pluginsdk/sink, pluginsdk/sdktest and the
// internal/ packages are test-only or unused by shipped code and so are not
// pinned). Pinned as an explicit list rather than derived, so an import
// outside it fails loudly instead of the list going stale silently.
var sharedSourceRoots = []string{
	"pluginsdk",
	"pluginsdk/metadata",
	"pluginsdk/subtitle",
	"pluginapi/v1",
}

// TestBundledSourceMatchesTheirGolden is the source guard. Same three rules as
// TestBundledManifestsMatchTheirGolden, over a source hash instead of a manifest
// hash: same version, different hash → FAIL; different version → FAIL (the golden
// mirrors what ships); shipped but absent from the golden, or the reverse → FAIL.
func TestBundledSourceMatchesTheirGolden(t *testing.T) {
	src := currentSourceGuardEntries(t)

	if *updateManifestGuardGolden {
		old := readOptionalGuardGolden(t, sourceGuardGoldenPath)
		updateGuardGolden(t, sourceGuardGoldenPath, src, old, writeSourceGuardGolden)
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
// isAllowlistedSourceFile) plus, per D003, every sharedSourceRoots tree: sorted
// repo-root-relative slash paths ("plugins/<id>/..." or e.g. "pluginsdk/..."
// — the repo-root prefix means a plugin's own file and a shared-root file can
// never collide), each path, its byte length, and its content fed to the hash
// in that order, so the result depends on nothing but what is actually on
// disk — no mtimes, no directory-listing order, no git — and no two distinct
// file sets can produce the same byte stream by shifting a boundary (the
// decimal length between path and content pins where each file's bytes end).
//
// A file that matches the allowlist by name but is not a regular file (e.g. a
// symlink) fails the test loudly rather than being silently hashed or skipped,
// and so does a symlinked DIRECTORY anywhere in the tree (filepath.WalkDir
// does not follow it, so it would otherwise be skipped without a trace): this
// guard's whole point is to know what ships, and "what does opening this path
// actually give a user" is not something a directory listing can answer. A
// hashed .go file with a real //go:embed directive also fails loudly: an
// embedded file ships as part of the binary without ever matching the
// allowlist by name, so the allowlist would need to learn about it
// explicitly.
func sourceHash(t *testing.T, id string) string {
	t.Helper()
	contents := make(map[string][]byte)
	imports := make(map[string][]string)
	hashRoot(t, filepath.Join("..", "..", "plugins", id), "plugins/"+id, true, contents, imports)
	for _, root := range sharedSourceRoots {
		// Non-recursive: a shared root is pinned at Go package granularity —
		// its own directory's files only. pluginsdk/, for one, has sink/,
		// sdktest/ and internal/ subdirectories that are real Go packages but
		// are not imported by any shipped plugin code (checked at pinning
		// time); walking into them would hash — and let every plugin import
		// — packages nothing actually ships.
		hashRoot(t, filepath.Join("..", "..", root), root, false, contents, imports)
	}
	checkSourceImports(t, id, imports)

	relPaths := make([]string, 0, len(contents))
	for rel := range contents {
		relPaths = append(relPaths, rel)
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

// hashRoot walks dir — recursive for a plugin's own plugins/<id> (its whole
// module tree, sub-packages included, ships as a unit), non-recursive for a
// pinned shared root (pinned at Go package granularity: only that directory's
// own files, not its subdirectories, which are separate packages that may not
// even be pinned) — collecting every allowlisted file's content into
// contents, keyed by its path relative to the repo root (repoRelPrefix + "/"
// + the file's path under dir), and, for each hashed .go file, its in-repo
// imports into imports (same key), for checkSourceImports to validate
// afterward.
func hashRoot(t *testing.T, dir, repoRelPrefix string, recursive bool, contents map[string][]byte, imports map[string][]string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, statErr := os.Stat(path)
			if statErr == nil && target.IsDir() {
				return fmt.Errorf("%s is a symlink to a directory — filepath.WalkDir does not follow it, "+
					"so a source file inside would ship unseen by this guard", path)
			}
		}
		if d.IsDir() {
			if path != dir && !recursive {
				return filepath.SkipDir
			}
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
		if strings.HasSuffix(d.Name(), ".go") && hasEmbedDirective(raw) {
			return fmt.Errorf("%s contains a //go:embed directive — the source allowlist must learn about "+
				"embedded files before this plugin can ship one", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = repoRelPrefix + "/" + filepath.ToSlash(rel)
		contents[rel] = raw
		if strings.HasSuffix(d.Name(), ".go") {
			imports[rel] = inRepoImports(t, path, raw)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}

// hasEmbedDirective reports whether raw contains a real //go:embed directive:
// a line whose first non-blank text is "//go:embed" followed by whitespace or
// end of line. "// go:embed" (a space before "go:embed") and prose containing
// the substring inside a longer comment do not count.
func hasEmbedDirective(raw []byte) bool {
	const directive = "//go:embed"
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, directive) {
			continue
		}
		rest := trimmed[len(directive):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return true
		}
	}
	return false
}

// inRepoImports parses path's import list (go/parser, ImportsOnly — build
// constraints are not evaluated, so a build-tagged file is parsed the same as
// any other) and returns the subset under this repo's module path.
func inRepoImports(t *testing.T, path string, raw []byte) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing imports of %s: %v", path, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s: unquoting import %s: %v", path, imp.Path.Value, err)
		}
		if p == inRepoModulePrefix || strings.HasPrefix(p, inRepoModulePrefix+"/") {
			out = append(out, p)
		}
	}
	return out
}

// checkSourceImports fails when a hashed file — a plugin's own or a shared
// root's, per D003 including imports made BY a shared root itself — imports
// an in-repo package outside id's own plugins/<id>/ and outside
// sharedSourceRoots.
func checkSourceImports(t *testing.T, id string, imports map[string][]string) {
	t.Helper()
	// The plugin's own dir is a whole tree (its subpackages, e.g.
	// plugins/tmdb/tmdb, are allowed too), so it is matched by prefix. Each
	// shared root is pinned at package granularity — hashRoot does not even
	// walk its subdirectories — so it is matched exactly: "pluginsdk" does
	// NOT license "pluginsdk/sink", which is a different, unpinned package.
	ownDir := inRepoModulePrefix + "/plugins/" + id
	sharedExact := make(map[string]bool, len(sharedSourceRoots))
	for _, root := range sharedSourceRoots {
		sharedExact[inRepoModulePrefix+"/"+root] = true
	}
	inOwnedTree := func(imp string) bool {
		return imp == ownDir || strings.HasPrefix(imp, ownDir+"/") || sharedExact[imp]
	}
	rels := make([]string, 0, len(imports))
	for rel := range imports {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		for _, imp := range imports[rel] {
			if inOwnedTree(imp) {
				continue
			}
			t.Errorf("%s imports %q, which is outside plugins/%s/ and outside the pinned "+
				"sharedSourceRoots (%s) — add it to sharedSourceRoots in source_guard_test.go if it is meant to ship",
				rel, imp, id, strings.Join(sharedSourceRoots, ", "))
		}
	}
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
