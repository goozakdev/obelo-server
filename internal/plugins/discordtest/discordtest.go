// Package discordtest builds the REFERENCE Installed plugin — the Discord Event
// sink — from source, so this repository's acceptance suite drives the same
// module an operator installs (.scratch/plugin-system issue 14).
//
// # Why the reference plugin is in this suite at all
//
// The authoring guide (docs/plugins/authoring.md) promises an author that a
// module built by the command it names, against the manifest it shows, will load
// and run. That promise is worth exactly as much as the thing that checks it. So
// the Discord plugin is built and driven here: a contract change that breaks the
// reference plugin breaks this build, on the maintainer's machine and in a clean
// clone alike.
//
// # Two copies of the source, and which one wins
//
// The plugin's home is a SIBLING REPOSITORY — its own git repository, its own
// go.mod, importing nothing of this module — because a plugin author's project is
// not inside this repository and an example that lived here would quietly be
// allowed to cheat. That repository is not a submodule and is not fetched by
// anything, so a clean clone of the server does not have it.
//
// Hence a VENDORED COPY under testdata/obelo-plugin-discord/: the same files,
// byte for byte, with the sibling commit they came from recorded in SOURCE.
//
//   - A clean clone builds the vendored copy, and every test here runs.
//   - A machine that HAS the sibling checked out beside this repository builds
//     the sibling, and TestTheVendoredCopyMatchesTheSibling fails if the two have
//     drifted — so the copy cannot rot the way a checked-in binary would.
//
// The sibling is preferred when present precisely so that an author editing it
// sees this suite react.
package discordtest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// SiblingDirName is where the plugin's own repository lives relative to this
// one: `../obelo-plugin-discord`. Relative, never absolute — the suite must not
// depend on where anybody checked this repository out.
const SiblingDirName = "obelo-plugin-discord"

// VendoredDir is the copy of the plugin's source inside this repository,
// relative to this package.
const VendoredDir = "testdata/" + SiblingDirName

// SourceFile records which sibling commit the vendored copy was taken from. It
// is provenance for a human, not a check: the drift test compares CONTENT,
// because a hash of a tree nobody can fetch proves nothing.
const SourceFile = "SOURCE"

// VendoredFiles is every file the vendored copy holds, and the list the drift
// test walks. SOURCE is not here: it is this repository's note about the sibling
// and has no counterpart over there.
var VendoredFiles = []string{
	".gitignore",
	"LICENSE",
	"Makefile",
	"README.md",
	"go.mod",
	"main.go",
	"manifest.json",
}

var (
	buildOnce  sync.Once
	pluginWasm []byte
	buildErr   error
)

// Module returns the compiled Discord plugin, building it on first use in this
// process with the ordinary command an author runs:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared
//
// Same toolchain, same flags, no TinyGo — which means this function also proves
// the authoring guide's build instructions work.
func Module(t *testing.T) []byte {
	t.Helper()
	buildOnce.Do(build)
	if buildErr != nil {
		t.Fatalf("building the Discord reference plugin: %v", buildErr)
	}
	return pluginWasm
}

// ManifestJSON is the plugin's manifest.json, byte for byte as its author wrote
// it — which is exactly what an install writes to disk, and exactly what a
// signature would one day have to cover.
func ManifestJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(SourceDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("reading the Discord plugin's manifest: %v", err)
	}
	return raw
}

// Manifest is ManifestJSON decoded, for a test that wants to assert on the
// author's declarations rather than on the bytes.
func Manifest(t *testing.T) pluginapi.Manifest {
	t.Helper()
	var m pluginapi.Manifest
	if err := json.Unmarshal(ManifestJSON(t), &m); err != nil {
		t.Fatalf("the Discord plugin's manifest is not valid JSON: %v", err)
	}
	return m
}

// SourceDir is the directory the plugin is built from: the sibling repository
// when it is checked out beside this one, the vendored copy otherwise.
func SourceDir(t *testing.T) string {
	t.Helper()
	dir, err := sourceDir()
	if err != nil {
		t.Fatalf("locating the Discord plugin's source: %v", err)
	}
	return dir
}

// SiblingDir is where the plugin's own repository would be, and whether it is
// there. A test uses it to decide whether a drift check is possible at all.
func SiblingDir() (string, bool) {
	root, err := repoRoot()
	if err != nil {
		return "", false
	}
	dir := filepath.Join(filepath.Dir(root), SiblingDirName)
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return dir, false
	}
	return dir, true
}

// VendoredPath is the vendored copy's directory.
func VendoredPath() (string, error) {
	pkg, err := packageDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(pkg, filepath.FromSlash(VendoredDir)), nil
}

func sourceDir() (string, error) {
	if dir, ok := SiblingDir(); ok {
		return dir, nil
	}
	dir, err := VendoredPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return "", fmt.Errorf("no plugin source at %s and no sibling checkout either: %w", dir, err)
	}
	return dir, nil
}

func build() {
	dir, err := sourceDir()
	if err != nil {
		buildErr = err
		return
	}
	out, err := os.CreateTemp("", "obelo-plugin-discord-*.wasm")
	if err != nil {
		buildErr = err
		return
	}
	path := out.Name()
	_ = out.Close()
	defer os.Remove(path)

	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", path, ".")
	cmd.Dir = dir
	// GOFLAGS is cleared for the reason plugintest clears it: the root module's
	// flags have nothing to do with a separate module that imports only the
	// standard library, and inheriting them is how this build breaks on somebody
	// else's machine.
	//
	// GOWORK IS OFF for plugintest's reason, restated because this one is the
	// point: the reference plugin's home is a SIBLING REPOSITORY with no workspace
	// around it, so anything the repository's own go.work would contribute here is
	// a difference between this build and the author's. It is also load-bearing —
	// without it a build of the VENDORED copy is inside the workspace, which does
	// not `use` it, and fails outright.
	cmd.Env = append(os.Environ(),
		"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off",
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		buildErr = fmt.Errorf("go build in %s: %w\n%s", dir, err, combined)
		return
	}
	pluginWasm, buildErr = os.ReadFile(path)
}

// packageDir is this package's own directory, so a test can find testdata from
// any working directory.
func packageDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("discordtest: cannot locate its own source")
	}
	return filepath.Dir(file), nil
}

// repoRoot is the server repository's root: three levels above
// internal/plugins/discordtest.
func repoRoot() (string, error) {
	pkg, err := packageDir()
	if err != nil {
		return "", err
	}
	root := filepath.Clean(filepath.Join(pkg, "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("discordtest: %s is not the repository root: %w", root, err)
	}
	return root, nil
}

// RepoRoot is repoRoot for the tests that need to read a document out of the
// docs tree.
func RepoRoot(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return root
}
