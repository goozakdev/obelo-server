package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue audio-streams/03 integration tests: the in-band HLS AUDIO rendition group.
// Against the shared multi-audio fixture (eng AAC stereo default, jpn AC3 5.1, an eng
// DTS commentary, an untagged AAC mono) these drive the real HTTP + ffmpeg stack to
// assert the DEMUXED layout end to end: a master playlist carrying an AUDIO group +
// video-only variant, per-Stream rendition playlists whose segments are valid audio,
// copy-vs-AAC per codec, cap exemption for the in-session audio encode, and that a
// single-audio File stays muxed (the regression pin).

// transcodeAudioMovieProfile cannot decode the fixture's h264 video (only vp9) nor
// its mkv container, so a remux cannot help — the File is fully TRANSCODED (a real
// cap-consuming video transcode). It still decodes aac/ac3 audio.
func transcodeAudioMovieProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "vp9", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac", "ac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "1080p"},
	}
}

// fetchAudioRenditionSegment resolves a demuxed session's master → the rendition
// media playlist for streamID (audio_<id>.m3u8) → its first segment bytes.
func fetchAudioRenditionSegment(t *testing.T, srv *testharness.Server, streamURL, token, streamID string) []byte {
	t.Helper()
	if !strings.HasSuffix(streamURL, "/master.m3u8") {
		t.Fatalf("expected a master playlist streamUrl, got %q", streamURL)
	}
	base := streamURL[:strings.LastIndex(streamURL, "/")+1]
	pl := fetchText(t, srv, base+"audio_"+streamID+".m3u8", token)
	if !strings.HasPrefix(strings.TrimSpace(pl), "#EXTM3U") {
		t.Fatalf("rendition playlist not HLS:\n%s", pl)
	}
	segments := parseSegments(pl)
	if len(segments) == 0 {
		t.Fatalf("rendition playlist lists no segments:\n%s", pl)
	}
	// The LAST listed segment also exercises a seek-to-end read: because an audio
	// rendition is produced whole (VOD), a seek reads an already-written segment.
	name := segments[len(segments)-1]
	resp := authStream(t, srv, base+name, token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rendition segment %s status = %d, want 200", name, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if len(b) == 0 {
		t.Fatalf("rendition segment %s body empty", name)
	}
	return b
}

// TestDemuxedMasterCarriesAudioGroup (acceptance 1, 2): an HLS session for the
// multi-audio fixture serves a master playlist with an AUDIO group (one rendition per
// audio Stream, exactly one DEFAULT=YES) and a video-only variant that references it;
// the video segment carries NO audio (demuxed) and the default rendition's segment is
// valid audio matching the reported default.
func TestDemuxedMasterCarriesAudioGroup(t *testing.T) {
	requireAudioFixtures(t)
	requireFFmpeg(t)
	srv, token, id := scanAudioMovieLib(t)

	dec := negotiateAudio(t, srv, token, id, remuxMultiAudioProfile())
	if dec.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream (remux)", dec.Tier)
	}
	if !strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("demuxed multi-audio streamUrl = %q, want a master playlist", dec.StreamURL)
	}
	master := fetchText(t, srv, dec.StreamURL, token)

	// One AUDIO rendition per audio Stream (4), exactly one default, group referenced.
	if n := strings.Count(master, "#EXT-X-MEDIA:TYPE=AUDIO"); n != 4 {
		t.Errorf("AUDIO rendition count = %d, want 4:\n%s", n, master)
	}
	if c := strings.Count(master, "DEFAULT=YES"); c != 1 {
		t.Errorf("DEFAULT=YES count = %d, want 1:\n%s", c, master)
	}
	if !strings.Contains(master, `#EXT-X-STREAM-INF`) || !strings.Contains(master, `AUDIO="aud"`) {
		t.Errorf("video variant must reference the AUDIO group:\n%s", master)
	}
	if !strings.Contains(master, "index.m3u8") {
		t.Errorf("master must reference the video-only variant index.m3u8:\n%s", master)
	}

	// The video variant is VIDEO-ONLY: its segment has video but NO audio (demux).
	videoSeg := fetchFirstSegment(t, srv, dec.StreamURL, token)
	v, a := ffprobeCodecs(t, videoSeg)
	if v == "" {
		t.Errorf("video variant segment has no video stream")
	}
	if a != "" {
		t.Errorf("demuxed video variant must carry NO audio, but delivered %q", a)
	}

	// The default rendition delivers valid audio matching the reported default (aac/2).
	seg := fetchDefaultAudioRenditionSegment(t, srv, dec.StreamURL, token)
	codec, ch := ffprobeAudioIdentity(t, seg)
	if codec != "aac" || ch != 2 {
		t.Errorf("default rendition delivered %s/%dch, want aac/2 (the reported default)", codec, ch)
	}
}

