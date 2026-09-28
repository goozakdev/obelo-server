package plugins_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestAnInstalledSignInProviderRegistersAndAnswers is the loader-level tracer:
// a manifest declaring sign-in-provider with the password flow reaches the
// registry with its capability, and the call crosses the sandbox both ways — an
// acceptance carrying the identity, and a rejection carrying none.
func TestAnInstalledSignInProviderRegistersAndAnswers(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	const accounts = "ada:pw:subject-ada:ada.l:family,media"
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", accounts))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"accounts": accounts})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)

	registration, ok := reg.SignInProvider("directory")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for directory")
	}
	d := registration.Descriptor
	if d.ExtensionPoint != pluginapi.ExtensionSignInProvider || !d.HasCapability(pluginapi.CapabilityPasswordSignIn) {
		t.Fatalf("descriptor = %+v, want a sign-in-provider declaring the password flow", d)
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	resp, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
	if err != nil {
		t.Fatalf("CheckPassword: %v", err)
	}
	if !resp.Accepted || resp.Identity == nil || resp.Identity.Subject != "subject-ada" ||
		resp.Identity.Username != "ada.l" || len(resp.Identity.Groups) != 2 {
		t.Fatalf("accepted answer = %+v (identity %+v), want subject-ada named ada.l in two groups", resp, resp.Identity)
	}

	resp, err = provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "wrong"})
	if err != nil {
		t.Fatalf("CheckPassword: %v", err)
	}
	if resp.Accepted || resp.Identity != nil {
		t.Fatalf("a wrong password answered %+v, want a rejection with no identity", resp)
	}
}

// TestAFailingSignInCallNeverRecordsThePassword: a guest that logs the password it
// was handed and fails the call with it in its error. The password appears in
// neither the Plugin's last error, the error the caller gets, nor any log line.
func TestAFailingSignInCallNeverRecordsThePassword(t *testing.T) {
	plugins.Parallel(t)
	const password = "s3cret-Correct-Horse"
	provider, set, log := failingSignInProvider(t)

	_, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: password})
	if err == nil {
		t.Fatal("CheckPassword answered no error from a guest that failed the call")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the caller's error carries the password: %q", err)
	}
	st, _ := set.Status("directory")
	if st.LastError == "" {
		t.Error("the failed call left no last error; it must still be recorded")
	}
	if strings.Contains(st.LastError, password) {
		t.Errorf("lastError carries the password: %q", st.LastError)
	}
	if strings.Contains(log.all(), password) {
		t.Errorf("a log line carries the password:\n%s", log.all())
	}
}

// TestFailingSignInCallsNeverDisableTheProvider: anyone can make a login, so a
// failing sign-in call must not be a strike — more failures than the threshold
// leave the provider enabled, while the failure is still recorded.
func TestFailingSignInCallsNeverDisableTheProvider(t *testing.T) {
	plugins.Parallel(t)
	provider, set, _ := failingSignInProvider(t)

	for i := 0; i < 2*plugins.DefaultFailureThreshold; i++ {
		_, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
		if err == nil {
			t.Fatalf("call %d answered no error from a guest that failed the call", i)
		}
	}
	st, _ := set.Status("directory")
	if st.Disabled {
		t.Fatalf("the provider was disabled after %d failed sign-in calls (%q); a login must not be able to disable it",
			2*plugins.DefaultFailureThreshold, st.LastError)
	}
	if st.LastError == "" {
		t.Error("the failed calls left no last error; they must still be recorded")
	}
}

// TestAPasswordThatIsAHostWordLeavesHostTextAlone: nothing is redacted — the host
// records only its own words. A password that is a word of the host's own
// sentences — "password", "the", the plugin id — must not be revealed by a
// "[redacted]" standing where that word should be, in the last error or in any
// log line; the guest's own log line is withheld whole, as one fixed line.
func TestAPasswordThatIsAHostWordLeavesHostTextAlone(t *testing.T) {
	plugins.Parallel(t)
	for _, password := range []string{"password", "the", "directory"} {
		t.Run(password, func(t *testing.T) {
			provider, set, log := failingSignInProvider(t)
			if _, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: password}); err == nil {
				t.Fatal("CheckPassword answered no error from a guest that failed the call")
			}
			st, _ := set.Status("directory")
			if st.LastError == "" || strings.Contains(st.LastError, "[redacted]") {
				t.Errorf("lastError = %q, want the host's own sentence, untouched", st.LastError)
			}
			guestLine := "obelo: plugin directory: its log lines during a call that carries a credential are withheld"
			var sawGuestLine bool
			for _, line := range strings.Split(log.all(), "\n") {
				if line == guestLine {
					sawGuestLine = true
					continue
				}
				if strings.Contains(line, "[redacted]") {
					t.Errorf("a host log line was redacted: %q", line)
				}
			}
			if !sawGuestLine {
				t.Errorf("no log line is %q; the log:\n%s", guestLine, log.all())
			}
		})
	}
}

