package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The raw-File session route (GET /sessions/{id}/stream and its stream-token form)
// serves the File's original bytes, which only a directPlay session is entitled to.
// A transcode or remux session's File may hold a video track the Playback ceiling
// refused, so the route answers it with the same 404 as an unknown session.

const rawStreamNotFoundBody = "session not found"

func assertRawStream404(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%s = %d, want 404 (raw File of a non-directPlay session); body: %.200s", what, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), rawStreamNotFoundBody) {
		t.Errorf("%s body = %.200s, want the %q hide-existence body", what, body, rawStreamNotFoundBody)
	}
}

// TestRawStreamRefusedForTranscodeSession: a transcode session's raw /stream is 404
// on both the bearer and the stream-token route.
func TestRawStreamRefusedForTranscodeSession(t *testing.T) {
	t.Parallel()
	requireVideoFixtures(t)
	requireFFmpeg(t)
	srv, token, id := scanVideoMovieLib(t)

	base := negotiateVideo(t, srv, token, id, withMaxResolution(mkvVideoProfile(), "144p"))
	col := videoStreamByLabel(t, base, "Colour")
	dec := negotiateVideo(t, srv, token, id, withVideoStreamId(withMaxResolution(mkvVideoProfile(), "144p"), col.ID))
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode", dec.Tier)
	}

	assertRawStream404(t, authStream(t, srv, "/api/v1/sessions/"+dec.SessionID+"/stream", token, ""), "bearer /stream")
	assertRawStream404(t, anonGET(t, srv, tokenStream(dec.StreamToken), ""), "token /stream")
}

// TestRawStreamRefusedForDirectStreamSession: a remux session's raw /stream is 404.
func TestRawStreamRefusedForDirectStreamSession(t *testing.T) {
	t.Parallel()
	requireVideoFixtures(t)
	srv, token, id := scanVideoMovieLib(t)

	base := negotiateVideo(t, srv, token, id, mkvVideoProfile())
	bw := videoStreamByLabel(t, base, "Black & White")
	dec := negotiateVideo(t, srv, token, id, withVideoStreamId(mkvVideoProfile(), bw.ID))
	if dec.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream", dec.Tier)
	}

	assertRawStream404(t, authStream(t, srv, "/api/v1/sessions/"+dec.SessionID+"/stream", token, ""), "bearer /stream")
	assertRawStream404(t, anonGET(t, srv, tokenStream(dec.StreamToken), ""), "token /stream")
}

// TestRawStreamStillServedForDirectPlaySession: the guard must not touch directPlay,
// Range included.
func TestRawStreamStillServedForDirectPlaySession(t *testing.T) {
	t.Parallel()
	requireVideoFixtures(t)
	srv, token, id := scanVideoMovieLib(t)

	dec := negotiateVideo(t, srv, token, id, mkvVideoProfile())
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %q, want directPlay", dec.Tier)
	}
	resp := authStream(t, srv, dec.StreamURL, token, "bytes=0-9")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged bearer /stream = %d, want 206", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); len(b) != 10 {
		t.Errorf("ranged body = %d bytes, want 10", len(b))
	}
	full := authStream(t, srv, dec.StreamURL, token, "")
	defer full.Body.Close()
	if full.StatusCode != http.StatusOK {
		t.Errorf("bearer /stream = %d, want 200", full.StatusCode)
	}
	tok := anonGET(t, srv, tokenStream(dec.StreamToken), "bytes=0-9")
	defer tok.Body.Close()
	if tok.StatusCode != http.StatusPartialContent {
		t.Errorf("ranged token /stream = %d, want 206", tok.StatusCode)
	}
}
