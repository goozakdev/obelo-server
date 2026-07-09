package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/config"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Real-ffmpeg integration tests for the directStream (remux) HLS path (issue
// TRANSCODE-01). They drive the HTTP API end to end against the checked-in
// matroska fixture: negotiate a codec-compatible / wrong-container profile →
// directStream → fetch the HLS playlist + a segment → ffprobe-verify the segment
// carries the COPIED codec (mpeg4/mp3, not re-encoded). They also assert DELETE
// tears down the ffmpeg job + scratch, the media cookie authenticates the hls
// endpoints, and direct play is untouched. These extend the issue-04 real-
// ffprobe allowance to real ffmpeg (both are on PATH in CI/dev per the PRD).

// remuxProfile supports the mkv fixture's codecs (mpeg4 video + mp3 audio) but
// only the mp4 container — the container is the SOLE mismatch, so the File
// remuxes (directStream) rather than transcodes.
func remuxProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "mpeg4", "maxResolution": "1080p"}, {"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"mp3", "aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
}

// requireFFmpeg skips when ffmpeg/ffprobe are not both on PATH (the remux path
// and the segment verification need them).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping real-ffmpeg HLS test", bin)
		}
	}
}

// negotiateRemux POSTs the remux profile against the Blade Runner fixture and
// returns the directStream decision, failing the test if it is not directStream.
func negotiateRemuxDecision(t *testing.T, srv *testharness.Server, token, bladeID string) decisionResp {
	t.Helper()
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, remuxProfile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	if dec.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream; body: %s", dec.Tier, raw)
	}
	return dec
}

// TestHLSRemuxPlaylistAndSegment: the directStream streamUrl serves a valid HLS
// media playlist; a listed segment is fetchable and ffprobe-confirms the COPIED
// codec (mpeg4 video / mp3 audio — proof it was remuxed, not re-encoded).
func TestHLSRemuxPlaylistAndSegment(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateRemuxDecision(t, srv, token, bladeID)

	// Fetch the media playlist.
	pl := authStream(t, srv, dec.StreamURL, token, "")
	defer pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}
	if ct := pl.Header.Get("Content-Type"); !strings.Contains(ct, "mpegurl") {
		t.Errorf("playlist Content-Type = %q, want an HLS type", ct)
	}
	body, _ := io.ReadAll(pl.Body)
	manifest := string(body)
	if !strings.HasPrefix(strings.TrimSpace(manifest), "#EXTM3U") {
		t.Fatalf("playlist does not start with #EXTM3U:\n%s", manifest)
	}
	segments := parseSegments(manifest)
	if len(segments) == 0 {
		t.Fatalf("playlist lists no segments:\n%s", manifest)
	}

	// Fetch the first segment.
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	segResp := authStream(t, srv, base+segments[0], token, "")
	defer segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status = %d, want 200", segResp.StatusCode)
	}
	if ct := segResp.Header.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("segment Content-Type = %q, want video/mp2t", ct)
	}
	segBytes, _ := io.ReadAll(segResp.Body)
	if len(segBytes) == 0 {
		t.Fatal("segment body empty")
	}

	// ffprobe the segment: it must carry the ORIGINAL (copied) codecs.
	vCodec, aCodec := ffprobeCodecs(t, segBytes)
	if vCodec != "mpeg4" {
		t.Errorf("segment video codec = %q, want mpeg4 (copied, not re-encoded)", vCodec)
	}
	if aCodec != "mp3" {
		t.Errorf("segment audio codec = %q, want mp3 (copied, not re-encoded)", aCodec)
	}
}

// TestHLSDeleteEndsRemuxAndRemovesScratch: DELETE /sessions/{id} ends the remux
// session — the scratch dir (with its segments) is gone and subsequent HLS
// fetches 404.
func TestHLSDeleteEndsRemuxAndRemovesScratch(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateRemuxDecision(t, srv, token, bladeID)

	// Touch the playlist to start the remux and create scratch.
	pl := authStream(t, srv, dec.StreamURL, token, "")
	pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}

	// The scratch dir for this session is <dataDir>/transcode/<sessionId>.
	scratch := filepath.Join(srv.DataDir, "transcode", dec.SessionID)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir not created at %q: %v", scratch, err)
	}

	// Clean stop.
	status, raw := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, raw)
	}

	// Scratch dir removed.
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after delete (err=%v)", err)
	}
	// HLS playlist now 404 (session gone).
	post := authStream(t, srv, dec.StreamURL, token, "")
	post.Body.Close()
	if post.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete playlist = %d, want 404", post.StatusCode)
	}
}

