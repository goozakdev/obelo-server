package plugins_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Uninstalling a Sign-in provider (ADR-0063 decision 10), at the Manager, over a
// REAL store: what is under test is which of the files, the registry and the one
// transaction moved, and a fake store could only say whether it was called.

type signInUninstallFixture struct {
	dir      string
	db       *store.DB
	registry *pluginapi.Registry
	manager  *plugins.Manager
	reloads  atomic.Int64
	// failReloads is how many of the next reloads fail.
	failReloads atomic.Int64
}

// newSignInUninstallFixture installs the Sign-in provider "directory" and gives
// ada, whose only path it is, its identity and a session.
func newSignInUninstallFixture(t *testing.T) *signInUninstallFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f := &signInUninstallFixture{dir: filepath.Join(t.TempDir(), plugins.DirName), db: db,
		registry: pluginapi.NewRegistry()}
	set, err := plugins.Load(context.Background(), f.dir, plugins.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("loading an empty plugins directory: %v", err)
	}
	f.manager = plugins.NewManager(plugins.ManagerConfig{
		Dir:      f.dir,
		Registry: f.registry,
		Set:      set,
		Store:    db,
		Loader:   plugins.Options{Logf: func(string, ...any) {}},
		Base:     func(*pluginapi.Registry) {},
		Reload: func(context.Context) error {
			f.reloads.Add(1)
			if f.failReloads.Add(-1) >= 0 {
				return errors.New("the reload was made to fail")
			}
			f.failReloads.Store(0)
			return nil
		},
		Logf: func(string, ...any) {},
	})
	t.Cleanup(func() { _ = f.manager.Close(context.Background()) })

	m := plugintest.SignInManifest("directory", "ada:ada-pw:s-ada::")
	if _, err := f.manager.Install(context.Background(), plugintest.ManifestJSON(t, m), plugintest.Guest(t), nil,
		plugins.SourceUpload); err != nil {
		t.Fatalf("installing the directory: %v", err)
	}
	if _, err := db.CreateExternalMember("ada", "ada", "directory", "s-ada", "ada", nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO devices (id, user_id, client_id, name, platform) VALUES ('d-ada', 'ada', 'c', 'n', 'p')`,
		`INSERT INTO auth_tokens (token_hash, device_id, user_id) VALUES ('t-ada', 'd-ada', 'ada')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// wantDirectoryUntouched fails unless ada, her identity and her session are all
// there, and the directory is installed, on disk and in the registry.
func (f *signInUninstallFixture) wantDirectoryUntouched(t *testing.T) {
	t.Helper()
	if _, err := f.db.UserByID("ada"); err != nil {
		t.Fatalf("ada: %v, want her still a User", err)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM external_identities WHERE plugin_id = 'directory'`: 1,
		`SELECT COUNT(*) FROM auth_tokens WHERE user_id = 'ada'`:                 1,
		`SELECT COUNT(*) FROM plugins WHERE id = 'directory'`:                    1,
	} {
		var n int
		if err := f.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s = %d, want %d", q, n, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "directory")); err != nil {
		t.Fatalf("the directory's files: %v, want them in place", err)
	}
	if _, ok := f.registry.SignInProvider("directory"); !ok {
		t.Fatal("the directory is not in the registry")
	}
}

// TestAFailedRebuildAbortsTheSignInUninstall: the rebuild that follows moving
// the files aside fails. The files go back, the registry is rebuilt with the
// directory in it, the error comes back, and nothing is deleted.
func TestAFailedRebuildAbortsTheSignInUninstall(t *testing.T) {
	plugins.Parallel(t)
	f := newSignInUninstallFixture(t)
	f.failReloads.Store(1)

	err := f.manager.UninstallConfirming(context.Background(), "directory", []string{"ada"})
	if err == nil {
		t.Fatal("the uninstall succeeded; want the failed rebuild to abort it")
	}
	f.wantDirectoryUntouched(t)
}

// TestAStaleConfirmationNeverTakesTheProviderOutOfTheRegistry: a confirm that
// does not name ada is refused before anything moves, so the registry is never
// rebuilt — not without the directory, and not with it again afterwards.
func TestAStaleConfirmationNeverTakesTheProviderOutOfTheRegistry(t *testing.T) {
	plugins.Parallel(t)
	f := newSignInUninstallFixture(t)
	before := f.reloads.Load()

	err := f.manager.UninstallConfirming(context.Background(), "directory", nil)
	var unconfirmed *store.UnconfirmedUninstallError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("err = %v, want an unconfirmed uninstall", err)
	}
	if n := f.reloads.Load() - before; n != 0 {
		t.Fatalf("a refused confirm rebuilt the registry %d times, want 0", n)
	}
	f.wantDirectoryUntouched(t)
}
