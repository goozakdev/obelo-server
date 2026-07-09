package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Real-ffmpeg integration tests for transcode governance (ADR-0009 / issue
// TRANSCODE-04), driven through the HTTP API with the cap set to 1 via the
// harness. They assert: a transcode-requiring playback at the cap returns 503
// SERVER_BUSY with a suggestedMaxBitrate (reject-don't-queue); a concurrent
// direct-play AND a remux request still succeed at the cap (only transcode is
// metered); and after ending the first transcode (DELETE), a new transcode
// succeeds (the slot was freed).

// busyResp is the SERVER_BUSY error envelope (api-contract.md): 503,
// code SERVER_BUSY, details { retryable, suggestedMaxBitrate }.
type busyResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Retryable           bool  `json:"retryable"`
			SuggestedMaxBitrate int64 `json:"suggestedMaxBitrate"`
		} `json:"details"`
	} `json:"error"`
}

// TestGovernanceTranscodeCapReturnsServerBusy is the core governance assertion.
func TestGovernanceTranscodeCapReturnsServerBusy(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	// Cap of 1: a single transcode saturates the server.
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner") // mkv mpeg4/mp3 — forces transcode under transcodeProfile

	// 1) First transcode takes the only slot.
	first := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())

	// 2) A second transcode-requiring request is rejected with 503 SERVER_BUSY.
	var busy busyResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, transcodeProfile(), &busy)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("second transcode status = %d, want 503; body: %s", status, raw)
	}
	if busy.Error.Code != "SERVER_BUSY" {
		t.Errorf("error code = %q, want SERVER_BUSY; body: %s", busy.Error.Code, raw)
	}
	if !busy.Error.Details.Retryable {
		t.Errorf("details.retryable = false, want true; body: %s", raw)
	}
	if busy.Error.Details.SuggestedMaxBitrate <= 0 {
		t.Errorf("details.suggestedMaxBitrate = %d, want > 0; body: %s", busy.Error.Details.SuggestedMaxBitrate, raw)
	}

	// 3) Direct play (Dune, mp4/h264/aac) succeeds at the cap — unmetered.
	duneID := findTitle(t, list, "Dune")
	var dp decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dp); s != http.StatusOK {
		t.Fatalf("direct play at cap status = %d, want 200; body: %s", s, b)
	}
	if dp.Tier != "directPlay" {
		t.Errorf("Dune tier = %q, want directPlay", dp.Tier)
	}

	// 4) Remux (Blade Runner under a container-only mismatch) succeeds at the cap —
	// also unmetered.
	var rm decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, remuxProfile(), &rm); s != http.StatusOK {
		t.Fatalf("remux at cap status = %d, want 200; body: %s", s, b)
	}
	if rm.Tier != "directStream" {
		t.Errorf("remux tier = %q, want directStream", rm.Tier)
	}

	// 5) End the first transcode (DELETE) — frees the slot.
	if s, b := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete first transcode status = %d, want 204; body: %s", s, b)
	}

	// 6) A new transcode now succeeds (previously-rejected path goes through).
	var third decisionResp
	s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, transcodeProfile(), &third)
	if s != http.StatusOK {
		t.Fatalf("transcode after freed slot status = %d, want 200; body: %s", s, b)
	}
	if third.Tier != "transcode" {
		t.Errorf("post-free tier = %q, want transcode", third.Tier)
	}
}

// TestGovernanceSuggestedBitrateIsLower: the SERVER_BUSY suggestedMaxBitrate is a
// genuine step below the rejected transcode's estimate, so a client retry is
// meaningful (it may land in a cheaper tier or a smaller transcode).
func TestGovernanceSuggestedBitrateIsLower(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	// Saturate the cap, capturing the first decision's estimatedBitrate.
	first := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())

	var busy busyResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+bladeID+"/playback", token, transcodeProfile(), &busy)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", status, raw)
	}
	if first.EstimatedBitrate > 0 && busy.Error.Details.SuggestedMaxBitrate >= first.EstimatedBitrate {
		t.Errorf("suggestedMaxBitrate %d not below estimate %d", busy.Error.Details.SuggestedMaxBitrate, first.EstimatedBitrate)
	}
}
