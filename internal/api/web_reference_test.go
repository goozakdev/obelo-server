package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Web reference provider Extension point: an Installed
// plugin placed under <dataDir>/plugins/<id>/, a Title whose folder asserts an
// IMDb id, and GET /titles/{id}/webReferences as an Admin and as a Member.
//
// The guest is the suite's own module, and the part it plays is chosen by the
// `mode` setting its manifest declares a default for. Everything asserted is what
// a client can see: which references come back, and which never do.

// heldIMDbID is the id the test Title's folder asserts — the ONE id this server
// holds for it.
const heldIMDbID = "tt1160419"

type webReferenceResp struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type webReferencesResp struct {
	References []webReferenceResp `json:"references"`
}

// webReferenceServer boots a server with one Installed Web reference provider
// playing mode, scans a Movie library holding one Title whose folder asserts
// heldIMDbID, and returns the server, an Admin token, the Library and the Title.
func webReferenceServer(t *testing.T, mode string) (*testharness.Server, string, string, string) {
	t.Helper()
	requireFixtures(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.WebReferenceManifest("example-refs", mode))

	root := t.TempDir()
	dir := filepath.Join(root, "Dune (2021) {imdb-"+heldIMDbID+"}")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the movie folder: %v", err)
	}
	clip, err := os.ReadFile(filepath.Join(fixtureRoot(t), "Dune (2021)", "Dune (2021).mp4"))
	if err != nil {
		t.Fatalf("reading the fixture clip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dune (2021).mp4"), clip, 0o644); err != nil {
		t.Fatalf("writing the movie: %v", err)
	}

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")
	return srv, token, libID, id
}

func readWebReferences(t *testing.T, srv *testharness.Server, token, titleID string) []webReferenceResp {
	t.Helper()
	var resp webReferencesResp
	status, body := srv.AuthGET("/api/v1/titles/"+titleID+"/webReferences", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET webReferences status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(string(body), `"references":[`) {
		t.Fatalf("GET webReferences body = %s, want a references array (never null)", body)
	}
	return resp.References
}

// TestAnHTTPWebReferenceForAHeldIDIsNeverShown: the guest links the one id the
// Title holds, but over plain http. The host keeps only https, so the Title has
// no visible Web references at all — not a flagged one, not a rewritten one.
func TestAnHTTPWebReferenceForAHeldIDIsNeverShown(t *testing.T) {
	srv, token, _, id := webReferenceServer(t, "http")

	if got := readWebReferences(t, srv, token, id); len(got) != 0 {
		t.Fatalf("web references = %+v, want none: an http:// address is never shown", got)
	}
}

// TestAWebReferenceForAnIDTheTitleDoesNotHoldIsNeverShown: the guest answers one
// good reference for the held id, one keyed to a different id in the same
// namespace, and one in a namespace the host never sent. Only the first survives.
func TestAWebReferenceForAnIDTheTitleDoesNotHoldIsNeverShown(t *testing.T) {
	srv, token, _, id := webReferenceServer(t, "foreign")

	got := readWebReferences(t, srv, token, id)
	want := webReferenceResp{Label: "Held imdb", URL: "https://refs.example.test/imdb/" + heldIMDbID}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("web references = %+v, want exactly %+v", got, want)
	}
}

// TestAnHTTPSWebReferenceForAHeldIDIsShownToEveryRole: the well-behaved guest's
// reference reaches an Admin AND a Member granted the Library, identically.
func TestAnHTTPSWebReferenceForAHeldIDIsShownToEveryRole(t *testing.T) {
	srv, admin, libID, id := webReferenceServer(t, "https")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libID)
	member := srv.LoginAs("kid", "memberpass123")

	want := webReferenceResp{Label: "Example imdb", URL: "https://refs.example.test/movie/imdb/" + heldIMDbID}
	for role, token := range map[string]string{"admin": admin, "member": member} {
		got := readWebReferences(t, srv, token, id)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s sees web references %+v, want exactly %+v", role, got, want)
		}
	}
}

// TestAWebReferenceProviderIsGivenNoNetwork: a guest that reaches for http_fetch
// while answering is told no by the host, and says so in its label — the fetch
// never left the sandbox.
func TestAWebReferenceProviderIsGivenNoNetwork(t *testing.T) {
	srv, token, _, id := webReferenceServer(t, "fetch")

	got := readWebReferences(t, srv, token, id)
	if len(got) != 1 || got[0].Label != "refused: this call has no network" {
		t.Fatalf("web references = %+v, want one labelled with the host's no-network refusal", got)
	}
}
