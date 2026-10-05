package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// TestAnAttachStartForAnUnknownProviderDoesNotSpendTheReauthGrant: a password-less
// User's re-auth grant is single-use. A start naming a provider that does not
// exist is refused BEFORE the grant is taken, so the same grant still works for
// the retry with a real provider.
func TestAnAttachStartForAnUnknownProviderDoesNotSpendTheReauthGrant(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))

	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	_, _, grant := reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")

	status, body, _ := startAttachWith(t, srv, ada.Token, map[string]any{"provider": "no-such-provider", "reauthGrant": grant.Grant})
	if status != http.StatusNotFound {
		t.Fatalf("attach start for an unknown provider = %d, want 404; body: %s", status, body)
	}
	// The grant survived the refusal.
	if run := startAttach(t, srv, ada.Token, map[string]any{"reauthGrant": grant.Grant}, "oauth"); run.state == "" {
		t.Fatal("the retry produced no state")
	}
}

// TestAnAttachStartWhoseProviderFailsDoesNotSpendTheReauthGrant: the proof was
// good and the provider is what failed, so the single-use grant is handed back and
// the same one starts the attach once the provider answers.
func TestAnAttachStartWhoseProviderFailsDoesNotSpendTheReauthGrant(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("broken",
		"https://oauth.example.test/authorize/"+plugintest.SignInRedirectFailsWithTheSecrets))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))

	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	_, _, grant := reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")

	status, body, _ := startAttachWith(t, srv, ada.Token, map[string]any{"provider": "broken", "reauthGrant": grant.Grant})
	if status != http.StatusBadGateway {
		t.Fatalf("attach start with a failing provider = %d, want 502; body: %s", status, body)
	}
	if run := startAttach(t, srv, ada.Token, map[string]any{"reauthGrant": grant.Grant}, "oauth"); run.state == "" {
		t.Fatal("the retry produced no state: the grant was burned by the provider failure")
	}
}
