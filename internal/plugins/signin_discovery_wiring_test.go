package plugins_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// issuerTransport answers the issuer's discovery document and the token endpoint
// it names, without a network, and counts what reached each.
type issuerTransport struct {
	issuer, token  string
	read, redeemed int
}

func (rt *issuerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := http.StatusNotFound, ""
	switch r.URL.String() {
	case rt.issuer + "/.well-known/openid-configuration":
		rt.read++
		raw, _ := json.Marshal(map[string]string{"issuer": rt.issuer, "token_endpoint": rt.token})
		status, body = http.StatusOK, string(raw)
	case rt.token:
		rt.redeemed++
		status, body = http.StatusOK, "{}"
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// TestARedirectSignInReachesTheTokenEndpointItsIssuerNamesOnAnotherHost is
// Google's shape end to end through the sandbox: a redirect Sign-in provider
// whose issuer's discovery document names its token endpoint on another host
// than the issuer — a host no manifest lists, resolving to a public address —
// reaches it during the exchange, and the sign-in completes. It is what makes a
// real redirect call read its issuer's discovery document at all.
func TestARedirectSignInReachesTheTokenEndpointItsIssuerNamesOnAnotherHost(t *testing.T) {
	plugins.ResolveAs(t, "142.250.72.10")
	rt := &issuerTransport{issuer: "https://idp.example.test", token: "https://oauth2.tokens.example.test/token"}

	m := plugintest.RedirectSignInManifest("oidc-test", "https://idp.example.test/authorize")
	m.Provides[0].IDToken = &pluginapi.ManifestIDToken{IssuerSetting: "issuer", ClientIDSetting: "client_id"}
	m.Settings.Fields = append(m.Settings.Fields,
		pluginapi.SettingsField{Key: "issuer", Type: pluginapi.FieldURL, Label: "Issuer"},
		pluginapi.SettingsField{Key: "client_id", Type: pluginapi.FieldString, Label: "Client ID"})
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, m)
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{HTTPClient: &http.Client{Transport: rt}})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{
			"authorize": "https://idp.example.test/authorize",
			"issuer":    rt.issuer,
			"client_id": "obelo",
		})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("oidc-test")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for oidc-test")
	}
	built, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider, ok := built.(pluginapi.SignInRedirectProvider)
	if !ok {
		t.Fatalf("the provider %T is not a redirect provider", built)
	}

	resp, err := provider.Exchange(context.Background(), pluginapi.SignInExchangeRequest{
		Code: plugintest.SignInRedirectDiscovers + "subject-ada|ada|crew",
	})
	if err != nil {
		t.Fatalf("Exchange after %d discovery reads and %d token requests: %v", rt.read, rt.redeemed, err)
	}
	if !resp.Accepted || resp.Identity == nil || resp.Identity.Subject != "subject-ada" || rt.redeemed != 1 {
		t.Fatalf("exchange = %+v (identity %+v) after %d token requests, want subject-ada accepted after 1",
			resp, resp.Identity, rt.redeemed)
	}
}
