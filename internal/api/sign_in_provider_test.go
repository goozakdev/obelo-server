package api_test

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Sign-in provider Extension point's password flow: one
// or two Installed Sign-in providers placed under <dataDir>/plugins/<id>/, and
// the ordinary POST /auth/login form.
//
// The guest is the suite's own module. Its "directory" is the `accounts` setting
// its manifest declares a default for — `login:password:subject:name:groups`
// entries — so each test says in one string who each directory knows. Everything
// asserted is what a client can see.

// signInServer boots a server with the given Installed Sign-in providers
// (id -> accounts) and a first Admin, brandon, who holds a Local password. It
// returns the server and brandon's token.
func signInServer(t *testing.T, directories map[string]string) (*testharness.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	for id, accounts := range directories {
		plugintest.Install(t, dataDir, plugintest.SignInManifest(id, accounts))
	}
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	return srv, adminToken(t, srv)
}

// rawLogin posts the login form and returns the status, the Content-Type and the
// body exactly as a client receives them.
func signInAttempt(t *testing.T, srv *testharness.Server, username, password string) (int, string, []byte) {
	t.Helper()
	status, header, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/login", "", "", nil, map[string]any{
		"username": username,
		"password": password,
		"device": map[string]any{
			"name":     "Laptop",
			"platform": "test",
			"clientId": "sign-in-client",
		},
	}, nil)
	return status, header.Get("Content-Type"), body
}

type signInUserDetail struct {
	ID         string   `json:"id"`
	Username   string   `json:"username"`
	Role       string   `json:"role"`
	LibraryIDs []string `json:"libraryIds"`
}

func listUsernames(t *testing.T, srv *testharness.Server, admin string) []string {
	t.Helper()
	var users usersListResp
	status, body := srv.AuthGET("/api/v1/users", admin, &users)
	if status != http.StatusOK {
		t.Fatalf("GET /users status = %d; body: %s", status, body)
	}
	var names []string
	for _, u := range users.Users {
		names = append(names, u.Username)
	}
	return names
}

func setSignInOrder(t *testing.T, srv *testharness.Server, admin string, ids ...string) {
	t.Helper()
	status, body := srv.JSON(http.MethodPut, "/api/v1/settings/sign-in-providers", admin,
		map[string]any{"order": ids}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT sign-in order %v: status %d; body: %s", ids, status, body)
	}
}

// TestAFirstTimeSignInProviderIdentityBecomesAMemberGrantedNothing: nobody named
// ada exists here; the directory accepts her. She is signed in as a NEW Member,
// and that Member holds no Library even though one exists.
func TestAFirstTimeSignInProviderIdentityBecomesAMemberGrantedNothing(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::family,media"})
	createMovieLibrary(t, srv, admin, t.TempDir())

	var out loginResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "ada", "password": "ada-pw",
		"device": map[string]any{"name": "Laptop", "platform": "test", "clientId": "ada-client"},
	}, &out)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", status, body)
	}
	if out.Token == "" || out.User.Username != "ada" || out.User.Role != "member" {
		t.Fatalf("login = %+v, want a token for a new member named ada", out)
	}

	var detail signInUserDetail
	if status, body := srv.AuthGET("/api/v1/users/"+out.User.ID, admin, &detail); status != http.StatusOK {
		t.Fatalf("GET /users/{id} status = %d; body: %s", status, body)
	}
	if detail.Role != "member" || len(detail.LibraryIDs) != 0 {
		t.Fatalf("the new User = %+v, want a member granted no library", detail)
	}
	var libs librariesListResp
	if status, body := srv.AuthGET("/api/v1/libraries", out.Token, &libs); status != http.StatusOK || len(libs.Libraries) != 0 {
		t.Fatalf("the new member sees libraries %+v (status %d, body %s), want none", libs, status, body)
	}
}

