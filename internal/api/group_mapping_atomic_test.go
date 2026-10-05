package api_test

import (
	"net/http"
	"testing"
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