// TestAGuestLogLineIsWithheldWhateverItsShape: a password with a newline in it,
// or one the 2000-byte cut would split, appears in no log line — the guest's line
// is withheld whole rather than flattened, truncated and searched.
func TestAGuestLogLineIsWithheldWhateverItsShape(t *testing.T) {
	plugins.Parallel(t)
	long := strings.Repeat("Correct-Horse-", 150)
	for name, password := range map[string]string{
		"newline":  "s3cret\nHorse",
		"carriage": "s3cret\rHorse",
		"cut":      long,
	} {
		t.Run(name, func(t *testing.T) {
			provider, _, log := failingSignInProvider(t)
			if _, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: password}); err == nil {
				t.Fatal("CheckPassword answered no error from a guest that failed the call")
			}
			all := log.all()
			for _, piece := range []string{"s3cret", "Correct-Horse-Correct-Horse"} {
				if strings.Contains(all, piece) {
					t.Errorf("a log line carries part of the password (%q):\n%s", piece, all)
				}
			}
		})
	}
}

// TestLoginsBehindAHungSignInGuestEndWithinTheCallTimeout: a guest that never
// answers holds its one instance until the deadline kills it. Logins queued behind
// it must give up by their own call timeout, not wait out every call ahead of
// them in turn.
func TestLoginsBehindAHungSignInGuestEndWithinTheCallTimeout(t *testing.T) {
	plugins.Parallel(t)
	const timeout = time.Second
	const logins = 5
	provider, _, _ := signInProviderWith(t, plugintest.SignInHangs, plugins.Options{CallTimeout: timeout})

	elapsed := make([]time.Duration, logins)
	errs := make([]error, logins)
	var wg sync.WaitGroup
	for i := 0; i < logins; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			_, errs[i] = provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
			elapsed[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] == nil {
			t.Errorf("login %d answered no error from a guest that never answers", i)
		}
		if elapsed[i] > timeout+time.Second {
			t.Errorf("login %d took %v, want no more than the %v call timeout and a little slack", i, elapsed[i], timeout)
		}
	}
}

// failingSignInProvider loads one Installed Sign-in provider whose guest fails
// every call with the password in its error, and returns it built.
func failingSignInProvider(t *testing.T) (pluginapi.SignInProvider, *plugins.Set, *logSink) {
	t.Helper()
	return signInProviderWith(t, plugintest.SignInFailsWithThePassword, plugins.Options{})
}

// signInProviderWith loads one Installed Sign-in provider, "directory", whose
// accounts setting is accounts and whose manifest allows allowedHosts, and
// returns it built.
func signInProviderWith(t *testing.T, accounts string, opts plugins.Options, allowedHosts ...string) (pluginapi.SignInProvider, *plugins.Set, *logSink) {
	t.Helper()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", accounts, allowedHosts...))
	log := &logSink{}
	set := loadWith(t, dataDir, log, opts)
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"accounts": accounts})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("directory")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for directory")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider, set, log
}

// redirectingTransport answers every request with a redirect to `to`, without a
// network: the redirect policy refuses `to` before anything is sent there.
type redirectingTransport struct{ to string }

func (rt redirectingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{rt.to}},
		Body:       http.NoBody,
		Request:    r,
	}, nil
}

// quotingKV is a key-value store that fails every call with the key in its error,
// as a store's own error text may.
type quotingKV struct{}

func (quotingKV) PluginKV(_, key string) ([]byte, bool, error) {
	return nil, false, errors.New("no row for key " + key)
}
func (quotingKV) SetPluginKV(_, key string, _ []byte) error {
	return errors.New("constraint failed on key " + key)
}
func (quotingKV) DeletePluginKV(_, key string) error { return errors.New("no row for key " + key) }

// TestASignInCallRecordsNoTextOfItsOwn: a guest that puts the password where the
// host writes text — a fetched URL that is redirected to a refused target, the
// name of a host it fetches, a key-value key, a log line, its own error. Neither
// the last error nor any log line carries the password or any part of the URL,
// host or key it was put in: while a sign-in call runs, what the host records is
// its own fixed sentence and the kind of thing that happened.
func TestASignInCallRecordsNoTextOfItsOwn(t *testing.T) {
	plugins.Parallel(t)
	const password = "s3cretcorrecthorse"
	const allowed = "203.0.113.7"
	for _, tc := range []struct {
		name      string
		accounts  string
		opts      plugins.Options
		hosts     []string
		recorded  bool
		forbidden []string
	}{{
		name:     "redirected fetch",
		accounts: plugintest.SignInFetchesWithThePassword + "http://" + allowed + "/login?pw=",
		opts: plugins.Options{HTTPClient: &http.Client{
			Transport: redirectingTransport{to: "http://127.0.0.1/landing?pw=" + password},
		}},
		hosts:     []string{allowed},
		recorded:  true,
		forbidden: []string{allowed, "127.0.0.1", "login", "landing", "pw="},
	}, {
		name:      "host named after the password",
		accounts:  plugintest.SignInFetchesThePasswordHost,
		recorded:  true,
		forbidden: []string{"example.test"},
	}, {
		name:      "key-value key",
		accounts:  plugintest.SignInStoresThePassword,
		opts:      plugins.Options{KV: quotingKV{}},
		forbidden: []string{"session-"},
	}, {
		name:      "log line",
		accounts:  plugintest.SignInLogsThePassword,
		forbidden: []string{"the password is"},
	}, {
		name:      "failed call",
		accounts:  plugintest.SignInFailsWithThePassword,
		recorded:  true,
		forbidden: []string{"rejected", "checking"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			provider, set, log := signInProviderWith(t, tc.accounts, tc.opts, tc.hosts...)
			_, _ = provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: password})

			st, _ := set.Status("directory")
			if tc.recorded && st.LastError == "" {
				t.Error("the call left no last error; what happened must still be recorded")
			}
			all := log.all()
			for _, text := range append([]string{password}, tc.forbidden...) {
				if strings.Contains(st.LastError, text) {
					t.Errorf("lastError carries %q: %q", text, st.LastError)
				}
				if strings.Contains(all, text) {
					t.Errorf("a log line carries %q:\n%s", text, all)
				}
			}
		})
	}
}

