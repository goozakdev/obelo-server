package bundled

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Every module this build ships must pass the install-time probe (ADR-0069 Q6): it
// starts, and exports every call its own manifest claims. If this fails the module or
// the manifest is wrong, not the probe.
//
// A missing module FAILS here rather than skipping, unlike requireModules: this is the
// only test that proves the shipped modules satisfy the probe, so a skip under make
// check would turn "the bundled plugins cannot be installed" into a green run. A clone
// that has not run `make plugins` gets the same failure TestEveryBundledModuleIsPresent
// already gives it, with the same instruction.
func TestEveryShippedModulePassesTheInstallProbe(t *testing.T) {
	for _, id := range shipped() {
		t.Run(id, func(t *testing.T) {
			if !has(id) {
				t.Fatalf("the bundled plugin %q has no module or no manifest — run make plugins", id)
			}
			manifest, err := ManifestBytes(id)
			if err != nil {
				t.Fatal(err)
			}
			module, err := Module(id)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), plugins.DirName)
			set, err := plugins.Load(context.Background(), dir, plugins.Options{Logf: func(string, ...any) {}})
			if err != nil {
				t.Fatal(err)
			}
			m := plugins.NewManager(plugins.ManagerConfig{
				Dir: dir, Registry: pluginapi.NewRegistry(), Set: set,
				Loader: plugins.Options{Logf: func(string, ...any) {}}, Logf: func(string, ...any) {},
			})
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			if _, err := m.Install(context.Background(), manifest, module, nil, plugins.SourceUpload); err != nil {
				t.Fatalf("the shipped %s does not pass the install probe: %v", id, err)
			}
		})
	}
}
