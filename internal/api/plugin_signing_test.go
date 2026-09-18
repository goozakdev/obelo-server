package api_test

import (
	"crypto/ed25519"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for manifest signing (.scratch/plugin-system issue 15): an
// Admin pins a publisher's key, and this server stops installing anything that
// publisher did not sign.
//
// The plugin is the reference one, built from its own source, and the manifest
// bytes signed here are byte for byte the bytes an install writes to disk —
// `discordtest.ManifestJSON` returns exactly what `Manager.install` stores, which
// is what makes a detached signature over them meaningful at all.
//
// The three states this file defends, and none of them is an implementation
// detail:
//
//  1. WITH NOTHING PINNED, NOTHING CHANGES. That is the shipped state (ADR-0001:
//     this project vouches for no publisher), and both a signed and an unsigned
//     plugin install exactly as they did before this feature existed.
//  2. WITH A KEY PINNED, a plugin signed by it installs and the screen says who
//     signed it. Anything else is refused, and the refusal NAMES THE PUBLISHER
//     THE PLUGIN CLAIMED — the one fact that tells an operator whether they
//     forgot to pin somebody or are looking at something they should not install.
//  3. THE `pluginsign` COMMAND'S OUTPUT IS WHAT THE SERVER ACCEPTS. The last test
//     runs the real command and feeds its file through the real endpoint.

const publishersPath = pluginsPath + "/publishers"

// --- wire shapes ----------------------------------------------------------------

type publisherResp struct {
	Publisher string `json:"publisher"`
	PublicKey string `json:"publicKey"`
	KeyID     string `json:"keyId"`
	AddedAt   string `json:"addedAt"`
}

type publishersResp struct {
	Publishers []publisherResp `json:"publishers"`
}

// signedPluginResp is the Plugins-screen row seen through the two fields this
// issue added. It is a second, narrower shape rather than an edit to the one in
// plugin_install_test.go, so the suite that proves installing still works stays
// exactly as issue 10 left it.
type signedPluginResp struct {
	ID                string `json:"id"`
	Publisher         string `json:"publisher"`
	KeyID             string `json:"keyId"`
	DisabledByFailure bool   `json:"disabledByFailure"`
	LastError         string `json:"lastError"`
}

type signedPluginsResp struct {
	Plugins []signedPluginResp `json:"plugins"`
}

// signerOf is the recorded signer of one installed plugin.
func signerOf(t *testing.T, srv *testharness.Server, token, id string) signedPluginResp {
	t.Helper()
	var resp signedPluginsResp
	status, body := srv.AuthGET(pluginsPath, token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET plugins status = %d, want 200; body: %s", status, body)
	}
	for _, p := range resp.Plugins {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no plugin %q on the Plugins screen; got %+v", id, resp.Plugins)
	return signedPluginResp{}
}

// --- helpers --------------------------------------------------------------------

func pinPublisher(t *testing.T, srv *testharness.Server, token, name string, pub ed25519.PublicKey) publishersResp {
	t.Helper()
	var resp publishersResp
	status, body := srv.JSON(http.MethodPut, publishersPath, token,
		map[string]any{"publisher": name, "publicKey": signing.EncodeKey(pub)}, &resp)
	if status != http.StatusOK {
		t.Fatalf("pinning %s: status = %d, want 200; body: %s", name, status, body)
	}
	return resp
}

// signDiscord makes the detached document for the reference plugin, over the
// exact bytes an install writes.
func signDiscord(t *testing.T, priv ed25519.PrivateKey, publisher string) []byte {
	t.Helper()
	sig, err := signing.Sign(priv, publisher, discordtest.ManifestJSON(t), discordtest.Module(t))
	if err != nil {
		t.Fatalf("signing the reference plugin: %v", err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatalf("encoding the signature: %v", err)
	}
	return doc
}

// uploadSignedPlugin is uploadPlugin with the optional third part.
func uploadSignedPlugin(t *testing.T, srv *testharness.Server, token string, manifest, module, signature []byte) (int, []byte) {
	t.Helper()
	parts := []testharness.MultipartFile{
		{Field: "manifest", Name: plugins.ManifestFile, ContentType: "application/json", Content: manifest},
		{Field: "module", Name: plugins.DefaultModuleFile, ContentType: "application/wasm", Content: module},
	}
	if len(signature) > 0 {
		parts = append(parts, testharness.MultipartFile{
			Field: "signature", Name: pluginapi.SignatureFile,
			ContentType: "application/json", Content: signature,
		})
	}
	return srv.MultipartFiles(http.MethodPost, pluginsPath, token, parts, nil)
}

func mustGenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatalf("generating a publisher key: %v", err)
	}
	return pub, priv
}

// --- with nothing pinned ---------------------------------------------------------

// TestWithNoPinnedKeysBothInstall — the shipped state, and the one that must not
// have changed. A signed plugin and an unsigned one are both installed, and
// neither is recorded as signed by anybody, because nobody checked.
func TestWithNoPinnedKeysBothInstall(t *testing.T) {
	_, priv := mustGenerateKey(t)

	t.Run("unsigned", func(t *testing.T) {
		srv := testharness.New(t)
		token := adminToken(t, srv)
		status, body := uploadSignedPlugin(t, srv, token,
			discordtest.ManifestJSON(t), discordtest.Module(t), nil)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", status, body)
		}
	})

	t.Run("signed by a key nobody pinned", func(t *testing.T) {
		srv := testharness.New(t)
		token := adminToken(t, srv)
		status, body := uploadSignedPlugin(t, srv, token,
			discordtest.ManifestJSON(t), discordtest.Module(t),
			signDiscord(t, priv, "Somebody Nobody Pinned"))
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", status, body)
		}
		// A signature that was never checked is NOT reported as a signature. The
		// screen would otherwise say "signed by X" on the strength of a string in a
		// file the author shipped, which is worse than saying nothing.
		view := signerOf(t, srv, token, "discord")
		if view.Publisher != "" || view.KeyID != "" {
			t.Fatalf("an unverified signature was recorded as %+v, want nothing", view)
		}
		// The document is still stored beside the manifest, as provenance, so an
		// operator who pins a key next month can check what they already have.
		path := filepath.Join(srv.DataDir, plugins.DirName, "discord", pluginapi.SignatureFile)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the signature that travelled with the plugin was not kept at %s: %v", path, err)
		}
	})

	// And nothing is pinned by default, which is what makes both of the above true.
	srv := testharness.New(t)
	token := adminToken(t, srv)
	var pinned publishersResp
	if status, body := srv.AuthGET(publishersPath, token, &pinned); status != http.StatusOK {
		t.Fatalf("GET publishers status = %d, want 200; body: %s", status, body)
	}
	if len(pinned.Publishers) != 0 {
		t.Fatalf("a fresh server has %+v pinned, want nothing", pinned.Publishers)
	}
}

