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

// Issue audio-streams/02 integration tests: deliver the audio the Decision reports.
// Against the shared multi-audio fixture (Audio Movie: eng AAC stereo default, jpn
// AC3 5.1, an eng DTS commentary, an untagged AAC mono) these assert the actual
// DELIVERED bytes through the real HTTP + ffmpeg stack — never the internal args —
// so they catch the latent bug where negotiation reported one audio Stream while
// ffmpeg's implicit selection delivered another. The regression assertions ffprobe
// the HLS segment and compare its audio identity to the Decision's reported Stream.

// mkvMultiAudioProfile plays the fixture natively: mkv container + h264 + aac/ac3
// within 8 channels → the default track direct-plays (used for the decision-list
// and audioStreamId tier assertions).
func mkvMultiAudioProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mkv"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac", "ac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "1080p"},
	}
}

// remuxMultiAudioProfile can decode the codecs but NOT the mkv container, so the
// File remuxes (directStream) — an HLS session whose single mapped audio Stream we
// can ffprobe. It decodes aac + ac3 within 8 channels so both the default and the
// jpn 5.1 track are copy-deliverable (the tier is remux regardless of the pick).
func remuxMultiAudioProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"}, // NOT mkv → forces a remux
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac", "ac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "1080p"},
	}
}

// withPreferredAudioLang returns a copy of profile with constraints.preferredAudioLang set.
func withPreferredAudioLang(profile map[string]any, lang string) map[string]any {
	cons := profile["constraints"].(map[string]any)
	cons["preferredAudioLang"] = lang
	return profile
}

// withAudioStreamId returns a copy of profile with a top-level audioStreamId set.
func withAudioStreamId(profile map[string]any, id string) map[string]any {
	profile["audioStreamId"] = id
	return profile
}

// scanAudioMovieLib scans the multi-audio fixture and returns (server, token, titleID).
func scanAudioMovieLib(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, audioStreamsRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Audio Movie")
	return srv, token, id
}

// negotiateAudio POSTs a playback request and returns the decision (failing on a
// non-200).
func negotiateAudio(t *testing.T, srv *testharness.Server, token, titleID string, body map[string]any) decisionResp {
	t.Helper()
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, body, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	return dec
}

// ffprobeAudioIdentity writes an HLS segment to disk and reads its FIRST audio
// stream's codec + channel count — the observable identity of the delivered audio,
// used to prove it is the Stream the Decision reported (not ffmpeg's implicit pick).
func ffprobeAudioIdentity(t *testing.T, seg []byte) (codec string, channels int) {
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
			Channels  int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parsing ffprobe json: %v\n%s", err, out)
	}
	for _, s := range probe.Streams {
		if s.CodecType == "audio" {
			return s.CodecName, s.Channels
		}
	}
	t.Fatalf("no audio stream in delivered segment; ffprobe: %s", out)
	return "", 0
}

// fetchDefaultAudioRenditionSegment resolves a demuxed session's master playlist to
// its DEFAULT=YES in-band AUDIO rendition (audio-streams/03) and returns that
// rendition's first segment bytes — the audio actually delivered for the resolved
// Stream. Under demuxing the video variant carries NO audio, so proving delivered ==
// reported means ffprobing the default rendition, not the video segment.
func fetchDefaultAudioRenditionSegment(t *testing.T, srv *testharness.Server, streamURL, token string) []byte {
	t.Helper()
	if !strings.HasSuffix(streamURL, "/master.m3u8") {
		t.Fatalf("expected a master playlist streamUrl for a demuxed session, got %q", streamURL)
	}
	master := fetchText(t, srv, streamURL, token)
	base := streamURL[:strings.LastIndex(streamURL, "/")+1]

	// The default audio rendition is the TYPE=AUDIO media line marked DEFAULT=YES.
	var renditionURI string
	for _, line := range strings.Split(master, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") || !strings.Contains(line, "DEFAULT=YES") {
			continue
		}
		key := `URI="`
		if i := strings.Index(line, key); i >= 0 {
			rest := line[i+len(key):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				renditionURI = rest[:j]
			}
		}
	}
	if renditionURI == "" {
		t.Fatalf("master has no DEFAULT=YES AUDIO rendition:\n%s", master)
	}

	pl := fetchText(t, srv, base+renditionURI, token)
	segments := parseSegments(pl)
	if len(segments) == 0 {
		t.Fatalf("audio rendition playlist lists no segments:\n%s", pl)
	}
	segResp := authStream(t, srv, base+segments[0], token, "")
	defer segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("audio rendition segment status = %d, want 200", segResp.StatusCode)
	}
	segBytes, _ := io.ReadAll(segResp.Body)
	if len(segBytes) == 0 {
		t.Fatal("audio rendition segment body empty")
	}
	return segBytes
}

// audioStreamByLabel finds a decision audio-Stream entry by its menu label.
func audioStreamByLabel(t *testing.T, dec decisionResp, label string) decisionAudioResp {
	t.Helper()
	for _, a := range dec.AudioStreams {
		if a.Label == label {
			return a
		}
	}
	t.Fatalf("no audio stream labeled %q in decision; got %+v", label, dec.AudioStreams)
	return decisionAudioResp{}
}