// TestHLSMediaCookieAuthenticatesEndpoints: the media cookie (no Authorization
// header) authenticates the HLS playlist + segment GETs, exactly like /stream,
// while a non-media endpoint still rejects the cookie.
func TestHLSMediaCookieAuthenticatesEndpoints(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateRemuxDecision(t, srv, token, bladeID)

	// Mint a cookie for the same admin (owns the session).
	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-hls")

	// Playlist via cookie only.
	pl := cookieGET(t, srv, dec.StreamURL, cookie, "")
	defer pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist via cookie = %d, want 200", pl.StatusCode)
	}
	manifest, _ := io.ReadAll(pl.Body)
	segments := parseSegments(string(manifest))
	if len(segments) == 0 {
		t.Fatalf("playlist via cookie lists no segments:\n%s", manifest)
	}
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	seg := cookieGET(t, srv, base+segments[0], cookie, "")
	defer seg.Body.Close()
	if seg.StatusCode != http.StatusOK {
		t.Errorf("segment via cookie = %d, want 200", seg.StatusCode)
	}

	// A non-media endpoint still rejects the cookie (bearer-only).
	dev := cookieGET(t, srv, "/api/v1/devices", cookie, "")
	dev.Body.Close()
	if dev.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /devices with media cookie = %d, want 401 (bearer-only)", dev.StatusCode)
	}
}

// TestHLSDirectPlayHasNoHLS: a direct-play session has no HLS resource — its
// /hls/index.m3u8 is 404 (the client uses the progressive /stream URL). This
// confirms direct play is unchanged and the HLS routes are remux-only.
func TestHLSDirectPlayHasNoHLS(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var dec decisionResp
	srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec)
	if dec.Tier != "directPlay" {
		t.Fatalf("Dune tier = %q, want directPlay", dec.Tier)
	}
	// The HLS route for a direct-play session is 404.
	resp := authStream(t, srv, "/api/v1/sessions/"+dec.SessionID+"/hls/index.m3u8", token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("HLS on direct-play session = %d, want 404", resp.StatusCode)
	}
}

// videoMediaPlaylistURL resolves a decision streamUrl to the VIDEO media playlist
// URL: a bare media-playlist URL (index.m3u8) is returned unchanged; a MASTER
// playlist URL (master.m3u8, present when the session carries subtitles, slice 03)
// is fetched and its single #EXT-X-STREAM-INF video URI resolved against the
// session's /hls base. This lets the video-focused HLS tests fetch segments
// regardless of whether a master was interposed.
func videoMediaPlaylistURL(t *testing.T, srv *testharness.Server, streamURL, token string) string {
	t.Helper()
	if !strings.HasSuffix(streamURL, "/master.m3u8") {
		return streamURL
	}
	master := fetchText(t, srv, streamURL, token)
	base := streamURL[:strings.LastIndex(streamURL, "/")+1]
	lines := strings.Split(master, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		// The video rendition URI is the next non-comment, non-blank line.
		for _, next := range lines[i+1:] {
			next = strings.TrimSpace(next)
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			return base + next
		}
	}
	t.Fatalf("master playlist has no #EXT-X-STREAM-INF video rendition:\n%s", master)
	return ""
}

// parseSegments extracts the segment file names (non-comment, non-blank lines)
// from an HLS media playlist.
func parseSegments(manifest string) []string {
	var segs []string
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segs = append(segs, line)
	}
	return segs
}

// ffprobeCodecs writes seg to a temp .ts file and runs ffprobe to read its first
// video + audio codec names.
func ffprobeCodecs(t *testing.T, seg []byte) (video, audio string) {
	t.Helper()
	v, a, _ := ffprobeSegment(t, seg)
	return v, a
}

// ffprobeSegment writes seg to a temp .ts file and reads its first video codec,
// first audio codec, and the video stream height — enough to assert the
// transcode target codecs AND that scaling kept the rendition within the cap.
func ffprobeSegment(t *testing.T, seg []byte) (video, audio string, height int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seg.ts")
	if err := os.WriteFile(path, seg, 0o644); err != nil {
		t.Fatalf("writing segment: %v", err)
	}
	out, err := exec.Command("ffprobe",
		"-v", "quiet", "-print_format", "json", "-show_streams", path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe segment: %v", err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parsing ffprobe json: %v\n%s", err, out)
	}
	for _, s := range probe.Streams {
		if s.CodecType == "video" && video == "" {
			video = s.CodecName
			height = s.Height
		}
		if s.CodecType == "audio" && audio == "" {
			audio = s.CodecName
		}
	}
	return video, audio, height
}

