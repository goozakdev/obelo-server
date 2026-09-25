package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for the socket grant's settings (ADR-0064): a Sign-in provider
// that declares a socket reaches the Plugins screen with the host's four settings
// after its own — the plaintext opt-out carrying the host's warning — and a save
// is judged by the host.

type socketFieldResp struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	Warning string `json:"warning"`
}

// TestASocketSignInProvidersSettingsCarryTheHostsWarning: the schema the screen
// draws from carries the opt-out as a switch with the host's warning, and a save
// choosing no encryption is refused under the Encryption control until the
// opt-out is on.
func TestASocketSignInProvidersSettingsCarryTheHostsWarning(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SocketSignInManifest("ldap-directory", ""))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)

	var list struct {
		Plugins []struct {
			ID             string            `json:"id"`
			SettingsSchema []socketFieldResp `json:"settingsSchema"`
		} `json:"plugins"`
	}
	if status, body := srv.AuthGET("/api/v1/settings/plugins", token, &list); status != http.StatusOK {
		t.Fatalf("GET plugins = %d; body: %s", status, body)
	}
	var schema []socketFieldResp
	for _, p := range list.Plugins {
		if p.ID == "ldap-directory" {
			schema = p.SettingsSchema
		}
	}
	byKey := map[string]socketFieldResp{}
	for _, f := range schema {
		byKey[f.Key] = f
	}
	for _, key := range []string{pluginapi.SocketAddressSetting, pluginapi.SocketTLSSetting,
		pluginapi.SocketAllowPlaintextSetting, pluginapi.SocketTrustedCASetting} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("the screen's schema has no %s; got %+v", key, schema)
		}
	}
	optOut := byKey[pluginapi.SocketAllowPlaintextSetting]
	if optOut.Type != "bool" || optOut.Label != "Allow unencrypted connection (plaintext passwords)" || optOut.Warning == "" {
		t.Fatalf("the opt-out reached the screen as %+v, want the host's switch with its warning", optOut)
	}

	var refusal fieldRefusalResp
	status, body := srv.JSON(http.MethodPut, pluginSettingsPath("ldap-directory"), token, map[string]any{"values": map[string]any{
		pluginapi.SocketAddressSetting: "ldap.example.test:389",
		pluginapi.SocketTLSSetting:     pluginapi.SocketTLSNone,
	}}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("saving encryption none without the opt-out = %d, want 400; body: %s", status, body)
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if f := refusal.Error.Details.Fields; len(f) != 1 || f[0].Key != pluginapi.SocketTLSSetting {
		t.Fatalf("refusal fields = %+v, want one under %s", f, pluginapi.SocketTLSSetting)
	}

	if status, body := saveDeclaredSettings(t, srv, token, "ldap-directory", map[string]any{
		pluginapi.SocketAddressSetting:        "ldap.example.test:389",
		pluginapi.SocketTLSSetting:            pluginapi.SocketTLSNone,
		pluginapi.SocketAllowPlaintextSetting: true,
	}); status != http.StatusOK {
		t.Fatalf("saving with the opt-out = %d, want 200; body: %s", status, body)
	}
}
