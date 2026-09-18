package bundled

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestEveryBundledModuleIsPresent is the loud half of the pair `make
// check-no-bundled-modules-tracked` completes (ADR-0059 decision 10): the modules
// are BUILD OUTPUT, never committed, so a clone that has not built them must fail
// here rather than start a server with no metadata providers in it.
//
// The message is the instruction, because the person reading it has just cloned
// the repository and has no reason to know that a directory they cannot see is
// empty.
func TestEveryBundledModuleIsPresent(t *testing.T) {
	for _, id := range shipped() {
		if !has(id) {
			t.Errorf("the bundled plugin %q has no module or no manifest in internal/bundled/modules — run make plugins", id)
		}
	}
}

// shipped is the ids this issue's slice actually ships a module for. [IDs] is the
// full seven from the first slice — the ORDER is decided once (ADR-0059 decision
// 3) — while the modules arrive one issue at a time, so a test that ranged over
// all seven today would demand six modules nobody has written yet.
//
// ISSUES 05-07: adding a plugin is adding ONE LINE here. It is written one id per
// line for exactly that reason — three agents adding three plugins in parallel each
// touch their own line, and a cherry-pick conflict is a "keep both".
func shipped() []string {
	return []string{
		"tmdb",
	}
}

// requireModules skips the rest of this file when `make plugins` has not run. It
// is a skip and not a failure precisely because the failure above is better: one
// sentence naming the command, rather than a dozen tests each failing for the
// same reason in its own vocabulary.
func requireModules(t *testing.T) {
	t.Helper()
	for _, id := range shipped() {
		if !has(id) {
			t.Skip("the bundled modules are not built; TestEveryBundledModuleIsPresent says so — run make plugins")
		}
	}
}

// The order IS the catalog order (ADR-0059 decision 3): the first authoritative
// Full provider of a kind is that kind's default lead, so TMDB must lead this
// list or it stops leading video.
func TestTheShippedOrderIsTheRegistrationOrder(t *testing.T) {
	want := []string{"tmdb", "omdb", "thetvdb", "anidb", "musicbrainz", "fanarttv", "theaudiodb"}
	got := IDs()
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IDs() = %v, want %v", got, want)
		}
	}
	// And it is a copy: a caller that sorts it must not reorder the catalog.
	got[0] = "mutated"
	if IDs()[0] != "tmdb" {
		t.Error("IDs() handed out the package's own slice")
	}
}

// Every shipped manifest has to satisfy the three things the HOST reads off it,
// and each of the three fails silently in a different place if it does not: an id
// that disagrees with its directory is refused at load, a missing probe makes
// "Test connection" say the provider declares none (issue 01), and a version the
// re-assert cannot order would be replaced on every boot forever.
func TestEveryBundledManifestIsUsable(t *testing.T) {
	requireModules(t)
	for _, id := range shipped() {
		m, err := Manifest(id)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if m.APIVersion != pluginapi.APIVersion {
			t.Errorf("%s: apiVersion = %d, want %d", id, m.APIVersion, pluginapi.APIVersion)
		}
		if strings.TrimSpace(m.Name) == "" {
			t.Errorf("%s: the manifest has no name, and the name is what an operator reads", id)
		}
		if _, ok := versionParts(m.Version); !ok {
			t.Errorf("%s: version %q is not a dotted run of digits, so the boot-time re-assert "+
				"cannot tell it from the one on disk", id, m.Version)
		}
		for _, p := range m.Provides {
			if p.Kind != pluginapi.ExtensionMetadataProvider {
				continue
			}
			if p.Probe == nil {
				t.Errorf("%s: its metadata-provider entry declares no probe, so \"Test connection\" "+
					"will say this provider has none (ADR-0059 decision 8)", id)
			}
		}
	}
}

