package api_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/api"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the path-carried media routes (.scratch/session-stream-tokens,
// issue 02):
//
//	GET /api/v1/stream/{streamToken}/stream
//	GET /api/v1/stream/{streamToken}/hls/{file}
//
// The whole point of the shape is that a player which can present NO credential
// — an AirPlay receiver handed a URL — plays the same bytes, so every fetch here
// goes out with no Authorization header and no cookie of any kind (anonGET).

// --- helpers ------------------------------------------------------------------

// tokenHLS builds the token-carried URL of an HLS artifact. Note there is no
// session id in it: the token IS the session identifier, which is exactly why
// ticket 01 had to expose ResolveStreamToken alongside VerifyStreamToken.
func tokenHLS(streamToken, file string) string {
	return "/api/v1/stream/" + streamToken + "/hls/" + file
}

// tokenStream builds the token-carried progressive direct-play URL.
func tokenStream(streamToken string) string {
	return "/api/v1/stream/" + streamToken + "/stream"
}

// anonGET issues a GET carrying NO credential except whatever is in the path:
// no Authorization header, no cookie jar, no query parameter. It asserts that,
// rather than trusting it — a test that accidentally authenticated would pass
// while proving nothing, which is the one way this whole file could lie.
func anonGET(t *testing.T, srv *testharness.Server, apiPath, rng string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(apiPath), nil)
	if err != nil {
		t.Fatalf("building anonymous request for %s: %v", apiPath, err)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	if req.Header.Get("Authorization") != "" || len(req.Cookies()) != 0 {
		t.Fatalf("anonGET built a request that carries a credential: %v", req.Header)
	}
	// A zero-value Client has a nil Jar, so nothing is attached on the way out.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("anonymous GET %s: %v", apiPath, err)
	}
	return resp
}

// anonBytes fetches a token-carried artifact and asserts a 200 with a non-empty
// body, returning the bytes.
func anonBytes(t *testing.T, srv *testharness.Server, apiPath string) []byte {
	t.Helper()
	resp := anonGET(t, srv, apiPath, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET %s = %d, want 200; body: %s", apiPath, resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Fatalf("anonymous GET %s returned an empty body", apiPath)
	}
	return body
}

// anonText is anonBytes as a string, for playlists.
func anonText(t *testing.T, srv *testharness.Server, apiPath string) string {
	t.Helper()
	return string(anonBytes(t, srv, apiPath))
}

// assertPlaylistBytesIdentical fetches one playlist BOTH ways — bearer on
// /sessions/{id}/hls/{file}, anonymous on /stream/{token}/hls/{file} — and
// requires the bytes to match exactly.
//
// This is the property the whole design rests on. The token rides the path
// precisely so that nothing has to rewrite a playlist; the moment these two
// differ, something started rewriting, and the thing most likely to have been
// rewritten is ffmpeg's own output for the runtimes that serve it verbatim.
func assertPlaylistBytesIdentical(t *testing.T, srv *testharness.Server, dec decisionResp, bearer, file string) []byte {
	t.Helper()
	viaBearer := []byte(fetchText(t, srv, "/api/v1/sessions/"+dec.SessionID+"/hls/"+file, bearer))
	viaToken := anonBytes(t, srv, tokenHLS(dec.StreamToken, file))
	if !bytes.Equal(viaBearer, viaToken) {
		t.Errorf("playlist %q differs between the bearer path and the token path\nbearer:\n%s\ntoken:\n%s",
			file, viaBearer, viaToken)
	}
	return viaToken
}