// TestDecisionListsAudioStreams: the playback Decision exposes the full selectable
// audio-Stream list with labels (parallel to the catalog), and reports the resolved
// Stream as audioStream — so a native client can build the same Audio menu.
func TestDecisionListsAudioStreams(t *testing.T) {
	requireAudioFixtures(t)
	srv, token, id := scanAudioMovieLib(t)

	dec := negotiateAudio(t, srv, token, id, mkvMultiAudioProfile())
	if len(dec.AudioStreams) != 4 {
		t.Fatalf("decision audioStreams count = %d, want 4; got %+v", len(dec.AudioStreams), dec.AudioStreams)
	}
	// Every entry carries an id (the audioStreamId selector) and a label; exactly one
	// is the default.
	defaults := 0
	for _, a := range dec.AudioStreams {
		if a.ID == "" || a.Label == "" {
			t.Errorf("audio stream missing id/label: %+v", a)
		}
		if a.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default audio stream count = %d, want 1", defaults)
	}
	// The resolved audioStream is the default English AAC stereo track.
	if dec.AudioStream == nil || dec.AudioStream.Codec != "aac" {
		t.Errorf("resolved audioStream = %+v, want aac (the default)", dec.AudioStream)
	}
	// The labeled projection matches slice 01 (a commentary is distinctly labeled).
	audioStreamByLabel(t, dec, "English Director's Commentary")
	audioStreamByLabel(t, dec, "Japanese 5.1")
}

// TestDeliveredAudioMatchesReportedStream is the bug's regression test: on a remux
// of the multi-audio fixture, the DELIVERED audio identity (ffprobed) must equal the
// Stream the Decision reports — NOT ffmpeg's implicit most-channels pick. Before the
// explicit -map fix, negotiating the default English AAC stereo would report aac/2ch
// while ffmpeg delivered the AC3 5.1; this pins delivered == reported for both the
// default and a preferred-language selection.
//
// Since audio-streams/03 a multi-audio session is DEMUXED: the video variant carries
// no audio and each Stream rides as its own in-band rendition. So the delivered audio
// is probed from the DEFAULT audio rendition's segment (the resolved Stream), not the
// video segment — the demuxed evolution of the same delivered-vs-reported pin.
func TestDeliveredAudioMatchesReportedStream(t *testing.T) {
	requireAudioFixtures(t)
	requireFFmpeg(t)
	srv, token, id := scanAudioMovieLib(t)

	// (a) Default resolution → the English AAC stereo track. Delivered must be aac/2.
	decEn := negotiateAudio(t, srv, token, id, withPreferredAudioLang(remuxMultiAudioProfile(), "en"))
	if decEn.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream (remux); body forces a container remux", decEn.Tier)
	}
	if decEn.AudioStream == nil || decEn.AudioStream.Codec != "aac" || decEn.AudioStream.Channels != 2 {
		t.Fatalf("reported audioStream = %+v, want aac/2ch (default)", decEn.AudioStream)
	}
	segEn := fetchDefaultAudioRenditionSegment(t, srv, decEn.StreamURL, token)
	codecEn, chEn := ffprobeAudioIdentity(t, segEn)
	if codecEn != decEn.AudioStream.Codec || chEn != decEn.AudioStream.Channels {
		t.Errorf("delivered audio = %s/%dch, but decision reported %s/%dch (reported-vs-audible divergence)",
			codecEn, chEn, decEn.AudioStream.Codec, decEn.AudioStream.Channels)
	}

	// (b) preferredAudioLang=ja → the Japanese AC3 5.1 track. Delivered audibly
	// changes to ac3/6ch, again matching the reported Stream.
	decJa := negotiateAudio(t, srv, token, id, withPreferredAudioLang(remuxMultiAudioProfile(), "ja"))
	if decJa.AudioStream == nil || decJa.AudioStream.Codec != "ac3" || decJa.AudioStream.Channels != 6 {
		t.Fatalf("reported audioStream = %+v, want ac3/6ch (preferred ja)", decJa.AudioStream)
	}
	segJa := fetchDefaultAudioRenditionSegment(t, srv, decJa.StreamURL, token)
	codecJa, chJa := ffprobeAudioIdentity(t, segJa)
	if codecJa != decJa.AudioStream.Codec || chJa != decJa.AudioStream.Channels {
		t.Errorf("delivered audio = %s/%dch, but decision reported %s/%dch",
			codecJa, chJa, decJa.AudioStream.Codec, decJa.AudioStream.Channels)
	}
	// The two preferences deliver DIFFERENT audio — preferredAudioLang is audible.
	if codecEn == codecJa && chEn == chJa {
		t.Errorf("preferredAudioLang en vs ja delivered identical audio (%s/%dch) — the preference is inert", codecEn, chEn)
	}
}

