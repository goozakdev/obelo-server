package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for uninstalling a Sign-in provider (ADR-0063 decision 10).
// Uninstalling one deletes every External identity it issued. A User left with
// no sign-in path at all is deleted outright — but only after an Admin has seen
// them listed by name and confirmed exactly that list. A User with another path
// keeps their account, that path and their sessions.

type uninstallPreviewView struct {
	SignInProvider bool `json:"signInProvider"`
	UsersToDelete  []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"usersToDelete"`
}

func uninstallPath(id string) string { return "/api/v1/settings/plugins/" + id }

// uninstallPreview is the confirmation step's question: who would this delete?
func uninstallPreview(t *testing.T, srv *testharness.Server, admin, id string) uninstallPreviewView {
	t.Helper()
	var out uninstallPreviewView
	if status, body := srv.AuthGET(uninstallPath(id)+"/uninstall", admin, &out); status != http.StatusOK {
		t.Fatalf("GET uninstall preview of %s = %d; body: %s", id, status, body)
	}
	return out
}

// previewNames is the preview's Users by name, sorted.
func previewNames(p uninstallPreviewView) []string {
	names := []string{}
	for _, u := range p.UsersToDelete {
		names = append(names, u.Username)
	}
	sort.Strings(names)
	return names
}

// previewIDs is the preview's Users by id, as the confirm request carries them.
func previewIDs(p uninstallPreviewView) []string {
	ids := []string{}
	for _, u := range p.UsersToDelete {
		ids = append(ids, u.ID)
	}
	return ids
}

// uninstallConfirming is the confirm request: DELETE carrying the ids the
// preview listed. A nil list sends no body at all, as a plain uninstall does.
func uninstallConfirming(t *testing.T, srv *testharness.Server, admin, id string, userIDs []string) (int, []byte) {
	t.Helper()
	var in any
	if userIDs != nil {
		in = map[string]any{"deleteUsers": userIDs}
	}
	return srv.JSON(http.MethodDelete, uninstallPath(id), admin, in, nil)
}

func pluginListed(t *testing.T, srv *testharness.Server, admin, id string) bool {
	t.Helper()
	var resp struct {
		Plugins []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"plugins"`
	}
	if status, body := srv.AuthGET("/api/v1/settings/plugins", admin, &resp); status != http.StatusOK {
		t.Fatalf("GET plugins = %d; body: %s", status, body)
	}
	for _, p := range resp.Plugins {
		if p.ID == id && p.State == "" {
			return true
		}
	}
	return false
}

