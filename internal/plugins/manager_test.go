package plugins_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The install lifecycle and the rebuild-and-swap (.scratch/plugin-system issue
// 10), tested against the same real module every other test in this package uses.
//
// The test this file exists for is the LAST one: a reader looping over the
// registry while installs and uninstalls run underneath it. Everything above it is
// there so that test is not the only thing standing between a swap and a bad day.

// --- a store ------------------------------------------------------------------

// memStore is the plugins table, in memory. The Manager's persistence is four
// small questions and this answers them without a database, which is what keeps
// this package's suite free of one.
type memStore struct {
	mu   sync.Mutex
	rows map[string]store.PluginRow
	// insertErr, when set, makes the next InsertPlugin fail — the one failure mode
	// that has to leave nothing behind on disk.
	insertErr error
}

func newMemStore() *memStore { return &memStore{rows: map[string]store.PluginRow{}} }

func (s *memStore) Plugins() ([]store.PluginRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.PluginRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *memStore) DisabledPluginIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, r := range s.rows {
		if !r.Enabled {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *memStore) InsertPlugin(p store.PluginInsert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	if _, exists := s.rows[p.ID]; exists {
		return fmt.Errorf("plugin %q is already recorded", p.ID)
	}
	s.rows[p.ID] = store.PluginRow{
		ID: p.ID, Name: p.Name, Version: p.Version, APIVersion: p.APIVersion,
		Provides: p.Provides, Enabled: true, Source: p.Source,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func (s *memStore) SetPluginEnabled(id string, enabled bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return false, nil
	}
	r.Enabled = enabled
	s.rows[id] = r
	return true, nil
}

func (s *memStore) SetPluginLastError(id, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		r.LastError = message
		s.rows[id] = r
	}
	return nil
}

func (s *memStore) DeletePlugin(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

// --- a harness ------------------------------------------------------------------

// managerFixture is a Manager over a temp plugins directory, with a live registry
// that always carries one Built-in — so every test here also checks the thing an
// install must never do, which is lose the Built-ins on the way through.
type managerFixture struct {
	dir      string
	registry *pluginapi.Registry
	store    *memStore
	manager  *plugins.Manager
	reloads  atomic.Int64
}

const builtinSlug = "a-built-in"

func newManagerFixture(t *testing.T) *managerFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), plugins.DirName)

	f := &managerFixture{dir: dir, registry: pluginapi.NewRegistry(), store: newMemStore()}
	base := func(reg *pluginapi.Registry) {
		reg.RegisterEventSink(pluginapi.EventSinkRegistration{
			Descriptor: pluginapi.Descriptor{Slug: builtinSlug, Name: "A Built-in"},
			New: func(pluginapi.Settings) (pluginapi.EventSink, error) {
				return nil, errors.New("this built-in exists to be counted, not called")
			},
		})
	}
	base(f.registry)

	set, err := plugins.Load(context.Background(), dir, plugins.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("loading an empty plugins directory: %v", err)
	}
	f.manager = plugins.NewManager(plugins.ManagerConfig{
		Dir:      dir,
		Registry: f.registry,
		Set:      set,
		Store:    f.store,
		Loader:   plugins.Options{Logf: func(string, ...any) {}},
		Base:     base,
		Reload:   func(context.Context) error { f.reloads.Add(1); return nil },
		Logf:     func(string, ...any) {},
	})
	t.Cleanup(func() { _ = f.manager.Close(context.Background()) })
	return f
}

// installGuest installs the real test module under an id, and fails the test if
// the server refuses it.
func (f *managerFixture) installGuest(t *testing.T, id string) plugins.Installed {
	t.Helper()
	m := plugintest.SinkManifest(id)
	got, err := f.manager.Install(context.Background(),
		plugintest.ManifestJSON(t, m), plugintest.Guest(t), plugins.SourceUpload)
	if err != nil {
		t.Fatalf("installing %s: %v", id, err)
	}
	return got
}

