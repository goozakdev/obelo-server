package bundled

// Unit coverage for the -update refusal (D002, D009): refusedGuardRows and
// classifyHeadGuardGolden are the pure decisions updateGuardGolden and
// readHeadGuardGolden are thin wrappers around, tested table-driven here
// without a real *testing.T failing.
//
// The two paths that DO call t.Errorf/t.Fatalf on a real *testing.T — a
// refused updateGuardGolden and readHeadGuardGolden hitting a git error —
// cannot be observed with a plain t.Run/ok check: a subtest's failure marks
// every ancestor *testing.T failed too (confirmed: an in-process t.Run of
// either scenario turns THIS package's `go test` exit code nonzero even
// though this test itself never calls t.Fail), which would make an
// unmutated `go test ./internal/bundled/` fail. So those two are run in a
// re-exec'd child process instead (the same pattern net/http and os/exec use
// for testing something that calls t.Fatal/os.Exit): TestGuardRefusalHelperProcess
// is a normal test, skipped unless a marker env var selects a scenario, and
// runGuardRefusalHelper starts `go test`'s own binary again with
// -test.run pinned to just that test, asserting on the CHILD's exit code
// and output instead of this process's *testing.T.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefusedGuardRows(t *testing.T) {
	entry := func(version, hash string) manifestGuardEntry {
		return manifestGuardEntry{Version: version, Hash: hash}
	}

	cases := []struct {
		name              string
		src, old, head    map[string]manifestGuardEntry
		wantIDs           []string
		wantOld, wantHead map[string]bool // per id in wantIDs
	}{
		{
			name:    "same version different hash vs working golden only",
			src:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "new")},
			old:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			head:    map[string]manifestGuardEntry{},
			wantIDs: []string{"tmdb"},
			wantOld: map[string]bool{"tmdb": true},
		},
		{
			name:     "same version different hash vs HEAD golden only",
			src:      map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "new")},
			old:      map[string]manifestGuardEntry{},
			head:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			wantIDs:  []string{"tmdb"},
			wantHead: map[string]bool{"tmdb": true},
		},
		{
			name:     "same version different hash vs both goldens is refused once with both flags set",
			src:      map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "new")},
			old:      map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			head:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			wantIDs:  []string{"tmdb"},
			wantOld:  map[string]bool{"tmdb": true},
			wantHead: map[string]bool{"tmdb": true},
		},
		{
			name:    "new id absent from both goldens is written, not refused",
			src:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "new")},
			old:     map[string]manifestGuardEntry{},
			head:    map[string]manifestGuardEntry{},
			wantIDs: nil,
		},
		{
			name:    "legit version bump is written, not refused",
			src:     map[string]manifestGuardEntry{"tmdb": entry("1.1.0", "new")},
			old:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			head:    map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")},
			wantIDs: nil,
		},
		{
			name:    "unchanged row is written, not refused",
			src:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "same")},
			old:     map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "same")},
			head:    map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "same")},
			wantIDs: nil,
		},
		{
			name:    "id in old and head but no longer shipped is dropped, not refused",
			src:     map[string]manifestGuardEntry{},
			old:     map[string]manifestGuardEntry{"gone": entry("1.0.0", "old")},
			head:    map[string]manifestGuardEntry{"gone": entry("1.0.0", "old")},
			wantIDs: nil,
		},
		{
			name: "one refused id and one legit id in the same run: only the refused one is reported",
			src: map[string]manifestGuardEntry{
				"tmdb": entry("1.0.0", "new"),
				"tvdb": entry("1.1.0", "new"),
			},
			old: map[string]manifestGuardEntry{
				"tmdb": entry("1.0.0", "old"),
				"tvdb": entry("1.0.0", "old"),
			},
			head:    map[string]manifestGuardEntry{},
			wantIDs: []string{"tmdb"},
			wantOld: map[string]bool{"tmdb": true},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := refusedGuardRows(c.src, c.old, c.head)
			if len(got) != len(c.wantIDs) {
				t.Fatalf("refusedGuardRows() = %+v, want ids %v", got, c.wantIDs)
			}
			for i, id := range c.wantIDs {
				if got[i].id != id {
					t.Fatalf("refusedGuardRows()[%d].id = %q, want %q (%+v)", i, got[i].id, id, got)
				}
				if got[i].matchedOld != c.wantOld[id] {
					t.Errorf("%s: matchedOld = %v, want %v", id, got[i].matchedOld, c.wantOld[id])
				}
				if got[i].matchedHead != c.wantHead[id] {
					t.Errorf("%s: matchedHead = %v, want %v", id, got[i].matchedHead, c.wantHead[id])
				}
			}
		})
	}
}