// TestAudioStreamIdTierEscalation: on a direct-play-capable File, selecting the
// DEFAULT audio by id keeps direct play, while a NON-DEFAULT selection escalates to
// remux (direct play carries only the default audio, ADR-0022).
func TestAudioStreamIdTierEscalation(t *testing.T) {
	requireAudioFixtures(t)
	srv, token, id := scanAudioMovieLib(t)

	// Discover the stream ids from a baseline decision.
	base := negotiateAudio(t, srv, token, id, mkvMultiAudioProfile())
	def := audioStreamByLabel(t, base, "English Stereo")
	ja := audioStreamByLabel(t, base, "Japanese 5.1")
	if base.Tier != "directPlay" {
		t.Fatalf("baseline tier = %q, want directPlay (native profile)", base.Tier)
	}

	// Default audio by id → still direct play, reporting that Stream.
	decDef := negotiateAudio(t, srv, token, id, withAudioStreamId(mkvMultiAudioProfile(), def.ID))
	if decDef.Tier != "directPlay" {
		t.Errorf("default-audioStreamId tier = %q, want directPlay (unchanged)", decDef.Tier)
	}
	if decDef.AudioStream == nil || decDef.AudioStream.Codec != "aac" {
		t.Errorf("default-audioStreamId reported %+v, want the aac default", decDef.AudioStream)
	}

	// Non-default (Japanese) by id → remux escalation, reporting the ac3 Stream.
	decJa := negotiateAudio(t, srv, token, id, withAudioStreamId(mkvMultiAudioProfile(), ja.ID))
	if decJa.Tier != "directStream" {
		t.Errorf("non-default-audioStreamId tier = %q, want directStream (remux escalation)", decJa.Tier)
	}
	if decJa.AudioStream == nil || decJa.AudioStream.Codec != "ac3" || decJa.AudioStream.Channels != 6 {
		t.Errorf("non-default-audioStreamId reported %+v, want ac3/6ch", decJa.AudioStream)
	}
}

// TestAudioStreamIdCodecTranscodeIsVideoCopyExempt: selecting the DTS commentary (a
// codec the client cannot decode) escalates to a transcode — but the fixture's video
// is h264 the client CAN decode, so the video is stream-COPIED and only the audio is
// re-encoded (ADR-0024). Such a video-copy transcode runs no video encoder, so it is
// UNMETERED (like remux): even at cap=1 a second DTS selection is NOT 503, and a
// concurrent GENUINE video transcode still fits. (The video-encode cap itself is
// covered by TestGovernanceTranscodeCapReturnsServerBusy, where the video IS
// re-encoded.)
func TestAudioStreamIdCodecTranscodeIsVideoCopyExempt(t *testing.T) {
	requireAudioFixtures(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, audioStreamsRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Audio Movie")

	base := negotiateAudio(t, srv, token, id, mkvMultiAudioProfile())
	dts := audioStreamByLabel(t, base, "English Director's Commentary")

	// 1) The DTS selection transcodes the AUDIO (dts is not in the profile); the h264
	// video is copied, so the session is unmetered.
	first := negotiateAudio(t, srv, token, id, withAudioStreamId(mkvMultiAudioProfile(), dts.ID))
	if first.Tier != "transcode" {
		t.Fatalf("dts-audioStreamId tier = %q, want transcode (undecodable audio codec)", first.Tier)
	}

	// 2) A second such selection at cap=1 is NOT rejected — a video-copy transcode never
	// took a cap slot, so there is nothing to be busy over (ADR-0024 exemption).
	second := negotiateAudio(t, srv, token, id, withAudioStreamId(mkvMultiAudioProfile(), dts.ID))
	if second.Tier != "transcode" {
		t.Fatalf("second dts selection tier = %q, want transcode (unmetered, not 503)", second.Tier)
	}

	// 3) A plain direct-play (default audio) is likewise unaffected by the cap. A SECOND
	// User with no memory genuinely default-plays (the admin's DTS pick above is now the
	// admin's Remembered audio, audio-streams/05), isolating "direct play is unmetered".
	otherID := srv.CreateUser(token, "kid", "memberpass123", "member")
	grantLibraries(t, srv, token, otherID, libID)
	other := srv.LoginAs("kid", "memberpass123")
	dp := negotiateAudio(t, srv, other, id, mkvMultiAudioProfile())
	if dp.Tier != "directPlay" {
		t.Errorf("no-selection tier at cap = %q, want directPlay (unmetered)", dp.Tier)
	}
}

// TestAudioStreamIdInvalidIs404: an audioStreamId that names no audio Stream of the
// Title fails structurally (404, hide existence), never a silent default.
func TestAudioStreamIdInvalidIs404(t *testing.T) {
	requireAudioFixtures(t)
	srv, token, id := scanAudioMovieLib(t)

	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, withAudioStreamId(mkvMultiAudioProfile(), "does-not-exist"), nil); s != http.StatusNotFound {
		t.Errorf("unknown audioStreamId status = %d, want 404; body: %s", s, b)
	}
}