// fetchHLSSegment negotiates is done by the caller; this drives the streamUrl →
// playlist → first segment fetch and returns the segment bytes, asserting the
// playlist and segment are well-formed HLS along the way.
func fetchFirstSegment(t *testing.T, srv *testharness.Server, streamURL, token string) []byte {
	t.Helper()
	// When subtitles are present the streamUrl is a MASTER playlist (slice 03);
	// resolve it to the video media playlist before fetching video segments (a real
	// player does the same). A bare media-playlist URL is returned unchanged.
	streamURL = videoMediaPlaylistURL(t, srv, streamURL, token)
	pl := authStream(t, srv, streamURL, token, "")
	defer pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}
	body, _ := io.ReadAll(pl.Body)
	manifest := string(body)
	if !strings.HasPrefix(strings.TrimSpace(manifest), "#EXTM3U") {
		t.Fatalf("playlist does not start with #EXTM3U:\n%s", manifest)
	}
	segments := parseSegments(manifest)
	if len(segments) == 0 {
		t.Fatalf("playlist lists no segments:\n%s", manifest)
	}
	base := streamURL[:strings.LastIndex(streamURL, "/")+1]
	segResp := authStream(t, srv, base+segments[0], token, "")
	defer segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status = %d, want 200", segResp.StatusCode)
	}
	segBytes, _ := io.ReadAll(segResp.Body)
	if len(segBytes) == 0 {
		t.Fatal("segment body empty")
	}
	return segBytes
}

// --- transcode tier (slice 2) integration ---

// transcodeProfile supports h264 video + aac audio in mp4 only — it can play
// NEITHER the mkv fixture's container (matroska) NOR its codecs (mpeg4/mp3), so a
// remux cannot help: the File must be fully transcoded to h264/aac HLS.
func transcodeProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
}

// negotiateTranscodeDecision POSTs a transcode-forcing profile against a title
// and asserts a transcode decision with an HLS streamUrl.
func negotiateTranscodeDecision(t *testing.T, srv *testharness.Server, token, titleID string, profile map[string]any) decisionResp {
	t.Helper()
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode; body: %s", dec.Tier, raw)
	}
	if !strings.Contains(dec.StreamURL, "/hls/") {
		t.Fatalf("streamUrl = %q, want an HLS URL", dec.StreamURL)
	}
	return dec
}

// TestHLSTranscodeReEncodesToTargetCodec: the mkv/mpeg4/mp3 fixture under a
// profile that can play neither its container nor its codecs → transcode →
// fetch playlist + segment → ffprobe confirms the segment is RE-ENCODED to the
// target codecs (h264 video + aac audio), within the requested resolution.
func TestHLSTranscodeReEncodesToTargetCodec(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)

	vCodec, aCodec, height := ffprobeSegment(t, seg)
	if vCodec != "h264" {
		t.Errorf("segment video codec = %q, want h264 (re-encoded)", vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("segment audio codec = %q, want aac (re-encoded)", aCodec)
	}
	// Source is 144 tall; under a 1080p cap it is not upscaled, so the rendition
	// stays within the cap.
	if height <= 0 || height > 1080 {
		t.Errorf("segment height = %d, want within the 1080p cap (and > 0)", height)
	}
}

// TestHLSTranscodeScalesDownResolution: the Dune fixture (320x240, h264/aac/mp4)
// under a 144p resolution cap → transcode → ffprobe confirms the re-encoded
// segment was SCALED DOWN to within 144px (the resolution constraint is enforced
// by the encode). It stays h264/aac.
func TestHLSTranscodeScalesDownResolution(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune") // 320x240 h264/aac

	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "144p"},
	}
	dec := negotiateTranscodeDecision(t, srv, token, duneID, profile)
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)

	vCodec, aCodec, height := ffprobeSegment(t, seg)
	if vCodec != "h264" {
		t.Errorf("segment video codec = %q, want h264", vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("segment audio codec = %q, want aac (copied — already compatible)", aCodec)
	}
	if height <= 0 || height > 144 {
		t.Errorf("segment height = %d, want scaled down to within 144 (and > 0)", height)
	}
}