// The module round-trips: gzipped in the binary, decompressed here, and what comes
// out is a WebAssembly module rather than whatever else might have been copied in.
func TestModuleDecompressesToWasm(t *testing.T) {
	requireModules(t)
	for _, id := range shipped() {
		module, err := Module(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(module) < 8 || string(module[:4]) != "\x00asm" {
			t.Errorf("%s: the decompressed module does not start with the wasm magic", id)
		}
	}
}

func TestMissingNamesWhatIsNotBuilt(t *testing.T) {
	requireModules(t)
	for _, id := range Missing() {
		for _, s := range shipped() {
			if id == s {
				t.Errorf("Missing() names %q, which this build ships", id)
			}
		}
	}
	for _, id := range Present() {
		if !has(id) {
			t.Errorf("Present() names %q, which has nothing embedded", id)
		}
	}
}

// --- The version rule ----------------------------------------------------------

func TestOlderThan(t *testing.T) {
	for _, tc := range []struct {
		installed, shipped string
		want               bool
		why                string
	}{
		{"0.9.0", "1.0.0", true, "an older bundled copy is replaced"},
		{"1.0.0", "1.0.0", false, "the same version is left alone"},
		{"1.1.0", "1.0.0", false, "a NEWER installed copy is left alone"},
		{"1.2", "1.2.1", true, "a longer version wins a prefix tie"},
		{"1.2.1", "1.2", false, "and the other way round"},
		{"1.10.0", "1.9.0", false, "segments are numbers, not strings"},
		{"", "1.0.0", true, "an empty version is unorderable, so it is older"},
		{"1.0.0-beta", "1.0.0", true, "and so is anything with a letter in it"},
		{"2024-01-01", "1.0.0", true, "a date is not a version this rule can read"},
		{"1.0.0", "", false, "an unreadable SHIPPED version replaces nothing — that is a bug in the tree"},
	} {
		if got := olderThan(tc.installed, tc.shipped); got != tc.want {
			t.Errorf("olderThan(%q, %q) = %v, want %v — %s", tc.installed, tc.shipped, got, tc.want, tc.why)
		}
	}
}

// --- The assertion table -------------------------------------------------------

func TestAssertInstallsOnAFirstBoot(t *testing.T) {
	requireModules(t)
	dir := t.TempDir()
	st := newAssertStore()

	NewSource(dir, st, quiet(t)).AssertAll(t.Context())

	pluginDir := filepath.Join(dir, plugins.DirName, "tmdb")
	for _, name := range []string{plugins.ManifestFile, plugins.DefaultModuleFile} {
		if _, err := os.Stat(filepath.Join(pluginDir, name)); err != nil {
			t.Fatalf("a first boot did not write %s: %v", name, err)
		}
	}
	row, ok := st.row("tmdb")
	if !ok {
		t.Fatal("a first boot wrote no plugins row")
	}
	if row.Origin != plugins.OriginBundled {
		t.Errorf("origin = %q, want %q", row.Origin, plugins.OriginBundled)
	}
	if row.Version == "" || row.Name == "" || row.APIVersion != pluginapi.APIVersion {
		t.Errorf("the row does not carry the manifest's facts: %+v", row)
	}

	// A SECOND boot changes nothing: the same version is not replaced and no
	// second row is written.
	before := st.inserts
	NewSource(dir, st, quiet(t)).AssertAll(t.Context())
	if st.inserts != before {
		t.Errorf("a second boot inserted the row again (%d inserts)", st.inserts)
	}
}

func TestAssertReplacesAnOlderBundledCopy(t *testing.T) {
	requireModules(t)
	dir := t.TempDir()
	st := newAssertStore()
	pluginDir := filepath.Join(dir, plugins.DirName, "tmdb")
	mustMkdir(t, pluginDir)
	writeFile(t, filepath.Join(pluginDir, plugins.ManifestFile),
		`{"id":"tmdb","name":"The Movie Database (TMDB)","version":"0.9.0","apiVersion":1,`+
			`"provides":[{"kind":"metadata-provider"}]}`)
	writeFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile), "an old module")
	st.put("tmdb", plugins.OriginBundled, "0.9.0")

	NewSource(dir, st, quiet(t)).AssertAll(t.Context())

	module := readFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile))
	if module == "an old module" {
		t.Fatal("the older bundled copy was not replaced")
	}
	shippedManifest, err := Manifest("tmdb")
	if err != nil {
		t.Fatal(err)
	}
	if got := versionOnDisk(t, pluginDir); got != shippedManifest.Version {
		t.Errorf("the manifest on disk says %q, want the shipped %q", got, shippedManifest.Version)
	}
	row, _ := st.row("tmdb")
	if row.Version != shippedManifest.Version {
		t.Errorf("the row still says %q, want %q", row.Version, shippedManifest.Version)
	}
	if st.inserts != 0 {
		t.Errorf("a replace inserted a row (%d); the id is the same, so the settings row, the "+
			"per-Library overrides and the item pins must all survive", st.inserts)
	}
}

func TestAssertLeavesAnAdminsOwnPluginAlone(t *testing.T) {
	requireModules(t)
	dir := t.TempDir()
	st := newAssertStore()
	pluginDir := filepath.Join(dir, plugins.DirName, "tmdb")
	mustMkdir(t, pluginDir)
	writeFile(t, filepath.Join(pluginDir, plugins.ManifestFile),
		`{"id":"tmdb","name":"My own TMDB","version":"0.0.1","apiVersion":1,`+
			`"provides":[{"kind":"metadata-provider"}]}`)
	writeFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile), "the admin's module")
	st.put("tmdb", plugins.OriginAdmin, "0.0.1")

	NewSource(dir, st, quiet(t)).AssertAll(t.Context())

	if got := readFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile)); got != "the admin's module" {
		t.Error("an admin's own plugin was overwritten by the shipped one; theirs wins, whatever the version")
	}
	if st.updates != 0 || st.inserts != 0 {
		t.Errorf("an admin's row was written to (%d inserts, %d updates)", st.inserts, st.updates)
	}
}