// --- with one key pinned ----------------------------------------------------------

// TestWithOnePinnedKeyOnlyThatPublishersPluginInstalls is the acceptance
// criterion, whole: the signed plugin installs, the same module with its
// signature stripped is refused, and the same module signed by another key is
// refused — both refusals naming the publisher claimed.
func TestWithOnePinnedKeyOnlyThatPublishersPluginInstalls(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	_, otherPriv := mustGenerateKey(t)
	const publisher = "Example Publisher"

	t.Run("signed by the pinned key", func(t *testing.T) {
		srv := testharness.New(t)
		token := adminToken(t, srv)
		view := pinPublisher(t, srv, token, publisher, pub)
		if len(view.Publishers) != 1 {
			t.Fatalf("pinning gave %+v, want one publisher", view.Publishers)
		}
		// The PUBLIC key comes back in full, unmasked. It is the one credential-
		// shaped field this API returns whole, because an operator has to compare it
		// against what a publisher advertises.
		if view.Publishers[0].PublicKey != signing.EncodeKey(pub) {
			t.Fatalf("publicKey = %q, want the key that was pinned", view.Publishers[0].PublicKey)
		}
		if view.Publishers[0].KeyID != signing.KeyID(pub) {
			t.Fatalf("keyId = %q, want the key's fingerprint", view.Publishers[0].KeyID)
		}

		status, body := uploadSignedPlugin(t, srv, token,
			discordtest.ManifestJSON(t), discordtest.Module(t), signDiscord(t, priv, publisher))
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", status, body)
		}
		installed := signerOf(t, srv, token, "discord")
		if installed.Publisher != publisher || installed.KeyID != signing.KeyID(pub) {
			t.Fatalf("the installed plugin reads as %+v, want it recorded as signed by %q", installed, publisher)
		}
		if installed.DisabledByFailure || installed.LastError != "" {
			t.Fatalf("a signed plugin came up as %+v, want it working", installed)
		}
	})

	for _, tc := range []struct {
		name      string
		signature []byte
		wantIn    []string
	}{
		{
			name:      "the signature stripped",
			signature: nil,
			// It cannot name a publisher there is none of, so it names who it WOULD
			// accept — which is the actionable half.
			wantIn: []string{"carries no signature", publisher},
		},
		{
			name:      "signed by another key",
			signature: signDiscord(t, otherPriv, publisher),
			wantIn:    []string{publisher, "does not verify"},
		},
		{
			name:      "signed by somebody else entirely",
			signature: signDiscord(t, otherPriv, "Somebody Else"),
			wantIn:    []string{"Somebody Else", "not a publisher pinned on this server"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t)
			token := adminToken(t, srv)
			pinPublisher(t, srv, token, publisher, pub)

			status, body := uploadSignedPlugin(t, srv, token,
				discordtest.ManifestJSON(t), discordtest.Module(t), tc.signature)
			refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
			if refusal.Error.Code != "PLUGIN_SIGNATURE" {
				t.Fatalf("code = %q, want PLUGIN_SIGNATURE; body: %s", refusal.Error.Code, body)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(refusal.Error.Message, want) {
					t.Fatalf("message = %q, want it to contain %q", refusal.Error.Message, want)
				}
			}
			// A refused install leaves nothing behind — the signature check runs
			// before a byte is written, which is the whole reason it sits where it
			// does in the install order.
			if got := readPlugins(t, srv, token); len(got.Plugins) != 0 {
				t.Fatalf("a refused install left %+v behind", got.Plugins)
			}
			dir := filepath.Join(srv.DataDir, plugins.DirName, "discord")
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("%s exists after a refused install (err = %v)", dir, err)
			}
		})
	}
}

