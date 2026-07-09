package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue selectable-video/02 integration tests: an explicit videoStreamId is a full
// re-negotiation (the image-subtitle RESTART model — there is no in-band video
// rendition). Against the shared multi-video fixture (Video Movie: a "Colour" cut, the
// container default + taller Stream, and a "Black & White" cut sharing one audio),
// these drive the real HTTP stack and assert the tier the pick escalates to, the
// Stream the Decision reports, that a pick equal to the default does not escalate off
// direct play, that a pick the client can't deliver without a video encode is governed
// by the transcode cap (503 SERVER_BUSY), and that an invalid id is a hard 404.
// Mirrors audio_delivery_test.go seam-for-seam.

// mkvVideoProfile plays the fixture natively: mkv container + h264 within 1080p + aac →
// the default (Colour) direct-plays. Used for the decision-list and tier assertions.
func mkvVideoProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mkv"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxResolution": "1080p"}},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "1080p"},
	}
}

// withMaxResolution returns a copy of profile with constraints.maxResolution set — used
// to cap below the taller "Colour" cut (240px) so selecting it needs a video re-encode.
func withMaxResolution(profile map[string]any, res string) map[string]any {
	cons := profile["constraints"].(map[string]any)
	cons["maxResolution"] = res
	return profile
}

// withVideoStreamId returns a copy of profile with a top-level videoStreamId set.
func withVideoStreamId(profile map[string]any, id string) map[string]any {
	profile["videoStreamId"] = id
	return profile
}

// scanVideoMovieLib scans the multi-video fixture and returns (server, token, titleID).
func scanVideoMovieLib(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, videoStreamsRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Video Movie")
	return srv, token, id
}

// negotiateVideo POSTs a playback request and returns the decision (failing on non-200).
func negotiateVideo(t *testing.T, srv *testharness.Server, token, titleID string, body map[string]any) decisionResp {
	t.Helper()
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, body, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	return dec
}

// videoStreamByLabel finds a decision video-Stream entry by its menu label.
func videoStreamByLabel(t *testing.T, dec decisionResp, label string) videoStreamResp {
	t.Helper()
	for _, v := range dec.VideoStreams {
		if v.Label == label {
			return v
		}
	}
	t.Fatalf("no video stream labeled %q in decision; got %+v", label, dec.VideoStreams)
	return videoStreamResp{}
}

// TestDecisionListsVideoStreams: the playback Decision exposes the full selectable
// video-Stream list with labels + ids (the videoStreamId selectors), reports the
// resolved Stream as videoStream, and flags exactly one default — so a client can build
// the Video menu and know which cut is playing.
func TestDecisionListsVideoStreams(t *testing.T) {
	requireVideoFixtures(t)
	srv, token, id := scanVideoMovieLib(t)

	dec := negotiateVideo(t, srv, token, id, mkvVideoProfile())
	if len(dec.VideoStreams) != 2 {
		t.Fatalf("decision videoStreams count = %d, want 2; got %+v", len(dec.VideoStreams), dec.VideoStreams)
	}
	defaults := 0
	for _, v := range dec.VideoStreams {
		if v.ID == "" || v.Label == "" {
			t.Errorf("video stream missing id/label: %+v", v)
		}
		if v.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default video stream count = %d, want 1", defaults)
	}
	// The resolved videoStream is the capability default: the taller "Colour" cut.
	col := videoStreamByLabel(t, dec, "Colour")
	if !col.IsDefault {
		t.Errorf("'Colour' should be the resolved default video stream")
	}
	videoStreamByLabel(t, dec, "Black & White")
}