// TestAReturningIdentityResolvesBySubjectAfterARename: the directory knows one
// person, subject-ada, under two logins — before and after a rename at the
// source. Both sign in as the SAME User, who keeps the name she was created with.
func TestAReturningIdentityResolvesBySubjectAfterARename(t *testing.T) {
	t.Parallel()
	srv, _ := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::;ada-lovelace:ada-pw:subject-ada::",
	})

	first := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	second := login(t, srv, "ada-lovelace", "ada-pw", "Laptop", "test", "ada-client")
	if second.User.ID != first.User.ID {
		t.Fatalf("after a rename at the source the identity signed in as user %q, want the same user %q",
			second.User.ID, first.User.ID)
	}
	if second.User.Username != "ada" {
		t.Fatalf("username after the rename = %q, want the name the User was created with, ada",
			second.User.Username)
	}
}

// TestSignInRefusalsAreIndistinguishable: a wrong Local password, a username the
// directory knows given the wrong password (every provider rejecting), and a
// username nobody knows produce the SAME response, byte for byte. The one the
// directory accepts for the first time is not a refusal at all.
func TestSignInRefusalsAreIndistinguishable(t *testing.T) {
	t.Parallel()
	srv, _ := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::"})

	type answer struct {
		status      int
		contentType string
		body        []byte
	}
	refusals := map[string]answer{}
	for name, creds := range map[string][2]string{
		"wrong local password":     {"brandon", "not-brandons-password"},
		"every provider rejecting": {"ada", "not-adas-password"},
		"unknown username":         {"nobody-at-all", "whatever-password"},
	} {
		status, ct, body := signInAttempt(t, srv, creds[0], creds[1])
		refusals[name] = answer{status, ct, body}
	}
	want := refusals["wrong local password"]
	if want.status != http.StatusUnauthorized {
		t.Fatalf("wrong local password: status %d, want 401; body: %s", want.status, want.body)
	}
	for name, got := range refusals {
		if got.status != want.status || got.contentType != want.contentType || !bytes.Equal(got.body, want.body) {
			t.Errorf("%s answered %d %q %s; want exactly the wrong-local-password answer %d %q %s",
				name, got.status, got.contentType, got.body, want.status, want.contentType, want.body)
		}
	}

	if status, _, body := signInAttempt(t, srv, "ada", "ada-pw"); status != http.StatusOK {
		t.Fatalf("an identity the directory accepts for the first time: status %d, want 200; body: %s", status, body)
	}
}

// TestAUsernameCollisionAfterAcceptGetsItsOwnMessage: the directory accepts a
// person whose name at the source is brandon — which is already the local Admin.
// The answer is its own, points at attaching from the profile or asking an Admin,
// and changes nothing: no new User, no link, and the same answer again next time.
func TestAUsernameCollisionAfterAcceptGetsItsOwnMessage(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{"directory": "brandon:directory-pw:subject-brandon::"})
	_, _, refusal := signInAttempt(t, srv, "nobody-at-all", "whatever-password")

	for attempt := 1; attempt <= 2; attempt++ {
		status, _, body := signInAttempt(t, srv, "brandon", "directory-pw")
		if status != http.StatusConflict {
			t.Fatalf("attempt %d: collision status = %d, want 409; body: %s", attempt, status, body)
		}
		if bytes.Equal(body, refusal) {
			t.Fatalf("attempt %d: the collision answered the refusal body %s; it must have its own", attempt, body)
		}
		msg := strings.ToLower(string(body))
		if !strings.Contains(body2code(body), "SIGN_IN_USERNAME_TAKEN") ||
			!strings.Contains(msg, "profile") || !strings.Contains(msg, "admin") {
			t.Fatalf("attempt %d: collision body = %s, want SIGN_IN_USERNAME_TAKEN pointing at the profile and an Admin",
				attempt, body)
		}
	}
	if names := listUsernames(t, srv, admin); len(names) != 1 || names[0] != "brandon" {
		t.Fatalf("users after the collision = %v, want only brandon", names)
	}
	// Nothing was linked: brandon's own Local password still signs in as the Admin,
	// and the directory's credential is still refused as a collision (checked above,
	// twice) rather than resolving to him.
	if got := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "test", "b-client"); got.User.Role != "admin" {
		t.Fatalf("brandon's local sign-in = %+v, want the admin", got.User)
	}
}