// TestASignatureOverOtherBytesIsItsOwnSentence. "These files do not go together"
// is a different thing to tell an operator from "this was not signed by who it
// says", and only one of them means somebody is lying.
func TestASignatureOverOtherBytesIsItsOwnSentence(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	const publisher = "Example Publisher"

	// Signed over a DIFFERENT module, with a real key the server really has pinned.
	sig, err := signing.Sign(priv, publisher, discordtest.ManifestJSON(t), []byte("a different module"))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, publisher, pub)

	status, body := uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), doc)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if !strings.Contains(refusal.Error.Message, "covers different files") {
		t.Fatalf("message = %q, want it to say the signature covers other files", refusal.Error.Message)
	}
	if !strings.Contains(refusal.Error.Message, publisher) {
		t.Fatalf("message = %q, want it to name the publisher claimed", refusal.Error.Message)
	}
}

// TestAnAlreadyInstalledPluginIsNotReVerified. Pinning a key is a decision about
// FUTURE installs. Re-checking what is already on disk would mean a publisher
// rotating a key, or an operator changing their mind, silently stopping plugins
// that were running — a change of policy turning into an outage. An operator who
// wants a plugin gone uninstalls it.
func TestAnAlreadyInstalledPluginIsNotReVerified(t *testing.T) {
	pub, _ := mustGenerateKey(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)

	// Installed with nothing pinned, so nothing was ever checked.
	if status, body := uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), nil); status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", status, body)
	}
	pinPublisher(t, srv, token, "Example Publisher", pub)

	// Disabling and re-enabling touches the files and the loader and asks nothing
	// about signatures.
	for _, verb := range []string{"disable", "enable", "reenable"} {
		status, body := srv.JSON(http.MethodPost, pluginsPath+"/discord/"+verb, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s after pinning a key: status = %d, want 200; body: %s", verb, status, body)
		}
	}
	view := signerOf(t, srv, token, "discord")
	if view.DisabledByFailure {
		t.Fatalf("pinning a key stopped an already-installed plugin: %+v", view)
	}
}