func userExists(t *testing.T, srv *testharness.Server, admin, id string) bool {
	t.Helper()
	status, body := srv.AuthGET("/api/v1/users/"+id, admin, nil)
	switch status {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("GET /users/%s = %d; body: %s", id, status, body)
	return false
}

func sessionWorks(t *testing.T, srv *testharness.Server, token string) bool {
	t.Helper()
	status, _ := srv.AuthGET("/api/v1/devices", token, nil)
	return status == http.StatusOK
}

func wantUnconfirmed(t *testing.T, what string, status int, body []byte) {
	t.Helper()
	if status != http.StatusConflict || body2code(body) != "UNINSTALL_NOT_CONFIRMED" {
		t.Fatalf("%s = %d %s, want 409 UNINSTALL_NOT_CONFIRMED", what, status, body)
	}
}

// TestUninstallingASignInProviderDeletesItsIdentitiesAndRevokesItsOnlyUsers: ada
// signs in only through the directory. Once the Admin confirms, her identity is
// gone, her session no longer authenticates and she is no longer a User; the
// directory is no longer installed, and her credential signs in as nobody.
func TestUninstallingASignInProviderDeletesItsIdentitiesAndRevokesItsOnlyUsers(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::"})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	preview := uninstallPreview(t, srv, admin, "directory")
	if !preview.SignInProvider || !reflect.DeepEqual(previewNames(preview), []string{"ada"}) {
		t.Fatalf("preview = %+v, want a Sign-in provider whose uninstall deletes ada", preview)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s, want 200", status, body)
	}

	if got := srv.ExternalIdentities(ada.User.ID); len(got) != 0 {
		t.Fatalf("ada's identities after the uninstall = %v, want none", got)
	}
	if sessionWorks(t, srv, ada.Token) {
		t.Fatal("ada's session still authenticates after the uninstall")
	}
	if userExists(t, srv, admin, ada.User.ID) {
		t.Fatal("ada is still a User after the uninstall")
	}
	if pluginListed(t, srv, admin, "directory") {
		t.Fatal("the directory is still installed after the uninstall")
	}
	if status, _, body := signInAttempt(t, srv, "ada", "ada-pw"); status != http.StatusUnauthorized {
		t.Fatalf("ada's directory credential after the uninstall = %d %s, want 401", status, body)
	}
}

// TestALocalPasswordUserSurvivesUninstallWithLocalSignInIntact: kid has a Local
// password and attached a directory identity. He is not listed, so a plain
// uninstall goes through with nothing to confirm; kid keeps his account, his
// session and his Local sign-in, and loses only the directory identity.
func TestALocalPasswordUserSurvivesUninstallWithLocalSignInIntact(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "kid-dir:kid-dir-pw:subject-kid::"})
	kidID := srv.CreateUser(admin, "kid", "kidpassword123", "")
	kidToken := srv.LoginAs("kid", "kidpassword123")
	if status, body := attachPassword(t, srv, kidToken, map[string]any{"currentPassword": "kidpassword123"},
		"directory", "kid-dir", "kid-dir-pw"); status != http.StatusOK {
		t.Fatalf("kid attaching the directory identity = %d %s, want 200", status, body)
	}

	if preview := uninstallPreview(t, srv, admin, "directory"); len(preview.UsersToDelete) != 0 {
		t.Fatalf("preview = %+v, want nobody to delete", preview)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", nil); status != http.StatusOK {
		t.Fatalf("uninstall with nobody to delete = %d %s, want 200", status, body)
	}

	if !userExists(t, srv, admin, kidID) {
		t.Fatal("kid was deleted by the uninstall")
	}
	if !sessionWorks(t, srv, kidToken) {
		t.Fatal("kid's session was revoked by the uninstall")
	}
	if got := srv.ExternalIdentities(kidID); len(got) != 0 {
		t.Fatalf("kid's identities after the uninstall = %v, want none", got)
	}
	if tok := srv.LoginAs("kid", "kidpassword123"); tok == "" {
		t.Fatal("kid's Local password no longer signs in")
	}
}

// TestAUserWithNoOtherPathIsNotDeletedWithoutConfirmation: ada is listed by name
// before anything happens. Looking at the list, a plain uninstall, and a confirm
// that lists nobody all leave the directory installed and ada exactly as she was.
func TestAUserWithNoOtherPathIsNotDeletedWithoutConfirmation(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::"})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	for i := 0; i < 2; i++ {
		if got := previewNames(uninstallPreview(t, srv, admin, "directory")); !reflect.DeepEqual(got, []string{"ada"}) {
			t.Fatalf("preview lists %v, want ada", got)
		}
	}
	status, body := uninstallConfirming(t, srv, admin, "directory", nil)
	wantUnconfirmed(t, "a plain uninstall", status, body)
	var env errorEnvelope
	_ = json.Unmarshal(body, &env)
	if listed, _ := json.Marshal(env.Error.Details["usersToDelete"]); !strings.Contains(string(listed), `"ada"`) {
		t.Fatalf("the refusal's details = %v, want ada listed", env.Error.Details)
	}
	status, body = uninstallConfirming(t, srv, admin, "directory", []string{})
	wantUnconfirmed(t, "a confirm listing nobody", status, body)

	if !pluginListed(t, srv, admin, "directory") {
		t.Fatal("the directory was uninstalled without confirmation")
	}
	if !userExists(t, srv, admin, ada.User.ID) || !sessionWorks(t, srv, ada.Token) {
		t.Fatal("ada or her session was touched without confirmation")
	}
	if got := identityKeys(srv, ada.User.ID); !reflect.DeepEqual(got, []string{"directory/subject-ada"}) {
		t.Fatalf("ada's identities = %v, want the directory's still", got)
	}
	if again := login(t, srv, "ada", "ada-pw", "Phone", "test", "ada-phone"); again.User.ID != ada.User.ID {
		t.Fatalf("ada signs in as %q, want still %q", again.User.ID, ada.User.ID)
	}
}