func body2code(body []byte) string {
	i := bytes.Index(body, []byte(`"code":"`))
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"code":"`):]
	if j := bytes.IndexByte(rest, '"'); j >= 0 {
		return string(rest[:j])
	}
	return ""
}

// TestTheSecondProviderInAdminOrderAcceptsWhenTheFirstRejects: directory A knows
// ada with a different password; directory B knows her with this one. In Admin
// order A then B, the sign-in succeeds on B's answer — the name B reports.
func TestTheSecondProviderInAdminOrderAcceptsWhenTheFirstRejects(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{
		"dir-a": "ada:a-password:subject-a::",
		"dir-b": "ada:b-password:subject-b:ada-from-b:",
	})
	setSignInOrder(t, srv, admin, "dir-a", "dir-b")

	got := login(t, srv, "ada", "b-password", "Laptop", "test", "ada-client")
	if got.User.Username != "ada-from-b" {
		t.Fatalf("signed in as %+v, want the member B's answer names, ada-from-b", got.User)
	}
}

// TestTheFirstAcceptingProviderInAdminOrderWins: both directories accept the
// same credential, naming the person differently. The Admin's order — the
// REVERSE of the order the plugins were loaded in — decides whose answer wins.
func TestTheFirstAcceptingProviderInAdminOrderWins(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{
		"dir-a": "ada:same-password:subject-a:ada-from-a:",
		"dir-b": "ada:same-password:subject-b:ada-from-b:",
	})

	var listed struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	setSignInOrder(t, srv, admin, "dir-b", "dir-a")
	if status, body := srv.AuthGET("/api/v1/settings/sign-in-providers", admin, &listed); status != http.StatusOK ||
		len(listed.Providers) != 2 || listed.Providers[0].ID != "dir-b" || listed.Providers[1].ID != "dir-a" {
		t.Fatalf("GET sign-in providers = %+v (status %d, body %s), want dir-b then dir-a", listed, status, body)
	}

	got := login(t, srv, "ada", "same-password", "Laptop", "test", "ada-client")
	if got.User.Username != "ada-from-b" {
		t.Fatalf("signed in as %+v, want the member dir-b names (first in Admin order)", got.User)
	}
}

// TestSignInProviderOrderIsAdminOnly: a Member cannot read or set the order, and
// an order naming a provider this server does not have is refused.
func TestSignInProviderOrderIsAdminOnly(t *testing.T) {
	t.Parallel()
	srv, admin := signInServer(t, map[string]string{"dir-a": "", "dir-b": ""})
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")

	if status, body := srv.AuthGET("/api/v1/settings/sign-in-providers", member, nil); status != http.StatusForbidden {
		t.Fatalf("member GET: status %d, want 403; body: %s", status, body)
	}
	if status, body := srv.JSON(http.MethodPut, "/api/v1/settings/sign-in-providers", admin,
		map[string]any{"order": []string{"dir-a", "not-installed"}}, nil); status != http.StatusBadRequest {
		t.Fatalf("an order naming an unknown provider: status %d, want 400; body: %s", status, body)
	}
}

// lockedBuffer is a log destination the server's goroutines may write while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAFailingSignInProviderNeverExposesThePassword: the directory's guest logs
// the password it is handed and fails with it in its error. After logins that
// reach it, the password is nowhere an Admin or an operator can read: not in the
// Admin plugins list, not in any log line. The login itself is an ordinary
// refusal.
func TestAFailingSignInProviderNeverExposesThePassword(t *testing.T) {
	// Not parallel: it captures the process-wide log output.
	logged := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	const password = "s3cret-Correct-Horse"
	srv, admin := signInServer(t, map[string]string{"directory": plugintest.SignInFailsWithThePassword})
	for i := 0; i < 2; i++ {
		if status, _, body := signInAttempt(t, srv, "ada", password); status != http.StatusUnauthorized {
			t.Fatalf("login %d status = %d, want 401; body: %s", i, status, body)
		}
	}

	status, body := srv.AuthGET("/api/v1/settings/plugins", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /settings/plugins status = %d; body: %s", status, body)
	}
	if !strings.Contains(string(body), "lastError") {
		t.Errorf("the plugins list records no last error for the failing provider: %s", body)
	}
	if strings.Contains(string(body), password) {
		t.Errorf("the Admin plugins list carries the password: %s", body)
	}
	if strings.Contains(logged.String(), password) {
		t.Errorf("a log line carries the password:\n%s", logged.String())
	}
}