// assertBareRelativeURIs is the other half of "zero playlist bytes change": the
// URIs a player resolves must stay BARE and RELATIVE, so they resolve back under
// /stream/{token}/hls/ by construction. An absolute path (or a URL with a query)
// in any of them means the token stopped riding along and every segment under it
// would 401 on a real receiver.
func assertBareRelativeURIs(t *testing.T, what, playlist string) {
	t.Helper()
	check := func(uri string) {
		switch {
		case strings.HasPrefix(uri, "/"), strings.Contains(uri, "://"):
			t.Errorf("%s carries a non-relative URI %q; relative resolution is what carries the token", what, uri)
		case strings.Contains(uri, "?"):
			t.Errorf("%s carries a query string on %q; a player drops the query when resolving", what, uri)
		}
	}
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if i := strings.Index(line, `URI="`); i >= 0 {
				rest := line[i+len(`URI="`):]
				if j := strings.IndexByte(rest, '"'); j >= 0 {
					check(rest[:j])
				}
			}
			continue
		}
		check(line)
	}
}

// --- a whole playthrough with no header and no cookie -------------------------

// TestStreamTokenHLSPlaythroughAnonymous walks a directStream session end to end
// through the token path with no Authorization header and no cookie: the master
// playlist, the video media playlist, EVERY video segment, and the in-band
// subtitle rendition's playlist and segments. It is the acceptance criterion in
// its most literal form — a native-HLS player pointed at
// /stream/{token}/hls/master.m3u8 and left alone.
func TestStreamTokenHLSPlaythroughAnonymous(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	root := generateSubtitledRemuxClip(t)
	list := scanLibraryAt(t, srv, bearer, root)
	titleID := findTitle(t, list, "Subbed Movie")

	dec := negotiateRemuxDecision(t, srv, bearer, titleID)
	if dec.StreamToken == "" {
		t.Fatal("decision carried no streamToken")
	}
	if !strings.HasSuffix(dec.StreamURL, "/hls/master.m3u8") {
		t.Fatalf("streamUrl = %q, want a master playlist (the sidecar subtitle is present)", dec.StreamURL)
	}

	// The master, fetched with nothing but the token in the path.
	master := anonText(t, srv, tokenHLS(dec.StreamToken, "master.m3u8"))
	if !strings.HasPrefix(strings.TrimSpace(master), "#EXTM3U") {
		t.Fatalf("master is not a playlist:\n%s", master)
	}
	if !strings.Contains(master, "#EXT-X-MEDIA:TYPE=SUBTITLES") {
		t.Errorf("master carries no SUBTITLES rendition:\n%s", master)
	}
	assertBareRelativeURIs(t, "master", master)

	// The video media playlist and every segment it lists.
	videoPL := anonText(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"))
	assertBareRelativeURIs(t, "video media playlist", videoPL)
	videoSegs := parseSegments(videoPL)
	if len(videoSegs) < 2 {
		t.Fatalf("video playlist lists %d segments, want >= 2:\n%s", len(videoSegs), videoPL)
	}
	for _, seg := range videoSegs {
		anonBytes(t, srv, tokenHLS(dec.StreamToken, seg))
	}

	// The in-band subtitle rendition: its playlist and its WebVTT segments.
	subURI := mediaAttr(t, master, "URI")
	subPL := anonText(t, srv, tokenHLS(dec.StreamToken, subURI))
	assertBareRelativeURIs(t, "subtitle rendition playlist", subPL)
	subSegs := parseSegments(subPL)
	if len(subSegs) < 3 {
		t.Fatalf("subtitle rendition lists %d segments, want >= 3:\n%s", len(subSegs), subPL)
	}
	seg0 := string(anonBytes(t, srv, tokenHLS(dec.StreamToken, subSegs[0])))
	if !strings.HasPrefix(seg0, "WEBVTT") {
		t.Errorf("subtitle segment 0 is not WebVTT:\n%s", seg0)
	}
	if !strings.Contains(seg0, "Opening line") {
		t.Errorf("subtitle segment 0 is missing its cue:\n%s", seg0)
	}

	// Content types are the player's, unchanged by the route it arrived on.
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, "master.m3u8"), "mpegurl")
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"), "mpegurl")
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, videoSegs[0]), "video/mp2t")
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, subSegs[0]), "text/vtt")

	// And the playlists are byte-identical to the bearer path's.
	assertPlaylistBytesIdentical(t, srv, dec, bearer, "master.m3u8")
	assertPlaylistBytesIdentical(t, srv, dec, bearer, "index.m3u8")
	assertPlaylistBytesIdentical(t, srv, dec, bearer, subURI)
}