// TestUnpinningTheLastKeyReturnsToInstallingAnything — the policy is the table,
// and emptying the table is how an operator opts back out.
func TestUnpinningTheLastKeyReturnsToInstallingAnything(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	const publisher = "Example Publisher"

	srv := testharness.New(t)
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, publisher, pub)

	// Refused while the key is pinned.
	status, body := uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), nil)
	if refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body); refusal.Error.Code != "PLUGIN_SIGNATURE" {
		t.Fatalf("code = %q, want PLUGIN_SIGNATURE", refusal.Error.Code)
	}

	var after publishersResp
	status, body = srv.JSON(http.MethodDelete, publishersPath+"/"+publisher, token, nil, &after)
	if status != http.StatusOK {
		t.Fatalf("unpinning: status = %d, want 200; body: %s", status, body)
	}
	if len(after.Publishers) != 0 {
		t.Fatalf("after unpinning, %+v is still pinned", after.Publishers)
	}

	status, body = uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), nil)
	if status != http.StatusCreated {
		t.Fatalf("after unpinning the last key: status = %d, want 201; body: %s", status, body)
	}
}

// TestPinningRefusesWhatIsNotAKey, and TestUnpinningANameNobodyPinnedIs404 —
// both sentences an Admin reads on a form.
func TestPinningRefusesWhatIsNotAKey(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, tc := range []struct {
		name, publisher, key, wantIn string
	}{
		{"no name", "", "AAAA", "publisher name"},
		{"not base64", "X", "!!!", "not valid base64"},
		{"the wrong length", "X", "bm9wZQ==", "32 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := srv.JSON(http.MethodPut, publishersPath, token,
				map[string]any{"publisher": tc.publisher, "publicKey": tc.key}, nil)
			refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
			if refusal.Error.Code != "PLUGIN_SIGNATURE" {
				t.Fatalf("code = %q, want PLUGIN_SIGNATURE; body: %s", refusal.Error.Code, body)
			}
			if !strings.Contains(refusal.Error.Message, tc.wantIn) {
				t.Fatalf("message = %q, want it to contain %q", refusal.Error.Message, tc.wantIn)
			}
		})
	}

	status, body := srv.JSON(http.MethodDelete, publishersPath+"/nobody", token, nil, nil)
	refusal := refusalOf(t, http.StatusNotFound, status, body)
	if refusal.Error.Code != "PLUGIN_UNKNOWN" {
		t.Fatalf("code = %q, want PLUGIN_UNKNOWN; body: %s", refusal.Error.Code, body)
	}
}

// TestAPublisherIsMatchedCaseInsensitively: an operator who pinned "Example
// Publisher" and a document that says "example publisher" mean the same
// publisher, and the alternative is a refusal with no visible cause.
func TestAPublisherIsMatchedCaseInsensitively(t *testing.T) {
	pub, priv := mustGenerateKey(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, "Example Publisher", pub)

	status, body := uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), signDiscord(t, priv, "example publisher"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", status, body)
	}
	// The name RECORDED is the pinned spelling, not the document's: it is the
	// operator's own word for this publisher and it is what they will recognise.
	if got := signerOf(t, srv, token, "discord"); got.Publisher != "Example Publisher" {
		t.Fatalf("publisher = %q, want the spelling the operator pinned", got.Publisher)
	}
}

// --- the signature fetched beside a manifest ---------------------------------------

// TestAURLInstallFetchesTheSignatureBesideTheManifest — the layout an author
// publishes, and the layout a catalog serves.
func TestAURLInstallFetchesTheSignatureBesideTheManifest(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	const publisher = "Example Publisher"

	source := discordSource(t, signDiscord(t, priv, publisher))
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, publisher, pub)

	manifestURL := source.URL + "/" + plugins.ManifestFile
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": manifestURL}, nil)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", status, body)
	}
	if got := signerOf(t, srv, token, "discord"); got.Publisher != publisher {
		t.Fatalf("publisher = %q, want %q — the signature beside the manifest was not used", got.Publisher, publisher)
	}
}