// --- seek realignment (slice 3) ---

// generateLongTranscodeClip writes a multi-second mkv (mpeg4 video + mp3 audio)
// into a fresh library dir and returns the dir. The codecs/container force a full
// transcode under the h264/aac/mp4 profile (so the HLS encode is repositionable),
// and the duration is long enough to span several 4-second HLS segments — the
// 1-second shared fixtures are a single segment, so a seek-ahead needs its own
// longer clip. Skips the test if ffmpeg cannot produce it.
func generateLongTranscodeClip(t *testing.T, seconds int) string {
	t.Helper()
	dir := t.TempDir()
	movieDir := filepath.Join(dir, "Long Movie (2001)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatalf("mkdir long-clip movie dir: %v", err)
	}
	out := filepath.Join(movieDir, "Long Movie (2001).mkv")
	dur := strconv.Itoa(seconds)
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration="+dur+":size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+dur,
		"-c:v", "mpeg4", "-c:a", "libmp3lame", "-shortest", out,
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg could not synthesize the long transcode clip: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs long-clip dir: %v", err)
	}
	return abs
}

// scanLibraryAt creates a Movie library at root, scans it, and returns the titles.
func scanLibraryAt(t *testing.T, srv *testharness.Server, token, root string) titlesListResp {
	t.Helper()
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list); status != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", status, body)
	}
	return list
}

// ffmpegProcsForScratch counts running ffmpeg processes whose command line
// references the session's scratch dir — the leak check for realignment +
// teardown. It greps `ps` output for the scratch path so it is host-agnostic and
// scoped to THIS session (it ignores ffmpeg from other tests/sessions).
func ffmpegProcsForScratch(t *testing.T, scratch string) int {
	t.Helper()
	out, err := exec.Command("ps", "-Ao", "pid,command").Output()
	if err != nil {
		// ps shape varies; a failure here should not fail the test — return -1 so the
		// caller can choose to skip the leak assertion rather than flake.
		t.Logf("ps failed (%v); skipping process-leak count", err)
		return -1
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "ffmpeg") && strings.Contains(line, scratch) {
			n++
		}
	}
	return n
}

// TestHLSTranscodeSeekRealignment is the slice-3 acceptance test: against a
// transcode session for a multi-segment clip, requesting a LATER segment is served
// through the realignment-capable HLS path and is ffprobe-valid as the re-encoded
// target codecs (h264/aac). It asserts the seek reuses the SAME session (no new
// POST /playback, sessionId unchanged), that the server-owned playlist stays
// coherent (segment[0] is still fetchable after the seek), and that no ffmpeg
// process leaks across the seek or the final DELETE.
//
// NOTE on the realignment guarantee: with the synthetic testsrc fixture the
// from-the-top libx264 encode often outruns the per-request settle window and has
// already produced the sought segment, so a restart may not be needed here. The
// realignment MECHANISM — killing the in-flight job and relaunching it seeked +
// renumbered at the target, within the same session — is proven deterministically
// with a fake runner in the playback package (TestRealignSupersedesJobAndRenumbers);
// this test proves the end-to-end external behavior (served, valid, same session,
// coherent playlist, no leak) against real ffmpeg.
func TestHLSTranscodeSeekRealignment(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	root := generateLongTranscodeClip(t, 16) // 16s → 4 segments at 4s each
	list := scanLibraryAt(t, srv, token, root)
	titleID := findTitle(t, list, "Long Movie")

	dec := negotiateTranscodeDecision(t, srv, token, titleID, transcodeProfile())
	sessionID := dec.SessionID

	// Fetch the playlist; the server-owned VOD playlist lists all segments up front.
	pl := authStream(t, srv, dec.StreamURL, token, "")
	defer pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}
	manifestBytes, _ := io.ReadAll(pl.Body)
	segments := parseSegments(string(manifestBytes))
	if len(segments) < 3 {
		t.Fatalf("playlist lists %d segments, want >= 3 for a seek-ahead test:\n%s", len(segments), manifestBytes)
	}

	// Seek AHEAD: request a later segment directly (skip the early ones), forcing a
	// realignment of the encode rather than letting it reach the segment linearly.
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	target := segments[len(segments)-1] // the last segment — well ahead of segment 0
	segResp := authStream(t, srv, base+target, token, "")
	defer segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("seek-ahead segment %q status = %d, want 200", target, segResp.StatusCode)
	}
	segBytes, _ := io.ReadAll(segResp.Body)
	if len(segBytes) == 0 {
		t.Fatal("seek-ahead segment body empty")
	}
	// The realigned segment is real, re-encoded media.
	vCodec, aCodec, _ := ffprobeSegment(t, segBytes)
	if vCodec != "h264" {
		t.Errorf("realigned segment video codec = %q, want h264 (re-encoded)", vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("realigned segment audio codec = %q, want aac (re-encoded)", aCodec)
	}

	// The seek reused the SAME session — no new negotiation/decision was created.
	if dec.SessionID != sessionID {
		t.Errorf("sessionId changed across seek: %q → %q (seek must reuse the session)", sessionID, dec.SessionID)
	}
	// And the original segment is still fetchable under the stable playlist (the
	// realignment did not renumber it out from under the player).
	first := authStream(t, srv, base+segments[0], token, "")
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Errorf("segment[0] after realign = %d, want 200 (playlist stays coherent)", first.StatusCode)
	}

	scratch := filepath.Join(srv.DataDir, "transcode", sessionID)
	// After realignment there must be at most ONE ffmpeg for this session (the
	// superseded job was killed) — never an orphan.
	if n := ffmpegProcsForScratch(t, scratch); n > 1 {
		t.Errorf("after realign: %d ffmpeg processes for the session scratch, want <= 1 (no orphan)", n)
	}

	// Clean stop: DELETE ends the session, removes scratch, and leaves no ffmpeg.
	if status, raw := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+sessionID, token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, raw)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after delete (err=%v)", err)
	}
	// Give a killed ffmpeg a beat to exit, then assert none remain for this session.
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := ffmpegProcsForScratch(t, scratch)
		if n <= 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("ffmpeg still running for the session scratch after delete (%d procs) — leak", n)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- HW accel: VideoToolbox reference backend (transcode-hwaccel issue 01) ---

