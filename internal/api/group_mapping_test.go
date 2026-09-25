package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for Group mapping (ADR-0063 decisions 4, 5 and 9): the
// Admin's mapping per Sign-in provider, the sync a sign-in and a "re-sync now"
// run, the lockout guard, and the Users page flag for a User left with no
// working way in. Everything asserted is what a client can see.

type groupMappingView struct {
	Rules []struct {
		Group      string   `json:"group"`
		Role       string   `json:"role"`
		LibraryIDs []string `json:"libraryIds"`
	} `json:"rules"`
	Recheck         bool `json:"recheck"`
	IntervalHours   int  `json:"intervalHours"`
	DefaultInterval bool `json:"defaultInterval"`
	Failing         *struct {
		Reason string `json:"reason"`
	} `json:"failing"`
}

type resyncView struct {
	Checked  int `json:"checked"`
	Remapped int `json:"remapped"`
	Failed   int `json:"failed"`
	Revoked  int `json:"revoked"`
}

func groupMappingPath(id string) string {
	return "/api/v1/settings/sign-in-providers/" + id + "/group-mapping"
}

func putGroupMapping(t *testing.T, srv *testharness.Server, admin, id string, body map[string]any) groupMappingView {
	t.Helper()
	var view groupMappingView
	status, raw := srv.JSON(http.MethodPut, groupMappingPath(id), admin, body, &view)
	if status != http.StatusOK {
		t.Fatalf("PUT %s group mapping = %d, want 200; body: %s", id, status, raw)
	}
	return view
}

func resyncNow(t *testing.T, srv *testharness.Server, admin, id string) resyncView {
	t.Helper()
	var out resyncView
	status, raw := srv.JSON(http.MethodPost, "/api/v1/settings/sign-in-providers/"+id+"/resync", admin, nil, &out)
	if status != http.StatusOK {
		t.Fatalf("POST %s resync = %d, want 200; body: %s", id, status, raw)
	}
	return out
}

func userDetail(t *testing.T, srv *testharness.Server, admin, id string) signInUserDetail {
	t.Helper()
	var d signInUserDetail
	if status, body := srv.AuthGET("/api/v1/users/"+id, admin, &d); status != http.StatusOK {
		t.Fatalf("GET /users/%s = %d; body: %s", id, status, body)
	}
	if d.LibraryIDs == nil {
		d.LibraryIDs = []string{}
	}
	return d
}

// TestAGroupMappingIsSavedReadBackAndCheckedByTheServer: a provider starts
// with no mapping and the 24-hour interval. The Admin's mapping and interval
// read back as saved; a malformed one is refused whole; an unknown provider is
// not found; a Member may not see any of it.
func TestAGroupMappingIsSavedReadBackAndCheckedByTheServer(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::family"})
	lib := createMovieLibrary(t, srv, admin, t.TempDir())

	var view groupMappingView
	if status, body := srv.AuthGET(groupMappingPath("directory"), admin, &view); status != http.StatusOK {
		t.Fatalf("GET group mapping = %d; body: %s", status, body)
	}
	if len(view.Rules) != 0 || view.Recheck || view.IntervalHours != 24 || !view.DefaultInterval {
		t.Fatalf("a fresh provider's mapping = %+v, want no rules, no re-check, the 24-hour default", view)
	}

	view = putGroupMapping(t, srv, admin, "directory", map[string]any{
		"rules": []map[string]any{
			{"group": "family", "role": "member", "libraryIds": []string{lib}},
			{"group": "admins", "role": "admin"},
		},
		"intervalHours": 6,
	})
	if len(view.Rules) != 2 || view.Rules[0].Group != "admins" || view.Rules[0].Role != "admin" ||
		view.Rules[1].Group != "family" || !reflect.DeepEqual(view.Rules[1].LibraryIDs, []string{lib}) ||
		view.IntervalHours != 6 || view.DefaultInterval {
		t.Fatalf("saved mapping = %+v, want admins and family, every 6 hours", view)
	}

	for name, body := range map[string]map[string]any{
		"an unknown role":      {"rules": []map[string]any{{"group": "x", "role": "owner"}}},
		"a group twice":        {"rules": []map[string]any{{"group": "x", "role": "member"}, {"group": "x", "role": "admin"}}},
		"no group":             {"rules": []map[string]any{{"group": " ", "role": "member"}}},
		"an unknown library":   {"rules": []map[string]any{{"group": "x", "role": "member", "libraryIds": []string{"nope"}}}},
		"too long an interval": {"rules": []map[string]any{}, "intervalHours": 10000},
	} {
		if status, raw := srv.JSON(http.MethodPut, groupMappingPath("directory"), admin, body, nil); status != http.StatusBadRequest {
			t.Errorf("PUT with %s = %d, want 400; body: %s", name, status, raw)
		}
	}
	if status, body := srv.AuthGET(groupMappingPath("directory"), admin, &view); status != http.StatusOK || len(view.Rules) != 2 {
		t.Fatalf("after the refused PUTs the mapping = %+v (%d %s), want the saved two rules", view, status, body)
	}
	if status, _ := srv.AuthGET(groupMappingPath("nobody"), admin, nil); status != http.StatusNotFound {
		t.Fatalf("GET an unknown provider's mapping = %d, want 404", status)
	}
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if status, _ := srv.AuthGET(groupMappingPath("directory"), ada.Token, nil); status != http.StatusForbidden {
		t.Fatalf("a Member reading a mapping = %d, want 403", status)
	}
}