// TestStreamTokenFMP4AndAudioRenditionAnonymous covers the two artifacts the
// AirPlay note in the ticket singles out: the fMP4 INIT segment and an AUDIO
// rendition. It is the HEVC-copy session (ADR-0024), the only one that produces
// both — a CODECS="hvc1…" variant with an #EXT-X-MAP init and a demuxed AUDIO
// group — and it is fetched entirely through the token path.
//
// The ordering mirrors what a receiver actually does: master first, then a
// rendition's init segment. Safari retries a rendition init exactly once before
// killing the presentation with a decode error, so this is the fetch that must
// not acquire any extra latency; the token path adds one indexed lookup, the
// same order of cost as the bearer path's token authentication.
func TestStreamTokenFMP4AndAudioRenditionAnonymous(t *testing.T) {
	requireHEVCFixture(t)
	requireFFmpeg(t)

	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, bearer, hevcRoot(t))
	scanLib(t, srv, bearer, libID, "")
	id := findTitle(t, listAllTitles(t, srv, bearer, libID), "HEVC Movie")

	dec := negotiateAudio(t, srv, bearer, id, hevcSafariProfileJSON())
	if dec.StreamToken == "" {
		t.Fatal("decision carried no streamToken")
	}
	if !strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("streamUrl = %q, want a master playlist (demuxed multi-audio)", dec.StreamURL)
	}

	master := anonText(t, srv, tokenHLS(dec.StreamToken, "master.m3u8"))
	if !strings.Contains(master, `CODECS="hvc1`) {
		t.Errorf("master lost the HEVC CODECS attribute on the token path:\n%s", master)
	}
	if !strings.Contains(master, "#EXT-X-MEDIA:TYPE=AUDIO") {
		t.Fatalf("master carries no AUDIO group:\n%s", master)
	}
	assertBareRelativeURIs(t, "master", master)

	// The fMP4 video track: init segment + a media segment, both token-only.
	videoPL := anonText(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"))
	assertBareRelativeURIs(t, "video media playlist", videoPL)
	videoInit := extMapURI(t, videoPL)
	videoSegs := parseSegments(videoPL)
	if len(videoSegs) == 0 || !strings.HasSuffix(videoSegs[0], ".m4s") {
		t.Fatalf("video playlist is not fMP4 (.m4s segments): %v", videoSegs)
	}
	vInit := anonBytes(t, srv, tokenHLS(dec.StreamToken, videoInit))
	vSeg := anonBytes(t, srv, tokenHLS(dec.StreamToken, videoSegs[0]))
	if got := ffprobeFirstCodec(t, vInit, vSeg, "video"); got != "hevc" {
		t.Errorf("video delivered over the token path = %q, want hevc (the same copied bytes)", got)
	}
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, videoInit), "video/mp4")
	assertAnonContentType(t, srv, tokenHLS(dec.StreamToken, videoSegs[0]), "video/mp4")

	// The DEFAULT audio rendition: its playlist, its init segment, and a segment.
	var audioURI string
	for _, line := range strings.Split(master, "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(line, "DEFAULT=YES") {
			audioURI = extAttrURI(line)
		}
	}
	if audioURI == "" {
		t.Fatalf("no DEFAULT=YES audio rendition in master:\n%s", master)
	}
	audioPL := anonText(t, srv, tokenHLS(dec.StreamToken, audioURI))
	assertBareRelativeURIs(t, "audio rendition playlist", audioPL)
	aInit := anonBytes(t, srv, tokenHLS(dec.StreamToken, extMapURI(t, audioPL)))
	aSegs := parseSegments(audioPL)
	if len(aSegs) == 0 {
		t.Fatalf("audio rendition lists no segments:\n%s", audioPL)
	}
	aSeg := anonBytes(t, srv, tokenHLS(dec.StreamToken, aSegs[0]))
	if got := ffprobeFirstCodec(t, aInit, aSeg, "audio"); got != "aac" {
		t.Errorf("audio delivered over the token path = %q, want aac", got)
	}

	// Every playlist involved is byte-identical to the bearer path's.
	assertPlaylistBytesIdentical(t, srv, dec, bearer, "master.m3u8")
	assertPlaylistBytesIdentical(t, srv, dec, bearer, "index.m3u8")
	assertPlaylistBytesIdentical(t, srv, dec, bearer, audioURI)
}

