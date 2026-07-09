package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box integration tests for issue 08: progress reporting, the server-side
// Watched threshold, manual watchState toggle, the computed /home rows, watch
// state surviving a file rename (identity-keyed), concurrent last-write-wins,
// owner-only progress, and session reaping. All driven through the HTTP API via
// the shared harness; fixtures are ~1s clips so a positionMs near the end
// crosses the 90% ceiling.

// --- wire shapes (issue 08 additions) ---------------------------------------

type wsFileResp struct {
	ID         string `json:"id"`
	DurationMs int64  `json:"durationMs"`
}

type wsEditionResp struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Files []wsFileResp `json:"files"`
}

type wsTitleDetailResp struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	ResumePositionMs int64           `json:"resumePositionMs"`
	Watched          bool            `json:"watched"`
	Editions         []wsEditionResp `json:"editions"`
}

type wsProgressResp struct {
	TitleID          string `json:"titleId"`
	ResumePositionMs int64  `json:"resumePositionMs"`
	Watched          bool   `json:"watched"`
}

type wsHomeTitleResp struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	ResumePositionMs int64  `json:"resumePositionMs"`
}

type wsHomeResp struct {
	ContinueWatching []wsHomeTitleResp `json:"continueWatching"`
	RecentlyAdded    []wsHomeTitleResp `json:"recentlyAdded"`
}

// negotiateDune negotiates direct play for the Dune fixture and returns the
// decision (session id + stream url).
func negotiateDune(t *testing.T, srv *testharness.Server, token, titleID string) decisionResp {
	t.Helper()
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	return dec
}

// titleDuration reads a Title's first File duration (ms) from its detail.
func titleDuration(t *testing.T, srv *testharness.Server, token, titleID string) int64 {
	t.Helper()
	var d wsTitleDetailResp
	if status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d); status != http.StatusOK {
		t.Fatalf("get title status = %d; body: %s", status, body)
	}
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 {
		t.Fatalf("title %q has no files", titleID)
	}
	dur := d.Editions[0].Files[0].DurationMs
	if dur <= 0 {
		t.Fatalf("title %q duration = %d, want > 0", titleID, dur)
	}
	return dur
}

// postProgress reports a raw position against a session and returns the resolved
// watch state. wantStatus asserts the HTTP status.
func postProgress(t *testing.T, srv *testharness.Server, token, sessionID string, posMs int64, wantStatus int) wsProgressResp {
	t.Helper()
	var out wsProgressResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+sessionID+"/progress", token,
		map[string]any{"positionMs": posMs, "state": "playing"}, &out)
	if status != wantStatus {
		t.Fatalf("progress status = %d, want %d; body: %s", status, wantStatus, body)
	}
	return out
}

func getTitleDetail(t *testing.T, srv *testharness.Server, token, titleID string) wsTitleDetailResp {
	t.Helper()
	var d wsTitleDetailResp
	if status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d); status != http.StatusOK {
		t.Fatalf("get title status = %d; body: %s", status, body)
	}
	return d
}

// TestProgressUpdatesResume: an in-band progress report stores the raw position
// as the resume offset, visible on the Title detail.
func TestProgressUpdatesResume(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	dur := titleDuration(t, srv, token, duneID)
	dec := negotiateDune(t, srv, token, duneID)

	mid := dur / 2 // ~50%: safely between the 2% floor and 90% ceiling
	out := postProgress(t, srv, token, dec.SessionID, mid, http.StatusOK)
	if out.ResumePositionMs != mid {
		t.Errorf("progress resume = %d, want %d", out.ResumePositionMs, mid)
	}
	if out.Watched {
		t.Errorf("watched = true at ~50%%, want false")
	}

	d := getTitleDetail(t, srv, token, duneID)
	if d.ResumePositionMs != mid {
		t.Errorf("title detail resumePositionMs = %d, want %d", d.ResumePositionMs, mid)
	}
	if d.Watched {
		t.Errorf("title detail watched = true, want false")
	}
}

// TestProgressCrossing90MarksWatched: a position past ~90% marks the Title
// watched, clears its resume, and removes it from Continue Watching.
func TestProgressCrossing90MarksWatched(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	dur := titleDuration(t, srv, token, duneID)
	dec := negotiateDune(t, srv, token, duneID)

	// First a mid report so it would be in Continue Watching, then cross 90%.
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)
	out := postProgress(t, srv, token, dec.SessionID, dur-1, http.StatusOK) // ~100% ≥ 90%
	if !out.Watched {
		t.Errorf("watched = false after crossing 90%%, want true")
	}
	if out.ResumePositionMs != 0 {
		t.Errorf("resume = %d after watched, want 0 (cleared)", out.ResumePositionMs)
	}

	d := getTitleDetail(t, srv, token, duneID)
	if !d.Watched || d.ResumePositionMs != 0 {
		t.Errorf("title detail watched=%v resume=%d, want watched=true resume=0", d.Watched, d.ResumePositionMs)
	}

	// Removed from Continue Watching.
	var home wsHomeResp
	srv.AuthGET("/api/v1/home", token, &home)
	for _, cw := range home.ContinueWatching {
		if cw.ID == duneID {
			t.Errorf("Dune still in Continue Watching after crossing 90%%")
		}
	}
}