// TestASignInCallsSecrecyEndsWithTheCall: one module that is both a Sign-in
// provider and a Web reference provider makes a sign-in call and then a Web
// reference call that reaches for the network. The second call carries no
// credential, so its refused fetch is audited with the host it named — the
// sign-in call's "record nothing of the guest's" must not outlive it.
func TestASignInCallsSecrecyEndsWithTheCall(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	const accounts = "ada:pw:subject-ada:ada:"
	m := plugintest.SignInManifest("both", accounts)
	refs := plugintest.WebReferenceManifest("both", "fetch")
	m.Provides = append(m.Provides, refs.Provides...)
	m.Settings.Fields = append(m.Settings.Fields, refs.Settings.Fields...)
	plugintest.Install(t, dataDir, m)
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"accounts": accounts, "mode": "fetch"})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)

	signIn, ok := reg.SignInProvider("both")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for both")
	}
	provider, err := signIn.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"}); err != nil || !resp.Accepted {
		t.Fatalf("CheckPassword = %+v, %v; want ada accepted", resp, err)
	}

	webRefs, ok := reg.WebReferenceProvider("both")
	if !ok {
		t.Fatal("the Set registered no Web reference provider for both")
	}
	links, err := webRefs.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.Links(context.Background(), pluginapi.WebReferencesRequest{
		Kind: "movie", IDs: map[string]string{"imdb": "tt1160419"},
	}); err != nil {
		t.Fatalf("Links: %v", err)
	}
	if !log.contains(t, "refused a fetch: plugin=both", "reason=no-network-call") {
		t.Fatalf("the Web reference call's refused fetch was not audited as a call without a credential:\n%s", log.all())
	}
}

// TestFetchViolationsDuringSignInCallsNeverDisableTheProvider: a guest that
// reaches for a host its manifest does not list on every sign-in call. Anyone
// can make a login, so those refusals must not be a way to take the provider
// off the server — exactly as a failing sign-in call is not — while what
// happened is still recorded.
func TestFetchViolationsDuringSignInCallsNeverDisableTheProvider(t *testing.T) {
	plugins.Parallel(t)
	provider, set, _ := signInProviderWith(t, plugintest.SignInFetchesThePasswordHost, plugins.Options{})

	for i := 0; i < 2*plugins.DefaultFailureThreshold; i++ {
		if _, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"}); err != nil {
			t.Fatalf("call %d: CheckPassword: %v", i, err)
		}
	}
	st, _ := set.Status("directory")
	if st.Disabled {
		t.Fatalf("the provider was disabled after %d refused fetches during sign-in calls (%q); a login must not be able to disable it",
			2*plugins.DefaultFailureThreshold, st.LastError)
	}
	if st.LastError == "" {
		t.Error("the refused fetches left no last error; they must still be recorded")
	}
}

// TestAQueuedSignInCallNamesThePluginOnce: logins queued behind a guest that
// never answers give up by their own deadline, and the error each one answers
// names the plugin once — not "plugin directory: plugin directory: …".
func TestAQueuedSignInCallNamesThePluginOnce(t *testing.T) {
	plugins.Parallel(t)
	provider, _, _ := signInProviderWith(t, plugintest.SignInHangs, plugins.Options{CallTimeout: 500 * time.Millisecond})

	const logins = 3
	errs := make([]error, logins)
	var wg sync.WaitGroup
	for i := 0; i < logins; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "pw"})
		}(i)
	}
	wg.Wait()
	var queued int
	for i, err := range errs {
		if err == nil {
			t.Fatalf("login %d answered no error from a guest that never answers", i)
		}
		if strings.Contains(err.Error(), "queued") {
			queued++
		}
		if n := strings.Count(err.Error(), "plugin directory"); n != 1 {
			t.Errorf("login %d's error names the plugin %d times, want once: %q", i, n, err)
		}
	}
	if queued == 0 {
		t.Fatal("no login gave up while queued; the test did not reach the queued-call error")
	}
}