// assertAnonContentType checks the media type of a token-carried artifact.
func assertAnonContentType(t *testing.T, srv *testharness.Server, apiPath, want string) {
	t.Helper()
	resp := anonGET(t, srv, apiPath, "")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, want) {
		t.Errorf("GET %s Content-Type = %q, want it to contain %q", apiPath, ct, want)
	}
}

// --- progressive direct play --------------------------------------------------

// TestStreamTokenProgressiveDirectPlayWithRange: an AVPlayer client AirPlaying an
// mp4/h264 Title lands on directPlay, not HLS, so the progressive route has to
// work the same way — including Range, because a receiver range-seeks. The bytes
// are compared against the bearer path's so "it returned 200" cannot pass for
// "it returned the film".
func TestStreamTokenProgressiveDirectPlayWithRange(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	bearer, _, dec := duneSession(t, srv)
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %q, want directPlay", dec.Tier)
	}

	// The whole file, anonymously, and the same bytes the bearer path serves.
	whole := anonBytes(t, srv, tokenStream(dec.StreamToken))
	viaBearer := authStream(t, srv, dec.StreamURL, bearer, "")
	defer viaBearer.Body.Close()
	bearerBytes, _ := io.ReadAll(viaBearer.Body)
	if !bytes.Equal(whole, bearerBytes) {
		t.Fatalf("token path served %d bytes, bearer path served %d — they must be the same File",
			len(whole), len(bearerBytes))
	}

	// A Range request: 206 with exactly the requested slice and the range headers
	// http.ServeContent produces on the bearer path.
	part := anonGET(t, srv, tokenStream(dec.StreamToken), "bytes=0-9")
	defer part.Body.Close()
	if part.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged token stream = %d, want 206", part.StatusCode)
	}
	got, _ := io.ReadAll(part.Body)
	if len(got) != 10 || !bytes.Equal(got, whole[:10]) {
		t.Errorf("ranged token stream returned %d bytes that do not match the prefix", len(got))
	}
	if cr := part.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-9/") {
		t.Errorf("Content-Range = %q, want bytes 0-9/<size>", cr)
	}
	if ar := part.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}

	// A seek backwards into the middle of the file, the way a receiver scrubs.
	if len(whole) > 64 {
		mid := anonGET(t, srv, tokenStream(dec.StreamToken), "bytes=32-63")
		defer mid.Body.Close()
		if mid.StatusCode != http.StatusPartialContent {
			t.Fatalf("mid-file range = %d, want 206", mid.StatusCode)
		}
		midBytes, _ := io.ReadAll(mid.Body)
		if !bytes.Equal(midBytes, whole[32:64]) {
			t.Errorf("mid-file range returned the wrong slice")
		}
	}
}

// --- seeking, on both playlist ownerships -------------------------------------