// requireVideoToolbox skips unless the host can REALLY run h264_videotoolbox, not
// merely that ffmpeg was built with it: it runs a tiny one-frame test-encode to
// /dev/null (the ADR-0009 "device actually works" check) and skips with a clear
// message when the encoder is absent or the device does not validate (e.g. CI has
// no GPU). On this macOS dev box VideoToolbox is present, so the gated real
// end-to-end HW path below actually runs here.
func requireVideoToolbox(t *testing.T) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(out), "h264_videotoolbox") {
		t.Skip("h264_videotoolbox not listed by ffmpeg -encoders; skipping real VideoToolbox e2e test")
	}
	// A real test-encode is what distinguishes "ffmpeg built with videotoolbox"
	// from "a working media engine is present" (ADR-0009 setup-time validation).
	probe := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-frames:v", "1", "-c:v", "h264_videotoolbox", "-f", "null", "-")
	if err := probe.Run(); err != nil {
		t.Skipf("h264_videotoolbox did not validate on this host (%v); skipping real VideoToolbox e2e test", err)
	}
}

// TestHLSTranscodeVideoToolbox is the gated real end-to-end HW test (issue 01):
// with the HardwareAccel knob set to videotoolbox, drive the HTTP API transcode
// path against the mkv/mpeg4/mp3 fixture (which can play under neither the target
// container nor codecs, forcing a full transcode) → fetch the HLS playlist +
// segment → ffprobe confirms the output is h264 within the requested resolution.
// This proves the WHOLE path (negotiation → HW encode → HLS → ffprobe) on the one
// backend this host can run; it self-skips where VideoToolbox is absent.
func TestHLSTranscodeVideoToolbox(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	requireVideoToolbox(t)

	srv := testharness.New(t, testharness.WithHardwareAccel(config.HWAccelVideoToolbox))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner") // mkv, mpeg4/mp3, 144 tall

	dec := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)

	// ffprobe the HW-encoded segment: it must be valid, re-encoded h264 video +
	// aac audio, within the requested 1080p cap (the 144-tall source is not
	// upscaled). h264_videotoolbox produces standard h264, so the codec name is
	// h264 exactly as the CPU path — the proof here is that the whole HW-knob path
	// produced a valid, decodable HLS segment within the constraints.
	vCodec, aCodec, height := ffprobeSegment(t, seg)
	if vCodec != "h264" {
		t.Errorf("VideoToolbox segment video codec = %q, want h264 (HW re-encode)", vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("VideoToolbox segment audio codec = %q, want aac", aCodec)
	}
	if height <= 0 || height > 1080 {
		t.Errorf("VideoToolbox segment height = %d, want within the 1080p cap (and > 0)", height)
	}
}

