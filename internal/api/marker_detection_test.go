package api_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/markerdetect"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Marker detection black-box (ADR-0065 §4): what triggers it, the per-Library
// toggle only a TV Library has, the Admin's "detect markers now", and — with the
// real decoder — Detected Markers reaching a player beside the Local ones, which
// win where both cover a span.

// recordingAnalyzer announces every File detection listens to and hears nothing.
type recordingAnalyzer struct{ calls chan string }

func newRecordingAnalyzer() *recordingAnalyzer {
	return &recordingAnalyzer{calls: make(chan string, 256)}
}

func (r *recordingAnalyzer) Analyze(_ context.Context, path string, _, _ int64) (markerdetect.Print, error) {
	r.calls <- path
	return markerdetect.Print{}, nil
}

func (r *recordingAnalyzer) expectCall(t *testing.T) string {
	t.Helper()
	select {
	case p := <-r.calls:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("marker detection never listened to anything")
		return ""
	}
}

func (r *recordingAnalyzer) expectNoCall(t *testing.T, why string) {
	t.Helper()
	select {
	case p := <-r.calls:
		t.Fatalf("%s, yet marker detection listened to %s", why, p)
	case <-time.After(400 * time.Millisecond):
	}
}

func (r *recordingAnalyzer) drain() {
	for {
		select {
		case <-r.calls:
		case <-time.After(400 * time.Millisecond):
			return
		}
	}
}

// TestMarkerDetectionStartsOnlyAfterAScanCompletes: creating a TV Library,
// reading it and its toggle start nothing; the first File is listened to only
// once the scan has completed.
func TestMarkerDetectionStartsOnlyAfterAScanCompletes(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	srv.AuthGET("/api/v1/libraries/"+libID, token, nil)
	srv.AuthGET("/api/v1/libraries/"+libID+"/marker-detection", token, nil)
	an.expectNoCall(t, "no scan has run")

	if status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/scan", token, nil, nil); status != http.StatusAccepted {
		t.Fatalf("scan = %d; body: %s", status, body)
	}
	an.expectCall(t)
	var st scanStatusResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/scan", token, &st)
	if st.State == "running" {
		t.Fatal("detection listened while the scan was still running")
	}
	an.drain()

	// Nothing changed: a rescan finds every File already heard at its mtime.
	scanLib(t, srv, token, libID, "")
	an.expectNoCall(t, "a rescan changed nothing")
}

// TestMarkerDetectionNotTriggeredByAFailedScan: a scan that fails (one root is
// offline) queues no detection — not when it starts, not when it fails. The
// second scan starts with the first one's Files already in the catalog and never
// heard, so detection queued at a scan's start would find them.
func TestMarkerDetectionNotTriggeredByAFailedScan(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	_, lib, raw := createLibrary(t, srv, token, map[string]any{
		"name":        "Shows",
		"kind":        "tv",
		"rootFolders": []string{tvRoot(t), filepath.Join(t.TempDir(), "offline")},
	})
	if lib.ID == "" {
		t.Fatalf("tv library not created; body: %s", raw)
	}
	for i := range 2 {
		if st := scanLib(t, srv, token, lib.ID, ""); st.State != "error" {
			t.Fatalf("scan %d settled %q, want error (a root is offline)", i+1, st.State)
		}
		an.expectNoCall(t, fmt.Sprintf("scan %d failed", i+1))
	}
	if len(listShows(t, srv, token, lib.ID).Shows) == 0 {
		t.Fatal("the failed scan catalogued nothing, so this proves nothing")
	}
}

// TestMarkerDetectionWaitsForAVideoCopyTranscode: a Transcode that copies the
// video and re-encodes only the audio holds no slot of the transcode cap, but it
// is a Transcode running on this host, so detection queued after a scan does not
// start until it ends.
func TestMarkerDetectionWaitsForAVideoCopyTranscode(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	requireAudioFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	movies := createMovieLibrary(t, srv, token, audioStreamsRoot(t))
	scanLib(t, srv, token, movies, "")
	id := findTitle(t, listAllTitles(t, srv, token, movies), "Audio Movie")
	dts := audioStreamByLabel(t, negotiateAudio(t, srv, token, id, mkvMultiAudioProfile()), "English Director's Commentary")
	dec := negotiateAudio(t, srv, token, id, withAudioStreamId(mkvMultiAudioProfile(), dts.ID))
	if dec.Tier != "transcode" {
		t.Fatalf("dts selection tier = %q, want transcode", dec.Tier)
	}
	var load struct {
		Transcodes struct {
			Active int `json:"active"`
		} `json:"transcodes"`
	}
	if st, body := srv.AuthGET("/api/v1/transcoding", token, &load); st != http.StatusOK || load.Transcodes.Active != 0 {
		t.Fatalf("transcoding = %d %s, want the video copy to hold no cap slot", st, body)
	}

	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	an.expectNoCall(t, "a video-copy Transcode is running")

	if st, body := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil); st != http.StatusNoContent {
		t.Fatalf("end session = %d; body: %s", st, body)
	}
	an.expectCall(t)
}