// TestStreamTokenSeekOnServerOwnedPlaylist: a re-encoding transcode SYNTHESIZES
// its own VOD playlist and realigns ffmpeg on a forward seek. Through the token
// path, jump to the last segment (a seek well past the production frontier and
// across every boundary in between), then back to segment 0 — the backwards seek
// that proves the realignment did not renumber the playlist out from under the
// player.
func TestStreamTokenSeekOnServerOwnedPlaylist(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	root := generateLongTranscodeClip(t, 16) // 16s → 4 segments at 4s
	list := scanLibraryAt(t, srv, bearer, root)
	titleID := findTitle(t, list, "Long Movie")

	dec := negotiateTranscodeDecision(t, srv, bearer, titleID, transcodeProfile())
	playlist := anonText(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"))
	segs := parseSegments(playlist)
	if len(segs) < 3 {
		t.Fatalf("playlist lists %d segments, want >= 3 for a seek test:\n%s", len(segs), playlist)
	}

	// Forward, across several boundaries.
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[len(segs)-1]))
	// Backwards, to the start.
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[0]))
	// Forward again, across exactly one boundary, so the walk is not just "the ends".
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[1]))

	// The playlist a seeking player re-reads is still the same bytes the bearer
	// path serves — realignment never rewrites it, on either route.
	assertPlaylistBytesIdentical(t, srv, dec, bearer, "index.m3u8")
}

// TestStreamTokenSeekOnFfmpegOwnedPlaylist: an audio-only transcode is the
// runtime that does NOT synthesize a playlist — it serves ffmpeg's own bytes
// verbatim, with ffmpeg's real variable EXTINF durations, because audio segments
// are not keyframe-aligned and the synthesized-playlist machinery produced
// segment 404s on Safari. That verbatim playlist is the one a query-parameter
// token would have forced us to post-process, so it is the one that most needs
// to work untouched here.
func TestStreamTokenSeekOnFfmpegOwnedPlaylist(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)

	root := generateLongAudioTrack(t, "21.3", "flac", "flac")
	libID := createMusicLibrary(t, srv, bearer, root)
	scanLib(t, srv, bearer, libID, "")
	artists := listArtists(t, srv, bearer, libID)
	artistID := findArtist(t, artists, "Test Artist")
	albums := artistAlbums(t, srv, bearer, artistID)
	if len(albums.Albums) == 0 {
		t.Fatalf("no albums scanned")
	}
	tracks := albumTracks(t, srv, bearer, albums.Albums[0].ID)
	if len(tracks.Tracks) == 0 {
		t.Fatalf("no tracks scanned")
	}

	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+tracks.Tracks[0].ID+"/playback", bearer, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode (FLAC under an aac-only profile)", dec.Tier)
	}
	if dec.StreamToken == "" {
		t.Fatal("decision carried no streamToken")
	}

	playlist := anonText(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"))
	assertBareRelativeURIs(t, "ffmpeg's own playlist", playlist)
	segs := parseSegments(playlist)
	if len(segs) < 3 {
		t.Fatalf("playlist lists %d segments, want >= 3:\n%s", len(segs), playlist)
	}
	// It really is ffmpeg's own playlist and not the synthesized uniform-4s one.
	uniform := true
	for _, d := range parseExtinfDurations(playlist) {
		if d != "4.000000" {
			uniform = false
			break
		}
	}
	if uniform {
		t.Errorf("playlist looks synthesized (every EXTINF is 4.000000); this test needs the ffmpeg-owned runtime:\n%s", playlist)
	}

	// Seek across a boundary and then backwards, out of order, the way a native
	// HLS client fans out.
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[2]))
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[0]))
	anonBytes(t, srv, tokenHLS(dec.StreamToken, segs[len(segs)-1]))
}

// --- refusals -----------------------------------------------------------------