// TestUpdateGuardGoldenWritesOnNoRefusal covers the pass path of
// updateGuardGolden directly (nothing here fails, so no subprocess needed):
// write is called with every src row.
func TestUpdateGuardGoldenWritesOnNoRefusal(t *testing.T) {
	entry := func(version, hash string) manifestGuardEntry {
		return manifestGuardEntry{Version: version, Hash: hash}
	}
	var got map[string]manifestGuardEntry
	src := map[string]manifestGuardEntry{"tmdb": entry("1.1.0", "new")}
	old := map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")}
	head := map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")}
	updateGuardGolden(t, "testdata/x.json", src, old, head, func(_ *testing.T, entries map[string]manifestGuardEntry) {
		got = entries
	})
	if len(got) != 1 || got["tmdb"] != src["tmdb"] {
		t.Errorf("write called with %+v, want %+v", got, src)
	}
}

// TestUpdateGuardGoldenRefusedWritesNothing covers the refusal path via a
// re-exec'd child process (see the file doc comment for why): write must
// never be called, and the child must exit nonzero naming the refused id.
func TestUpdateGuardGoldenRefusedWritesNothing(t *testing.T) {
	out, exitCode := runGuardRefusalHelper(t, "update-refused")
	if exitCode == 0 {
		t.Fatalf("expected the helper process to fail (refusal), got exit 0; output:\n%s", out)
	}
	if strings.Contains(out, "write must not be called on refusal") {
		t.Errorf("write was called despite the refusal; output:\n%s", out)
	}
	if !strings.Contains(out, `tmdb's content changed without a version bump (working golden testdata/x.json)`) {
		t.Errorf("expected the working-golden refusal message in output:\n%s", out)
	}
}

func TestClassifyHeadGuardGolden(t *testing.T) {
	notExistErr := errors.New("git show exited 128")

	cases := []struct {
		name      string
		out       []byte
		err       error
		stderr    string
		want      map[string]manifestGuardEntry
		wantFatal bool
	}{
		{
			name: "valid JSON parses",
			out:  []byte(`{"tmdb":{"version":"1.0.0","hash":"abc"}}`),
			want: map[string]manifestGuardEntry{"tmdb": {Version: "1.0.0", Hash: "abc"}},
		},
		{
			name:   "path never committed (does not exist in) is an empty map, not fatal",
			err:    notExistErr,
			stderr: "fatal: path 'internal/bundled/testdata/x.json' does not exist in 'HEAD'",
			want:   map[string]manifestGuardEntry{},
		},
		{
			name:   "path never committed (exists on disk, but not in) is an empty map, not fatal",
			err:    notExistErr,
			stderr: "fatal: path 'internal/bundled/testdata/x.json' exists on disk, but not in 'HEAD'",
			want:   map[string]manifestGuardEntry{},
		},
		{
			name:      "any other git error is fatal",
			err:       notExistErr,
			stderr:    "fatal: not a git repository (or any of the parent directories): .git",
			wantFatal: true,
		},
		{
			name:      "invalid JSON is fatal",
			out:       []byte("not json"),
			wantFatal: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, fatal := classifyHeadGuardGolden("testdata/x.json", c.out, c.err, c.stderr)
			if c.wantFatal {
				if fatal == "" {
					t.Fatalf("classifyHeadGuardGolden() fatal = %q, want non-empty", fatal)
				}
				return
			}
			if fatal != "" {
				t.Fatalf("classifyHeadGuardGolden() fatal = %q, want empty", fatal)
			}
			if len(m) != len(c.want) {
				t.Fatalf("classifyHeadGuardGolden() = %+v, want %+v", m, c.want)
			}
			for id, want := range c.want {
				if m[id] != want {
					t.Errorf("%s = %+v, want %+v", id, m[id], want)
				}
			}
		})
	}
}

