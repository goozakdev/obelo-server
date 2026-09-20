package plugins_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The module split's load-bearing consequence (.scratch/bundled-plugins issue 03,
// ADR-0059 decision 9).
//
// `plugins/<id>/` is its own Go module from issue 04 onward, and the reason is
// not tidiness: a provider that could reach into internal/ would go on reaching
// into it, and the seven bundled plugins would become seven more things coupled
// to the server's private types — which is precisely the mistake ADR-0057 cites
// Jellyfin for. Go's internal-package rule enforces that BY CONSTRUCTION once the
// plugin is a separate module, and this test is the proof that it does.
//
// It is a test and not a note because the rule is invisible: nothing in the
// repository fails if someone quietly adds the server module to a plugin's go.mod
// and imports enrich.TMDBProvider, until a reviewer happens to read the go.mod.

// TestAPluginModuleCannotImportTheServersInternalPackages generates a throwaway
// plugin module that requires the server and imports internal/enrich, builds it,
// and requires the standard refusal.
func TestAPluginModuleCannotImportTheServersInternalPackages(t *testing.T) {
	if testing.Short() {
		t.Skip("this test shells out to `go build`")
	}
	root := sdkRepoRoot(t)
	dir := t.TempDir()

	// A plugin author's module, with the ONE line that would make internal/
	// reachable if the rule did not exist: a require on the server itself.
	//
	// Its `go` directive is READ from the server's go.mod rather than written
	// here: a module that requires another with a higher directive is refused
	// before any package is loaded, so a hardcoded version would turn this into a
	// test that passes for the wrong reason the next time the server's is bumped.
	gomod := "module obelo-plugin-example\n\ngo " + sdkGoDirective(t, root) + "\n\n" +
		"require github.com/goozakdev/obelo-server v0.0.0\n\n" +
		"replace github.com/goozakdev/obelo-server => " + filepath.ToSlash(root) + "\n\n" +
		"replace github.com/goozakdev/obelo-server/pluginapi => " + filepath.ToSlash(filepath.Join(root, "pluginapi")) + "\n\n" +
		"replace github.com/goozakdev/obelo-server/pluginsdk => " + filepath.ToSlash(filepath.Join(root, "pluginsdk")) + "\n"
	write(t, filepath.Join(dir, "go.mod"), gomod)
	write(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport _ \"github.com/goozakdev/obelo-server/internal/enrich\"\n\nfunc main() {}\n")

	// The server's own go.sum, so the build resolves the server's dependency graph
	// out of the module cache rather than reaching for the network.
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading the server's go.sum: %v", err)
	}
	write(t, filepath.Join(dir, "go.sum"), string(sum))

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	// GOWORK=off because this module is NOT in the repository's workspace — a
	// plugin author's project is not inside this repository, and a workspace that
	// joined it would be answering a different question. -mod=mod lets the build
	// settle the require graph in the temporary module rather than refusing on a
	// go.mod it is allowed to write.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a plugin module compiled an import of internal/enrich. "+
			"The internal-package rule is the only thing keeping a bundled plugin off the server's "+
			"private types, and it just did not fire.\n%s", out)
	}
	const want = "use of internal package github.com/goozakdev/obelo-server/internal/enrich not allowed"
	if !strings.Contains(string(out), want) {
		t.Fatalf("the build failed for the wrong reason.\nwant a message containing: %s\ngot:\n%s", want, out)
	}
}

// sdkRepoRoot is the repository root, found from this file so the test does not
// depend on a working directory.
func sdkRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	// .../internal/plugins/sdk_module_boundary_test.go -> the repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// sdkGoDirective is the server go.mod's `go` version.
func sdkGoDirective(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading the server's go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("the server's go.mod has no `go` directive")
	return ""
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
