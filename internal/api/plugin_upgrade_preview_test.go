package api_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Preview and confirm over HTTP (ADR-0069, .scratch/plugin-inplace-upgrade issue 06).

type previewResp struct {
	Staged    string `json:"staged"`
	ExpiresAt string `json:"expiresAt"`
	Preview   struct {
		From              string   `json:"from"`
		To                string   `json:"to"`
		AuthorUnconfirmed bool     `json:"authorUnconfirmed"`
		HostsAdded        []string `json:"hostsAdded"`
	} `json:"preview"`
}

type confirmedResp struct {
	installedPluginResp
	Upgrade struct {
		From        string `json:"from"`
		To          string `json:"to"`
		StagedBy    string `json:"stagedBy"`
		ConfirmedBy string `json:"confirmedBy"`
	} `json:"upgrade"`
}

// wideningPackage is signedPackage plus a network host, so upgrading to it needs a
// confirmation.
func wideningPackage(t *testing.T, id, version string, priv ed25519.PrivateKey) []byte {
	t.Helper()
	m := plugintest.SinkManifest(id)
	m.Version = version
	m.Network.Hosts = []string{"api.example.test"}
	manifest := plugintest.ManifestJSON(t, m)
	module := plugintest.Guest(t)
	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatal(err)
	}
	return plugintest.PackageZip(t, manifest, module, doc)
}

func stageOver(t *testing.T, srv *testharness.Server, token string, archive []byte) previewResp {
	t.Helper()
	status, body := uploadPackage(t, srv, token, archive)
	if status != http.StatusAccepted {
		t.Fatalf("upload status = %d, want 202; body: %s", status, body)
	}
	var got previewResp
	if err := json.Unmarshal(body, &got); err != nil || got.Staged == "" || got.ExpiresAt == "" {
		t.Fatalf("the preview answer is malformed (%v): %s", err, body)
	}
	return got
}

func TestAWideningUploadAnswers202WithAPreviewAndAnyAdminConfirmsIt(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	ada := adminToken(t, srv)
	srv.CreateUser(ada, "bea", "adminpass1234", "admin")
	bea := srv.LoginAs("bea", "adminpass1234")
	_, priv, _ := signing.GenerateKey()
	if status, body := uploadPackage(t, srv, ada, signedPackage(t, "up-sink", "1.0.0", priv)); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}

	got := stageOver(t, srv, ada, wideningPackage(t, "up-sink", "1.1.0", priv))

	if got.Preview.From != "1.0.0" || got.Preview.To != "1.1.0" || got.Preview.AuthorUnconfirmed ||
		len(got.Preview.HostsAdded) != 1 || got.Preview.HostsAdded[0] != "api.example.test" {
		t.Fatalf("preview = %+v, want 1.0.0 -> 1.1.0 adding api.example.test", got.Preview)
	}
	if v := pluginNamed(t, readPlugins(t, srv, ada), "up-sink").Version; v != "1.0.0" {
		t.Fatalf("the Plugins screen lists %q before confirmation, want 1.0.0", v)
	}

	// A different Admin confirms.
	var done confirmedResp
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/up-sink/upgrade", bea, map[string]any{"staged": got.Staged}, &done)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200; body: %s", status, body)
	}
	if done.Version != "1.1.0" || done.Upgrade.From != "1.0.0" || done.Upgrade.To != "1.1.0" ||
		done.Upgrade.StagedBy == "" || done.Upgrade.ConfirmedBy != "bea" {
		t.Fatalf("confirmed = %+v, want 1.1.0 staged by the uploader and confirmed by bea", done)
	}
	if v := pluginNamed(t, readPlugins(t, srv, ada), "up-sink").Version; v != "1.1.0" {
		t.Fatalf("the Plugins screen lists %q after confirmation, want 1.1.0", v)
	}
}

func TestAnUnsignedInstalledPluginStagesWithTheAuthorUnconfirmed(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	installGuestPlugin(t, srv, token, "plain-sink")

	got := stageOver(t, srv, token, signedPackage(t, "plain-sink", "1.1.0", priv))

	if !got.Preview.AuthorUnconfirmed {
		t.Fatalf("preview = %+v, want authorUnconfirmed", got.Preview)
	}
}