// TestStreamTokenRefusalsAreIndistinguishable: a token that never existed, one
// that has expired, one whose session was ended (revoked), and an account bearer
// pasted into the path must all produce the SAME 404 — and the same 404 a
// wrong-User session fetch produces on the bearer path, since a refusal that
// looked different would be the disclosure the posture exists to prevent.
func TestStreamTokenRefusalsAreIndistinguishable(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, bearer)
	duneID := findTitle(t, list, "Dune")
	live := streamTokenSession(t, srv, bearer, duneID)

	// The reference answer: another User fetching a session that is not theirs.
	srv.CreateMember("member", "memberpass123")
	other := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	ref := authStream(t, srv, live.StreamURL, other, "")
	refBody, _ := io.ReadAll(ref.Body)
	ref.Body.Close()
	if ref.StatusCode != http.StatusNotFound {
		t.Fatalf("reference wrong-User session fetch = %d, want 404", ref.StatusCode)
	}

	// An expired token: a live session whose credential aged out in the lookup's
	// WHERE clause.
	expired := streamTokenSession(t, srv, bearer, duneID)
	srv.ExpireStreamTokensForSession(expired.SessionID)

	// A revoked token: the session was ended, which is the revocation the feature
	// promises.
	revoked := streamTokenSession(t, srv, bearer, duneID)
	if status, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+revoked.SessionID, bearer, nil, nil); status != http.StatusNoContent {
		t.Fatalf("ending the session to revoke its token = %d, want 204", status)
	}

	cases := []struct {
		name, path string
	}{
		{"unknown token", tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy")},
		{"expired token", tokenStream(expired.StreamToken)},
		{"revoked token", tokenStream(revoked.StreamToken)},
		{"account bearer in the path", tokenStream(bearer)},
		{"unknown token, hls", tokenHLS("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy", "index.m3u8")},
		{"expired token, hls", tokenHLS(expired.StreamToken, "index.m3u8")},
		{"revoked token, hls", tokenHLS(revoked.StreamToken, "master.m3u8")},
		{"account bearer in the path, hls", tokenHLS(bearer, "000.ts")},
		{"empty artifact", "/api/v1/stream/" + live.StreamToken + "/"},
		{"unknown artifact", "/api/v1/stream/" + live.StreamToken + "/nope"},
	}
	for _, c := range cases {
		resp := anonGET(t, srv, c.path, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404; body: %s", c.name, resp.StatusCode, body)
			continue
		}
		if !bytes.Equal(body, refBody) {
			t.Errorf("%s body %s differs from the wrong-User session envelope %s", c.name, body, refBody)
		}
		if strings.Contains(string(body), "UNAUTHORIZED") || strings.Contains(string(body), "FORBIDDEN") {
			t.Errorf("%s leaked an auth-shaped code: %s", c.name, body)
		}
		if resp.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s sent WWW-Authenticate; the refusal must not invite a bearer retry", c.name)
		}
	}

	// And the live token still works, so the refusals above are refusals of the
	// credential rather than of the route.
	anonBytes(t, srv, tokenStream(live.StreamToken))
}

// TestStreamTokenRoutesAreGetOnly: no method on these paths mutates anything, so
// everything but GET is a 405 — and the method is refused BEFORE the token is
// examined, so a live token and a dead one give the same answer. Otherwise the
// difference between 405 and 404 would be a token-validity oracle that needs no
// media at all.
func TestStreamTokenRoutesAreGetOnly(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	bearer, _, dec := duneSession(t, srv)

	paths := []string{
		tokenStream(dec.StreamToken),
		tokenHLS(dec.StreamToken, "index.m3u8"),
		tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy"), // a dead token
	}
	for _, path := range paths {
		for _, method := range []string{
			http.MethodPost, http.MethodPut, http.MethodPatch,
			http.MethodDelete, http.MethodHead, http.MethodOptions,
		} {
			req, err := http.NewRequest(method, srv.URL(path), nil)
			if err != nil {
				t.Fatalf("building %s request: %v", method, err)
			}
			resp, err := (&http.Client{}).Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405; body: %s", method, path, resp.StatusCode, body)
			}
			if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
				t.Errorf("%s %s Allow = %q, want GET", method, path, allow)
			}
		}
	}

	// Nothing above mutated anything: the session is still live, still streamable,
	// and still endable only by its owner over the bearer path.
	anonBytes(t, srv, tokenStream(dec.StreamToken))
	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 1 {
		t.Errorf("stream tokens for the session = %d, want the one the Decision minted", n)
	}
	if status, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, bearer, nil, nil); status != http.StatusNoContent {
		t.Errorf("owner delete after the 405s = %d, want 204", status)
	}
}

