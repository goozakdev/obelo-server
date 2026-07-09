package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for Library management: drive the wired server over HTTP and
// assert only the wire shapes and observable state (PRD testing contract).

type libraryRootResp struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type libraryResp struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	CreatedAt   string            `json:"createdAt"`
	RootFolders []libraryRootResp `json:"rootFolders"`
}

type librariesListResp struct {
	Libraries []libraryResp `json:"libraries"`
}

// adminToken sets up the first Admin and logs in, returning a usable token.
func adminToken(t *testing.T, srv *testharness.Server) string {
	t.Helper()
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	return login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "admin-client").Token
}

// createLibrary POSTs a library and returns status, body, and decoded library.
func createLibrary(t *testing.T, srv *testharness.Server, token string, body map[string]any) (int, libraryResp, []byte) {
	t.Helper()
	var out libraryResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/libraries", token, body, &out)
	return status, out, raw
}

// TestCreateLibraryHappyPath: an Admin creates a Movie Library over multiple
// roots and gets it back with its merged root folders.
func TestCreateLibraryHappyPath(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	status, lib, body := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies", "/mnt/films"},
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", status, body)
	}
	if lib.ID == "" {
		t.Errorf("library id is empty; body: %s", body)
	}
	if lib.Name != "Movies" || lib.Kind != "movie" {
		t.Errorf("library = %+v, want name=Movies kind=movie", lib)
	}
	if len(lib.RootFolders) != 2 {
		t.Fatalf("rootFolders count = %d, want 2; body: %s", len(lib.RootFolders), body)
	}
	for _, r := range lib.RootFolders {
		if r.ID == "" || r.Path == "" {
			t.Errorf("root folder missing fields: %+v", r)
		}
	}
}

// TestCreateLibraryNormalizesPaths: trailing slashes / "." collapse so an
// otherwise-equal root compares equal.
func TestCreateLibraryNormalizesPaths(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	_, lib, body := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies/./"},
	})
	if len(lib.RootFolders) != 1 {
		t.Fatalf("rootFolders = %+v; body: %s", lib.RootFolders, body)
	}
	if got := lib.RootFolders[0].Path; got != "/data/movies" {
		t.Errorf("normalized path = %q, want /data/movies", got)
	}
}

// TestListAndGetLibrary: list returns created libraries with roots; get-by-id
// returns the same one.
func TestListAndGetLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	_, created, _ := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies"},
	})

	var list librariesListResp
	status, body := srv.AuthGET("/api/v1/libraries", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", status, body)
	}
	if len(list.Libraries) != 1 {
		t.Fatalf("library count = %d, want 1; body: %s", len(list.Libraries), body)
	}
	if list.Libraries[0].ID != created.ID {
		t.Errorf("listed id = %q, want %q", list.Libraries[0].ID, created.ID)
	}
	if len(list.Libraries[0].RootFolders) != 1 {
		t.Errorf("listed roots = %+v, want 1", list.Libraries[0].RootFolders)
	}

	var got libraryResp
	status, body = srv.AuthGET("/api/v1/libraries/"+created.ID, token, &got)
	if status != http.StatusOK {
		t.Fatalf("get status = %d; body: %s", status, body)
	}
	if got.ID != created.ID || got.Name != "Movies" {
		t.Errorf("get = %+v, want id=%s name=Movies", got, created.ID)
	}
}

// TestGetMissingLibrary returns 404 with the standard envelope.
func TestGetMissingLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var env errorEnvelope
	status, body := srv.AuthGET("/api/v1/libraries/no-such-id", token, &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", env.Error.Code)
	}
}

// TestDeleteLibrary removes a Library; a follow-up get is 404.
func TestDeleteLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	_, created, _ := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies"},
	})

	status, body := srv.JSON(http.MethodDelete, "/api/v1/libraries/"+created.ID, token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, body)
	}

	status, _ = srv.AuthGET("/api/v1/libraries/"+created.ID, token, nil)
	if status != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", status)
	}
}

// TestDeleteMissingLibrary returns 404 — consistent with the get-missing posture.
func TestDeleteMissingLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var env errorEnvelope
	status, body := srv.JSON(http.MethodDelete, "/api/v1/libraries/no-such-id", token, nil, &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", env.Error.Code)
	}
}

