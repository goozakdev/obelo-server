package api_test

import (
	"bytes"
	"net/http"
	"testing"
)

// The one re-resolve of an Online session (ADR-0068 decision 10, issue 06), through
// the endpoints a client reaches: a media URL the host refuses is resolved again
// once and playback continues; when that cannot be done the session ends and the
// player's next keepalive is told why.

func resolveCalls(src *onlineSource) int {
	src.mu.Lock()
	defer src.mu.Unlock()
	return len(src.resolved)
}

// TestAnExpiredMediaURLIsReResolvedOnceAndTheRelayContinues: the media host answers
// 410 to the first fetch; the Server resolves again and the client gets the bytes.
func TestAnExpiredMediaURLIsReResolvedOnceAndTheRelayContinues(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)

	status, play, raw := playOnline(t, srv, admin, "expiring", canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	resp, body := getBytes(t, srv, play.StreamURL, map[string]string{"Range": "bytes=1000-1999"})
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, media.body[1000:2000]) {
		t.Fatalf("range after the refusal = %d (%d bytes), want 206 and bytes 1000-1999", resp.StatusCode, len(body))
	}
	if n := resolveCalls(src); n != 2 {
		t.Fatalf("resolve ran %d times, want 2 (the play and one re-resolve)", n)
	}
	if n := media.hitCount("/expiring.mp4"); n != 2 {
		t.Fatalf("media host was asked %d times, want 2", n)
	}
	// Healthy from here: no further resolve.
	if resp, _ = getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("whole stream after recovery = %d, want 200", resp.StatusCode)
	}
	if n := resolveCalls(src); n != 2 {
		t.Fatalf("resolve ran %d times after recovery, want still 2", n)
	}
}

// TestAMediaURLThatStaysRefusedEndsTheSessionAndThePlayerIsToldWhy: the re-resolved
// URL is refused too; the session ends, the stream says 410, and the keepalive says
// the message the player shows, to the session's own User only.
func TestAMediaURLThatStaysRefusedEndsTheSessionAndThePlayerIsToldWhy(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)

	status, play, raw := playOnline(t, srv, admin, "dead", canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	if resp, _ := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("stream of a dead URL = %d, want 410", resp.StatusCode)
	}
	if n := resolveCalls(src); n != 2 {
		t.Fatalf("resolve ran %d times, want 2", n)
	}

	var env errorEnvelope
	st, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
		map[string]any{"positionMs": 1, "state": "playing"}, &env)
	if st != http.StatusGone || env.Error.Code != "SOURCE_GONE" || env.Error.Message != "This video is no longer available from "+onlineName {
		t.Fatalf("keepalive after the end = %d %s, want 410 SOURCE_GONE with the message", st, body)
	}
	if n := srv.CountStreamTokensForSession(play.SessionID); n != 0 {
		t.Fatalf("stream tokens after the session ended = %d, want 0", n)
	}
	if resp, _ := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stream after the session ended = %d, want 404", resp.StatusCode)
	}
}