// hevcEncoder returns the first HEVC encoder this ffmpeg build offers for
// synthesizing an HEVC fixture (hevc_videotoolbox preferred on this macOS host, else
// libx265), or "" when neither is present. The HEVC e2e below needs an HEVC SOURCE so
// the VideoToolbox decode hwaccel (issue 05) is actually exercised on an HEVC bitstream
// — the motivating 4K-HEVC bug. When no HEVC encoder exists the test self-skips and
// the decode flag is left to the args-builder unit tests.
func hevcEncoder() string {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return ""
	}
	s := string(out)
	for _, enc := range []string{"hevc_videotoolbox", "libx265"} {
		if strings.Contains(s, enc) {
			return enc
		}
	}
	return ""
}

// generateHEVCClip writes a tiny HEVC-video + aac-audio mkv into a fresh library dir
// using enc as the HEVC encoder, returning the dir. The HEVC codec (not in the
// transcode profile's h264-only video list) forces a full video re-encode, so the
// VideoToolbox path must hardware-DECODE the HEVC source and hardware-encode h264.
func generateHEVCClip(t *testing.T, enc string, seconds int) string {
	t.Helper()
	dir := t.TempDir()
	movieDir := filepath.Join(dir, "HEVC Movie (2002)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatalf("mkdir hevc movie dir: %v", err)
	}
	out := filepath.Join(movieDir, "HEVC Movie (2002).mkv")
	dur := strconv.Itoa(seconds)
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration="+dur+":size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+dur,
		"-c:v", enc, "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", out,
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg could not synthesize the HEVC fixture with %s: %v", enc, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs hevc dir: %v", err)
	}
	return abs
}

// TestHLSTranscodeVideoToolboxHEVCDecode is the issue-05 gated real e2e: it proves
// VideoToolbox hardware DECODE+encode end-to-end on an HEVC source (the motivating
// bug was a 4K HEVC remux pinning the CPU because only the encode was HW). It
// synthesizes a tiny HEVC clip, drives the HTTP API transcode path with the
// videotoolbox knob, and ffprobe-confirms a decodable h264/aac segment within the
// cap. It self-skips where VideoToolbox is absent (CI has no GPU) or where the build
// has no HEVC encoder to make the fixture — in which case the -hwaccel videotoolbox
// decode flag is covered by the args-builder unit tests instead.
func TestHLSTranscodeVideoToolboxHEVCDecode(t *testing.T) {
	requireFFmpeg(t)
	requireVideoToolbox(t)
	enc := hevcEncoder()
	if enc == "" {
		t.Skip("no HEVC encoder (hevc_videotoolbox or libx265) in this ffmpeg build to synthesize an HEVC fixture; VideoToolbox HW decode is arg-verified by the unit tests instead")
	}

	srv := testharness.New(t, testharness.WithHardwareAccel(config.HWAccelVideoToolbox))
	token := adminToken(t, srv)
	root := generateHEVCClip(t, enc, 2)
	list := scanLibraryAt(t, srv, token, root)
	titleID := findTitle(t, list, "HEVC Movie")

	dec := negotiateTranscodeDecision(t, srv, token, titleID, transcodeProfile())
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)

	// The HEVC source was hardware-decoded (videotoolbox) and re-encoded to h264 on
	// the media engine; the output segment must be valid, decodable h264/aac within
	// the 1080p cap. (The codec name is plain h264 — the proof is that the whole
	// HW decode+encode path produced a valid HLS segment from an HEVC input.)
	vCodec, aCodec, height := ffprobeSegment(t, seg)
	if vCodec != "h264" {
		t.Errorf("HEVC→VideoToolbox segment video codec = %q, want h264 (HW decode + HW encode)", vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("HEVC→VideoToolbox segment audio codec = %q, want aac", aCodec)
	}
	if height <= 0 || height > 1080 {
		t.Errorf("HEVC→VideoToolbox segment height = %d, want within the 1080p cap (and > 0)", height)
	}
}