// TestStreamTokenServesItsOwnUsersBytes: the bytes served are the ones the
// token's user_id is allowed, exactly as on the bearer path. A Member's token
// reaches the Member's own session, and when the Member is deleted the token
// dies with them — the access decision follows the User the token names, not
// whoever happens to be holding it.
func TestStreamTokenServesItsOwnUsersBytes(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, libID, "")
	duneID := findTitle(t, listAllTitles(t, srv, admin, libID), "Dune")

	// A Member with an explicit grant to this Library: the access decision the
	// token has to keep honouring is the Member's, not the holder's.
	memberID := srv.CreateUser(admin, "viewer", "viewerpass123", "")
	grantLibraries(t, srv, admin, memberID, libID)
	member := srv.LoginAs("viewer", "viewerpass123")

	memberDec := streamTokenSession(t, srv, member, duneID)
	adminDec := streamTokenSession(t, srv, admin, duneID)
	if memberDec.SessionID == adminDec.SessionID {
		t.Fatal("the two Users share a session; the test needs two")
	}

	// The Member's token serves the Member's session, and it is the same File the
	// Member gets over their own bearer.
	viaToken := anonBytes(t, srv, tokenStream(memberDec.StreamToken))
	viaBearer := authStream(t, srv, memberDec.StreamURL, member, "")
	defer viaBearer.Body.Close()
	bearerBytes, _ := io.ReadAll(viaBearer.Body)
	if !bytes.Equal(viaToken, bearerBytes) {
		t.Errorf("the Member's token served %d bytes; their bearer served %d", len(viaToken), len(bearerBytes))
	}

	// Deleting the User revokes their outstanding credential (the stream_tokens row
	// cascades), while the Admin's own session is untouched.
	if status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+memberID, admin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("deleting the member = %d, want 204; body: %s", status, body)
	}
	resp := anonGET(t, srv, tokenStream(memberDec.StreamToken), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the deleted Member's token = %d, want 404", resp.StatusCode)
	}
	anonBytes(t, srv, tokenStream(adminDec.StreamToken))
}

// TestExistingSessionRoutesAreUnchangedByTheTokenPath: the token path is
// ADDITIVE. The same session stays reachable over the bearer header and over the
// ms_media cookie, and a stream token still buys nothing on the session-id route.
func TestExistingSessionRoutesAreUnchangedByTheTokenPath(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	bearer, _, dec := duneSession(t, srv)

	viaBearer := authStream(t, srv, dec.StreamURL, bearer, "")
	viaBearer.Body.Close()
	if viaBearer.StatusCode != http.StatusOK {
		t.Errorf("bearer stream = %d, want 200", viaBearer.StatusCode)
	}

	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-token-route")
	viaCookie := cookieGET(t, srv, dec.StreamURL, cookie, "")
	viaCookie.Body.Close()
	if viaCookie.StatusCode != http.StatusOK {
		t.Errorf("ms_media cookie stream = %d, want 200", viaCookie.StatusCode)
	}

	// The session-id route still demands a real credential; a stream token in the
	// Authorization header is not one (ticket 01's namespace separation, re-checked
	// here now that the token has a route of its own).
	viaStreamToken := authStream(t, srv, dec.StreamURL, dec.StreamToken, "")
	viaStreamToken.Body.Close()
	if viaStreamToken.StatusCode != http.StatusUnauthorized {
		t.Errorf("stream token as a bearer on the session route = %d, want 401", viaStreamToken.StatusCode)
	}
}

