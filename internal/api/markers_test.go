package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Markers end to end (ADR-0065): an `.edl` beside a real File is read by the
// Scanner, served on GET /sessions/{id}/markers, and — for Credits at or past
// halfway — moves the Watched ceiling of that File to the Credits start.

type markersResp struct {
	Markers []struct {
		Kind    string `json:"kind"`
		Source  string `json:"source"`
		StartMs int64  `json:"startMs"`
		EndMs   int64  `json:"endMs"`
	} `json:"markers"`
}

// markerLibrary copies the Dune fixture into a fresh root, scans it, then writes
// an `.edl` with an Intro and a Credits span starting at creditsFrac of the File
// and rescans. It returns the Dune Title id and the File's duration.
func markerLibrary(t *testing.T, srv *testharness.Server, token string, creditsFrac float64) (string, int64) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Dune (2021)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(fixtureRoot(t), "Dune (2021)", "Dune (2021).mp4"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dune (2021).mp4"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list); status != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", status, body)
	}
	duneID := findTitle(t, list, "Dune")
	dur := titleDuration(t, srv, token, duneID)

	secs := float64(dur) / 1000
	edl := fmt.Sprintf("0 %.3f 0 Intro\n%.3f %.3f 3 # Credits\n", secs*0.2, secs*creditsFrac, secs)
	if err := os.WriteFile(filepath.Join(dir, "Dune (2021).edl"), []byte(edl), 0o644); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")
	return duneID, dur
}

func getMarkers(t *testing.T, srv *testharness.Server, token, sessionID string, wantStatus int) markersResp {
	t.Helper()
	var out markersResp
	status, body := srv.AuthGET("/api/v1/sessions/"+sessionID+"/markers", token, &out)
	if status != wantStatus {
		t.Fatalf("markers status = %d, want %d; body: %s", status, wantStatus, body)
	}
	return out
}

// TestMarkersFromEDLAreServedForTheSession: the Scanner's Local Intro and Credits
// reach the player through the session, and only its owner sees them.
func TestMarkersFromEDLAreServedForTheSession(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	duneID, _ := markerLibrary(t, srv, token, 0.55)
	dec := negotiateDune(t, srv, token, duneID)

	got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK)
	if len(got.Markers) != 2 || got.Markers[0].Kind != "intro" || got.Markers[1].Kind != "credits" {
		t.Fatalf("markers = %+v, want an intro then a credits", got.Markers)
	}
	for _, m := range got.Markers {
		if m.Source != "local" {
			t.Errorf("%s marker source = %q, want local", m.Kind, m.Source)
		}
	}

	srv.CreateMember("member", "memberpass123")
	other := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	getMarkers(t, srv, other, dec.SessionID, http.StatusNotFound)
	getMarkers(t, srv, token, "no-such-session", http.StatusNotFound)
}

// TestCreditsMarkerPastHalfwayMarksWatched: Credits at 55% — progress at the
// Credits start marks the Title watched, well before the flat ~90%.
func TestCreditsMarkerPastHalfwayMarksWatched(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	duneID, _ := markerLibrary(t, srv, token, 0.55)
	dec := negotiateDune(t, srv, token, duneID)

	credits := getMarkers(t, srv, token, dec.SessionID, http.StatusOK).Markers[1]
	if out := postProgress(t, srv, token, dec.SessionID, credits.StartMs-50, http.StatusOK); out.Watched {
		t.Fatalf("just before the Credits start: %+v, want unwatched", out)
	}
	out := postProgress(t, srv, token, dec.SessionID, credits.StartMs, http.StatusOK)
	if !out.Watched || out.ResumePositionMs != 0 {
		t.Errorf("at the Credits start: %+v, want watched with resume cleared", out)
	}
}

// TestCreditsMarkerBeforeHalfwayIsIgnored: Credits at 40% do not mark the Title
// watched; the flat ~90% ceiling still does.
func TestCreditsMarkerBeforeHalfwayIsIgnored(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	duneID, dur := markerLibrary(t, srv, token, 0.40)
	dec := negotiateDune(t, srv, token, duneID)

	credits := getMarkers(t, srv, token, dec.SessionID, http.StatusOK).Markers[1]
	if out := postProgress(t, srv, token, dec.SessionID, credits.StartMs, http.StatusOK); out.Watched {
		t.Fatalf("at a Credits start of 40%%: %+v, want unwatched", out)
	}
	if out := postProgress(t, srv, token, dec.SessionID, dur*95/100, http.StatusOK); !out.Watched {
		t.Errorf("at 95%%: %+v, want watched by the flat ceiling", out)
	}
}