// TestProgressBelowFloorNoResume: a stop below the ~2% floor records no resume,
// so the Title never enters Continue Watching.
func TestProgressBelowFloorNoResume(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	titleDuration(t, srv, token, duneID) // ensure duration is known
	dec := negotiateDune(t, srv, token, duneID)

	out := postProgress(t, srv, token, dec.SessionID, 1, http.StatusOK) // 1ms ≈ 0%
	if out.ResumePositionMs != 0 {
		t.Errorf("resume = %d below floor, want 0 (not recorded)", out.ResumePositionMs)
	}

	d := getTitleDetail(t, srv, token, duneID)
	if d.ResumePositionMs != 0 {
		t.Errorf("title detail resume = %d, want 0", d.ResumePositionMs)
	}

	var home wsHomeResp
	srv.AuthGET("/api/v1/home", token, &home)
	for _, cw := range home.ContinueWatching {
		if cw.ID == duneID {
			t.Errorf("Dune in Continue Watching after a below-floor stop")
		}
	}
}

// TestWatchStateManualToggle: PUT /titles/{id}/watchState toggles watched
// bypassing the threshold; unwatched resets.
func TestWatchStateManualToggle(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	// Mark watched with no playback at all — bypasses the threshold entirely.
	var out wsProgressResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+duneID+"/watchState", token,
		map[string]any{"watched": true}, &out)
	if status != http.StatusOK {
		t.Fatalf("put watchState status = %d, want 200; body: %s", status, body)
	}
	if !out.Watched {
		t.Errorf("watched = false after manual mark, want true")
	}
	if d := getTitleDetail(t, srv, token, duneID); !d.Watched {
		t.Errorf("title detail watched = false after manual mark, want true")
	}

	// Toggle back to unwatched: clears watched + resume.
	status, body = srv.JSON(http.MethodPut, "/api/v1/titles/"+duneID+"/watchState", token,
		map[string]any{"watched": false}, &out)
	if status != http.StatusOK {
		t.Fatalf("put watchState (unwatch) status = %d; body: %s", status, body)
	}
	if out.Watched || out.ResumePositionMs != 0 {
		t.Errorf("after unwatch: watched=%v resume=%d, want false/0", out.Watched, out.ResumePositionMs)
	}

	// Unknown Title → 404 (hide existence).
	status, _ = srv.JSON(http.MethodPut, "/api/v1/titles/no-such/watchState", token,
		map[string]any{"watched": true}, nil)
	if status != http.StatusNotFound {
		t.Errorf("watchState unknown title = %d, want 404", status)
	}
}