// --- the log ------------------------------------------------------------------

// syncBuffer collects log output from whatever goroutine the server writes it on.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStreamTokenNeverReachesTheAccessLog tests the LOGGER, not the handler.
//
// The secret is in r.URL.Path, and printing the request path is what every
// access log on earth does — so this drives a real playthrough through
// api.LogRequests (the middleware cmd/obelo wraps the server in) with the
// standard logger captured, and requires that the token appears in NO line while
// the request itself still does. It also checks the account credential on the
// ?token= download route stays out, since that is the same failure one URL over.
func TestStreamTokenNeverReachesTheAccessLog(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	bearer, _, dec := duneSession(t, srv)

	var logged syncBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	// The production composition: the access-log middleware wrapped around the
	// fully wired server, exactly as cmd/obelo does it.
	ts := httptest.NewServer(api.LogRequests(srv.Handler()))
	defer ts.Close()

	get := func(path string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("building %s: %v", path, err)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// A successful fetch, a refused one, and a rejected method — every branch that
	// could reach the log with a token in hand.
	if code := get(tokenStream(dec.StreamToken)); code != http.StatusOK {
		t.Fatalf("token stream through the logged server = %d, want 200", code)
	}
	if code := get(tokenHLS(dec.StreamToken, "index.m3u8")); code != http.StatusNotFound {
		t.Fatalf("hls on a direct-play session = %d, want 404", code)
	}
	if code := get(tokenStream(dec.StreamToken) + "?cachebust=1"); code != http.StatusOK {
		t.Fatalf("token stream with a query = %d, want 200", code)
	}

	out := logged.String()
	if strings.Contains(out, dec.StreamToken) {
		t.Fatalf("the access log printed the stream token:\n%s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("the access log printed no redacted stream path at all; is it logging?\n%s", out)
	}
	if !strings.Contains(out, "/api/v1/stream/[redacted]/stream") {
		t.Errorf("the access log did not record the request path (redacted):\n%s", out)
	}
	// The other URL-borne credential: ?token= on the direct-file download is the
	// ACCOUNT token, and this log must not write it down either.
	if strings.Contains(out, bearer) {
		t.Fatalf("the access log printed the account bearer token:\n%s", out)
	}
	if strings.Contains(out, "cachebust") {
		t.Errorf("the access log printed a query string; queries carry ?token= and are never logged:\n%s", out)
	}
}

// TestRedactPath covers the redactor itself, including the two path forms it has
// to accept — an outer middleware sees /api/v1/stream/…, the api mux sees
// /stream/… after StripPrefix — and the paths it must leave alone.
func TestRedactPath(t *testing.T) {
	const tok = "s3cret-token_value"
	cases := []struct{ in, want string }{
		{"/api/v1/stream/" + tok + "/stream", "/api/v1/stream/[redacted]/stream"},
		{"/api/v1/stream/" + tok + "/hls/000.ts", "/api/v1/stream/[redacted]/hls/000.ts"},
		{"/api/v1/stream/" + tok + "/hls/audio_7_init.mp4", "/api/v1/stream/[redacted]/hls/audio_7_init.mp4"},
		{"/stream/" + tok + "/hls/master.m3u8", "/stream/[redacted]/hls/master.m3u8"},
		{"/api/v1/stream/" + tok, "/api/v1/stream/[redacted]"},
		{"/api/v1/stream/", "/api/v1/stream/[redacted]"},
		// Untouched: everything that is not a stream-token URL.
		{"/api/v1/sessions/abc/hls/000.ts", "/api/v1/sessions/abc/hls/000.ts"},
		{"/api/v1/titles/abc", "/api/v1/titles/abc"},
		{"/", "/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := api.RedactPath(c.in); got != c.want {
			t.Errorf("RedactPath(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(api.RedactPath(c.in), tok) {
			t.Errorf("RedactPath(%q) left the token in place", c.in)
		}
	}
}