// TestMarkersOfAnEndedSessionAre404: once the owner ends a session, its Markers
// are gone with it — the same answer as a session that never was.
func TestMarkersOfAnEndedSessionAre404(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	duneID, _ := markerLibrary(t, srv, token, 0.55)
	dec := negotiateDune(t, srv, token, duneID)
	getMarkers(t, srv, token, dec.SessionID, http.StatusOK)

	endSession(t, srv, token, dec.SessionID)
	getMarkers(t, srv, token, dec.SessionID, http.StatusNotFound)
}

// TestMarkersOfARelayedSessionWhoseSharerHasNoneAreEmpty: a relayed session
// serves the sharer's Markers, and a sharer with none for the File answers 200
// with an empty list, not an error, so a player simply offers no Skip.
func TestMarkersOfARelayedSessionWhoseSharerHasNoneAreEmpty(t *testing.T) {
	t.Parallel()
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d, want 200; body: %s", status, body)
	}
	var raw map[string]json.RawMessage
	if st, b := f.home.AuthGET("/api/v1/sessions/"+dec.SessionID+"/markers", f.homeAdmin, &raw); st != http.StatusOK {
		t.Fatalf("markers of a relayed session = %d, want 200; body: %s", st, b)
	}
	if got := string(raw["markers"]); got != "[]" {
		t.Errorf("markers of a relayed session = %s, want []", got)
	}
}

// TestMarkersOfARelayedSessionAreTheSharers: a relayed session asks the sharer,
// once, for the Markers of the session it opened, serves them, and measures its
// own Watched point against them.
func TestMarkersOfARelayedSessionAreTheSharers(t *testing.T) {
	t.Parallel()
	f := linkForRelay(t)
	f.sharer.Exec(`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		SELECT 'relay-intro', path, 'intro', 'local', 0, duration_ms / 5 FROM files WHERE path LIKE '%Dune (2021).mp4'`)
	f.sharer.Exec(`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		SELECT 'relay-credits', path, 'credits', 'local', duration_ms * 55 / 100, duration_ms FROM files WHERE path LIKE '%Dune (2021).mp4'`)
	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d, want 200; body: %s", status, body)
	}

	got := getMarkers(t, f.home, f.homeAdmin, dec.SessionID, http.StatusOK)
	if len(got.Markers) != 2 || got.Markers[0].Kind != "intro" || got.Markers[1].Kind != "credits" {
		t.Fatalf("markers of a relayed session = %+v, want the sharer's intro then credits", got.Markers)
	}
	credits := got.Markers[1]
	if out := postProgress(t, f.home, f.homeAdmin, dec.SessionID, credits.StartMs-50, http.StatusOK); out.Watched {
		t.Fatalf("just before the sharer's Credits: %+v, want unwatched", out)
	}
	if out := postProgress(t, f.home, f.homeAdmin, dec.SessionID, credits.StartMs, http.StatusOK); !out.Watched {
		t.Errorf("at the sharer's Credits (55%%): %+v, want watched", out)
	}
	getMarkers(t, f.home, f.homeAdmin, dec.SessionID, http.StatusOK)
	asked := 0
	for _, c := range f.rec.snapshot() {
		if strings.HasSuffix(c.Path, "/markers") {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("the sharer was asked for markers %d times, want once", asked)
	}
}

// TestMarkersOfARelayedSessionNeverAskThisServersProviders: the mirrored File is
// the sharer's, so a read of its Markers is the sharer's answer alone, even
// with a Marker provider installed here.
func TestMarkersOfARelayedSessionNeverAskThisServersProviders(t *testing.T) {
	t.Parallel()
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "intro", 0.1, 0.2, 0)}
	})
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MarkerManifest("example-markers", src.srv.URL))
	f := linkForRelay(t, testharness.WithDataDir(dataDir))
	f.sharer.Exec(`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		SELECT 'relay-credits', path, 'credits', 'local', duration_ms * 55 / 100, duration_ms FROM files WHERE path LIKE '%Dune (2021).mp4'`)
	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d, want 200; body: %s", status, body)
	}

	got := getMarkers(t, f.home, f.homeAdmin, dec.SessionID, http.StatusOK)
	if len(got.Markers) != 1 || got.Markers[0].Kind != "credits" {
		t.Errorf("markers of a relayed session = %+v, want the sharer's credits alone", got.Markers)
	}
	if asked := src.questions(); len(asked) != 0 {
		t.Errorf("this Server's provider was asked %d times about a relayed File, want never: %+v", len(asked), asked)
	}
}