// TestAConfirmedUninstallDeletesTheListedUsersAndTheirWatchState: ada, whose only
// path is the directory, watched a film; kid, who has a Local password, keeps his
// account and Local sign-in. After the confirm ada cannot be resolved by id and
// her watch state is gone with her.
func TestAConfirmedUninstallDeletesTheListedUsersAndTheirWatchState(t *testing.T) {
	requireFixtures(t)
	srv, admin := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::family;kid-dir:kid-dir-pw:subject-kid::",
	})
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "family", "role": "member", "libraryIds": []string{lib}},
	}})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	title := firstTitleID(t, srv, admin, lib)
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+title+"/watchState", ada.Token,
		map[string]any{"watched": true}, nil); status >= 300 {
		t.Fatalf("ada marking a film watched = %d %s", status, body)
	}
	if n := srv.CountWatchStateForUser(ada.User.ID); n != 1 {
		t.Fatalf("ada's watch state rows = %d, want 1", n)
	}
	kidID := srv.CreateUser(admin, "kid", "kidpassword123", "")
	kidToken := srv.LoginAs("kid", "kidpassword123")
	if status, body := attachPassword(t, srv, kidToken, map[string]any{"currentPassword": "kidpassword123"},
		"directory", "kid-dir", "kid-dir-pw"); status != http.StatusOK {
		t.Fatalf("kid attaching = %d %s", status, body)
	}

	preview := uninstallPreview(t, srv, admin, "directory")
	if got := previewNames(preview); !reflect.DeepEqual(got, []string{"ada"}) {
		t.Fatalf("preview lists %v, want only ada", got)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s, want 200", status, body)
	}

	if userExists(t, srv, admin, ada.User.ID) {
		t.Fatal("ada can still be resolved by id")
	}
	if n := srv.CountWatchStateForUser(ada.User.ID); n != 0 {
		t.Fatalf("ada's watch state rows after the uninstall = %d, want 0", n)
	}
	if !userExists(t, srv, admin, kidID) || !sessionWorks(t, srv, kidToken) {
		t.Fatal("kid or his session did not survive the uninstall")
	}
	if tok := srv.LoginAs("kid", "kidpassword123"); tok == "" {
		t.Fatal("kid's Local password no longer signs in")
	}
}

// TestAConfirmThatNoLongerMatchesIsRefusedAndChangesNothing: the Admin was shown
// ada. Before they confirm, bob signs in through the directory for the first
// time, so the uninstall would now delete him too. The confirm naming only ada is
// refused, and so is one naming somebody who would not be deleted; nothing
// changes until a confirm names exactly who would be.
func TestAConfirmThatNoLongerMatchesIsRefusedAndChangesNothing(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::;bob:bob-pw:subject-bob::",
	})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	shown := uninstallPreview(t, srv, admin, "directory")
	bob := login(t, srv, "bob", "bob-pw", "Laptop", "test", "bob-client")

	status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(shown))
	wantUnconfirmed(t, "a confirm of the stale list", status, body)
	status, body = uninstallConfirming(t, srv, admin, "directory",
		[]string{ada.User.ID, bob.User.ID, meID(t, srv, admin, "brandon")})
	wantUnconfirmed(t, "a confirm naming a User who would be kept", status, body)

	if !pluginListed(t, srv, admin, "directory") {
		t.Fatal("a refused confirm uninstalled the directory")
	}
	for _, u := range []loginResp{ada, bob} {
		if !userExists(t, srv, admin, u.User.ID) || !sessionWorks(t, srv, u.Token) ||
			len(srv.ExternalIdentities(u.User.ID)) != 1 {
			t.Fatalf("%s was touched by a refused confirm", u.User.Username)
		}
	}

	now := uninstallPreview(t, srv, admin, "directory")
	if got := previewNames(now); !reflect.DeepEqual(got, []string{"ada", "bob"}) {
		t.Fatalf("preview now lists %v, want ada and bob", got)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(now)); status != http.StatusOK {
		t.Fatalf("the matching confirm = %d %s, want 200", status, body)
	}
	if userExists(t, srv, admin, ada.User.ID) || userExists(t, srv, admin, bob.User.ID) {
		t.Fatal("a confirmed User survived")
	}
}

