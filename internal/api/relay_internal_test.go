package api

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/playback"
)

// Unit tests for the two pure decisions the relay transport makes: which remote
// paths a session may reach, and what a playlist looks like once it has been
// rewritten (.scratch/linked-servers issue 09).

func relayTestSession() playback.Session {
	return playback.Session{
		ID:              "local-1",
		RemoteSessionID: "remote-9",
		RemoteTitleID:   "title-7",
		RelayLinkID:     "link-1",
	}
}

func TestRelayTailAllowed(t *testing.T) {
	sess := relayTestSession()
	cases := []struct {
		tail string
		want bool
	}{
		{"sessions/remote-9/stream", true},
		{"sessions/remote-9/hls/index.m3u8", true},
		{"sessions/remote-9/hls/master.m3u8", true},
		{"sessions/remote-9/hls/000.ts", true},
		{"sessions/remote-9/hls/init.mp4", true},
		{"sessions/remote-9/hls/audio_3.m3u8", true},
		{"sessions/remote-9/hls/subs_4_000.vtt", true},
		{"titles/title-7/subtitles/sub-2.vtt", true},

		// Another session's media, another Title's subtitles, a nested path, a
		// traversal, and anything that is not media at all: the relay holds this
		// household's credential and must not be a way into a friend's server.
		{"sessions/other/stream", false},
		{"sessions/other/hls/index.m3u8", false},
		{"titles/other/subtitles/sub-2.vtt", false},
		{"sessions/remote-9/hls/nested/000.ts", false},
		{"sessions/remote-9/../../libraries", false},
		{"libraries", false},
		{"users", false},
		{"", false},
	}
	for _, c := range cases {
		if got := relayTailAllowed(sess, c.tail); got != c.want {
			t.Errorf("relayTailAllowed(%q) = %v, want %v", c.tail, got, c.want)
		}
	}
}

// TestRewriteRelayPlaylistLeavesRelativeURIsAlone is the case that matters most:
// every playlist this project emits is relative, and a relative URI already
// resolves onto the relay path because the remote tail is preserved. Rewriting
// one would BREAK it.
func TestRewriteRelayPlaylistLeavesRelativeURIsAlone(t *testing.T) {
	in := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:7",
		`#EXT-X-MAP:URI="init.mp4"`,
		"#EXTINF:4.000,",
		"000.ts",
		"#EXTINF:4.000,",
		"001.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n")
	got := string(rewriteRelayPlaylist([]byte(in), relayURIMapper("local-1")))
	if got != in {
		t.Errorf("a relative playlist was rewritten:\n--- got\n%s\n--- want\n%s", got, in)
	}
}

// TestRewriteRelayPlaylistMapsAbsoluteURIs covers the version-skew case the
// rewrite exists for: a sharer that emits absolute API paths would otherwise
// point this household's player at its own server, where the session does not
// exist. Every place a URI can hide is checked.
func TestRewriteRelayPlaylistMapsAbsoluteURIs(t *testing.T) {
	in := strings.Join([]string{
		"#EXTM3U",
		`#EXT-X-MAP:URI="/api/v1/sessions/remote-9/hls/init.mp4"`,
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English",URI="/api/v1/sessions/remote-9/hls/audio_3.m3u8"`,
		`#EXT-X-KEY:METHOD=AES-128,URI="/api/v1/sessions/remote-9/hls/key.bin"`,
		"#EXTINF:4.000,",
		"/api/v1/sessions/remote-9/hls/000.ts",
		"https://elsewhere.example/000.ts",
		"",
	}, "\n")
	got := string(rewriteRelayPlaylist([]byte(in), relayURIMapper("local-1")))

	for _, want := range []string{
		`#EXT-X-MAP:URI="/api/v1/relay/local-1/sessions/remote-9/hls/init.mp4"`,
		`URI="/api/v1/relay/local-1/sessions/remote-9/hls/audio_3.m3u8"`,
		`URI="/api/v1/relay/local-1/sessions/remote-9/hls/key.bin"`,
		"\n/api/v1/relay/local-1/sessions/remote-9/hls/000.ts\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten playlist is missing %q:\n%s", want, got)
		}
	}
	// A URI that is not one of this API's paths is left exactly as it was: this is
	// a rewrite, not a proxy for the whole internet.
	if !strings.Contains(got, "https://elsewhere.example/000.ts") {
		t.Errorf("a foreign URI was rewritten:\n%s", got)
	}
}

