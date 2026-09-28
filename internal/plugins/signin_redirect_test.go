package plugins_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestAnInstalledRedirectSignInProviderRegistersAndAnswers is the redirect
// flow's loader-level tracer: the capability reaches the registry, the value the
// factory builds is a redirect provider, and both calls cross the sandbox with
// the host's values in and the guest's answer out. A provider that declares no
// idToken tells the host so.
func TestAnInstalledRedirectSignInProviderRegistersAndAnswers(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"authorize": "https://oauth.example.test/authorize"})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)

	registration, ok := reg.SignInProvider("oauth")
	if !ok || !registration.Descriptor.HasCapability(pluginapi.CapabilityRedirectSignIn) {
		t.Fatalf("registration = %+v, %v; want a sign-in provider declaring the redirect flow", registration.Descriptor, ok)
	}
	built, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider, ok := built.(pluginapi.SignInRedirectProvider)
	if !ok {
		t.Fatalf("the provider %T is not a redirect provider", built)
	}
	audience, ok := built.(interface {
		IDTokenAudience() (string, string, bool)
	})
	if !ok {
		t.Fatalf("the provider %T does not say what its ID tokens are verified against", built)
	}
	if _, _, declared := audience.IDTokenAudience(); declared {
		t.Fatal("a provider that declares no idToken said it did")
	}

	authz, err := provider.AuthorizeURL(context.Background(), pluginapi.SignInAuthorizeRequest{
		State: "st-1", CodeChallenge: "ch-1", CodeChallengeMethod: "S256", Nonce: "n-1",
		RedirectURI: "https://obelo.example/sign-in/callback",
	})
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(authz.URL)
	if err != nil || u.Host != "oauth.example.test" || u.Query().Get("state") != "st-1" ||
		u.Query().Get("code_challenge") != "ch-1" {
		t.Fatalf("authorize URL = %q, want the plugin's with the host's state and challenge", authz.URL)
	}

	resp, err := provider.Exchange(context.Background(), pluginapi.SignInExchangeRequest{Code: "subject-1|ada|crew"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !resp.Accepted || resp.Identity == nil || resp.Identity.Subject != "subject-1" || resp.IDToken != "" {
		t.Fatalf("exchange = %+v (identity %+v), want subject-1 accepted with no ID token", resp, resp.Identity)
	}
}

// TestAnIDTokenDeclarationMustNameOperatorTypedSettings: the two settings an
// idToken declaration names are what every ID token is verified against, so a
// manifest whose declaration names a missing field, a field of the wrong type, a
// field with a default the author typed, or sits on an entry that is not the
// redirect flow is refused at load, naming why.
func TestAnIDTokenDeclarationMustNameOperatorTypedSettings(t *testing.T) {
	plugins.Parallel(t)
	fields := func(issuerDefault bool) []pluginapi.SettingsField {
		issuer := pluginapi.SettingsField{Key: "issuer", Type: pluginapi.FieldURL}
		if issuerDefault {
			issuer.Default = json.RawMessage(`"https://idp.example"`)
		}
		return []pluginapi.SettingsField{issuer, {Key: "client_id", Type: pluginapi.FieldString}}
	}
	manifest := func(caps []pluginapi.Capability, idToken pluginapi.ManifestIDToken, f []pluginapi.SettingsField) pluginapi.Manifest {
		m := plugintest.RedirectSignInManifest("oidc-test", "https://idp.example/authorize")
		m.Provides[0].Capabilities = caps
		m.Provides[0].IDToken = &idToken
		m.Settings.Fields = f
		return m
	}
	redirect := []pluginapi.Capability{pluginapi.CapabilityRedirectSignIn}
	good := pluginapi.ManifestIDToken{IssuerSetting: "issuer", ClientIDSetting: "client_id"}

	for _, tc := range []struct {
		name string
		m    pluginapi.Manifest
		want string
	}{
		{"a well-formed declaration", manifest(redirect, good, fields(false)), ""},
		{"a missing field", manifest(redirect, pluginapi.ManifestIDToken{IssuerSetting: "issuer", ClientIDSetting: "nope"},
			fields(false)), "not a declared settings field"},
		{"the wrong type", manifest(redirect, pluginapi.ManifestIDToken{IssuerSetting: "client_id", ClientIDSetting: "client_id"},
			fields(false)), "must be a url field"},
		{"an author's default", manifest(redirect, good, fields(true)), "declares a default"},
		{"not the redirect flow", manifest([]pluginapi.Capability{pluginapi.CapabilityPasswordSignIn}, good, fields(false)),
			"belongs on a sign-in-provider entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			plugintest.Install(t, dataDir, tc.m)
			set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
			st, ok := set.Status("oidc-test")
			if !ok {
				t.Fatal("the Plugin is not listed")
			}
			if tc.want == "" {
				if st.Disabled {
					t.Fatalf("a well-formed declaration was refused: %s", st.LastError)
				}
				return
			}
			if !st.Disabled || !strings.Contains(st.LastError, tc.want) {
				t.Fatalf("status = %+v, want refused saying %q", st, tc.want)
			}
		})
	}
}