// TestAnIdentityAtAnotherInstalledProviderIsAPathEvenWhileItIsDisabled: ada also
// holds an identity at backup, which the host has automatically disabled; cy
// also holds one at spare, which the Admin switched off. Neither is listed or
// deleted, and their sessions are left alone; dora, who has only the directory,
// is the one User the uninstall deletes.
func TestAnIdentityAtAnotherInstalledProviderIsAPathEvenWhileItIsDisabled(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory",
		"ada:ada-pw:subject-ada::;cy:cy-pw:subject-cy::;dora:dora-pw:subject-dora::"))
	plugintest.Install(t, dataDir, plugintest.LookupSignInManifest("backup",
		"ada-bk:ada-bk-pw:subject-ada-bk::", plugintest.SignInLookupFetchesABlockedHost))
	plugintest.Install(t, dataDir, plugintest.SignInManifest("spare", "cy-sp:cy-sp-pw:subject-cy-sp::"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)

	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	cy := login(t, srv, "cy", "cy-pw", "Laptop", "test", "cy-client")
	dora := login(t, srv, "dora", "dora-pw", "Laptop", "test", "dora-client")
	for _, a := range []struct {
		who                         loginResp
		login, pw, provider, u, upw string
	}{
		{ada, "ada", "ada-pw", "backup", "ada-bk", "ada-bk-pw"},
		{cy, "cy", "cy-pw", "spare", "cy-sp", "cy-sp-pw"},
	} {
		status, raw, grant := reauthPassword(t, srv, a.who.Token, "directory", a.login, a.pw)
		if status != http.StatusOK {
			t.Fatalf("%s re-auth = %d %s", a.login, status, raw)
		}
		if status, raw := attachPasswordWith(t, srv, a.who.Token, map[string]any{
			"provider": a.provider, "username": a.u, "password": a.upw, "reauthGrant": grant.Grant,
		}); status != http.StatusOK {
			t.Fatalf("%s attaching %s = %d %s", a.login, a.provider, status, raw)
		}
	}
	for i := 0; i < 4; i++ {
		resyncNow(t, srv, admin, "backup")
	}
	if !pluginDisabled(t, srv, admin, "backup") {
		t.Fatal("backup was never disabled by the host")
	}
	if status, body := srv.JSON(http.MethodPost, uninstallPath("spare")+"/disable", admin, nil, nil); status != http.StatusOK {
		t.Fatalf("switching spare off = %d %s", status, body)
	}

	preview := uninstallPreview(t, srv, admin, "directory")
	if got := previewNames(preview); !reflect.DeepEqual(got, []string{"dora"}) {
		t.Fatalf("preview lists %v, want only dora", got)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s, want 200", status, body)
	}

	if userExists(t, srv, admin, dora.User.ID) || sessionWorks(t, srv, dora.Token) {
		t.Fatal("dora or her session survived")
	}
	for _, k := range []struct {
		who  loginResp
		want string
	}{{ada, "backup/subject-ada-bk"}, {cy, "spare/subject-cy-sp"}} {
		if !userExists(t, srv, admin, k.who.User.ID) {
			t.Fatalf("%s was deleted", k.who.User.Username)
		}
		if !sessionWorks(t, srv, k.who.Token) {
			t.Fatalf("%s's session was revoked", k.who.User.Username)
		}
		if got := identityKeys(srv, k.who.User.ID); !reflect.DeepEqual(got, []string{k.want}) {
			t.Fatalf("%s's identities = %v, want only %s", k.who.User.Username, got, k.want)
		}
	}
}

