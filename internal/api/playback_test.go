package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box integration tests for the direct-play tier (issue 07): negotiate a
// Capability profile against a scanned Title, stream the returned URL with
// byte-range support, and end the session. We reuse the checked-in movie
// fixtures (Dune mp4/h264/aac, Blade Runner mkv/mpeg4/mp3) to exercise both the
// directPlay-yes and TRANSCODE_REQUIRED outcomes.

// --- wire shapes ------------------------------------------------------------

type decisionStreamResp struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
}

type decisionSubtitleResp struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Language string `json:"language"`
	Forced   bool   `json:"forced"`
	Label    string `json:"label"`
	URL      string `json:"url"`
	Format   string `json:"format"`
}

type decisionResp struct {
	SessionID string `json:"sessionId"`
	Tier      string `json:"tier"`
	StreamURL string `json:"streamUrl"`
	Edition   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"edition"`
	// Both are pointers because both are omitted when there is no such Stream: video
	// for an audio-only Track (ADR-0017), audio for a silent File. A value type here
	// cannot tell "omitted" from "zero", which is the very bug this shape now guards.
	VideoStream      *decisionStreamResp    `json:"videoStream"`
	AudioStream      *decisionStreamResp    `json:"audioStream"`
	AudioStreams     []decisionAudioResp    `json:"audioStreams"`
	VideoStreams     []videoStreamResp      `json:"videoStreams"`
	Subtitles        []decisionSubtitleResp `json:"subtitles"`
	EstimatedBitrate int64                  `json:"estimatedBitrate"`
}

// decisionAudioResp is one entry of the decision's selectable audio-Stream list
// (audio-streams/02), the same labeled projection the catalog exposes per File.
type decisionAudioResp struct {
	ID        string `json:"id"`
	Codec     string `json:"codec"`
	Language  string `json:"language"`
	Channels  int    `json:"channels"`
	Layout    string `json:"layout"`
	IsDefault bool   `json:"isDefault"`
	Label     string `json:"label"`
}

// mp4Profile is a Capability profile that supports the Dune/Sample fixtures
// (mp4 + h264@1080p + aac), with generous constraints.
func mp4Profile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers": []string{"mp4", "mkv"},
			"videoCodecs": []map[string]any{
				{"codec": "h264", "maxLevel": "4.2", "maxResolution": "1080p"},
			},
			"audioCodecs":      []string{"aac", "ac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{
			"maxBitrate":    100000000,
			"maxResolution": "1080p",
		},
	}
}

// scanFixtureLibrary creates a Movie Library over the checked-in fixtures, scans
// it, and returns (token, list of titles).
func scanFixtureLibrary(t *testing.T, srv *testharness.Server, token string) titlesListResp {
	t.Helper()
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list); status != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", status, body)
	}
	return list
}

// TestPlaybackDirectPlay: a profile that supports the File returns a directPlay
// decision with a progressive streamUrl and the selected edition/streams.
func TestPlaybackDirectPlay(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "directPlay" {
		t.Errorf("tier = %q, want directPlay; body: %s", dec.Tier, body)
	}
	if dec.SessionID == "" {
		t.Errorf("sessionId empty; body: %s", body)
	}
	wantURL := "/api/v1/sessions/" + dec.SessionID + "/stream"
	if dec.StreamURL != wantURL {
		t.Errorf("streamUrl = %q, want %q", dec.StreamURL, wantURL)
	}
	if dec.Edition.ID == "" {
		t.Errorf("edition id empty; body: %s", body)
	}
	// The counterweight to TestMusicTrackDecisionOmitsVideoStreamKey: a Movie must
	// still CARRY videoStream. Omitting it for audio-only is only correct if real
	// video is unaffected, so the two assertions are only meaningful as a pair.
	if dec.VideoStream == nil {
		t.Fatalf("movie decision omits videoStream, want it present; body: %s", body)
	}
	if dec.VideoStream.Codec != "h264" {
		t.Errorf("videoStream codec = %q, want h264", dec.VideoStream.Codec)
	}
	if dec.AudioStream == nil || dec.AudioStream.Codec != "aac" {
		t.Errorf("audioStream = %+v, want codec aac", dec.AudioStream)
	}
	// Dune carries one sidecar English SubRip track (Dune (2021).en.srt): the
	// decision lists it as a deliverable text track with an out-of-band .vtt URL.
	if dec.Subtitles == nil {
		t.Fatalf("subtitles is null, want a list; body: %s", body)
	}
	var enText *decisionSubtitleResp
	for i := range dec.Subtitles {
		if dec.Subtitles[i].Kind == "text" && dec.Subtitles[i].Language == "en" {
			enText = &dec.Subtitles[i]
		}
	}
	if enText == nil {
		t.Fatalf("no English text track in the decision; got %+v", dec.Subtitles)
	}
	if enText.Source != "sidecar" {
		t.Errorf("English track source = %q, want sidecar", enText.Source)
	}
	if enText.URL == "" || !strings.HasSuffix(enText.URL, ".vtt") {
		t.Errorf("English text track URL = %q, want a non-empty .vtt endpoint", enText.URL)
	}
	if dec.EstimatedBitrate <= 0 {
		t.Errorf("estimatedBitrate = %d, want > 0", dec.EstimatedBitrate)
	}
}

// TestPlaybackDirectStreamContainerOnly: a profile that supports the File's
// codecs (mpeg4 + mp3) but NOT its container (mkv) now negotiates the
// directStream (remux) tier — the streamUrl is an HLS media playlist, not the
// progressive stream and not a TRANSCODE_REQUIRED error (slice 1).
func TestPlaybackDirectStreamContainerOnly(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	// Profile supports only the mp4 container, but the mkv fixture's mpeg4/mp3
	// codecs are fine — the container is the SOLE mismatch → remux.
	body := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}, {"codec": "mpeg4", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac", "mp3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, body, &dec)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (directStream); body: %s", status, raw)
	}
	if dec.Tier != "directStream" {
		t.Errorf("tier = %q, want directStream; body: %s", dec.Tier, raw)
	}
	wantURL := "/api/v1/sessions/" + dec.SessionID + "/hls/index.m3u8"
	if dec.StreamURL != wantURL {
		t.Errorf("streamUrl = %q, want %q (HLS playlist)", dec.StreamURL, wantURL)
	}
	if dec.VideoStream.Codec != "mpeg4" {
		t.Errorf("videoStream codec = %q, want mpeg4 (copied, not re-encoded)", dec.VideoStream.Codec)
	}
}

// assertTranscodeDecision POSTs a playback request expected to yield a transcode
// decision and asserts the tier + an HLS streamUrl (the transcode tier is now
// delivered, not TRANSCODE_REQUIRED). It returns the decision for any further
// per-test checks.
func assertTranscodeDecision(t *testing.T, srv *testharness.Server, token, titleID string, body map[string]any) decisionResp {
	t.Helper()
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, body, &dec)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (transcode); body: %s", status, raw)
	}
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode; body: %s", dec.Tier, raw)
	}
	// The HLS streamUrl is the bare media playlist, OR the master playlist when the
	// File carries a deliverable text subtitle (ADR-0020, slice 03) — e.g. the Dune
	// fixture has an English sidecar, so its transcode is a master.
	base := "/api/v1/sessions/" + dec.SessionID + "/hls/"
	if dec.StreamURL != base+"index.m3u8" && dec.StreamURL != base+"master.m3u8" {
		t.Errorf("streamUrl = %q, want %sindex.m3u8 or %smaster.m3u8", dec.StreamURL, base, base)
	}
	return dec
}

// TestPlaybackTranscodeCodec: an mkv-capable profile that lacks the video codec
// (mpeg4) — a remux cannot fix a codec mismatch — now negotiates the transcode
// tier with an HLS streamUrl (no longer TRANSCODE_REQUIRED).
func TestPlaybackTranscodeCodec(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	body := map[string]any{
		"deviceProfile": map[string]any{
			"containers":  []string{"mkv", "mp4"},
			"videoCodecs": []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs": []string{"aac", "mp3"},
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	assertTranscodeDecision(t, srv, token, bladeID, body)
}

// TestPlaybackTranscodeBitrate: a maxBitrate below the File's bitrate flips an
// otherwise-playable File to the transcode tier (the bitrate must be re-encoded
// down).
func TestPlaybackTranscodeBitrate(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	body := map[string]any{
		"deviceProfile": map[string]any{
			"containers":  []string{"mp4"},
			"videoCodecs": []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs": []string{"aac"},
		},
		"constraints": map[string]any{"maxBitrate": 1}, // 1 bit/s: nothing fits
	}
	assertTranscodeDecision(t, srv, token, duneID, body)
}

// TestPlaybackTranscodeResolution: a constraint maxResolution below the File's
// height flips an otherwise-playable File to the transcode tier (scaled down).
// The Dune fixture is 320x240; a 144p cap is below its 240px height. A sufficient
// cap (480p ≥ 240) still direct-plays — direct play is unchanged.
func TestPlaybackTranscodeResolution(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune") // 320x240

	body := map[string]any{
		"deviceProfile": map[string]any{
			"containers":  []string{"mp4"},
			"videoCodecs": []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs": []string{"aac"},
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "144p"},
	}
	assertTranscodeDecision(t, srv, token, duneID, body)

	// A sufficient cap (480p ≥ 240) direct-plays (direct play unchanged).
	body["constraints"].(map[string]any)["maxResolution"] = "480p"
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, body, &dec)
	if status != http.StatusOK || dec.Tier != "directPlay" {
		t.Fatalf("480p cap status = %d tier = %q, want 200 directPlay; body: %s", status, dec.Tier, raw)
	}
}

// TestPlaybackStreamWithRange: the streamUrl serves the File bytes, and a Range
// request returns 206 Partial Content with the correct slice.
func TestPlaybackStreamWithRange(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var dec decisionResp
	if status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec); status != http.StatusOK {
		t.Fatalf("playback status = %d; body: %s", status, body)
	}

	// Full GET: 200 with the whole file.
	full := authStream(t, srv, dec.StreamURL, token, "")
	defer full.Body.Close()
	if full.StatusCode != http.StatusOK {
		t.Fatalf("full stream status = %d, want 200", full.StatusCode)
	}
	whole, _ := io.ReadAll(full.Body)
	if len(whole) == 0 {
		t.Fatal("full stream returned empty body")
	}

	// Range GET: first 10 bytes → 206 with exactly those bytes.
	part := authStream(t, srv, dec.StreamURL, token, "bytes=0-9")
	defer part.Body.Close()
	if part.StatusCode != http.StatusPartialContent {
		t.Fatalf("range stream status = %d, want 206", part.StatusCode)
	}
	got, _ := io.ReadAll(part.Body)
	if len(got) != 10 {
		t.Fatalf("range body length = %d, want 10", len(got))
	}
	if string(got) != string(whole[:10]) {
		t.Errorf("range bytes mismatch with full prefix")
	}
	if cr := part.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-9/") {
		t.Errorf("Content-Range = %q, want bytes 0-9/<size>", cr)
	}
	if ar := part.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
}

// TestPlaybackDeleteEndsSession: DELETE /sessions/{id} ends the session; a
// subsequent stream of the same URL is 404.
func TestPlaybackDeleteEndsSession(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var dec decisionResp
	srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec)

	// Stream works before delete.
	pre := authStream(t, srv, dec.StreamURL, token, "")
	pre.Body.Close()
	if pre.StatusCode != http.StatusOK {
		t.Fatalf("pre-delete stream = %d, want 200", pre.StatusCode)
	}

	status, body := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, body)
	}

	// Stream now 404 (session gone).
	post := authStream(t, srv, dec.StreamURL, token, "")
	post.Body.Close()
	if post.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete stream = %d, want 404", post.StatusCode)
	}

	// Deleting again is 404 (already ended).
	status, _ = srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil)
	if status != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", status)
	}
}

// TestPlaybackOtherUserSessionForbidden: a different User cannot stream or end
// another User's session — it is hidden as 404 (existence-hiding posture).
func TestPlaybackOtherUserSessionForbidden(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var dec decisionResp
	srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec)

	// A second User (Member) with their own token.
	srv.CreateMember("member", "memberpass123")
	other := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	resp := authStream(t, srv, dec.StreamURL, other, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("other-user stream = %d, want 404", resp.StatusCode)
	}
	status, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, other, nil, nil)
	if status != http.StatusNotFound {
		t.Errorf("other-user delete = %d, want 404", status)
	}
}

// TestPlaybackRequiresAuth: playback negotiation and streaming both require a
// bearer token.
func TestPlaybackRequiresAuth(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	status, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", "", mp4Profile(), nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unauth playback = %d, want 401", status)
	}
	// Create a real session, then stream it without a token.
	var dec decisionResp
	srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec)
	resp := authStream(t, srv, dec.StreamURL, "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth stream = %d, want 401", resp.StatusCode)
	}
}

// TestPlaybackMissingTitle: negotiating an unknown Title is 404 (hide existence).
func TestPlaybackMissingTitle(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/no-such-title/playback", token, mp4Profile(), &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", env.Error.Code)
	}
}

// TestPlaybackExplicitEdition: an explicit editionId is honored — the decision's
// edition matches the requested one.
func TestPlaybackExplicitEdition(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	// Discover Dune's edition id via the title detail.
	var detail titleDetailResp
	srv.AuthGET("/api/v1/titles/"+duneID, token, &detail)
	if len(detail.Editions) == 0 {
		t.Fatal("Dune has no editions")
	}
	edID := detail.Editions[0].ID

	body := mp4Profile()
	body["editionId"] = edID
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, body, &dec)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, raw)
	}
	if dec.Edition.ID != edID {
		t.Errorf("edition id = %q, want %q", dec.Edition.ID, edID)
	}
}

// authStream issues a GET against an absolute API path (the streamUrl already
// includes the /api/v1 prefix) with an optional bearer token and Range header,
// returning the raw response (caller closes Body). It does not decode JSON
// because the stream body is binary.
func authStream(t *testing.T, srv *testharness.Server, apiPath, token, rng string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(apiPath), nil)
	if err != nil {
		t.Fatalf("building stream request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	return resp
}