// TestMarkerDetectionOffWithoutFFmpeg: on a host with no usable ffmpeg,
// detection does not run at all — a completed scan listens to nothing, the
// toggle says detection is unavailable rather than on, and "detect markers now"
// says it is unavailable rather than accepting work it would only fail.
func TestMarkerDetectionOffWithoutFFmpeg(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithFFmpegAvailability(false), testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	var got struct {
		Enabled   bool  `json:"enabled"`
		Available *bool `json:"available"`
	}
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/marker-detection", token, &got); status != http.StatusOK || got.Available == nil || *got.Available {
		t.Fatalf("toggle without ffmpeg = %d %s, want 200 available:false", status, body)
	}
	scanLib(t, srv, token, libID, "")
	an.expectNoCall(t, "there is no ffmpeg")
	showID, _ := bearSeason1Episodes(t, srv, token, libID)
	if status, body := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/detect-markers", token, nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("detect-markers without ffmpeg = %d, want 503; body: %s", status, body)
	}
	an.expectNoCall(t, "there is no ffmpeg")
}

// TestDetectMarkersNowOnAShow: the Admin's action listens to that Show's Files
// immediately, even ones already heard; a Member may not, an unknown Show is 404.
func TestDetectMarkersNowOnAShow(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	an.drain()
	showID, _ := bearSeason1Episodes(t, srv, token, libID)

	srv.CreateMember("member", "memberpass123")
	member := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/detect-markers", member, nil, nil); status != http.StatusForbidden {
		t.Errorf("member detect-markers = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/nope/detect-markers", token, nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown show detect-markers = %d, want 404", status)
	}
	an.expectNoCall(t, "nothing was accepted")

	if status, body := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/detect-markers", token, nil, nil); status != http.StatusAccepted {
		t.Fatalf("detect-markers = %d, want 202; body: %s", status, body)
	}
	if p := an.expectCall(t); !strings.Contains(p, "The Bear") {
		t.Errorf("detect-markers on The Bear listened to %s", p)
	}
}

// TestMarkerDetectionToggleIsTVOnly: a TV Library's toggle is on by default and
// turning it off stops post-scan detection; a music or movie Library has no
// toggle at all.
func TestMarkerDetectionToggleIsTVOnly(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	requireMusicFixtures(t)
	an := newRecordingAnalyzer()
	srv := testharness.New(t, testharness.WithMarkerAnalyzer(an))
	token := adminToken(t, srv)
	tvID := createTVLibrary(t, srv, token, tvRoot(t))

	var got struct {
		Enabled   bool `json:"enabled"`
		Available bool `json:"available"`
	}
	if status, body := srv.AuthGET("/api/v1/libraries/"+tvID+"/marker-detection", token, &got); status != http.StatusOK || !got.Enabled || !got.Available {
		t.Fatalf("TV toggle = %d %s, want 200 enabled and available", status, body)
	}
	if status, body := srv.JSON(http.MethodPut, "/api/v1/libraries/"+tvID+"/marker-detection", token, map[string]any{"enabled": false}, &got); status != http.StatusOK || got.Enabled {
		t.Fatalf("PUT off = %d %s, want 200 disabled", status, body)
	}
	scanLib(t, srv, token, tvID, "")
	an.expectNoCall(t, "detection is off for this Library")

	musicID := createMusicLibrary(t, srv, token, musicRoot(t))
	movieID := createMovieLibrary(t, srv, token, t.TempDir())
	for name, id := range map[string]string{"music": musicID, "movie": movieID} {
		if status, _ := srv.AuthGET("/api/v1/libraries/"+id+"/marker-detection", token, nil); status != http.StatusNotFound {
			t.Errorf("%s toggle GET = %d, want 404 (no toggle at all)", name, status)
		}
		if status, _ := srv.JSON(http.MethodPut, "/api/v1/libraries/"+id+"/marker-detection", token, map[string]any{"enabled": true}, nil); status != http.StatusNotFound {
			t.Errorf("%s toggle PUT = %d, want 404", name, status)
		}
	}
	scanLib(t, srv, token, musicID, "")
	an.expectNoCall(t, "a music Library has no detection")

	srv.CreateMember("member", "memberpass123")
	member := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	if status, _ := srv.AuthGET("/api/v1/libraries/"+tvID+"/marker-detection", member, nil); status != http.StatusForbidden {
		t.Errorf("member toggle GET = %d, want 403", status)
	}
}