// TestUninstallNeverDeletesALocalPasswordAdmin: brandon, the Local-password
// Admin, attached a directory identity; ada is an Admin only by Group mapping.
// ada is listed and deleted; brandon is never listed, and keeps his session and
// his Local sign-in.
func TestUninstallNeverDeletesALocalPasswordAdmin(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{
		"directory": "bj:bj-pw:subject-bj::;ada:ada-pw:subject-ada::admins",
	})
	brandon := meID(t, srv, admin, "brandon")
	if status, body := attachPassword(t, srv, admin, asBrandon, "directory", "bj", "bj-pw"); status != http.StatusOK {
		t.Fatalf("brandon attaching = %d %s", status, body)
	}
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "admins", "role": "admin"},
	}})
	if ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client"); ada.User.Role != "admin" {
		t.Fatalf("ada = %+v, want an Admin by mapping", ada.User)
	}

	preview := uninstallPreview(t, srv, admin, "directory")
	if got := previewNames(preview); !reflect.DeepEqual(got, []string{"ada"}) {
		t.Fatalf("preview lists %v, want only ada", got)
	}
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s, want 200", status, body)
	}
	if !userExists(t, srv, admin, brandon) || !sessionWorks(t, srv, admin) {
		t.Fatal("brandon or his session did not survive")
	}
	if tok := srv.LoginAs("brandon", brandonPassword); tok == "" {
		t.Fatal("brandon's Local password no longer signs in")
	}
	if names := listUsernames(t, srv, admin); !reflect.DeepEqual(names, []string{"brandon"}) {
		t.Fatalf("users after the uninstall = %v, want only brandon", names)
	}
}

// TestAFailedUninstallChangesNothing: the uninstall is made to fail after it has
// deleted ada, while it is removing kid's identity. Nothing it did stays done:
// ada, her session and her identity are back, kid keeps his identity, and the
// directory is still installed and still signs people in.
func TestAFailedUninstallChangesNothing(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::;kid-dir:kid-dir-pw:subject-kid::",
	})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	kidID := srv.CreateUser(admin, "kid", "kidpassword123", "")
	kidToken := srv.LoginAs("kid", "kidpassword123")
	if status, body := attachPassword(t, srv, kidToken, map[string]any{"currentPassword": "kidpassword123"},
		"directory", "kid-dir", "kid-dir-pw"); status != http.StatusOK {
		t.Fatalf("kid attaching = %d %s", status, body)
	}
	srv.Exec(`CREATE TRIGGER refuse_kid BEFORE DELETE ON external_identities
	          WHEN OLD.subject = 'subject-kid' BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`)

	preview := uninstallPreview(t, srv, admin, "directory")
	status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview))
	if status < 500 {
		t.Fatalf("the failing uninstall = %d %s, want a server error", status, body)
	}

	if !pluginListed(t, srv, admin, "directory") {
		t.Fatal("the directory was uninstalled by a failed uninstall")
	}
	if !userExists(t, srv, admin, ada.User.ID) || !sessionWorks(t, srv, ada.Token) {
		t.Fatal("ada or her session was lost to a failed uninstall")
	}
	if got := identityKeys(srv, ada.User.ID); !reflect.DeepEqual(got, []string{"directory/subject-ada"}) {
		t.Fatalf("ada's identities = %v, want the directory's", got)
	}
	if got := identityKeys(srv, kidID); !reflect.DeepEqual(got, []string{"directory/subject-kid"}) {
		t.Fatalf("kid's identities = %v, want the directory's", got)
	}
	if again := login(t, srv, "ada", "ada-pw", "Phone", "test", "ada-phone"); again.User.ID != ada.User.ID {
		t.Fatalf("ada signs in as %q after the failed uninstall, want %q", again.User.ID, ada.User.ID)
	}
}

// TestAnAutoDisabledSignInProviderDeletesNothingItIssued: a provider the host
// disabled is not an uninstall. ada, whose only path it is, keeps her account,
// her identity and her session, and is flagged.
func TestAnAutoDisabledSignInProviderDeletesNothingItIssued(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.LookupSignInManifest("directory",
		"ada:ada-pw:subject-ada::", plugintest.SignInLookupFetchesABlockedHost))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	for i := 0; i < 4; i++ {
		resyncNow(t, srv, admin, "directory")
	}
	if !pluginDisabled(t, srv, admin, "directory") {
		t.Fatal("the directory was never disabled by the host")
	}
	if !userExists(t, srv, admin, ada.User.ID) || !sessionWorks(t, srv, ada.Token) {
		t.Fatal("ada or her session was lost to an automatic disable")
	}
	if got := identityKeys(srv, ada.User.ID); !reflect.DeepEqual(got, []string{"directory/subject-ada"}) {
		t.Fatalf("ada's identities = %v, want the directory's still", got)
	}
	var raw struct {
		Users []map[string]any `json:"users"`
	}
	srv.AuthGET("/api/v1/users", admin, &raw)
	for _, u := range raw.Users {
		if u["username"] == "ada" && u["noWorkingSignInPath"] != true {
			t.Fatalf("ada = %v, want her flagged", u)
		}
	}
}