func TestConfirmAndCancelRefuseAMemberAndAnUnknownOrCancelledToken(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	_, priv, _ := signing.GenerateKey()
	if status, body := uploadPackage(t, srv, admin, signedPackage(t, "up-sink", "1.0.0", priv)); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}
	got := stageOver(t, srv, admin, wideningPackage(t, "up-sink", "1.1.0", priv))

	if status, body := srv.JSON(http.MethodPost, pluginsPath+"/up-sink/upgrade", member, map[string]any{"staged": got.Staged}, nil); status != http.StatusForbidden {
		t.Fatalf("member confirm status = %d, want 403; body: %s", status, body)
	}
	if status, body := srv.JSON(http.MethodDelete, pluginsPath+"/up-sink/upgrade?staged="+got.Staged, member, nil, nil); status != http.StatusForbidden {
		t.Fatalf("member cancel status = %d, want 403; body: %s", status, body)
	}
	// Anything sent beside the token is refused, not applied: only the staged bytes apply.
	if status, body := srv.JSON(http.MethodPost, pluginsPath+"/up-sink/upgrade", admin,
		map[string]any{"staged": got.Staged, "package": "AAAA"}, nil); status != http.StatusBadRequest {
		t.Fatalf("confirm with extra content status = %d, want 400; body: %s", status, body)
	}
	status, body := srv.JSON(http.MethodPost, pluginsPath+"/up-sink/upgrade", admin, map[string]any{"staged": "nope"}, nil)
	if refusal := refusalOf(t, http.StatusConflict, status, body); refusal.Error.Code != "PLUGIN_UPGRADE_STAGED" {
		t.Fatalf("code = %q, want PLUGIN_UPGRADE_STAGED", refusal.Error.Code)
	}

	if status, body := srv.JSON(http.MethodDelete, pluginsPath+"/up-sink/upgrade?staged="+got.Staged, admin, nil, nil); status != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200; body: %s", status, body)
	}
	status, body = srv.JSON(http.MethodPost, pluginsPath+"/up-sink/upgrade", admin, map[string]any{"staged": got.Staged}, nil)
	if refusal := refusalOf(t, http.StatusConflict, status, body); !strings.Contains(refusal.Error.Message, "cancelled") {
		t.Fatalf("message = %q, want it to say it was cancelled", refusal.Error.Message)
	}
	if v := pluginNamed(t, readPlugins(t, srv, admin), "up-sink").Version; v != "1.0.0" {
		t.Fatalf("version = %q after a cancelled upgrade, want 1.0.0", v)
	}
	if status, _ := srv.JSON(http.MethodDelete, pluginsPath+"/up-sink/upgrade?staged="+got.Staged, admin, nil, nil); status != http.StatusNotFound {
		t.Fatalf("cancelling twice status = %d, want 404", status)
	}
}

// The 202 body names the plugin it is about, because the confirm and cancel routes are
// per id and the Admin may have uploaded through a form that never named one.
func TestAStagedUpgradeAnswerNamesThePluginOnTheUploadAndFromURLRoutes(t *testing.T) {
	t.Parallel()
	host := newPackageHost(t)
	srv := testharness.New(t, testharness.WithPluginSourcesFromPrivateAddresses())
	token := adminToken(t, srv)
	_, priv, _ := signing.GenerateKey()
	if status, body := uploadPackage(t, srv, token, signedPackage(t, "up-sink", "1.0.0", priv)); status != http.StatusCreated {
		t.Fatalf("first install status = %d; body: %s", status, body)
	}
	wantName := plugintest.SinkManifest("up-sink").Name

	check := func(route string, status int, body []byte) {
		t.Helper()
		if status != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202; body: %s", route, status, body)
		}
		var got struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &got); err != nil || got.ID != "up-sink" || got.Name != wantName || wantName == "" {
			t.Fatalf("%s: id/name = %q/%q (%v), want up-sink/%q; body: %s", route, got.ID, got.Name, err, wantName, body)
		}
	}
	status, body := uploadPackage(t, srv, token, wideningPackage(t, "up-sink", "1.1.0", priv))
	check("upload", status, body)
	url := host.serve("wide.zip", wideningPackage(t, "up-sink", "1.2.0", priv))
	status, body = postFromURL(t, srv, token, url)
	check("from-url", status, body)
}