// TestRelayTokenURIMapperKeepsTheTokenOutOfPlaylists: under a stream token there
// is no session id in the URL, so an absolute artifact URI is mapped back to its
// bare filename — which resolves correctly against the token playlist — rather
// than to a path this Server would have to write the secret into.
func TestRelayTokenURIMapperKeepsTheTokenOutOfPlaylists(t *testing.T) {
	mapURI := relayTokenURIMapper(relayTestSession())
	if got := mapURI("/api/v1/sessions/remote-9/hls/000.ts"); got != "000.ts" {
		t.Errorf("mapped to %q, want the bare filename", got)
	}
	// Anything else has no expressible form under a token and is left alone.
	for _, uri := range []string{
		"000.ts",
		"/api/v1/sessions/other/hls/000.ts",
		"/api/v1/titles/title-7/subtitles/sub-2.vtt",
	} {
		if got := mapURI(uri); got != uri {
			t.Errorf("mapURI(%q) = %q, want it unchanged", uri, got)
		}
	}
}

// TestRelayTailForArtifact maps the stream-token route's two artifact shapes onto
// the sharer's paths, and refuses anything else.
func TestRelayTailForArtifact(t *testing.T) {
	sess := relayTestSession()
	if tail, ok := relayTailForArtifact(sess, "stream"); !ok || tail != "sessions/remote-9/stream" {
		t.Errorf("progressive artifact → %q, %v", tail, ok)
	}
	if tail, ok := relayTailForArtifact(sess, "hls/index.m3u8"); !ok || tail != "sessions/remote-9/hls/index.m3u8" {
		t.Errorf("hls artifact → %q, %v", tail, ok)
	}
	for _, artifact := range []string{"hls/", "hls/nested/000.ts", "progress", ""} {
		if _, ok := relayTailForArtifact(sess, artifact); ok {
			t.Errorf("artifact %q was accepted", artifact)
		}
	}
}

// TestRelayDecisionResponseRewritesAndDropsTheSharersToken: the sharer's answer
// passes through whole, with its ids and URLs made local and its own stream token
// dropped (a credential this Server's routes cannot spend).
func TestRelayDecisionResponseRewritesAndDropsTheSharersToken(t *testing.T) {
	dec := playback.Decision{Relay: &playback.Relayed{
		RemoteSessionID: "remote-9",
		Decision: map[string]any{
			"sessionId":            "remote-9",
			"tier":                 "directStream",
			"streamUrl":            "/api/v1/sessions/remote-9/hls/master.m3u8",
			"estimatedBitrate":     float64(112023),
			"somethingNewer":       "kept",
			"streamToken":          "the-sharers-secret",
			"streamTokenExpiresAt": "2026-01-01T00:00:00Z",
			"subtitles": []any{
				map[string]any{"id": "sub-2", "url": "/api/v1/titles/title-7/subtitles/sub-2.vtt"},
				map[string]any{"id": "sub-3"},
			},
		},
	}}
	out := relayDecisionResponse(dec, "local-1")

	if out["sessionId"] != "local-1" {
		t.Errorf("sessionId = %v, want the local session", out["sessionId"])
	}
	if out["streamUrl"] != "/api/v1/relay/local-1/sessions/remote-9/hls/master.m3u8" {
		t.Errorf("streamUrl = %v", out["streamUrl"])
	}
	if _, ok := out["streamToken"]; ok {
		t.Error("the sharer's stream token was passed on")
	}
	if _, ok := out["streamTokenExpiresAt"]; ok {
		t.Error("the sharer's stream token expiry was passed on")
	}
	// Verbatim means verbatim: a field this build has never heard of still reaches
	// a client that has.
	if out["somethingNewer"] != "kept" || out["tier"] != "directStream" || out["estimatedBitrate"] != float64(112023) {
		t.Errorf("the sharer's own fields did not pass through: %#v", out)
	}
	subs, _ := out["subtitles"].([]any)
	if len(subs) != 2 {
		t.Fatalf("subtitles = %#v", out["subtitles"])
	}
	first, _ := subs[0].(map[string]any)
	if first["url"] != "/api/v1/relay/local-1/titles/title-7/subtitles/sub-2.vtt" {
		t.Errorf("subtitle url = %v", first["url"])
	}
	// The sharer's own copy is untouched by the rewrite (the map it decoded into is
	// not the map that goes out).
	if dec.Relay.Decision["sessionId"] != "remote-9" {
		t.Error("rewriting mutated the sharer's decoded answer")
	}
}