// TestUninstallingAPluginThatIsNotASignInProviderIsUnchanged: an event sink has
// no preview to confirm, and a plain uninstall removes it as it always did.
func TestUninstallingAPluginThatIsNotASignInProviderIsUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("example-sink"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)

	if preview := uninstallPreview(t, srv, admin, "example-sink"); preview.SignInProvider || len(preview.UsersToDelete) != 0 {
		t.Fatalf("preview of a sink = %+v, want no Sign-in provider and nobody to delete", preview)
	}
	if status, body := uninstallConfirming(t, srv, admin, "example-sink", nil); status != http.StatusOK {
		t.Fatalf("uninstalling the sink = %d %s, want 200", status, body)
	}
	if pluginListed(t, srv, admin, "example-sink") {
		t.Fatal("the sink is still installed")
	}
}

// TestTheUninstallPreviewIsAdminOnly: a Member can neither see who an uninstall
// would delete nor confirm one.
func TestTheUninstallPreviewIsAdminOnly(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::"})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	if status, _ := srv.AuthGET(uninstallPath("directory")+"/uninstall", ada.Token, nil); status != http.StatusForbidden {
		t.Fatalf("a Member reading the preview = %d, want 403", status)
	}
	if status, _ := uninstallConfirming(t, srv, ada.Token, "directory", []string{ada.User.ID}); status != http.StatusForbidden {
		t.Fatalf("a Member confirming = %d, want 403", status)
	}
	if status, _ := srv.AuthGET(uninstallPath("nobody")+"/uninstall", admin, nil); status != http.StatusNotFound {
		t.Fatalf("the preview of an unknown plugin = %d, want 404", status)
	}
	if !pluginListed(t, srv, admin, "directory") || !userExists(t, srv, admin, ada.User.ID) {
		t.Fatal("a Member's request changed something")
	}
}

// TestAReinstallUnderTheSameIDInheritsNoGroupMapping: the directory's "admins"
// group was mapped to admin. After an uninstall and an upload under the same
// id, nobody has mapped anything for the new Plugin, so ada's first sign-in in
// that group makes her a Member.
func TestAReinstallUnderTheSameIDInheritsNoGroupMapping(t *testing.T) {
	const accounts = "ada:ada-pw:subject-ada::admins"
	srv, admin := signInServer(t, map[string]string{"directory": accounts})
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "admins", "role": "admin"},
	}})
	if ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client"); ada.User.Role != "admin" {
		t.Fatalf("ada = %+v, want an Admin by mapping", ada.User)
	}
	preview := uninstallPreview(t, srv, admin, "directory")
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s, want 200", status, body)
	}

	status, body := uploadPlugin(t, srv, admin,
		plugintest.ManifestJSON(t, plugintest.SignInManifest("directory", accounts)), plugintest.Guest(t))
	if status != http.StatusCreated {
		t.Fatalf("reinstalling the directory = %d %s, want 201", status, body)
	}
	if again := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client"); again.User.Role != "member" {
		t.Fatalf("ada after the reinstall = %+v, want a Member", again.User)
	}
}

// TestUninstallingAPluginThatIsNotASignInProviderIgnoresTheBody: as before
// Sign-in providers had anything to confirm, a sink's uninstall reads no body —
// neither one that is not JSON nor one with fields it has never heard of.
func TestUninstallingAPluginThatIsNotASignInProviderIgnoresTheBody(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SinkManifest("malformed-sink"))
	plugintest.Install(t, dataDir, plugintest.SinkManifest("unknown-field-sink"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)

	req, err := http.NewRequest(http.MethodDelete, srv.URL(uninstallPath("malformed-sink")), strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uninstalling a sink with a body that is not JSON = %d, want 200", resp.StatusCode)
	}
	if status, body := srv.JSON(http.MethodDelete, uninstallPath("unknown-field-sink"), admin,
		map[string]any{"somethingElse": true}, nil); status != http.StatusOK {
		t.Fatalf("uninstalling a sink with an unknown field = %d %s, want 200", status, body)
	}
	for _, id := range []string{"malformed-sink", "unknown-field-sink"} {
		if pluginListed(t, srv, admin, id) {
			t.Fatalf("%s is still installed", id)
		}
	}
}