func (f *managerFixture) sinkSlugs() []string {
	var out []string
	for _, reg := range f.registry.EventSinks() {
		out = append(out, reg.Descriptor.Slug)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// refusalReason reports the Reason of a refusal, or "" for anything else.
func refusalReason(err error) string {
	var r *plugins.Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// --- install --------------------------------------------------------------------

// TestInstallingAPluginRegistersItWithoutARestart is the tracer: two files go in,
// the Plugin comes out registered beside the Built-in, and the Managers that
// compose from the registry were told to rebuild.
func TestInstallingAPluginRegistersItWithoutARestart(t *testing.T) {
	f := newManagerFixture(t)

	if got := f.sinkSlugs(); len(got) != 1 || got[0] != builtinSlug {
		t.Fatalf("before the install the registry holds %v, want just the Built-in", got)
	}

	installed := f.installGuest(t, "example-sink")
	if installed.ID != "example-sink" || installed.Name != "Test Sink (example-sink)" || installed.Version != "1.0.0" {
		t.Fatalf("the installed plugin reads as %+v, want the manifest's own facts", installed)
	}
	if !installed.Enabled || installed.Disabled || installed.LastError != "" {
		t.Fatalf("a freshly installed plugin reads as %+v, want enabled and working", installed)
	}
	if len(installed.Provides) != 1 || installed.Provides[0] != string(pluginapi.ExtensionEventSink) {
		t.Fatalf("provides = %v, want the one extension point the manifest names", installed.Provides)
	}

	slugs := f.sinkSlugs()
	if !contains(slugs, builtinSlug) {
		t.Fatalf("the Built-in was lost by the swap: %v", slugs)
	}
	if !contains(slugs, "example-sink") {
		t.Fatalf("the installed Plugin is not registered: %v", slugs)
	}
	if f.reloads.Load() == 0 {
		t.Fatal("nothing composed from the registry was told to rebuild, so a live sink would still be the old set")
	}

	// The files are where the loader looks for them, under the names it expects.
	for _, name := range []string{plugins.ManifestFile, plugins.DefaultModuleFile} {
		if _, err := os.Stat(filepath.Join(f.dir, "example-sink", name)); err != nil {
			t.Fatalf("after an install there is no %s on disk: %v", name, err)
		}
	}
}

// TestEveryInstallRefusalLeavesNothingBehind is the property the staging directory
// exists for. Each refusal is its own reason, and after each one the plugins
// directory holds nothing — no half-written module, no orphaned row, no staging
// directory a later boot would trip over.
func TestEveryInstallRefusalLeavesNothingBehind(t *testing.T) {
	good := plugintest.SinkManifest("example-sink")

	futureVersion := plugintest.SinkManifest("future-sink")
	futureVersion.APIVersion = 2

	noProvides := plugintest.SinkManifest("empty-sink")
	noProvides.Provides = nil

	for _, tc := range []struct {
		name       string
		manifest   []byte
		module     []byte
		wantReason string
		wantIn     string
	}{
		{
			name:       "not JSON at all",
			manifest:   []byte("this is not a manifest"),
			wantReason: plugins.ReasonManifest,
			wantIn:     "is not valid JSON",
		},
		{
			name:       "a manifest that provides nothing",
			manifest:   mustJSON(t, noProvides),
			wantReason: plugins.ReasonManifest,
			wantIn:     "provides nothing",
		},
		{
			name:       "an API version this server does not speak",
			manifest:   mustJSON(t, futureVersion),
			wantReason: plugins.ReasonAPIVersion,
			wantIn:     "upgrade the server",
		},
		{
			name:       "no module beside the manifest",
			manifest:   mustJSON(t, good),
			module:     []byte{},
			wantReason: plugins.ReasonModule,
			wantIn:     "there is no module",
		},
		{
			name:       "a module that is not WebAssembly",
			manifest:   mustJSON(t, good),
			module:     []byte("MZ\x00\x00 definitely a native binary"),
			wantReason: plugins.ReasonModule,
			wantIn:     "",
		},
		{
			name:       "no manifest at all",
			manifest:   nil,
			wantReason: plugins.ReasonManifest,
			wantIn:     "there is no manifest.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			module := tc.module
			if module == nil {
				module = plugintest.Guest(t)
			}
			_, err := f.manager.Install(context.Background(), tc.manifest, module, plugins.SourceUpload)
			if err == nil {
				t.Fatal("the install was accepted")
			}
			if got := refusalReason(err); got != tc.wantReason {
				t.Fatalf("reason = %q, want %q (message: %v)", got, tc.wantReason, err)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("message = %q, want it to contain %q", err, tc.wantIn)
			}
			assertPluginsDirEmpty(t, f.dir)
			rows, _ := f.store.Plugins()
			if len(rows) != 0 {
				t.Fatalf("a refused install left %d rows behind", len(rows))
			}
		})
	}
}

// TestADuplicateIdIsRefusedThreeWays: the id is the directory, the settings row
// and the registry slug all at once, so three different things can already hold
// it — and installing over a Built-in would be the worst of them, because it would
// move an Admin's signing secret onto code the maintainer did not write.
func TestADuplicateIdIsRefusedThreeWays(t *testing.T) {
	f := newManagerFixture(t)
	f.installGuest(t, "example-sink")

	for _, id := range []string{"example-sink", builtinSlug} {
		t.Run(id, func(t *testing.T) {
			m := plugintest.SinkManifest(id)
			_, err := f.manager.Install(context.Background(),
				mustJSON(t, m), plugintest.Guest(t), plugins.SourceUpload)
			if got := refusalReason(err); got != plugins.ReasonDuplicate {
				t.Fatalf("installing over %q gave reason %q, want %q (err: %v)",
					id, got, plugins.ReasonDuplicate, err)
			}
			if !strings.Contains(err.Error(), id) {
				t.Fatalf("the refusal %q does not name the id that is taken", err)
			}
		})
	}
	// The one that was already there is untouched.
	if !contains(f.sinkSlugs(), "example-sink") {
		t.Fatal("a refused duplicate install unregistered the plugin it collided with")
	}
}

// TestAFailedRowWriteRollsTheFilesBack: the database is the last thing an install
// touches, and when it says no the files go too. A directory with no row is a
// Plugin nobody can switch off.
func TestAFailedRowWriteRollsTheFilesBack(t *testing.T) {
	f := newManagerFixture(t)
	f.store.insertErr = errors.New("the disk is full")

	_, err := f.manager.Install(context.Background(),
		mustJSON(t, plugintest.SinkManifest("example-sink")), plugintest.Guest(t), plugins.SourceUpload)
	if err == nil {
		t.Fatal("an install whose row could not be written reported success")
	}
	assertPluginsDirEmpty(t, f.dir)
}

// --- lifecycle --------------------------------------------------------------------

// TestDisablingUnregistersAndEnablingBringsItBack. Disable is not a flag the
// delivery path checks — it is the Plugin not being in the registry at all, which
// is why nothing downstream had to learn what a disabled Plugin is.
func TestDisablingUnregistersAndEnablingBringsItBack(t *testing.T) {
	f := newManagerFixture(t)
	f.installGuest(t, "example-sink")

	view, err := f.manager.SetEnabled(context.Background(), "example-sink", false)
	if err != nil {
		t.Fatalf("disabling: %v", err)
	}
	if view.Enabled {
		t.Fatalf("after Disable the plugin reads as %+v, want enabled false", view)
	}
	if contains(f.sinkSlugs(), "example-sink") {
		t.Fatalf("a disabled Plugin is still registered: %v", f.sinkSlugs())
	}
	if !contains(f.sinkSlugs(), builtinSlug) {
		t.Fatal("disabling a Plugin lost the Built-ins")
	}
	// Still on the screen, with its name and version — an Admin has to be able to
	// find the thing they turned off.
	list, err := f.manager.List(context.Background())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 1 || list[0].ID != "example-sink" || list[0].Name == "" {
		t.Fatalf("a disabled Plugin lists as %+v, want it named and present", list)
	}

	if _, err := f.manager.SetEnabled(context.Background(), "example-sink", true); err != nil {
		t.Fatalf("enabling: %v", err)
	}
	if !contains(f.sinkSlugs(), "example-sink") {
		t.Fatalf("a re-enabled Plugin is not registered: %v", f.sinkSlugs())
	}
}

// TestReenableClearsTheRecordedFailure: the button an Admin presses after they
// have fixed whatever the Plugin was complaining about.
func TestReenableClearsTheRecordedFailure(t *testing.T) {
	f := newManagerFixture(t)
	f.installGuest(t, "example-sink")

	// Drive the Plugin into the disabled state the ordinary way: a guest that traps
	// on every call, as many times in a row as the loader disables on.
	p := pluginNamed(t, f, "example-sink")
	sink, err := f.registrationFor("example-sink").New(
		pluginapi.Settings{URL: "http://127.0.0.1:1/?obelo-mode=panic"})
	if err != nil {
		t.Fatalf("building the sink: %v", err)
	}
	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		_ = sink.Deliver(context.Background(), pluginapi.SinkEvent{
			ID: fmt.Sprintf("ev-%d", i), Type: pluginapi.EventScanCompleted,
		})
	}
	if !p.Disabled() {
		t.Fatal("a Plugin that trapped on every delivery is not disabled, so this test proves nothing")
	}

	view, reErr := f.manager.Reenable(context.Background(), "example-sink")
	if reErr != nil {
		t.Fatalf("re-enabling: %v", reErr)
	}
	if view.Disabled || view.LastError != "" {
		t.Fatalf("after Re-enable the plugin reads as %+v, want a clean record", view)
	}
	if fresh := pluginNamed(t, f, "example-sink"); fresh.Disabled() {
		t.Fatal("Re-enable did not rebuild the Plugin from disk")
	}
}

// TestUninstallRemovesTheFilesAndTheRegistration.
func TestUninstallRemovesTheFilesAndTheRegistration(t *testing.T) {
	f := newManagerFixture(t)
	f.installGuest(t, "example-sink")

	if err := f.manager.Uninstall(context.Background(), "example-sink"); err != nil {
		t.Fatalf("uninstalling: %v", err)
	}
	if contains(f.sinkSlugs(), "example-sink") {
		t.Fatalf("an uninstalled Plugin is still registered: %v", f.sinkSlugs())
	}
	if !contains(f.sinkSlugs(), builtinSlug) {
		t.Fatal("uninstalling lost the Built-ins")
	}
	assertPluginsDirEmpty(t, f.dir)
	if rows, _ := f.store.Plugins(); len(rows) != 0 {
		t.Fatalf("uninstalling left %d rows behind", len(rows))
	}
	if list, _ := f.manager.List(context.Background()); len(list) != 0 {
		t.Fatalf("an uninstalled Plugin is still listed: %+v", list)
	}
	// And the id is free again.
	f.installGuest(t, "example-sink")
}

// TestALifecycleVerbOnAPluginThatIsNotInstalled.
func TestALifecycleVerbOnAPluginThatIsNotInstalled(t *testing.T) {
	f := newManagerFixture(t)
	if _, err := f.manager.SetEnabled(context.Background(), "nobody", false); refusalReason(err) != plugins.ReasonUnknown {
		t.Fatalf("Disable on an absent plugin gave %v, want an unknown-plugin refusal", err)
	}
	if _, err := f.manager.Reenable(context.Background(), "nobody"); refusalReason(err) != plugins.ReasonUnknown {
		t.Fatalf("Re-enable on an absent plugin gave %v, want an unknown-plugin refusal", err)
	}
	if err := f.manager.Uninstall(context.Background(), "nobody"); refusalReason(err) != plugins.ReasonUnknown {
		t.Fatalf("Uninstall on an absent plugin gave %v, want an unknown-plugin refusal", err)
	}
}

// TestAHandPlacedPluginCanStillBeSwitchedOff: every Installed plugin arrived by
// hand before this issue existed, and those have files and no row. The screen
// lists them, and their buttons work — the row is created the moment one is
// needed.
func TestAHandPlacedPluginCanStillBeSwitchedOff(t *testing.T) {
	f := newManagerFixture(t)
	dataDir := filepath.Dir(f.dir)
	plugintest.Install(t, dataDir, plugintest.SinkManifest("by-hand"))

	if _, err := f.manager.SetEnabled(context.Background(), "by-hand", false); err != nil {
		t.Fatalf("disabling a hand-placed Plugin: %v", err)
	}
	if contains(f.sinkSlugs(), "by-hand") {
		t.Fatalf("a hand-placed Plugin switched off is still registered: %v", f.sinkSlugs())
	}
	list, _ := f.manager.List(context.Background())
	if len(list) != 1 || list[0].ID != "by-hand" || list[0].Enabled {
		t.Fatalf("list = %+v, want the hand-placed Plugin, switched off", list)
	}
}

// --- the race ---------------------------------------------------------------------

// TestAReaderNeverSeesAHalfBuiltRegistry is the test this whole design exists to
// pass. One goroutine reads the registry in a tight loop — as the settings API,
// the sink Manager and the enrichment Catalog all do, on paths that never expected
// to be interrupted — while another installs and uninstalls Plugins underneath it.
//
// Two things are asserted, and the second matters as much as the first. Under
// `go test -race` there must be no data race: the registrations are copy-on-write
// behind an atomic pointer, so a reader holds a value nobody is appending to.
// And EVERY SNAPSHOT MUST BE WHOLE — the Built-in present, every registration
// carrying its slug and its factory — because "no race" would still be true of a
// design that published a registry with the Built-ins registered and the Plugins
// not yet.
func TestAReaderNeverSeesAHalfBuiltRegistry(t *testing.T) {
	f := newManagerFixture(t)

	stop := make(chan struct{})
	var reads atomic.Int64
	var bad atomic.Value // string

	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sinks := f.registry.EventSinks()
				reads.Add(1)
				sawBuiltin := false
				for _, reg := range sinks {
					if reg.Descriptor.Slug == "" {
						bad.Store("a registration with no slug")
						return
					}
					if reg.New == nil {
						bad.Store(fmt.Sprintf("the registration %q has no factory", reg.Descriptor.Slug))
						return
					}
					if reg.Descriptor.ExtensionPoint != pluginapi.ExtensionEventSink {
						bad.Store(fmt.Sprintf("the registration %q is not an event sink", reg.Descriptor.Slug))
						return
					}
					if reg.Descriptor.Slug == builtinSlug {
						sawBuiltin = true
					}
				}
				if !sawBuiltin {
					// The Built-in is in every snapshot, before, during and after
					// every install. A snapshot without it is a half-built value.
					bad.Store("a snapshot with no Built-in in it")
					return
				}
				// Also read the other two seams, which the same Swap publishes.
				_ = f.registry.MetadataProviders()
				_, _ = f.registry.SubtitleProvider("nobody")
			}
		}()
	}

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("racer-%d", i)
		f.installGuest(t, id)
		if err := f.manager.Uninstall(context.Background(), id); err != nil {
			t.Fatalf("uninstalling %s: %v", id, err)
		}
	}

	close(stop)
	readers.Wait()

	if msg, ok := bad.Load().(string); ok {
		t.Fatalf("a reader observed %s", msg)
	}
	if reads.Load() == 0 {
		t.Fatal("no reads happened, so nothing was raced against")
	}
}

// --- helpers ------------------------------------------------------------------

func (f *managerFixture) registrationFor(slug string) pluginapi.EventSinkRegistration {
	reg, _ := f.registry.EventSink(slug)
	return reg
}

func pluginNamed(t *testing.T, f *managerFixture, id string) *plugins.Plugin {
	t.Helper()
	for _, p := range f.manager.Plugins().Plugins() {
		if p.ID() == id {
			return p
		}
	}
	t.Fatalf("no loaded Plugin %q", id)
	return nil
}

func mustJSON(t *testing.T, m pluginapi.Manifest) []byte {
	t.Helper()
	return plugintest.ManifestJSON(t, m)
}

// assertPluginsDirEmpty fails unless the plugins directory holds nothing at all —
// no Plugin, and no staging or uninstall directory either. Those are dot-prefixed
// and the loader skips them, which is exactly why a test has to look.
func assertPluginsDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s holds %v, want nothing", dir, names)
	}
}
