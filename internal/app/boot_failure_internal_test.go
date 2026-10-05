package app

import (
	"os"
	"testing"

	"github.com/goozakdev/obelo-server/internal/config"
)

// TestNewFailureAfterPluginLoadUnwinds: a boot failure past the plugin load goes
// through the cleanup stack (plugins, broker, sink dispatcher) and the DB close
// without panicking or hanging, and still reports the cause.
func TestNewFailureAfterPluginLoadUnwinds(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	// A regular file where the subtitle cache directory must go makes
	// subfetch.EnsureCacheDir fail, which is after plugins.Load.
	if err := os.WriteFile(cfg.SubtitleCacheDir(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err == nil {
		_ = a.Close()
		t.Fatal("New succeeded, want the subtitle cache failure")
	}
}