// TestAnUninstallThatWouldDeleteTheLastAdminIsRefused: ada is an Admin only by
// Group mapping, and the only Admin left. Uninstalling the directory would
// delete her, so even her own confirm of exactly that list is 409 LAST_ADMIN,
// and nothing changes.
func TestAnUninstallThatWouldDeleteTheLastAdminIsRefused(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::admins"})
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{
		{"group": "admins", "role": "admin"},
	}})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if ada.User.Role != "admin" {
		t.Fatalf("ada = %+v, want an Admin by mapping", ada.User)
	}
	srv.Exec(`UPDATE users SET role = 'member' WHERE username = 'brandon'`)

	preview := uninstallPreview(t, srv, ada.Token, "directory")
	status, body := uninstallConfirming(t, srv, ada.Token, "directory", previewIDs(preview))
	if status != http.StatusConflict || body2code(body) != "LAST_ADMIN" {
		t.Fatalf("an uninstall deleting the last Admin = %d %s, want 409 LAST_ADMIN", status, body)
	}
	if !pluginListed(t, srv, ada.Token, "directory") {
		t.Fatal("a refused uninstall removed the directory")
	}
	if !userExists(t, srv, ada.Token, ada.User.ID) || !sessionWorks(t, srv, ada.Token) {
		t.Fatal("ada or her session was lost to a refused uninstall")
	}
}

// TestARedirectSignInWhoseProviderWasUninstalledIsRefused: the provider
// answers the callback, but its Plugin was uninstalled before the sign-in
// could write anything. The callback is the one refusal, and nobody is created.
func TestARedirectSignInWhoseProviderWasUninstalledIsRefused(t *testing.T) {
	srv, admin, idp := oidcServer(t)
	before := listUsernames(t, srv, admin)

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key), userinfo("subject-ada", "ada"))
	srv.MarkSignInProviderUninstalled("oidc")
	status, body, out := finishRedirect(t, srv, run, "code-1")
	wantRefused(t, srv, admin, status, body, out, before)
}

// TestAnAttachWhoseProviderWasUninstalledIsRefused: by either flow, the
// provider answers an attach but its Plugin was uninstalled before the attach
// could write anything. Each is 401 SIGN_IN_REFUSED, as a refused sign-in is,
// and brandon gains no identity.
func TestAnAttachWhoseProviderWasUninstalledIsRefused(t *testing.T) {
	t.Run("password", func(t *testing.T) {
		srv, admin := signInServer(t, map[string]string{"directory": "brandon:dir-pw:subject-bj::"})
		brandon := meID(t, srv, admin, "brandon")
		srv.MarkSignInProviderUninstalled("directory")

		status, body := attachPassword(t, srv, admin, asBrandon, "directory", "brandon", "dir-pw")
		if status != http.StatusUnauthorized || body2code(body) != "SIGN_IN_REFUSED" {
			t.Fatalf("password attach = %d %s, want 401 SIGN_IN_REFUSED", status, body)
		}
		if got := identityKeys(srv, brandon); len(got) != 0 {
			t.Fatalf("brandon's identities = %v, want none", got)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		srv, admin, idp := oidcServer(t)
		brandon := meID(t, srv, admin, "brandon")

		run := startAttach(t, srv, admin, asBrandon, "oidc")
		idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-bj", "brandon"), idp.key), userinfo("subject-bj", "brandon"))
		srv.MarkSignInProviderUninstalled("oidc")
		status, body, _ := finishAttach(t, srv, admin, run, "code-1")
		if status != http.StatusUnauthorized || body2code(body) != "SIGN_IN_REFUSED" {
			t.Fatalf("redirect attach = %d %s, want 401 SIGN_IN_REFUSED", status, body)
		}
		if got := identityKeys(srv, brandon); len(got) != 0 {
			t.Fatalf("brandon's identities = %v, want none", got)
		}
	})
}