// TestDetectedMarkersReachThePlayerAndLocalWins drives the real decoder over
// three generated episodes sharing an intro (at different offsets) and a closing
// segment. Episode 2's session serves Detected Intro and Credits; episode 1 has
// an `.edl` naming its Intro, and that Local one is what its session serves.
func TestDetectedMarkersReachThePlayerAndLocalWins(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "Echo Show (2020)", "Season 01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	offsets := []float64{2, 6.5, 11}
	for i, off := range offsets {
		writeDetectionEpisode(t, filepath.Join(dir, fmt.Sprintf("Echo Show (2020) - S01E%02d.mkv", i+1)), i, off)
	}
	// Episode 1's own Local Intro, deliberately not where detection will put it.
	if err := os.WriteFile(filepath.Join(dir, "Echo Show (2020) - S01E01.edl"), []byte("1.0 21.0 0 Intro\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := testharness.New(t, testharness.WithMarkerAnalyzer(markerdetect.FFmpeg{}))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	showID := findShow(t, listShows(t, srv, token, libID), "Echo Show")
	seasons := showSeasons(t, srv, token, showID)
	eps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	if len(eps.Episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(eps.Episodes))
	}

	ep2 := negotiateEpisode(t, srv, token, eps.Episodes[1].ID)
	got := waitForMarkers(t, srv, token, ep2.SessionID, 2)
	intro, credits := got.Markers[0], got.Markers[1]
	if intro.Kind != "intro" || intro.Source != "detected" || absMs(intro.StartMs-6500) > 1500 || absMs(intro.EndMs-26500) > 1500 {
		t.Errorf("episode 2 intro = %+v, want detected ~[6500, 26500)", intro)
	}
	if credits.Kind != "credits" || credits.Source != "detected" || absMs(credits.StartMs-(6500+20000+40000)) > 1500 {
		t.Errorf("episode 2 credits = %+v, want detected from ~66500", credits)
	}

	ep1 := negotiateEpisode(t, srv, token, eps.Episodes[0].ID)
	got = waitForMarkers(t, srv, token, ep1.SessionID, 2)
	if m := got.Markers[0]; m.Kind != "intro" || m.Source != "local" || m.StartMs != 1000 || m.EndMs != 21000 {
		t.Errorf("episode 1 intro = %+v, want the Local one [1000, 21000)", m)
	}
	if m := got.Markers[1]; m.Kind != "credits" || m.Source != "detected" {
		t.Errorf("episode 1 second marker = %+v, want the Detected credits", m)
	}
}

// writeDetectionEpisode renders a tiny-video episode: unique noise, a shared
// 20 s theme at offset, 40 s of unique noise, a shared 20 s ending.
func writeDetectionEpisode(t *testing.T, path string, seed int, offset float64) {
	t.Helper()
	const themeSrc = "aevalsrc='0.3*sin(2*PI*t*(330+110*floor(mod(t*3,7))))+0.05*sin(2*PI*t*97)':s=44100"
	const endingSrc = "aevalsrc='0.3*sin(2*PI*t*(520-60*floor(mod(t*2.5,5))))*(0.6+0.4*sin(2*PI*t*0.8))':s=44100"
	noise := func(s int) string { return fmt.Sprintf("anoisesrc=c=pink:a=0.25:r=44100:s=%d", s) }
	pieces := []struct {
		src  string
		secs float64
	}{{noise(10 + seed), offset}, {themeSrc, 20}, {noise(20 + seed), 40}, {endingSrc, 20}}
	total := 0.0
	args := []string{"-y", "-nostdin", "-loglevel", "error"}
	var fc strings.Builder
	for i, p := range pieces {
		args = append(args, "-f", "lavfi", "-t", fmt.Sprintf("%.3f", p.secs), "-i", p.src)
		fmt.Fprintf(&fc, "[%d:a]", i)
		total += p.secs
	}
	fmt.Fprintf(&fc, "concat=n=%d:v=0:a=1[a]", len(pieces))
	args = append(args,
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=64x48:r=2:d=%.3f", total),
		"-filter_complex", fc.String(),
		"-map", fmt.Sprintf("%d:v", len(pieces)), "-map", "[a]",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

// waitForMarkers polls a session's Markers until n have been served.
func waitForMarkers(t *testing.T, srv *testharness.Server, token, sessionID string, n int) markersResp {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := getMarkers(t, srv, token, sessionID, http.StatusOK)
		if len(got.Markers) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("session markers = %+v, want %d", got.Markers, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func absMs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