// TestAPasswordThatIsAHostWordLeavesTheAdminListAlone: a password that is a word
// of the host's own sentences — "password", "the", the provider's id — must not be
// revealed by a "[redacted]" standing in for that word in the Admin plugins list
// or in any log line.
func TestAPasswordThatIsAHostWordLeavesTheAdminListAlone(t *testing.T) {
	// Not parallel: it captures the process-wide log output.
	for _, password := range []string{"password", "the", "directory"} {
		t.Run(password, func(t *testing.T) {
			logged := &lockedBuffer{}
			prev := log.Writer()
			log.SetOutput(logged)
			t.Cleanup(func() { log.SetOutput(prev) })

			srv, admin := signInServer(t, map[string]string{"directory": plugintest.SignInFailsWithThePassword})
			if status, _, body := signInAttempt(t, srv, "ada", password); status != http.StatusUnauthorized {
				t.Fatalf("login status = %d, want 401; body: %s", status, body)
			}
			status, body := srv.AuthGET("/api/v1/settings/plugins", admin, nil)
			if status != http.StatusOK {
				t.Fatalf("GET /settings/plugins status = %d; body: %s", status, body)
			}
			if !strings.Contains(string(body), "lastError") {
				t.Errorf("the plugins list records no last error for the failing provider: %s", body)
			}
			if strings.Contains(string(body), "[redacted]") {
				t.Errorf("the Admin plugins list shows host text redacted: %s", body)
			}
			for _, line := range strings.Split(logged.String(), "\n") {
				if strings.Contains(line, "[redacted]") {
					t.Errorf("a host log line was redacted: %q", line)
				}
			}
		})
	}
}

// TestASignInCallShowsTheAdminNoTextOfItsOwn: a directory that puts the password
// in the name of a host it fetches, in a log line, or in its own error. After a
// login reaches it, neither the Admin plugins list nor any log line carries the
// password or the host, line or error it was put in.
func TestASignInCallShowsTheAdminNoTextOfItsOwn(t *testing.T) {
	// Not parallel: it captures the process-wide log output.
	const password = "s3cretcorrecthorse"
	for _, tc := range []struct {
		name      string
		accounts  string
		forbidden []string
	}{
		{"host named after the password", plugintest.SignInFetchesThePasswordHost, []string{"example.test"}},
		{"log line", plugintest.SignInLogsThePassword, []string{"the password is"}},
		{"failed call", plugintest.SignInFailsWithThePassword, []string{"rejected", "checking"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &lockedBuffer{}
			prev := log.Writer()
			log.SetOutput(logged)
			t.Cleanup(func() { log.SetOutput(prev) })

			srv, admin := signInServer(t, map[string]string{"directory": tc.accounts})
			if status, _, body := signInAttempt(t, srv, "ada", password); status != http.StatusUnauthorized {
				t.Fatalf("login status = %d, want 401; body: %s", status, body)
			}
			status, body := srv.AuthGET("/api/v1/settings/plugins", admin, nil)
			if status != http.StatusOK {
				t.Fatalf("GET /settings/plugins status = %d; body: %s", status, body)
			}
			for _, text := range append([]string{password}, tc.forbidden...) {
				if strings.Contains(string(body), text) {
					t.Errorf("the Admin plugins list carries %q: %s", text, body)
				}
				if strings.Contains(logged.String(), text) {
					t.Errorf("a log line carries %q:\n%s", text, logged.String())
				}
			}
		})
	}
}