// TestASignInSyncsTheGroupMapping: the directory knows one person, subject-ada,
// under two passwords — one answering her in "family", one in "admins". Each
// sign-in answers with the role and grants its groups map to.
func TestASignInSyncsTheGroupMapping(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::family;ada:ada-admin-pw:subject-ada::admins",
	})
	lib := createMovieLibrary(t, srv, admin, t.TempDir())
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "family", "role": "member", "libraryIds": []string{lib}},
		{"group": "admins", "role": "admin"},
	}})

	first := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if d := userDetail(t, srv, admin, first.User.ID); first.User.Role != "member" || !reflect.DeepEqual(d.LibraryIDs, []string{lib}) {
		t.Fatalf("in family: role %q, grants %v; want a member granted the library", first.User.Role, d.LibraryIDs)
	}
	second := login(t, srv, "ada", "ada-admin-pw", "Laptop", "test", "ada-client")
	if second.User.ID != first.User.ID || second.User.Role != "admin" {
		t.Fatalf("in admins: %+v, want the same User, now an admin", second.User)
	}
	third := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if third.User.Role != "member" {
		t.Fatalf("back in family: role %q, want member", third.User.Role)
	}
}

// TestTheLastAdminWithALocalPasswordCannotBeDeletedByAMappedAdmin: ada is an
// Admin by mapping and has no Local password. Deleting brandon, the one Admin
// who has one, is refused; brandon deleting ada goes through.
func TestTheLastAdminWithALocalPasswordCannotBeDeletedByAMappedAdmin(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::admins"})
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "admins", "role": "admin"},
	}})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if ada.User.Role != "admin" {
		t.Fatalf("ada = %+v, want an admin by mapping", ada.User)
	}
	var brandon string
	var users usersListResp
	srv.AuthGET("/api/v1/users", admin, &users)
	for _, u := range users.Users {
		if u.Username == "brandon" {
			brandon = u.ID
		}
	}

	status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+brandon, ada.Token, nil, nil)
	if status != http.StatusConflict || !strings.Contains(string(body), "LAST_ADMIN") {
		t.Fatalf("ada deleting brandon = %d %s, want 409 LAST_ADMIN", status, body)
	}
	if status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+ada.User.ID, admin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("brandon deleting ada = %d %s, want 204", status, body)
	}
}

// TestReSyncNowRemapsAProviderWithoutLookup: the directory declares no lookup,
// so nothing asks it between sign-ins. The Admin maps ada's group after she
// signed in; "re-sync now" applies it from the groups she last signed in with.
func TestReSyncNowRemapsAProviderWithoutLookup(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::family"})
	lib := createMovieLibrary(t, srv, admin, t.TempDir())
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "family", "role": "member", "libraryIds": []string{lib}},
	}})
	if d := userDetail(t, srv, admin, ada.User.ID); len(d.LibraryIDs) != 0 {
		t.Fatalf("before re-sync now ada holds %v, want nothing", d.LibraryIDs)
	}

	if res := resyncNow(t, srv, admin, "directory"); res.Remapped != 1 || res.Checked != 0 {
		t.Fatalf("re-sync now = %+v, want one identity re-mapped and none asked", res)
	}
	if d := userDetail(t, srv, admin, ada.User.ID); !reflect.DeepEqual(d.LibraryIDs, []string{lib}) {
		t.Fatalf("after re-sync now ada holds %v, want the library", d.LibraryIDs)
	}
}

