package api_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// TestARefusedGroupMappingLeavesTheIntervalAlone: rules and interval are one
// write, so a PUT whose rule grants a Library that does not exist does not move
// the re-check interval either.
func TestARefusedGroupMappingLeavesTheIntervalAlone(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::family"})
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{}, "intervalHours": 6})

	status, raw := srv.JSON(http.MethodPut, groupMappingPath("directory"), admin, map[string]any{
		"rules":         []map[string]any{{"group": "x", "role": "member", "libraryIds": []string{"nope"}}},
		"intervalHours": 12,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT granting an unknown library = %d, want 400; body: %s", status, raw)
	}
	var view groupMappingView
	if status, body := srv.AuthGET(groupMappingPath("directory"), admin, &view); status != http.StatusOK {
		t.Fatalf("GET = %d; body: %s", status, body)
	}
	if view.IntervalHours != 6 {
		t.Fatalf("interval after a refused PUT = %d hours, want it unchanged at 6", view.IntervalHours)
	}
}

// TestAFailedIntervalWriteLeavesTheRulesUnsaved: the other half of the
// all-or-nothing PUT. The interval write is made to fail, and the rules the same
// request carried must not have been saved ahead of it.
func TestAFailedIntervalWriteLeavesTheRulesUnsaved(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::family"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)

	db, err := store.Open(filepath.Join(dataDir, "obelo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse_interval BEFORE INSERT ON sign_in_recheck_intervals
		BEGIN SELECT RAISE(ABORT, 'interval refused'); END`); err != nil {
		t.Fatal(err)
	}

	status, raw := srv.JSON(http.MethodPut, groupMappingPath("directory"), admin, map[string]any{
		"rules":         []map[string]any{{"group": "kids", "role": "member", "libraryIds": []string{}}},
		"intervalHours": 12,
	}, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("PUT with a failing interval write = %d, want 500; body: %s", status, raw)
	}
	var view groupMappingView
	if status, body := srv.AuthGET(groupMappingPath("directory"), admin, &view); status != http.StatusOK {
		t.Fatalf("GET = %d; body: %s", status, body)
	}
	if len(view.Rules) != 0 {
		t.Fatalf("rules after a failed PUT = %+v, want none saved", view.Rules)
	}
}
