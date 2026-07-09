package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue subtitles/04 integration test: IMAGE subtitle burn-in through the real
// HTTP API. Using the "Sub Movie" fixture (embedded text streams + sidecar image
// tracks: a .sup and a VOBSUB .idx/.sub), a playback request carrying a
// burnSubtitleId that names an image track must ESCALATE to the transcode tier
// (ADR-0020), count against the transcode governance cap (ADR-0009, so a second
// one at the cap → 503 SERVER_BUSY with a suggestedMaxBitrate), and leave a
// no-burn direct-play request unaffected. These assertions read the decision +
// error envelopes only, so they never start ffmpeg (the dummy sidecar bytes are
// never rasterized) — the args-builder + escalation unit tests cover the
// mechanics, and Playwright covers the browser re-negotiation.

// firstImageSubtitleID negotiates a direct-play decision and returns the id of an
// image Subtitle track (the sidecar .sup / VOBSUB) the fixture offers.
func firstImageSubtitleID(t *testing.T, srv *testharness.Server, token, titleID string) string {
	t.Helper()
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, mkvSubProfile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	for _, s := range dec.Subtitles {
		if s.Kind == "image" {
			return s.ID
		}
	}
	t.Fatalf("no image subtitle track in the decision; got %+v", dec.Subtitles)
	return ""
}

// burnRequest is mkvSubProfile plus a burnSubtitleId — the shape the client sends
// when the viewer selects an image track from the captions menu.
func burnRequest(subID string) map[string]any {
	req := mkvSubProfile()
	req["burnSubtitleId"] = subID
	return req
}

// TestSubtitleBurnInEscalatesToTranscode: a burnSubtitleId on an image sub returns
// a transcode-tier decision with an HLS streamUrl, even though the File would
// otherwise direct-play.
func TestSubtitleBurnInEscalatesToTranscode(t *testing.T) {
	requireSubtitleFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Sub Movie")

	imageID := firstImageSubtitleID(t, srv, token, id)

	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest(imageID), &dec)
	if status != http.StatusOK {
		t.Fatalf("burn playback status = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "transcode" {
		t.Errorf("burn tier = %q, want transcode (an image sub escalates); body: %s", dec.Tier, body)
	}
	if dec.StreamURL == "" {
		t.Errorf("burn decision has no streamUrl; body: %s", body)
	}
}

// TestSubtitleBurnInUnknownIs404: a burnSubtitleId that is not a burnable image
// track — an unknown id or a TEXT track (text is delivered selectably, never
// burned) — is 404, not a transcode.
func TestSubtitleBurnInUnknownIs404(t *testing.T) {
	requireSubtitleFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Sub Movie")

	// An unknown id.
	if status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest("does-not-exist"), nil); status != http.StatusNotFound {
		t.Errorf("burn unknown-id status = %d, want 404; body: %s", status, body)
	}

	// A TEXT track id (from the decision) is not burnable.
	var dec decisionResp
	srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, mkvSubProfile(), &dec)
	var textID string
	for _, s := range dec.Subtitles {
		if s.Kind == "text" {
			textID = s.ID
			break
		}
	}
	if textID == "" {
		t.Fatalf("no text track to test; got %+v", dec.Subtitles)
	}
	if status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest(textID), nil); status != http.StatusNotFound {
		t.Errorf("burn text-track status = %d, want 404 (text is never burned); body: %s", status, body)
	}
}

// TestSubtitleBurnInIsGoverned: burning an image sub counts against the transcode
// cap (ADR-0009). With cap=1, the first burn takes the slot; a second is rejected
// with 503 SERVER_BUSY + suggestedMaxBitrate; a no-burn direct-play is unaffected;
// and after ending the first, a new burn succeeds (the slot freed).
func TestSubtitleBurnInIsGoverned(t *testing.T) {
	requireSubtitleFixtures(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Sub Movie")

	imageID := firstImageSubtitleID(t, srv, token, id)

	// 1) First burn takes the only transcode slot.
	var first decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest(imageID), &first); s != http.StatusOK || first.Tier != "transcode" {
		t.Fatalf("first burn status/tier = %d/%q, want 200/transcode; body: %s", s, first.Tier, b)
	}

	// 2) A second burn at the cap → 503 SERVER_BUSY with a suggested lower bitrate.
	var busy busyResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest(imageID), &busy); s != http.StatusServiceUnavailable {
		t.Fatalf("second burn status = %d, want 503; body: %s", s, b)
	}
	if busy.Error.Code != "SERVER_BUSY" || !busy.Error.Details.Retryable || busy.Error.Details.SuggestedMaxBitrate <= 0 {
		t.Errorf("busy envelope = %+v, want SERVER_BUSY/retryable/suggested>0", busy.Error)
	}

	// 3) A no-burn direct-play request is unaffected by the transcode cap.
	var dp decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, mkvSubProfile(), &dp); s != http.StatusOK || dp.Tier != "directPlay" {
		t.Errorf("no-burn direct play at cap status/tier = %d/%q, want 200/directPlay; body: %s", s, dp.Tier, b)
	}

	// 4) End the first burn (frees the slot) → a new burn succeeds.
	if s, b := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete first burn status = %d, want 204; body: %s", s, b)
	}
	var third decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, burnRequest(imageID), &third); s != http.StatusOK || third.Tier != "transcode" {
		t.Errorf("post-free burn status/tier = %d/%q, want 200/transcode; body: %s", s, third.Tier, b)
	}
}
