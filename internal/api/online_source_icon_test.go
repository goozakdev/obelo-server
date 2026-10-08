package api_test

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The source tile icon (ADR-0068 decision 12, issue 08): an optional icon.png beside
// the Plugin's manifest, served from the Server's own origin at
// GET /onlineSources/{id}/icon and named by iconUrl in the tile list.

func onlineIconPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 40, 40))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// onlineServerWithIcon is onlineServer for a Plugin whose directory also holds an
// icon.png, as an install of a package carrying one leaves it.
func onlineServerWithIcon(t *testing.T, icon []byte) (*testharness.Server, string, *onlineSource) {
	t.Helper()
	media := newOnlineMedia(t)
	src := newOnlineSource(t, media)
	dataDir := t.TempDir()
	dir := plugintest.Install(t, dataDir, plugintest.OnlineSourceManifest(onlineSlug, onlineName, src.srv.URL))
	if err := os.WriteFile(filepath.Join(dir, plugins.IconFile), icon, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithPluginFetchesExemptAt(src.srv.Listener.Addr().String()),
		testharness.WithOnlineSourceClient(media.srv.Client()),
	)
	return srv, adminToken(t, srv), src
}

func TestASourceIconIsServedFromTheServersOwnOriginToAnAdminBearerOrCookie(t *testing.T) {
	t.Parallel()
	icon := onlineIconPNG(t)
	srv, admin, src := onlineServerWithIcon(t, icon)

	var list onlineSourcesResp
	if st, body := srv.AuthGET(onlineBase, admin, &list); st != http.StatusOK || len(list.Sources) != 1 {
		t.Fatalf("GET /onlineSources = %d %s, want the one source", st, body)
	}
	iconURL := onlineBase + "/" + onlineSlug + "/icon"
	if got := list.Sources[0].IconURL; got == nil || *got != iconURL {
		t.Fatalf("iconUrl = %v, want the Server's own path %q (never a third-party host)", got, iconURL)
	}

	resp, body := getBytes(t, srv, iconURL, map[string]string{"Authorization": "Bearer " + admin})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, icon) {
		t.Fatalf("GET icon with the bearer token = %d (%d bytes), want 200 and the packaged PNG", resp.StatusCode, len(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q, want image/png", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the icon is served without X-Content-Type-Options: nosniff")
	}

	// A browser <img> carries the media cookie and no Authorization header.
	_, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")
	resp, body = getBytes(t, srv, iconURL, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, icon) {
		t.Fatalf("GET icon with the media cookie = %d, want 200 and the PNG", resp.StatusCode)
	}
	if resp, _ = getBytes(t, srv, iconURL, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET icon with no credential = %d, want 401", resp.StatusCode)
	}
	if n := src.calls(); n != 0 {
		t.Fatalf("serving the icon made %d Plugin calls, want 0", n)
	}
}

func TestAnUngrantedMemberAndARemoteCallerGetA404FromTheIconRoute(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineServerWithIcon(t, onlineIconPNG(t))
	iconURL := onlineBase + "/" + onlineSlug + "/icon"

	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	var remoteOut struct {
		ID string `json:"id"`
	}
	if st, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "peer", "role": "remote"}, &remoteOut); st != http.StatusCreated {
		t.Fatalf("creating the remote User = %d; body: %s", st, body)
	}
	remote := srv.IssueTokenForUser(remoteOut.ID, "peer-server-id")

	// The Admin gets the bytes, so a 404 below is the access rule and not a missing route.
	if st, _ := srv.AuthGET(iconURL, admin, nil); st != http.StatusOK {
		t.Fatalf("admin: GET icon = %d, want 200", st)
	}
	for name, token := range map[string]string{"member": member, "remote": remote} {
		if st, _ := srv.AuthGET(iconURL, token, nil); st != http.StatusNotFound {
			t.Fatalf("%s: GET icon = %d, want 404", name, st)
		}
	}
	// The same through the cookie an <img> uses.
	_, cookie := loginWithCookie(t, srv, "kid", "memberpass123", "web-client")
	if resp, _ := getBytes(t, srv, iconURL, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member with the media cookie: GET icon = %d, want 404", resp.StatusCode)
	}
}

func TestASourceWithNoIconHasNullIconUrlAndNoIconRoute(t *testing.T) {
	t.Parallel()
	srv, admin, _, _ := onlineServer(t)

	var list onlineSourcesResp
	if st, body := srv.AuthGET(onlineBase, admin, &list); st != http.StatusOK || len(list.Sources) != 1 {
		t.Fatalf("GET /onlineSources = %d %s", st, body)
	}
	if list.Sources[0].IconURL != nil {
		t.Fatalf("iconUrl = %q, want null for a package with no icon", *list.Sources[0].IconURL)
	}
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/icon", admin, nil); st != http.StatusNotFound {
		t.Fatalf("GET icon of an icon-less source = %d, want 404", st)
	}
	if st, _ := srv.AuthGET(onlineBase+"/nope/icon", admin, nil); st != http.StatusNotFound {
		t.Fatalf("GET icon of an unknown source = %d, want 404", st)
	}
}

// An icon file that is not a PNG on disk (hand-placed, or damaged) is never served:
// the install check is the gate, and the route does not trust the file's name.
func TestAnIconFileThatIsNotAPNGOnDiskIsNotServed(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineServerWithIcon(t, []byte("<html>not a png</html>"))

	var list onlineSourcesResp
	srv.AuthGET(onlineBase, admin, &list)
	if len(list.Sources) != 1 || list.Sources[0].IconURL != nil {
		t.Fatalf("sources = %+v, want the tile with a null iconUrl", list.Sources)
	}
	if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/icon", admin, nil); st != http.StatusNotFound || strings.Contains(string(body), "html") {
		t.Fatalf("GET icon = %d %s, want 404", st, body)
	}
}