func TestAssertLeavesAHandPlacedPluginAlone(t *testing.T) {
	requireModules(t)
	dir := t.TempDir()
	st := newAssertStore() // no row at all
	pluginDir := filepath.Join(dir, plugins.DirName, "tmdb")
	mustMkdir(t, pluginDir)
	writeFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile), "placed by hand")

	NewSource(dir, st, quiet(t)).AssertAll(t.Context())

	if got := readFile(t, filepath.Join(pluginDir, plugins.DefaultModuleFile)); got != "placed by hand" {
		t.Error("a plugin an operator placed by hand was overwritten")
	}
}

func TestAssertSkipsADeclinedPlugin(t *testing.T) {
	requireModules(t)
	dir := t.TempDir()
	st := newAssertStore()
	st.decline("tmdb")

	NewSource(dir, st, quiet(t)).AssertAll(t.Context())

	if _, err := os.Stat(filepath.Join(dir, plugins.DirName, "tmdb")); err == nil {
		t.Fatal("a declined plugin was reinstalled on boot, so uninstall means 'until you restart'")
	}
	if st.inserts != 0 {
		t.Errorf("a declined plugin got a row (%d inserts)", st.inserts)
	}

	// Clearing the mark — which is what "reinstall the shipped version" does —
	// brings it back on the next assertion.
	st.undecline("tmdb")
	if err := NewSource(dir, st, quiet(t)).Assert(t.Context(), "tmdb"); err != nil {
		t.Fatalf("Assert after undecline: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, plugins.DirName, "tmdb")); err != nil {
		t.Fatalf("clearing the declined mark did not bring the plugin back: %v", err)
	}
}

func TestAssertRefusesAnIDThisServerDoesNotShip(t *testing.T) {
	err := NewSource(t.TempDir(), newAssertStore(), quiet(t)).Assert(t.Context(), "not-a-bundled-plugin")
	if err == nil {
		t.Fatal("asserting an id this server does not ship succeeded")
	}
	if !strings.Contains(err.Error(), "not-a-bundled-plugin") {
		t.Errorf("the refusal does not name the id: %v", err)
	}
}

func versionOnDisk(t *testing.T, dir string) string {
	t.Helper()
	raw := readFile(t, filepath.Join(dir, plugins.ManifestFile))
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("the manifest on disk is not JSON: %v", err)
	}
	return m.Version
}

// --- The fake store and the small file helpers ---------------------------------

// assertStore is the two tables the assertion reads and writes, in maps. It is
// here rather than a *store.DB because what is under test is the TABLE of
// decisions, and a decision is easier to state as a row than as a migration.
type assertStore struct {
	rows     map[string]store.PluginRow
	declined map[string]bool
	inserts  int
	updates  int
}

func newAssertStore() *assertStore {
	return &assertStore{rows: map[string]store.PluginRow{}, declined: map[string]bool{}}
}

func (s *assertStore) Plugins() ([]store.PluginRow, error) {
	out := make([]store.PluginRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *assertStore) InsertPlugin(p store.PluginInsert) error {
	s.inserts++
	s.rows[p.ID] = store.PluginRow{
		ID: p.ID, Name: p.Name, Version: p.Version, APIVersion: p.APIVersion,
		Provides: p.Provides, Enabled: true, Source: p.Source, Origin: p.Origin,
	}
	delete(s.declined, p.ID)
	return nil
}

func (s *assertStore) UpdatePluginManifest(p store.PluginInsert) error {
	s.updates++
	row := s.rows[p.ID]
	row.Name, row.Version, row.APIVersion, row.Provides = p.Name, p.Version, p.APIVersion, p.Provides
	s.rows[p.ID] = row
	return nil
}

func (s *assertStore) DeclinedPluginIDs() ([]string, error) {
	var out []string
	for id, yes := range s.declined {
		if yes {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *assertStore) put(id, origin, version string) {
	s.rows[id] = store.PluginRow{ID: id, Name: id, Version: version, APIVersion: 1, Origin: origin, Enabled: true}
}

func (s *assertStore) row(id string) (store.PluginRow, bool) {
	r, ok := s.rows[id]
	return r, ok
}

func (s *assertStore) decline(id string)   { s.declined[id] = true }
func (s *assertStore) undecline(id string) { delete(s.declined, id) }

// quiet sends the assertion's log lines to the test's own output, so a failing
// case shows what the server would have printed instead of polluting stderr.
func quiet(t *testing.T) func(string, ...any) {
	t.Helper()
	return func(format string, args ...any) { t.Logf(format, args...) }
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
