// Package bundledtest makes the Bundled plugins available to a `go test` run on a
// tree where `make plugins` has not been built (.scratch/bundled-plugins issue 04).
//
// # Why this exists
//
// internal/bundled embeds BUILD OUTPUT. On a clone that has not run
// `make plugins` it embeds nothing, so the boot-time assertion installs nothing,
// so a server the test harness builds has no TMDB — and the several hundred
// black-box tests that expect TMDB to lead video fail for a reason that has
// nothing whatever to do with what they test. A developer who typed
// `go test ./...` would read a wall of red about enrichment.
//
// So the harness asks here first. If this binary was built with the modules, this
// does nothing at all and the tests exercise exactly what ships. If it was not,
// each missing module is compiled from plugins/<id>/ with THE SAME COMMAND
// `make plugins` runs, once per process, and handed to internal/bundled —
// so the suite still drives the REAL module through the REAL sandbox, and the
// only thing that changed is where the bytes came from.
//
// It follows internal/plugins/sdkguesttest, which does the same thing for the
// SDK's test guest, for the same reason: a checked-in .wasm would rot silently
// the way internal/webui/dist/index.html did, and nobody reviews a binary diff.
//
// # The cost
//
// One `go build` per missing module per test process — about two seconds for the
// TMDB module on a warm build cache — and then nothing. The wazero side is cached
// too (internal/plugins keeps one compilation cache for the process), so the
// hundreds of servers a suite builds pay ~20 ms each rather than ~670 ms.
package bundledtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/bundled"
)

var (
	once sync.Once
	err  error
)

// Ensure makes every Bundled plugin this repository holds source for available to
// internal/bundled, building what this binary was not built with. It runs at most
// once per process and is safe to call from every test that builds a server.
//
// A build failure FAILS the test rather than skipping it: a plugin that will not
// compile is exactly the thing this suite exists to catch, and a skip here would
// be a guard that passes because it did not run.
func Ensure(t *testing.T) {
	t.Helper()
	once.Do(build)
	if err != nil {
		t.Fatalf("building the bundled plugin modules: %v", err)
	}
}

func build() {
	missing := bundled.Missing()
	if len(missing) == 0 {
		return
	}
	root, rootErr := repoRoot()
	if rootErr != nil {
		err = rootErr
		return
	}
	for _, id := range missing {
		dir := filepath.Join(root, "plugins", id)
		manifestPath := filepath.Join(dir, "manifest.json")
		manifest, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			// This build ships no source for the id either. That is the ordinary
			// state of issues 05-07's plugins before they land: internal/bundled's
			// ordered list names all seven from the first slice, and a module that
			// does not exist yet is simply not installed.
			continue
		}
		module, buildErr := compile(dir)
		if buildErr != nil {
			err = buildErr
			return
		}
		if supplyErr := bundled.SupplyForTests(id, manifest, module); supplyErr != nil {
			err = supplyErr
			return
		}
	}
}

// compile runs the plugin's own build command — the reference plugin's, character
// for character, which is the command `make plugins` runs and the command the
// authoring guide names.
//
// GOFLAGS is cleared for sdkguesttest's reason: the server module's flags have
// nothing to do with a module that imports only the contract, the SDK and the
// standard library, and inheriting them is how this build breaks on somebody
// else's machine. GOWORK is left alone — plugins/<id>/go.mod carries `replace`
// lines for both local modules, so it resolves with or without the workspace.
func compile(dir string) ([]byte, error) {
	out, err := os.CreateTemp("", "obelo-bundled-*.wasm")
	if err != nil {
		return nil, err
	}
	path := out.Name()
	_ = out.Close()
	defer os.Remove(path)

	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", path, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=")
	if combined, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build in %s: %w\n%s", dir, err, combined)
	}
	return os.ReadFile(path)
}

// repoRoot is found from this file rather than from the working directory, so a
// test in any package can ask.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("bundledtest: cannot locate its own source")
	}
	// .../internal/bundled/bundledtest/bundledtest.go -> the repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("bundledtest: no repository root at %s: %w", root, err)
	}
	return root, nil
}