// TestVideoStreamIdTierEscalation: on a direct-play-capable File, selecting the DEFAULT
// video by id keeps direct play, while a NON-DEFAULT selection escalates to remux
// (direct play carries only the default video, ADR-0025). The reported videoStream
// follows the pick.
func TestVideoStreamIdTierEscalation(t *testing.T) {
	requireVideoFixtures(t)
	srv, token, id := scanVideoMovieLib(t)

	base := negotiateVideo(t, srv, token, id, mkvVideoProfile())
	if base.Tier != "directPlay" {
		t.Fatalf("baseline tier = %q, want directPlay (native profile)", base.Tier)
	}
	col := videoStreamByLabel(t, base, "Colour")       // default
	bw := videoStreamByLabel(t, base, "Black & White") // non-default

	// Default video by id → still direct play, reporting that Stream.
	decDef := negotiateVideo(t, srv, token, id, withVideoStreamId(mkvVideoProfile(), col.ID))
	if decDef.Tier != "directPlay" {
		t.Errorf("default-videoStreamId tier = %q, want directPlay (unchanged)", decDef.Tier)
	}
	if decDef.VideoStream.Height != col.Height {
		t.Errorf("default-videoStreamId reported height %d, want %d (Colour)", decDef.VideoStream.Height, col.Height)
	}

	// Non-default (Black & White) by id → remux escalation, reporting that Stream.
	decBW := negotiateVideo(t, srv, token, id, withVideoStreamId(mkvVideoProfile(), bw.ID))
	if decBW.Tier != "directStream" {
		t.Errorf("non-default-videoStreamId tier = %q, want directStream (remux escalation)", decBW.Tier)
	}
	if decBW.VideoStream.Height != bw.Height {
		t.Errorf("non-default-videoStreamId reported height %d, want %d (Black & White)", decBW.VideoStream.Height, bw.Height)
	}
}

// TestVideoStreamIdUndecodableIsGovernedTranscode: under a resolution cap below the
// taller "Colour" cut, the default is the co-packaged "Black & White" cut (direct
// play). Selecting Colour — which the client cannot take without a video re-encode —
// escalates to a genuine transcode that IS governed by the concurrent-transcode cap: a
// second such switch at cap=1 returns 503 SERVER_BUSY (NOT cap-exempt, contrast the
// in-band audio switch). Covers acceptance: transcode for THAT Stream + the cap/503.
func TestVideoStreamIdUndecodableIsGovernedTranscode(t *testing.T) {
	requireVideoFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, videoStreamsRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Video Movie")

	// Discover the ids from a baseline at the capped resolution: the default is now the
	// shorter Black & White cut (direct play), Colour exceeds the cap.
	base := negotiateVideo(t, srv, token, id, withMaxResolution(mkvVideoProfile(), "144p"))
	if base.Tier != "directPlay" {
		t.Fatalf("capped baseline tier = %q, want directPlay (default is the short cut)", base.Tier)
	}
	col := videoStreamByLabel(t, base, "Colour")

	// 1) Selecting Colour re-encodes the video (the cap forbids copying it) → transcode,
	// reporting the Colour Stream. It takes the only cap slot.
	first := negotiateVideo(t, srv, token, id, withVideoStreamId(withMaxResolution(mkvVideoProfile(), "144p"), col.ID))
	if first.Tier != "transcode" {
		t.Fatalf("colour-under-cap tier = %q, want transcode (video must re-encode)", first.Tier)
	}
	if first.VideoStream.Height != col.Height {
		t.Errorf("transcode reported height %d, want %d (Colour, not a fallback)", first.VideoStream.Height, col.Height)
	}

	// 2) A second such switch at cap=1 is rejected with 503 SERVER_BUSY — a video-
	// re-encoding switch is metered, not exempt.
	var busy busyResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token,
		withVideoStreamId(withMaxResolution(mkvVideoProfile(), "144p"), col.ID), &busy)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("second video switch status = %d, want 503; body: %s", status, raw)
	}
	if busy.Error.Code != "SERVER_BUSY" {
		t.Errorf("error code = %q, want SERVER_BUSY; body: %s", busy.Error.Code, raw)
	}
	if !busy.Error.Details.Retryable {
		t.Errorf("details.retryable = false, want true; body: %s", raw)
	}

	// 3) Freeing the slot lets the switch through — the cap governs it like any start.
	if s, b := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete first transcode status = %d, want 204; body: %s", s, b)
	}
	third := negotiateVideo(t, srv, token, id, withVideoStreamId(withMaxResolution(mkvVideoProfile(), "144p"), col.ID))
	if third.Tier != "transcode" {
		t.Errorf("post-free tier = %q, want transcode", third.Tier)
	}
}

// TestVideoStreamIdInvalidIs404: a videoStreamId that names no selectable video Stream
// of the Title fails structurally (404, hide existence), never a silent default.
func TestVideoStreamIdInvalidIs404(t *testing.T) {
	requireVideoFixtures(t)
	srv, token, id := scanVideoMovieLib(t)

	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token,
		withVideoStreamId(mkvVideoProfile(), "does-not-exist"), nil); s != http.StatusNotFound {
		t.Errorf("unknown videoStreamId status = %d, want 404; body: %s", s, b)
	}
}