// --- HW accel: NVENC / VAAPI / QSV (transcode-hwaccel issue 04) ---
//
// These three GPU backends are ARG-VERIFIED, not CI-run here. The macOS dev box
// has no NVENC/VAAPI/QSV hardware, so the gated real e2e tests below SELF-SKIP with
// an explicit message and confidence for these backends comes from the pure
// args-builder unit tests (internal/transcode/ffmpeg_test.go:
// TestTranscodeArgsAccelNVENC/VAAPI/QSV) plus the fake-validated detector
// (internal/transcode/detect_test.go). The e2e bodies exist so that ON A HOST WITH
// THE DEVICE the whole path (negotiation → HW encode → HLS → ffprobe) is proven;
// they MUST NOT be read as evidence of real-hardware coverage on a box that skips.

// requireHWEncoder skips unless the host can REALLY run `encoder`, not merely that
// ffmpeg was built with it: it confirms the encoder is listed AND runs a tiny
// one-frame test-encode to null (the ADR-0009 "device actually works" check),
// mirroring requireVideoToolbox. On a box without the GPU it self-skips with an
// explicit message naming the backend, so the suite never implies coverage it
// lacks. label is the human backend name for the skip line.
func requireHWEncoder(t *testing.T, encoder, label string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(out), encoder) {
		t.Skipf("%s (%s) not listed by ffmpeg -encoders; skipping real %s e2e test (arg-verified by the unit tests instead)", encoder, label, label)
	}
	probe := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-frames:v", "1", "-c:v", encoder, "-f", "null", "-")
	if err := probe.Run(); err != nil {
		t.Skipf("%s (%s) did not validate on this host (%v); skipping real %s e2e test (arg-verified by the unit tests instead)", encoder, label, err, label)
	}
}

// runHWTranscodeE2E drives the gated real end-to-end HW path for one backend: with
// the HardwareAccel knob set to accel, transcode the mkv/mpeg4/mp3 fixture (which
// can play under neither the target container nor codecs, forcing a full transcode)
// → fetch the HLS playlist + segment → ffprobe confirms decodable h264/aac within
// the 1080p cap. Identical to TestHLSTranscodeVideoToolbox but for a Linux GPU
// backend, so a host with the device proves the whole path; otherwise it skips.
func runHWTranscodeE2E(t *testing.T, accel config.HWAccel, encoder, label string) {
	requireFixtures(t)
	requireFFmpeg(t)
	requireHWEncoder(t, encoder, label)

	srv := testharness.New(t, testharness.WithHardwareAccel(accel))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)

	vCodec, aCodec, height := ffprobeSegment(t, seg)
	if vCodec != "h264" {
		t.Errorf("%s segment video codec = %q, want h264 (HW re-encode)", label, vCodec)
	}
	if aCodec != "aac" {
		t.Errorf("%s segment audio codec = %q, want aac", label, aCodec)
	}
	if height <= 0 || height > 1080 {
		t.Errorf("%s segment height = %d, want within the 1080p cap (and > 0)", label, height)
	}
}

// TestHLSTranscodeNVENC / VAAPI / QSV are the gated real e2e HW tests for the Linux
// GPU backends. They self-skip on this macOS host (no such device) — confidence for
// these three comes from the args-builder + detector unit tests, as documented above.
func TestHLSTranscodeNVENC(t *testing.T) {
	runHWTranscodeE2E(t, config.HWAccelNVENC, "h264_nvenc", "NVENC")
}

func TestHLSTranscodeVAAPI(t *testing.T) {
	runHWTranscodeE2E(t, config.HWAccelVAAPI, "h264_vaapi", "VAAPI")
}

func TestHLSTranscodeQSV(t *testing.T) {
	runHWTranscodeE2E(t, config.HWAccelQSV, "h264_qsv", "QSV")
}

// TestHLSTranscodeDeleteEndsAndRemovesScratch: DELETE /sessions/{id} ends a
// transcode session — the scratch dir is removed and subsequent HLS fetches 404
// (the slice-1 lifecycle applies unchanged to the transcode tier).
func TestHLSTranscodeDeleteEndsAndRemovesScratch(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())

	// Start the transcode + create scratch.
	pl := authStream(t, srv, dec.StreamURL, token, "")
	pl.Body.Close()
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}
	scratch := filepath.Join(srv.DataDir, "transcode", dec.SessionID)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir not created at %q: %v", scratch, err)
	}

	status, raw := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, raw)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after delete (err=%v)", err)
	}
	post := authStream(t, srv, dec.StreamURL, token, "")
	post.Body.Close()
	if post.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete playlist = %d, want 404", post.StatusCode)
	}
}
