package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue "Back to the Future" (ADR-0024) delivery test: a HEVC File whose audio the
// client can't decode must be delivered by COPYING the HEVC video (not re-encoding it
// to h264) and transcoding only the audio, as fMP4 HLS that Safari can play. This
// asserts the observable delivery end to end through real ffmpeg: the master carries a
// CODECS="hvc1…" video variant + an AUDIO group, the video media playlist is
// fragmented-MP4 (an #EXT-X-MAP init segment + .m4s), the DELIVERED video stream is
// still hevc (proving a copy, not a 4K→h264 re-encode), and the delivered audio is aac
// (proving the audio was transcoded).

const hevcRootRel = "hevc"

var hevcFixtureAvailable bool

func init() { hevcFixtureAvailable = ensureHEVCFixture() }

func requireHEVCFixture(t *testing.T) {
	t.Helper()
	if !hevcFixtureAvailable {
		t.Skip("hevc fixture unavailable (ffmpeg/libx265 not on PATH)")
	}
}

// ensureHEVCFixture generates a small HEVC (libx265) mkv carrying two audio Streams
// the browser can't decode (E-AC3 5.1 default + AC3 stereo) so the session must copy
// the video and transcode the audio.
func ensureHEVCFixture() bool {
	dir := filepath.Join("testdata", hevcRootRel, "HEVC Movie (2015)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	out := filepath.Join(dir, "HEVC Movie (2015).mkv")
	if fileExists(out) {
		return true
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "aevalsrc=0.1*sin(1000*t):duration=2:channel_layout=5.1",
		"-map", "0:v", "-map", "1:a", "-map", "1:a",
		"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a:0", "eac3", "-ac:a:0", "6", "-metadata:s:a:0", "language=eng", "-disposition:a:0", "default",
		"-c:a:1", "ac3", "-ac:a:1", "2", "-metadata:s:a:1", "language=jpn", "-disposition:a:1", "0",
		"-shortest", out)
	return cmd.Run() == nil
}

// hevcSafariProfileJSON: plays HEVC up to 2160p (like Safari on Apple silicon) but only
// h264 up to 1080p; decodes AAC only (NOT ac3/eac3); mp4 container (NOT mkv). So a HEVC
// mkv with E-AC3 audio → the container+audio force a transcode, the HEVC video is
// copied, the audio is re-encoded to AAC, and delivery is fMP4.
func hevcSafariProfileJSON() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers": []string{"mp4"},
			"videoCodecs": []map[string]any{
				{"codec": "h264", "maxResolution": "1080p"},
				{"codec": "hevc", "maxResolution": "2160p"},
			},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "2160p"},
	}
}

func fetchBytes(t *testing.T, srv *testharness.Server, apiPath, token string) []byte {
	t.Helper()
	resp := authStream(t, srv, apiPath, token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", apiPath, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if len(b) == 0 {
		t.Fatalf("GET %s returned empty body", apiPath)
	}
	return b
}

// extMapURI extracts the #EXT-X-MAP:URI="…" init-segment name from a media playlist.
func extMapURI(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-MAP:") {
			continue
		}
		if i := strings.Index(line, `URI="`); i >= 0 {
			rest := line[i+len(`URI="`):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				return rest[:j]
			}
		}
	}
	t.Fatalf("no #EXT-X-MAP in playlist:\n%s", playlist)
	return ""
}

// ffprobeFirstCodec concatenates an fMP4 init segment + a media segment (a CMAF
// fragment is only decodable with its init) and returns the codec of the first stream
// of codecType ("video"|"audio").
func ffprobeFirstCodec(t *testing.T, init, seg []byte, codecType string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "frag.mp4")
	if err := os.WriteFile(path, append(append([]byte{}, init...), seg...), 0o644); err != nil {
		t.Fatalf("writing fragment: %v", err)
	}
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe fragment (%s): %v", codecType, err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parsing ffprobe json: %v\n%s", err, out)
	}
	for _, s := range probe.Streams {
		if s.CodecType == codecType {
			return s.CodecName
		}
	}
	t.Fatalf("no %s stream in delivered fragment; ffprobe: %s", codecType, out)
	return ""
}