// TestFetchViolationsDuringRedirectCallsNeverDisableTheProvider: a redirect
// provider that reaches for a host its manifest does not list on every authorize
// or exchange. Anyone can start a sign-in or come back with a code, so those
// refusals must not be a way to take the provider off the server, while what
// happened is still recorded.
func TestFetchViolationsDuringRedirectCallsNeverDisableTheProvider(t *testing.T) {
	plugins.Parallel(t)
	const authorize = "https://oauth.example.test/" + plugintest.SignInRedirectFetchesABlockedHost
	calls := map[string]func(pluginapi.SignInRedirectProvider) error{
		"authorize": func(p pluginapi.SignInRedirectProvider) error {
			_, err := p.AuthorizeURL(context.Background(), pluginapi.SignInAuthorizeRequest{
				State: "st-1", CodeChallenge: "ch-1", CodeChallengeMethod: "S256", Nonce: "n-1",
				RedirectURI: "https://obelo.example/sign-in/callback",
			})
			return err
		},
		"exchange": func(p pluginapi.SignInRedirectProvider) error {
			_, err := p.Exchange(context.Background(), pluginapi.SignInExchangeRequest{Code: plugintest.SignInRedirectFetchesABlockedHost})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", authorize))
			set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
			for _, p := range set.Plugins() {
				p.SetSettingValues(map[string]any{"authorize": authorize})
			}
			reg := pluginapi.NewRegistry()
			set.Register(reg)
			registration, ok := reg.SignInProvider("oauth")
			if !ok {
				t.Fatal("the Set registered no Sign-in provider for oauth")
			}
			built, err := registration.New(pluginapi.Settings{Enabled: true})
			if err != nil {
				t.Fatalf("building the provider: %v", err)
			}
			provider := built.(pluginapi.SignInRedirectProvider)

			for i := 0; i < 2*plugins.DefaultFailureThreshold; i++ {
				if err := call(provider); err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
			}
			st, _ := set.Status("oauth")
			if st.Disabled {
				t.Fatalf("the provider was disabled after %d refused fetches during %s calls (%q); a sign-in must not be able to disable it",
					2*plugins.DefaultFailureThreshold, name, st.LastError)
			}
			if st.LastError == "" {
				t.Error("the refused fetches left no last error; they must still be recorded")
			}
		})
	}
}

// TestFetchViolationsDuringReChecksStillDisableTheProvider: a re-check is made
// by the host's schedule or an Admin, never by whoever reaches the login screen,
// so a provider that reaches for a host its manifest does not list on every
// re-check is disabled like any other Plugin that breaks its network policy.
func TestFetchViolationsDuringReChecksStillDisableTheProvider(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.LookupSignInManifest("directory", "", plugintest.SignInLookupFetchesABlockedHost))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"lookup": plugintest.SignInLookupFetchesABlockedHost})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("directory")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for directory")
	}
	built, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider, ok := built.(pluginapi.SignInLookupProvider)
	if !ok {
		t.Fatalf("the provider %T does not answer re-checks", built)
	}

	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		_, _ = provider.Lookup(context.Background(), pluginapi.SignInLookupRequest{Subject: "subject-ada"})
	}
	if st, _ := set.Status("directory"); !st.Disabled {
		t.Fatalf("the provider is still enabled after %d refused fetches during re-checks (%q); re-checks must still strike",
			plugins.DefaultFailureThreshold, st.LastError)
	}
}