// TestReSyncNowAsksALookupProviderAndRevokesAGoneIdentity: the directory
// declares lookup. "Re-sync now" asks it at once and applies the groups it
// answers; once it says ada is gone, her session is revoked.
func TestReSyncNowAsksALookupProviderAndRevokesAGoneIdentity(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.LookupSignInManifest("directory",
		"ada:ada-pw:subject-ada::", "subject-ada:active:family"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, t.TempDir())
	view := putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "family", "role": "member", "libraryIds": []string{lib}},
	}})
	if !view.Recheck {
		t.Fatalf("a provider declaring lookup = %+v, want recheck", view)
	}
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	if res := resyncNow(t, srv, admin, "directory"); res.Checked != 1 || res.Failed != 0 {
		t.Fatalf("re-sync now = %+v, want one identity asked and answered", res)
	}
	if d := userDetail(t, srv, admin, ada.User.ID); !reflect.DeepEqual(d.LibraryIDs, []string{lib}) {
		t.Fatalf("after the lookup ada holds %v, want the library", d.LibraryIDs)
	}

	if status, body := saveDeclaredSettings(t, srv, admin, "directory", map[string]any{"lookup": "subject-ada:gone:"}); status != http.StatusOK {
		t.Fatalf("saving the lookup directory = %d; body: %s", status, body)
	}
	if res := resyncNow(t, srv, admin, "directory"); res.Revoked != 1 {
		t.Fatalf("re-sync now of a gone identity = %+v, want one User revoked", res)
	}
	if status, _ := srv.AuthGET("/api/v1/auth/external-identities", ada.Token, nil); status != http.StatusUnauthorized {
		t.Fatalf("ada's session after she is gone = %d, want 401", status)
	}
}

// TestARefreshingProviderIsReCheckedWithItsRefreshToken: a redirect provider
// declaring refresh hands back a refresh token at sign-in; "re-sync now"
// redeems it and applies the groups the refresh answers.
func TestARefreshingProviderIsReCheckedWithItsRefreshToken(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RefreshingRedirectSignInManifest("oauth",
		"https://oauth.example.test/authorize", "subject-octavia:active:crew"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, t.TempDir())
	putGroupMapping(t, srv, admin, "oauth", map[string]any{"rules": []map[string]any{
		{"group": "crew", "role": "member", "libraryIds": []string{lib}},
	}})

	status, body, out := finishRedirect(t, srv, startRedirect(t, srv, "oauth"), "refresh|subject-octavia|octavia|")
	if status != http.StatusOK {
		t.Fatalf("callback = %d; body: %s", status, body)
	}
	if d := userDetail(t, srv, admin, out.User.ID); len(d.LibraryIDs) != 0 {
		t.Fatalf("signed in in no group, octavia holds %v", d.LibraryIDs)
	}
	if res := resyncNow(t, srv, admin, "oauth"); res.Checked != 1 || res.Failed != 0 {
		t.Fatalf("re-sync now = %+v, want one identity refreshed", res)
	}
	if d := userDetail(t, srv, admin, out.User.ID); !reflect.DeepEqual(d.LibraryIDs, []string{lib}) {
		t.Fatalf("after the refresh octavia holds %v, want the library", d.LibraryIDs)
	}
}

// TestAnAutoDisabledSignInProviderKeepsSessionsAndFlagsItsUsers: the directory
// breaks its allowlist on every re-check until the host disables it. ada, who
// signs in only through it, keeps her session and is flagged on the Users list
// as having no working sign-in path; brandon, with a Local password, is not.
// The provider is flagged on its mapping from the first failure.
func TestAnAutoDisabledSignInProviderKeepsSessionsAndFlagsItsUsers(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.LookupSignInManifest("directory",
		"ada:ada-pw:subject-ada::", plugintest.SignInLookupFetchesABlockedHost))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	resyncNow(t, srv, admin, "directory")
	var view groupMappingView
	srv.AuthGET(groupMappingPath("directory"), admin, &view)
	if view.Failing == nil {
		t.Fatalf("after a failed re-check the mapping = %+v, want the provider flagged", view)
	}
	for i := 0; i < 3; i++ {
		resyncNow(t, srv, admin, "directory")
	}
	if !pluginDisabled(t, srv, admin, "directory") {
		t.Fatal("the directory was never disabled by the host")
	}

	if status, body := srv.AuthGET("/api/v1/auth/external-identities", ada.Token, nil); status != http.StatusOK {
		t.Fatalf("ada's session with the provider disabled = %d %s, want 200", status, body)
	}
	var raw struct {
		Users []map[string]any `json:"users"`
	}
	if status, body := srv.AuthGET("/api/v1/users", admin, &raw); status != http.StatusOK {
		t.Fatalf("GET /users = %d; body: %s", status, body)
	}
	flagged := map[string]bool{}
	for _, u := range raw.Users {
		flagged[u["username"].(string)] = u["noWorkingSignInPath"] == true
	}
	if !flagged["ada"] || flagged["brandon"] {
		t.Fatalf("flags = %v, want ada flagged and brandon not", flagged)
	}
}

func pluginDisabled(t *testing.T, srv *testharness.Server, admin, id string) bool {
	t.Helper()
	var resp struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	if status, body := srv.AuthGET("/api/v1/settings/plugins", admin, &resp); status != http.StatusOK {
		t.Fatalf("GET plugins = %d; body: %s", status, body)
	}
	for _, raw := range resp.Plugins {
		var p struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabledByFailure"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.ID == id {
			return p.Disabled
		}
	}
	t.Fatalf("no plugin %q listed", id)
	return false
}