// TestReadHeadGuardGolden exercises readHeadGuardGolden's own git call — the
// thin wrapper classifyHeadGuardGolden's table above does not reach — against
// a real temporary git repository, for the two outcomes that do not fail:
// a committed golden parses, and a path never committed is an empty map.
func TestReadHeadGuardGolden(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; readHeadGuardGolden needs it")
	}

	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-q", "-m", "root")

	const rel = "testdata/committed.json"
	abs := filepath.Join(repo, "internal", "bundled", rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(`{"tmdb":{"version":"1.0.0","hash":"abc"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "add golden")

	t.Run("committed golden parses", func(t *testing.T) {
		got := readHeadGuardGolden(t, repo, rel)
		want := map[string]manifestGuardEntry{"tmdb": {Version: "1.0.0", Hash: "abc"}}
		if len(got) != 1 || got["tmdb"] != want["tmdb"] {
			t.Errorf("readHeadGuardGolden() = %+v, want %+v", got, want)
		}
	})

	t.Run("path never committed is an empty map", func(t *testing.T) {
		got := readHeadGuardGolden(t, repo, "testdata/never-committed.json")
		if len(got) != 0 {
			t.Errorf("readHeadGuardGolden() = %+v, want empty", got)
		}
	})
}

// TestReadHeadGuardGoldenNotARepo covers readHeadGuardGolden's fatal branch
// via the same re-exec'd child process pattern as
// TestUpdateGuardGoldenRefusedWritesNothing.
func TestReadHeadGuardGoldenNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; readHeadGuardGolden needs it")
	}
	out, exitCode := runGuardRefusalHelper(t, "read-head-not-a-repo")
	if exitCode == 0 {
		t.Fatalf("expected the helper process to fail (not a git repository), got exit 0; output:\n%s", out)
	}
	if !strings.Contains(out, "-update's refusal check needs git") {
		t.Errorf("expected readHeadGuardGolden's fatal message in output:\n%s", out)
	}
}

// guardRefusalHelperScenarioEnv names the env var runGuardRefusalHelper sets
// and TestGuardRefusalHelperProcess reads to pick which failing scenario to run
// in the child process.
const guardRefusalHelperScenarioEnv = "OBELO_GUARD_REFUSAL_HELPER_SCENARIO"

// TestGuardRefusalHelperProcess is not a real test: it does nothing unless
// guardRefusalHelperScenarioEnv is set, in which case it runs exactly one of
// the failing scenarios against a REAL *testing.T (so the production
// t.Errorf/t.Fatalf call sites run unmodified) and lets that *testing.T's own
// failure become this child process's exit code, for the parent process to
// observe from the outside instead of via t.Run/ok.
func TestGuardRefusalHelperProcess(t *testing.T) {
	scenario := os.Getenv(guardRefusalHelperScenarioEnv)
	if scenario == "" {
		t.Skip("not invoked as a guard-refusal helper process")
	}
	switch scenario {
	case "update-refused":
		entry := func(version, hash string) manifestGuardEntry {
			return manifestGuardEntry{Version: version, Hash: hash}
		}
		src := map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "new")}
		old := map[string]manifestGuardEntry{"tmdb": entry("1.0.0", "old")}
		head := map[string]manifestGuardEntry{}
		updateGuardGolden(t, "testdata/x.json", src, old, head, func(*testing.T, map[string]manifestGuardEntry) {
			t.Error("write must not be called on refusal")
		})
	case "read-head-not-a-repo":
		// Point git at a repository that does not exist rather than relying
		// on t.TempDir() being outside every repository: with TMPDIR inside
		// a checkout git walks up, finds it, and answers "does not exist in
		// 'HEAD'" — the non-fatal branch — instead of failing.
		t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nonexistent"))
		readHeadGuardGolden(t, t.TempDir(), "testdata/whatever.json")
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
}

// runGuardRefusalHelper re-execs the current test binary with -test.run
// pinned to TestGuardRefusalHelperProcess and scenario selected via
// guardRefusalHelperScenarioEnv, returning the child's combined output and
// exit code.
func runGuardRefusalHelper(t *testing.T, scenario string) (output string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardRefusalHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), guardRefusalHelperScenarioEnv+"="+scenario)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("running guard-refusal helper process: %v\n%s", err, out)
	return "", -1
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The developer's own global/system git config must not decide this
	// test's outcome (commit.gpgsign, core.hooksPath, init.templateDir …):
	// read neither.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