// TestHEVCCopyDeliversFMP4: the full delivery for the Back to the Future case.
func TestHEVCCopyDeliversFMP4(t *testing.T) {
	requireHEVCFixture(t)
	requireFFmpeg(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, hevcRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "HEVC Movie")

	dec := negotiateAudio(t, srv, token, id, hevcSafariProfileJSON())
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode (mkv container + undecodable audio force it)", dec.Tier)
	}
	if !strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("streamUrl = %q, want a master playlist (demuxed multi-audio)", dec.StreamURL)
	}
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]

	// The master advertises the HEVC variant with a CODECS attribute + an AUDIO group.
	master := fetchText(t, srv, dec.StreamURL, token)
	if !strings.Contains(master, `CODECS="hvc1`) {
		t.Errorf("master missing HEVC CODECS attribute:\n%s", master)
	}
	if !strings.Contains(master, "#EXT-X-MEDIA:TYPE=AUDIO") {
		t.Errorf("master missing AUDIO group:\n%s", master)
	}

	// The video media playlist is fMP4: an EXT-X-MAP init segment + .m4s segments.
	videoPL := fetchText(t, srv, base+"index.m3u8", token)
	videoInit := extMapURI(t, videoPL)
	if !strings.HasSuffix(videoInit, ".mp4") {
		t.Errorf("video init segment = %q, want an .mp4 init", videoInit)
	}
	videoSegs := parseSegments(videoPL)
	if len(videoSegs) == 0 || !strings.HasSuffix(videoSegs[0], ".m4s") {
		t.Fatalf("video playlist is not fMP4 (.m4s segments): %v", videoSegs)
	}

	// The DELIVERED video is still hevc → the 4K video was COPIED, not re-encoded to h264.
	vInit := fetchBytes(t, srv, base+videoInit, token)
	vSeg := fetchBytes(t, srv, base+videoSegs[0], token)
	if got := ffprobeFirstCodec(t, vInit, vSeg, "video"); got != "hevc" {
		t.Errorf("delivered video codec = %q, want hevc (copied, not re-encoded)", got)
	}

	// The DEFAULT audio rendition is fMP4 and its delivered audio is aac (transcoded
	// from E-AC3 the client can't decode).
	var audioURI string
	for _, line := range strings.Split(master, "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(line, "DEFAULT=YES") {
			audioURI = extAttrURI(line)
		}
	}
	if audioURI == "" {
		t.Fatalf("no DEFAULT=YES audio rendition in master:\n%s", master)
	}
	audioPL := fetchText(t, srv, base+audioURI, token)
	aInitName := extMapURI(t, audioPL)
	aSegs := parseSegments(audioPL)
	if len(aSegs) == 0 || !strings.HasSuffix(aSegs[0], ".m4s") {
		t.Fatalf("audio rendition is not fMP4 (.m4s): %v", aSegs)
	}
	aInit := fetchBytes(t, srv, base+aInitName, token)
	aSeg := fetchBytes(t, srv, base+aSegs[0], token)
	if got := ffprobeFirstCodec(t, aInit, aSeg, "audio"); got != "aac" {
		t.Errorf("delivered audio codec = %q, want aac (transcoded from E-AC3)", got)
	}
}

// TestHEVCCopyDeliversMpegTSForHlsJS: the same HEVC-copy session negotiated by an
// hls.js (MSE) client — profile hevcInMpegts:true — must be delivered over the
// MPEG-TS pipeline instead of fMP4: hls.js ≥ 1.6 demuxes HEVC-in-TS itself, and only
// the TS path has dictated cuts, so its synthesized playlist is exact — the fMP4
// hls-muxer's cut grid can drift from the synthesized playlist, which stalls strict
// MSE playback (the Chrome bufferStalledError bug). The video must still be COPIED
// hevc (not re-encoded) and the audio transcoded to AAC.
func TestHEVCCopyDeliversMpegTSForHlsJS(t *testing.T) {
	requireHEVCFixture(t)
	requireFFmpeg(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, hevcRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "HEVC Movie")

	profile := hevcSafariProfileJSON()
	profile["deviceProfile"].(map[string]any)["hevcInMpegts"] = true

	dec := negotiateAudio(t, srv, token, id, profile)
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode", dec.Tier)
	}
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]

	// The video media playlist is MPEG-TS: .ts segments, no EXT-X-MAP init.
	videoPL := fetchText(t, srv, base+"index.m3u8", token)
	if strings.Contains(videoPL, "#EXT-X-MAP:") {
		t.Errorf("hls.js client got an fMP4 init segment; want MPEG-TS:\n%s", videoPL)
	}
	videoSegs := parseSegments(videoPL)
	if len(videoSegs) == 0 || !strings.HasSuffix(videoSegs[0], ".ts") {
		t.Fatalf("video playlist is not MPEG-TS (.ts segments): %v", videoSegs)
	}

	// The DELIVERED video is still hevc — copied into TS, not re-encoded to h264.
	vSeg := fetchBytes(t, srv, base+videoSegs[0], token)
	if got := ffprobeFirstCodec(t, nil, vSeg, "video"); got != "hevc" {
		t.Errorf("delivered video codec = %q, want hevc (copied into MPEG-TS)", got)
	}

	// The DEFAULT audio rendition is TS too, transcoded to AAC.
	master := fetchText(t, srv, dec.StreamURL, token)
	var audioURI string
	for _, line := range strings.Split(master, "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(line, "DEFAULT=YES") {
			audioURI = extAttrURI(line)
		}
	}
	if audioURI == "" {
		t.Fatalf("no DEFAULT=YES audio rendition in master:\n%s", master)
	}
	audioPL := fetchText(t, srv, base+audioURI, token)
	aSegs := parseSegments(audioPL)
	if len(aSegs) == 0 || !strings.HasSuffix(aSegs[0], ".ts") {
		t.Fatalf("audio rendition is not MPEG-TS (.ts): %v", aSegs)
	}
	aSeg := fetchBytes(t, srv, base+aSegs[0], token)
	if got := ffprobeFirstCodec(t, nil, aSeg, "audio"); got != "aac" {
		t.Errorf("delivered audio codec = %q, want aac (transcoded)", got)
	}
}

// extAttrURI pulls URI="…" out of an #EXT-X-MEDIA line.
func extAttrURI(line string) string {
	if i := strings.Index(line, `URI="`); i >= 0 {
		rest := line[i+len(`URI="`):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	return ""
}

func hevcRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", hevcRootRel))
	if err != nil {
		t.Fatalf("resolving hevc root: %v", err)
	}
	return abs
}