// TestFolderOverlapRejected: exact, parent, and child overlap with a folder
// already owned by another Library are all rejected.
func TestFolderOverlapRejected(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	if status, _, body := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies"},
	}); status != http.StatusCreated {
		t.Fatalf("seed library status = %d; body: %s", status, body)
	}

	cases := []struct {
		name string
		path string
	}{
		{"exact", "/data/movies"},
		{"exact with trailing slash", "/data/movies/"},
		{"child", "/data/movies/4k"},
		{"parent", "/data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env errorEnvelope
			status, body := srv.JSON(http.MethodPost, "/api/v1/libraries", token, map[string]any{
				"name":        "Other",
				"kind":        "movie",
				"rootFolders": []string{tc.path},
			}, &env)
			if status != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body: %s", status, body)
			}
			if env.Error.Code != "FOLDER_OVERLAP" {
				t.Errorf("code = %q, want FOLDER_OVERLAP; body: %s", env.Error.Code, body)
			}
		})
	}

	// A genuinely sibling path that merely shares a string prefix is allowed.
	if status, _, body := createLibrary(t, srv, token, map[string]any{
		"name":        "Sibling",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies-extra"},
	}); status != http.StatusCreated {
		t.Fatalf("sibling path should be allowed, status = %d; body: %s", status, body)
	}
}

// TestAcceptTVAndMusicKinds: tv and music are now valid kinds (tv-music PRD);
// only a genuinely-unknown kind is BAD_REQUEST. Movie is unchanged.
func TestAcceptTVAndMusicKinds(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, k := range []string{"tv", "music"} {
		var lib libraryResp
		status, body := srv.JSON(http.MethodPost, "/api/v1/libraries", token, map[string]any{
			"name":        "Lib " + k,
			"kind":        k,
			"rootFolders": []string{"/data/" + k},
		}, &lib)
		if status != http.StatusCreated {
			t.Fatalf("create %s status = %d, want 201; body: %s", k, status, body)
		}
		if lib.Kind != k {
			t.Errorf("created kind = %q, want %q", lib.Kind, k)
		}
	}

	// An unknown kind is still rejected.
	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries", token, map[string]any{
		"name":        "Bad",
		"kind":        "photos",
		"rootFolders": []string{"/data/photos"},
	}, &env)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown kind status = %d, want 400; body: %s", status, body)
	}
	if env.Error.Code != "BAD_REQUEST" {
		t.Errorf("code = %q, want BAD_REQUEST", env.Error.Code)
	}
}

// TestRejectEmptyNameAndNoRoots: missing name or empty rootFolders is BAD_REQUEST.
func TestRejectEmptyNameAndNoRoots(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty name", map[string]any{"name": "  ", "kind": "movie", "rootFolders": []string{"/data/movies"}}},
		{"no roots", map[string]any{"name": "Movies", "kind": "movie", "rootFolders": []string{}}},
		{"relative root", map[string]any{"name": "Movies", "kind": "movie", "rootFolders": []string{"relative/path"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env errorEnvelope
			status, body := srv.JSON(http.MethodPost, "/api/v1/libraries", token, tc.body, &env)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}
			if env.Error.Code != "BAD_REQUEST" {
				t.Errorf("code = %q, want BAD_REQUEST; body: %s", env.Error.Code, body)
			}
		})
	}
}

// TestLibraryManagementAdminOnly: Library *management* (POST create) stays
// Admin-only (403 for a Member, 401 unauthenticated). Listing is no longer
// admin-gated — a Member GETs /libraries scoped to their grants (covered by the
// access-control grant tests); with no grants that is an empty 200.
func TestLibraryManagementAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	// Bootstrap the Admin (so setup is closed), then seed a Member directly.
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	srv.CreateMember("member", "memberpass123")
	memberToken := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	// GET /libraries is now scoped, not admin-gated: a Member with no grants sees
	// an empty list (200), not a 403.
	var listed librariesListResp
	status, body := srv.AuthGET("/api/v1/libraries", memberToken, &listed)
	if status != http.StatusOK {
		t.Fatalf("member GET status = %d, want 200; body: %s", status, body)
	}
	if len(listed.Libraries) != 0 {
		t.Errorf("member with no grants saw %d libraries, want 0", len(listed.Libraries))
	}

	// Member cannot create.
	var env errorEnvelope
	status, body = srv.JSON(http.MethodPost, "/api/v1/libraries", memberToken, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{"/data/movies"},
	}, &env)
	if status != http.StatusForbidden {
		t.Fatalf("member POST status = %d, want 403; body: %s", status, body)
	}
	if env.Error.Code != "FORBIDDEN" {
		t.Errorf("member POST code = %q, want FORBIDDEN; body: %s", env.Error.Code, body)
	}

	// Unauthenticated → 401.
	status, _ = srv.AuthGET("/api/v1/libraries", "", &env)
	if status != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", status)
	}
}