// TestAudioRenditionCopyVsTranscode (acceptance 3): a codec-compatible Stream is
// stream-copied (ac3 stays ac3); the incompatible DTS commentary transcodes to AAC —
// so every listed rendition is actually playable on the client.
func TestAudioRenditionCopyVsTranscode(t *testing.T) {
	requireAudioFixtures(t)
	requireFFmpeg(t)
	srv, token, id := scanAudioMovieLib(t)

	dec := negotiateAudio(t, srv, token, id, remuxMultiAudioProfile())
	ac3 := audioStreamByLabel(t, dec, "Japanese 5.1")                  // ac3 — client-decodable → copied
	dts := audioStreamByLabel(t, dec, "English Director's Commentary") // dts — undecodable → AAC

	ac3Codec, _ := ffprobeAudioIdentity(t, fetchAudioRenditionSegment(t, srv, dec.StreamURL, token, ac3.ID))
	if ac3Codec != "ac3" {
		t.Errorf("compatible ac3 rendition delivered %q, want ac3 (stream-copied)", ac3Codec)
	}

	dtsCodec, _ := ffprobeAudioIdentity(t, fetchAudioRenditionSegment(t, srv, dec.StreamURL, token, dts.ID))
	if dtsCodec != "aac" {
		t.Errorf("incompatible dts rendition delivered %q, want aac (transcoded)", dtsCodec)
	}
}

// TestAudioRenditionEncodeIsCapExempt (acceptance 3): the in-session AAC encode of an
// incompatible rendition does NOT consume a transcode cap slot — with cap=1, after
// triggering the DTS rendition's AAC encode on a remux session, a concurrent genuine
// video transcode still fits (ADR-0022's scoped ADR-0017 amendment).
func TestAudioRenditionEncodeIsCapExempt(t *testing.T) {
	requireAudioFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, audioStreamsRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Audio Movie")

	// A demuxed remux session (remux itself is unmetered).
	remux := negotiateAudio(t, srv, token, id, remuxMultiAudioProfile())
	if remux.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream", remux.Tier)
	}
	dts := audioStreamByLabel(t, remux, "English Director's Commentary")
	// Trigger the DTS rendition's AAC encode — cap-exempt.
	fetchAudioRenditionSegment(t, srv, remux.StreamURL, token, dts.ID)

	// A concurrent genuine video transcode still fits at cap=1 — the audio encode took
	// no slot. (If the audio AAC encode had consumed the slot, this would 503.)
	var vt decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, transcodeAudioMovieProfile(), &vt); s != http.StatusOK {
		t.Fatalf("concurrent video transcode status = %d, want 200 (audio encode must be cap-exempt); body: %s", s, b)
	}
	if vt.Tier != "transcode" {
		t.Fatalf("concurrent session tier = %q, want transcode", vt.Tier)
	}

	// The single slot is now genuinely held by the video transcode: a SECOND video
	// transcode is busy-rejected — confirming the cap works and only the video
	// transcode was counted.
	var busy busyResp
	if s, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, transcodeAudioMovieProfile(), &busy); s != http.StatusServiceUnavailable {
		t.Errorf("second video transcode status = %d, want 503 (the one slot is held by the video transcode)", s)
	}
}

// TestSingleAudioStaysMuxed (acceptance 5): a single-audio File keeps the muxed
// pipeline — no master playlist, and the video segment carries BOTH video and audio.
func TestSingleAudioStaysMuxed(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateRemuxDecision(t, srv, token, bladeID)
	if strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("single-audio session streamUrl = %q, want the bare media playlist (no demux)", dec.StreamURL)
	}
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)
	v, a := ffprobeCodecs(t, seg)
	if v == "" || a == "" {
		t.Errorf("single-audio remux must stay MUXED (video=%q audio=%q, want both present)", v, a)
	}
}