// TestHomeRows: /home returns Continue Watching (in-band, most-recent first) and
// Recently Added.
func TestHomeRows(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")
	bladeID := findTitle(t, list, "Blade Runner")

	duneDur := titleDuration(t, srv, token, duneID)

	// Put Dune in Continue Watching first.
	duneDec := negotiateDune(t, srv, token, duneID)
	postProgress(t, srv, token, duneDec.SessionID, duneDur/2, http.StatusOK)

	// Then Blade Runner — its profile is mkv; reuse mp4Profile won't direct-play
	// it, so toggle resume via a session against Dune is not possible. Instead use
	// the manual path is for watched only; Continue Watching needs a resume, which
	// only progress writes. Blade Runner negotiation needs an mkv-capable profile.
	var bladeDec decisionResp
	mkv := map[string]any{
		"deviceProfile": map[string]any{
			"containers":  []string{"mkv", "mp4"},
			"videoCodecs": []map[string]any{{"codec": "mpeg4"}, {"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs": []string{"mp3", "aac"},
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, mkv, &bladeDec)
	if status != http.StatusOK {
		t.Fatalf("blade playback status = %d; body: %s", status, body)
	}
	bladeDur := titleDuration(t, srv, token, bladeID)
	// Report Blade Runner LAST so it is the most-recently-played. watch_state's
	// updated_at is millisecond-resolution (strftime %f), and Continue Watching
	// orders by it; without a gap the two writes can land in the same millisecond
	// and the tie falls to a non-deterministic id order. A short pause guarantees
	// Blade's timestamp is strictly later, matching the "played later" scenario.
	time.Sleep(10 * time.Millisecond)
	postProgress(t, srv, token, bladeDec.SessionID, bladeDur/2, http.StatusOK)

	var home wsHomeResp
	if status, body := srv.AuthGET("/api/v1/home", token, &home); status != http.StatusOK {
		t.Fatalf("home status = %d; body: %s", status, body)
	}

	// Continue Watching: both present, Blade Runner first (most recent).
	if len(home.ContinueWatching) != 2 {
		t.Fatalf("continueWatching len = %d, want 2; %+v", len(home.ContinueWatching), home.ContinueWatching)
	}
	if home.ContinueWatching[0].ID != bladeID {
		t.Errorf("continueWatching[0] = %q, want Blade Runner (most recent)", home.ContinueWatching[0].Title)
	}
	if home.ContinueWatching[0].ResumePositionMs != bladeDur/2 {
		t.Errorf("continueWatching[0] resume = %d, want %d", home.ContinueWatching[0].ResumePositionMs, bladeDur/2)
	}

	// Recently Added: all three fixtures present.
	if len(home.RecentlyAdded) != 3 {
		t.Errorf("recentlyAdded len = %d, want 3", len(home.RecentlyAdded))
	}
}

// TestWatchStateSurvivesRename: watch state is keyed to the Title identity, so a
// file rename (same Title id) preserves the resume position.
func TestWatchStateSurvivesRename(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Stable Movie (2006)")
	makeMovie(t, filepath.Join(folder, "Stable Movie (2006).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	if len(list.Titles) != 1 {
		t.Fatalf("titles = %d, want 1", len(list.Titles))
	}
	titleID := list.Titles[0].ID

	// Record a mid resume via a real progress report.
	dur := titleDuration(t, srv, token, titleID)
	dec := negotiateDune(t, srv, token, titleID)
	resume := dur / 2
	postProgress(t, srv, token, dec.SessionID, resume, http.StatusOK)

	// Rename the file within the folder (an Edition/file change, identity stable).
	if err := os.Rename(
		filepath.Join(folder, "Stable Movie (2006).mp4"),
		filepath.Join(folder, "Stable Movie (2006) - 1080p.mkv"),
	); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")

	after := listAllTitles(t, srv, token, libID)
	if len(after.Titles) != 1 || after.Titles[0].ID != titleID {
		t.Fatalf("after rename: titles=%d id=%q, want 1 / %q", len(after.Titles), after.Titles[0].ID, titleID)
	}
	// Watch state survived: same Title id still carries the resume.
	d := getTitleDetail(t, srv, token, titleID)
	if d.ResumePositionMs != resume {
		t.Errorf("resume after rename = %d, want %d (watch state must survive)", d.ResumePositionMs, resume)
	}
}

// TestConcurrentProgressLastWriteWins: two Devices reporting progress for the
// same Title resolve last-write-wins without error.
func TestConcurrentProgressLastWriteWins(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	dur := titleDuration(t, srv, token, duneID)

	// Two separate sessions (two Devices) over the same Title.
	decA := negotiateDune(t, srv, token, duneID)
	decB := negotiateDune(t, srv, token, duneID)

	var wg sync.WaitGroup
	wg.Add(2)
	posA := dur / 4
	posB := dur / 2
	go func() { defer wg.Done(); postProgress(t, srv, token, decA.SessionID, posA, http.StatusOK) }()
	go func() { defer wg.Done(); postProgress(t, srv, token, decB.SessionID, posB, http.StatusOK) }()
	wg.Wait()

	// A definitive last write settles the value (no error from the race above).
	out := postProgress(t, srv, token, decB.SessionID, posB, http.StatusOK)
	if out.ResumePositionMs != posB {
		t.Errorf("final resume = %d, want %d (last-write-wins)", out.ResumePositionMs, posB)
	}
	d := getTitleDetail(t, srv, token, duneID)
	if d.ResumePositionMs != posB {
		t.Errorf("title detail resume = %d, want %d", d.ResumePositionMs, posB)
	}
}

// TestProgressOwnerOnly: another User cannot report progress against a session —
// it is 404 (hide existence), like stream/delete.
func TestProgressOwnerOnly(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	dec := negotiateDune(t, srv, token, duneID)

	srv.CreateMember("member", "memberpass123")
	other := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	status, _ := srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/progress", other,
		map[string]any{"positionMs": 500, "state": "playing"}, nil)
	if status != http.StatusNotFound {
		t.Errorf("other-user progress = %d, want 404", status)
	}
	// Unauthenticated → 401.
	status, _ = srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/progress", "",
		map[string]any{"positionMs": 500, "state": "playing"}, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unauth progress = %d, want 401", status)
	}
}

// TestSessionReaped: a session idle past a short configured timeout is reaped;
// its progress endpoint then answers 404.
func TestSessionReaped(t *testing.T) {
	requireFixtures(t)
	// Short idle timeout so the reaper sweeps the session quickly. The reaper
	// sweep cadence tracks the timeout (app caps it at the timeout), so a 150ms
	// timeout is swept within ~150ms.
	srv := testharness.New(t, testharness.WithSessionIdleTimeout(150*time.Millisecond))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	dec := negotiateDune(t, srv, token, duneID)

	// Progress works immediately (session live).
	postProgress(t, srv, token, dec.SessionID, 1, http.StatusOK)

	// Stay SILENT past the idle window (no progress reports — those would Touch
	// the session and reset the idle clock, defeating the reaper). We poll for the
	// reaped state via the stream endpoint, which does NOT count as keepalive.
	deadline := time.Now().Add(5 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		resp := authStream(t, srv, dec.StreamURL, token, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			reaped = true
			break
		}
	}
	if !reaped {
		t.Fatalf("session not reaped within deadline")
	}

	// Progress against the reaped session is now 404 (acceptance criterion).
	postProgress(t, srv, token, dec.SessionID, 1, http.StatusNotFound)
}