// TestAURLInstallWithNoSignatureBesideItIsRefusedByName: a 404 on the signature
// is not an error on its own, and the pinned-key policy is the one thing that
// decides what it costs.
func TestAURLInstallWithNoSignatureBesideItIsRefusedByName(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	const publisher = "Example Publisher"

	source := discordSource(t, nil) // answers 404 for the signature
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, publisher, pub)

	status, body := srv.JSON(http.MethodPost, pluginsPath+"/from-url", token,
		map[string]any{"url": source.URL + "/" + plugins.ManifestFile}, nil)
	refusal := refusalOf(t, http.StatusUnprocessableEntity, status, body)
	if refusal.Error.Code != "PLUGIN_SIGNATURE" {
		t.Fatalf("code = %q, want PLUGIN_SIGNATURE; body: %s", refusal.Error.Code, body)
	}
	if !strings.Contains(refusal.Error.Message, publisher) {
		t.Fatalf("message = %q, want it to name who this server would accept", refusal.Error.Message)
	}
}

// --- the command --------------------------------------------------------------------

// TestThePluginsignCommandsOutputInstalls. The acceptance criterion that no unit
// test can stand in for: the REAL command is run, in a temp directory, over the
// reference plugin's real bytes, and the file it wrote is uploaded to the real
// endpoint against the key the command printed.
//
// It runs `go run ./cmd/pluginsign` rather than calling the package, because the
// package is already covered and what is in doubt here is the COMMAND: its flags,
// its file writing, and whether what it puts on disk is what the server reads.
func TestThePluginsignCommandsOutputInstalls(t *testing.T) {
	const publisher = "Example Publisher"
	dir := t.TempDir()
	root := discordtest.RepoRoot(t)

	manifestPath := filepath.Join(dir, plugins.ManifestFile)
	modulePath := filepath.Join(dir, plugins.DefaultModuleFile)
	keyPath := filepath.Join(dir, "publisher.key")
	sigPath := filepath.Join(dir, pluginapi.SignatureFile)

	if err := os.WriteFile(manifestPath, discordtest.ManifestJSON(t), 0o644); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}
	if err := os.WriteFile(modulePath, discordtest.Module(t), 0o644); err != nil {
		t.Fatalf("writing the module: %v", err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"run", "./cmd/pluginsign"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pluginsign %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	keygen := run("keygen", "-out", keyPath)
	// The public key is printed for the operator to pin, and it is the only way to
	// get it: the private key file holds the private key and nothing else.
	var publicKey string
	for _, line := range strings.Split(keygen, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "public key:"); ok {
			publicKey = strings.TrimSpace(rest)
		}
	}
	if publicKey == "" {
		t.Fatalf("keygen printed no public key:\n%s", keygen)
	}
	pub, err := signing.ParsePublicKey(publicKey)
	if err != nil {
		t.Fatalf("the key keygen printed is not a public key: %v", err)
	}

	run("sign",
		"-key", keyPath, "-publisher", publisher,
		"-manifest", manifestPath, "-module", modulePath, "-out", sigPath)

	// The command's own verify agrees with it — and it is the SAME code the server
	// runs, which is the point of the package being shared.
	run("verify", "-sig", sigPath, "-pub", publicKey,
		"-manifest", manifestPath, "-module", modulePath)

	doc, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("reading what the command wrote: %v", err)
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	pinPublisher(t, srv, token, publisher, pub)

	status, body := uploadSignedPlugin(t, srv, token,
		discordtest.ManifestJSON(t), discordtest.Module(t), doc)
	if status != http.StatusCreated {
		t.Fatalf("the command's own signature was refused by the server: status = %d; body: %s", status, body)
	}
	if got := signerOf(t, srv, token, "discord"); got.Publisher != publisher {
		t.Fatalf("publisher = %q, want %q", got.Publisher, publisher)
	}
}

// TestThePublisherRoutesAreAdminOnly.
func TestThePublisherRoutesAreAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")

	if status, _ := srv.AuthGET(publishersPath, member, nil); status != http.StatusForbidden {
		t.Fatalf("a Member GETting the pinned keys got %d, want 403", status)
	}
	status, _ := srv.JSON(http.MethodPut, publishersPath, member,
		map[string]any{"publisher": "X", "publicKey": "Y"}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a Member pinning a key got %d, want 403", status)
	}
	status, _ = srv.JSON(http.MethodDelete, publishersPath+"/X", member, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a Member unpinning a key got %d, want 403", status)
	}
}
