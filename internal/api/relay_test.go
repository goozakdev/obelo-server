package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the one-hop relay (.scratch/linked-servers issue 09,
// ADR-0056 §5): what happens when somebody presses play on a Title whose file is
// in another household.
//
// Two whole Servers over real HTTP again, for issue 06/07/08's reason — the claim
// is about the space between two machines — plus one thing those tests did not
// need: a RECORDER between them. Every request the home Server makes to the
// sharer passes through it, which is how "the sharer never learns who is
// watching" (ADR-0054 §3) becomes an assertion rather than a hope.

// --- the recorder -------------------------------------------------------------

// relayCall is one request the home Server made to the sharer.
type relayCall struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// relayRecorder wraps the sharer's handler and keeps every request the home
// Server sent it, headers and body included.
type relayRecorder struct {
	mu    sync.Mutex
	calls []relayCall
}

func (rec *relayRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, relayCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   body,
		})
		rec.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (rec *relayRecorder) snapshot() []relayCall {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]relayCall, len(rec.calls))
	copy(out, rec.calls)
	return out
}

// countPath is how many recorded calls hit a path (prefix match).
func (rec *relayRecorder) countPath(prefix string) int {
	n := 0
	for _, c := range rec.snapshot() {
		if strings.HasPrefix(c.Path, prefix) {
			n++
		}
	}
	return n
}

// awaitCall waits briefly for a call matching method+path to be recorded. It
// exists for the ONE thing the relay does out of band: ending the sharer's
// session when this one ends, which hangs off the session-ended observer and runs
// in the background.
func (rec *relayRecorder) awaitCall(t *testing.T, method, path string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range rec.snapshot() {
			if c.Method == method && c.Path == path {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// --- the fixture --------------------------------------------------------------

// relayFixture is a sharer with a scanned Library granted to a `remote` User, a
// home Server linked to it through the recorder, and the ids on both sides.
type relayFixture struct {
	sharer      *testharness.Server
	sharerAdmin string
	remoteUser  string
	srcLib      string
	home        *testharness.Server
	homeAdmin   string
	mirrorLib   string
	rec         *relayRecorder
	proxy       *httptest.Server
}

// linkForRelay scans the movie fixtures on a sharer, grants them to a `remote`
// User, and links a home Server to it at an origin that records everything.
func linkForRelay(t *testing.T) *relayFixture {
	t.Helper()
	requireFixtures(t)

	f := &relayFixture{rec: &relayRecorder{}}
	f.sharer = testharness.New(t)
	f.sharerAdmin = adminToken(t, f.sharer)
	f.srcLib = createMovieLibrary(t, f.sharer, f.sharerAdmin, fixtureRoot(t))
	scanLib(t, f.sharer, f.sharerAdmin, f.srcLib, "")
	f.remoteUser = createRemoteUser(t, f.sharer, f.sharerAdmin, "The other household")
	grantLibraries(t, f.sharer, f.sharerAdmin, f.remoteUser, f.srcLib)

	// The origin in the invite is the RECORDER, not the sharer's own listener, so
	// every byte the home Server sends is inspectable.
	f.proxy = httptest.NewServer(f.rec.wrap(f.sharer.Handler()))
	t.Cleanup(f.proxy.Close)

	f.home = testharness.New(t)
	f.homeAdmin = adminToken(t, f.home)
	invite := mintInvite(t, f.sharer, f.sharerAdmin, f.remoteUser, f.proxy.URL).Invite
	var l linkResp
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &l); status != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201; body: %s", status, body)
	}
	if len(l.Libraries) != 1 {
		t.Fatalf("the link brought %d libraries, want 1: %+v", len(l.Libraries), l.Libraries)
	}
	f.mirrorLib = l.Libraries[0].ID
	return f
}

// mirroredTitle finds a mirrored Title by name on the home Server.
func (f *relayFixture) mirroredTitle(t *testing.T, name string) string {
	t.Helper()
	var list titlesListResp
	if status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirrorLib+"/titles", f.homeAdmin, &list); status != http.StatusOK {
		t.Fatalf("listing the mirror = %d; body: %s", status, body)
	}
	return findTitle(t, list, name)
}

// play negotiates a relayed play on the home Server.
func (f *relayFixture) play(t *testing.T, token, titleID string, body map[string]any) (int, decisionResp, []byte) {
	t.Helper()
	var dec decisionResp
	status, raw := f.home.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, body, &dec)
	return status, dec, raw
}

// linkState reads the Link's state on the home Server.
func (f *relayFixture) linkState(t *testing.T) string {
	t.Helper()
	var links []linkResp
	if status, body := f.home.AuthGET("/api/v1/links", f.homeAdmin, &links); status != http.StatusOK {
		t.Fatalf("GET /links = %d; body: %s", status, body)
	}
	if len(links) != 1 {
		t.Fatalf("want exactly one link, got %d", len(links))
	}
	return links[0].State
}

// --- direct play --------------------------------------------------------------

// TestRelayDirectPlay is the tracer bullet: a mirrored Title negotiates on the
// sharer, the answer comes back rewritten onto this Server, and the bytes flow
// through it — with Range, and with the session ending on both sides.
func TestRelayDirectPlay(t *testing.T) {
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")

	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %q, want directPlay (the sharer's own answer); body: %s", dec.Tier, body)
	}
	// The URL is this Server's, and it carries the sharer's path tail — which is
	// what makes a relative playlist URI resolve (ADR-0039's reason, one hop out).
	wantPrefix := "/api/v1/relay/" + dec.SessionID + "/sessions/"
	if !strings.HasPrefix(dec.StreamURL, wantPrefix) {
		t.Fatalf("streamUrl = %q, want a %q… relay path", dec.StreamURL, wantPrefix)
	}
	if !strings.HasSuffix(dec.StreamURL, "/stream") {
		t.Fatalf("streamUrl = %q, want the sharer's progressive tail preserved", dec.StreamURL)
	}
	// The sharer's own stream token must not be handed on; this Server mints its
	// own, which is the only one its routes can spend.
	if dec.StreamToken == "" {
		t.Error("a relayed decision carried no stream token of this server's")
	}

	// The whole file, through the relay.
	resp := authStream(t, f.home, dec.StreamURL, f.homeAdmin, "")
	whole, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relayed stream = %d, want 200", resp.StatusCode)
	}
	if len(whole) == 0 {
		t.Fatal("the relayed stream carried no bytes")
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes (passed through)", got)
	}
	if resp.Header.Get("Content-Type") == "" {
		t.Error("the relayed stream carried no Content-Type")
	}

	// A seek is a byte range, and it is answered by the sharer through this Server.
	ranged := authStream(t, f.home, dec.StreamURL, f.homeAdmin, "bytes=0-99")
	part, _ := io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged relay = %d, want 206", ranged.StatusCode)
	}
	if len(part) != 100 {
		t.Errorf("ranged relay returned %d bytes, want 100", len(part))
	}
	if cr := ranged.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-99/") {
		t.Errorf("Content-Range = %q, want bytes 0-99/…", cr)
	}
	if !bytes.Equal(part, whole[:100]) {
		t.Error("the ranged bytes are not the first 100 bytes of the whole stream")
	}

	// Watch state is written HERE, against the mirrored Title, exactly as for a
	// local play (ADR-0056 §5) — the person watching is on this Server and their
	// place in a friend's film is theirs.
	//
	// The position reported is 95% of the fixture's 1s duration, which is past the
	// Watched threshold — and the threshold is a fraction of the session's
	// DurationMs, which for a relay comes from the MIRRORED Edition. So "watched,
	// resume cleared" is also the assertion that the sharer's Edition was mapped
	// back through remote_id: with no duration the same report would land as a bare
	// 950ms resume instead.
	var progress struct {
		TitleID          string `json:"titleId"`
		ResumePositionMs int64  `json:"resumePositionMs"`
		Watched          bool   `json:"watched"`
	}
	if st, raw := f.home.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/progress", f.homeAdmin,
		map[string]any{"positionMs": 950, "state": "playing"}, &progress); st != http.StatusOK {
		t.Fatalf("progress on a relayed session = %d; body: %s", st, raw)
	}
	if !progress.Watched || progress.ResumePositionMs != 0 {
		t.Errorf("progress = %+v, want watched with the resume cleared (the mirrored Edition's duration)",
			progress)
	}
	var detail struct {
		Watched bool `json:"watched"`
	}
	if st, raw := f.home.AuthGET("/api/v1/titles/"+titleID, f.homeAdmin, &detail); st != http.StatusOK || !detail.Watched {
		t.Errorf("the mirrored Title reads back watched=%v (status %d); body: %s", detail.Watched, st, raw)
	}

	// Ending the local session ends the sharer's.
	remoteSession := relayRemoteSessionOf(t, dec.StreamURL)
	if st, body := f.home.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, f.homeAdmin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("DELETE session = %d, want 204; body: %s", st, body)
	}
	if !f.rec.awaitCall(t, http.MethodDelete, "/api/v1/sessions/"+remoteSession) {
		t.Error("ending the local session did not end the sharer's")
	}
	// And the relay path is dead with it.
	gone := authStream(t, f.home, dec.StreamURL, f.homeAdmin, "")
	gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Errorf("streaming an ended relay session = %d, want 404", gone.StatusCode)
	}
}

// relayRemoteSessionOf pulls the sharer's session id out of a rewritten relay
// URL — the tail is theirs, which is exactly what makes this readable.
func relayRemoteSessionOf(t *testing.T, streamURL string) string {
	t.Helper()
	_, tail, ok := strings.Cut(streamURL, "/sessions/")
	if !ok {
		t.Fatalf("no remote session in %q", streamURL)
	}
	id, _, _ := strings.Cut(tail, "/")
	return id
}

// TestRelayCarriesNothingAboutTheViewer is ADR-0054 §3 as an assertion: the
// sharer sees the Link and never the person. Not one request the home Server
// makes — header or body — may carry the viewer's User id, username or Device
// name.
func TestRelayCarriesNothingAboutTheViewer(t *testing.T) {
	f := linkForRelay(t)

	// A named member with a named device, so the strings are unmistakable if they
	// ever leak.
	f.home.CreateUser(f.homeAdmin, "harriet", "hunter2hunter2", "member")
	var users struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	}
	if st, body := f.home.AuthGET("/api/v1/users", f.homeAdmin, &users); st != http.StatusOK {
		t.Fatalf("GET /users = %d; body: %s", st, body)
	}
	var harrietID string
	for _, u := range users.Users {
		if u.Username == "harriet" {
			harrietID = u.ID
		}
	}
	if harrietID == "" {
		t.Fatal("the member was not created")
	}
	grantLibraries(t, f.home, f.homeAdmin, harrietID, f.mirrorLib)
	session := login(t, f.home, "harriet", "hunter2hunter2", "Harriets-iPad", "ios", "harriet-ipad-client")

	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, session.Token, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback as a member = %d, want 200; body: %s", status, body)
	}
	resp := authStream(t, f.home, dec.StreamURL, session.Token, "bytes=0-99")
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	secrets := map[string]string{
		"the viewer's user id":     harrietID,
		"the viewer's username":    "harriet",
		"the viewer's device name": "Harriets-iPad",
		"the viewer's device id":   session.Device.ID,
		"the viewer's own token":   session.Token,
	}
	calls := f.rec.snapshot()
	if len(calls) == 0 {
		t.Fatal("no calls to the sharer were recorded")
	}
	for _, c := range calls {
		for name, secret := range secrets {
			if secret == "" {
				continue
			}
			for key, values := range c.Header {
				for _, v := range values {
					if strings.Contains(v, secret) {
						t.Errorf("%s %s: header %s carries %s", c.Method, c.Path, key, name)
					}
				}
			}
			if strings.Contains(string(c.Body), secret) {
				t.Errorf("%s %s: the body carries %s", c.Method, c.Path, name)
			}
			if strings.Contains(c.Path, secret) || strings.Contains(c.Query, secret) {
				t.Errorf("%s %s?%s: the url carries %s", c.Method, c.Path, c.Query, name)
			}
		}
	}
}

// --- the sharer's answers pass through ----------------------------------------

// TestRelayPassesTheSharersRefusalThrough: the `remote` User's stream ceiling is
// the SHARER's lever (ADR-0054 §2), and its refusal reaches the home client whole
// — the status, the code and the counts — because nothing here can improve on it.
func TestRelayPassesTheSharersRefusalThrough(t *testing.T) {
	f := linkForRelay(t)
	setPlaybackCeiling(t, f.sharer, f.sharerAdmin, f.remoteUser, map[string]any{"maxStreams": 1})

	titleID := f.mirroredTitle(t, "Dune")
	if status, _, body := f.play(t, f.homeAdmin, titleID, mp4Profile()); status != http.StatusOK {
		t.Fatalf("the first relayed play = %d, want 200; body: %s", status, body)
	}

	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", f.homeAdmin, mp4Profile(), &env)
	if status != http.StatusTooManyRequests {
		t.Fatalf("the second relayed play = %d, want 429 (the sharer's own refusal); body: %s", status, body)
	}
	if env.Error.Code != "STREAM_LIMIT" {
		t.Errorf("code = %q, want STREAM_LIMIT passed through; body: %s", env.Error.Code, body)
	}
	if env.Error.Details["limit"] != float64(1) {
		t.Errorf("details = %v, want the sharer's counts passed through; body: %s", env.Error.Details, body)
	}
	// The Link is fine: a refusal is the sharer at its healthiest.
	if got := f.linkState(t); got != "connected" {
		t.Errorf("link state = %q after a refusal, want connected", got)
	}
}

// TestRelayHonoursTheSharersResolutionCeiling is the issue's second acceptance
// criterion at the negotiation level: a `remote` User capped at 1080p gets a
// transcoded-down rendition of a 4K source, decided over there, with this Server
// re-encoding nothing.
//
// The 4K source is the fixture with its probed dimensions rewritten on the
// SHARER: the tiering reads the catalog, so a 320x180 clip that says it is 4K
// exercises the identical decision without a 4K file or a real transcode.
func TestRelayHonoursTheSharersResolutionCeiling(t *testing.T) {
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")

	// Say the sharer's copy is 4K.
	f.sharer.Exec(`UPDATE files SET width = 3840, height = 2160
	                WHERE edition_id IN (SELECT e.id FROM editions e
	                                      JOIN titles t ON t.id = e.title_id
	                                     WHERE t.title = 'Dune')`)
	f.sharer.Exec(`UPDATE streams SET width = 3840, height = 2160
	                WHERE kind = 'video' AND file_id IN (
	                      SELECT fi.id FROM files fi
	                        JOIN editions e ON e.id = fi.edition_id
	                        JOIN titles t ON t.id = e.title_id
	                       WHERE t.title = 'Dune')`)

	// A client that can decode 4K h264 and asks for no cap of its own.
	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers": []string{"mp4", "mkv"},
			"videoCodecs": []map[string]any{
				{"codec": "h264", "maxResolution": "2160p"},
			},
			"audioCodecs":      []string{"aac", "ac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{},
	}

	// Uncapped, the sharer direct-plays it: the control that proves the cap below
	// is what changed the answer.
	status, dec, body := f.play(t, f.homeAdmin, titleID, profile)
	if status != http.StatusOK {
		t.Fatalf("uncapped relayed play = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "directPlay" {
		t.Fatalf("uncapped tier = %q, want directPlay; body: %s", dec.Tier, body)
	}
	f.home.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, f.homeAdmin, nil, nil)

	setPlaybackCeiling(t, f.sharer, f.sharerAdmin, f.remoteUser, map[string]any{"maxResolution": "1080p"})

	status, capped, body := f.play(t, f.homeAdmin, titleID, profile)
	if status != http.StatusOK {
		t.Fatalf("capped relayed play = %d, want 200; body: %s", status, body)
	}
	if capped.Tier == "directPlay" {
		t.Fatalf("capped tier = directPlay, want the sharer to transcode the 4K source down; body: %s", body)
	}
	if capped.VideoStream != nil && capped.VideoStream.Height > 2160 {
		t.Errorf("the capped decision reports %dp", capped.VideoStream.Height)
	}
	// And this Server encoded nothing: the relay costs it bandwidth, never CPU.
	var load struct {
		Transcodes struct {
			Active int `json:"active"`
		} `json:"transcodes"`
	}
	if st, raw := f.home.AuthGET("/api/v1/transcoding", f.homeAdmin, &load); st == http.StatusOK && load.Transcodes.Active != 0 {
		t.Errorf("the home server holds %d transcodes for a relayed play, want 0; body: %s", load.Transcodes.Active, raw)
	}
}

// --- the two link states ------------------------------------------------------

// TestRelayAnswersLinkRevoked: the sharer deleted the `remote` User, so the
// credential is dead (ADR-0056 §6). The play says so, and the Link is marked.
func TestRelayAnswersLinkRevoked(t *testing.T) {
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")

	if st, body := f.sharer.JSON(http.MethodDelete, "/api/v1/users/"+f.remoteUser, f.sharerAdmin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("deleting the remote user = %d; body: %s", st, body)
	}

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", f.homeAdmin, mp4Profile(), &env)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("play against a revoked link = %d, want 503; body: %s", status, body)
	}
	if env.Error.Code != "LINK_REVOKED" {
		t.Errorf("code = %q, want LINK_REVOKED; body: %s", env.Error.Code, body)
	}
	if got := f.linkState(t); got != "revoked" {
		t.Errorf("link state = %q, want revoked", got)
	}
	// The Title is still there. Only the play failed.
	if got := f.mirroredTitle(t, "Dune"); got != titleID {
		t.Error("the mirrored Title moved or vanished when the credential died")
	}
}

// TestRelayAnswersLinkUnreachable: the friend's Server is off. The mirror stays,
// the play says why, and the Link is marked unreachable (ADR-0056 §6).
func TestRelayAnswersLinkUnreachable(t *testing.T) {
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")

	f.proxy.Close()
	f.sharer.Close()

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", f.homeAdmin, mp4Profile(), &env)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("play against an unreachable sharer = %d, want 503; body: %s", status, body)
	}
	if env.Error.Code != "LINK_UNREACHABLE" {
		t.Errorf("code = %q, want LINK_UNREACHABLE; body: %s", env.Error.Code, body)
	}
	if got := f.linkState(t); got != "unreachable" {
		t.Errorf("link state = %q, want unreachable", got)
	}
	var list titlesListResp
	if st, _ := f.home.AuthGET("/api/v1/libraries/"+f.mirrorLib+"/titles", f.homeAdmin, &list); st != http.StatusOK || len(list.Titles) == 0 {
		t.Error("the mirrored Titles vanished when the sharer went away")
	}
}

// --- HLS ----------------------------------------------------------------------

// TestRelayHLSPlaylistAndSegments: the HLS tiers, end to end. The sharer
// transcodes (its ffmpeg, its governance), and this Server serves the playlist
// and the segments the playlist names — with the relative URIs resolving onto the
// relay path because the remote tail was preserved.
//
// It needs ffmpeg on the SHARER's host, so it skips like every other real-ffmpeg
// test in this package.
func TestRelayHLSPlaylistAndSegments(t *testing.T) {
	requireFFmpeg(t)
	f := linkForRelay(t)
	// Blade Runner is mkv/mpeg4/mp3: an mp4+h264+aac client cannot direct-play it,
	// so the sharer picks an HLS tier.
	titleID := f.mirroredTitle(t, "Blade Runner")

	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed HLS playback = %d, want 200; body: %s", status, body)
	}
	if dec.Tier == "directPlay" {
		t.Fatalf("tier = directPlay, want an HLS tier for the mkv fixture; body: %s", body)
	}
	if !strings.HasSuffix(dec.StreamURL, ".m3u8") {
		t.Fatalf("streamUrl = %q, want an HLS playlist", dec.StreamURL)
	}

	playlist := relayFetchText(t, f.home, dec.StreamURL, f.homeAdmin)
	if !strings.HasPrefix(playlist, "#EXTM3U") {
		t.Fatalf("the relayed playlist is not a playlist: %.80q", playlist)
	}
	// If it is a master, follow it to the media playlist first.
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	if strings.Contains(playlist, "#EXT-X-STREAM-INF") {
		media := firstPlaylistURI(t, playlist)
		playlist = relayFetchText(t, f.home, base+media, f.homeAdmin)
		if !strings.HasPrefix(playlist, "#EXTM3U") {
			t.Fatalf("the relayed media playlist is not a playlist: %.80q", playlist)
		}
	}
	segment := firstPlaylistURI(t, playlist)
	if strings.HasPrefix(segment, "/") || strings.Contains(segment, "://") {
		t.Fatalf("segment URI %q is not relative — the rewrite must keep it resolvable", segment)
	}

	resp := authStream(t, f.home, base+segment, f.homeAdmin, "")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relayed segment %q = %d, want 200", segment, resp.StatusCode)
	}
	if len(data) == 0 {
		t.Error("the relayed segment carried no bytes")
	}
	if ct := resp.Header.Get("Content-Type"); ct == "" {
		t.Error("the relayed segment carried no Content-Type")
	}
}

// relayFetchText GETs a relay path and returns its body as text.
func relayFetchText(t *testing.T, srv *testharness.Server, path, token string) string {
	t.Helper()
	resp := authStream(t, srv, path, token, "")
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %.200s", path, resp.StatusCode, data)
	}
	return string(data)
}

// firstPlaylistURI returns the first non-comment line of a playlist.
func firstPlaylistURI(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatalf("no URI in playlist:\n%s", playlist)
	return ""
}

// --- artwork ------------------------------------------------------------------

// TestRelayArtworkIsFetchedOnceAndCached: a mirrored Title's poster is fetched
// from the sharer on the FIRST request and served from this Server's own artwork
// cache afterwards (ADR-0056 §5) — the feed carries no artwork, so without this a
// friend's whole library is placeholders.
func TestRelayArtworkIsFetchedOnceAndCached(t *testing.T) {
	f := linkForRelay(t)

	// A poster on the sharer's side, uploaded so the test owns the exact bytes.
	sharerTitles := listTitles(t, f.sharer, f.sharerAdmin, f.srcLib)
	sharerDune := findTitle(t, sharerTitles, "Dune")
	poster := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("poster", 40))
	if st := uploadArtwork(t, f.sharer, f.sharerAdmin,
		"/api/v1/titles/"+sharerDune+"/artworkUpload?role=poster", "image/png", poster, nil); st != http.StatusOK {
		t.Fatalf("uploading the sharer's poster = %d", st)
	}

	titleID := f.mirroredTitle(t, "Dune")
	path := "/api/v1/titles/" + titleID + "/artwork/poster"

	resp := authStream(t, f.home, path, f.homeAdmin, "")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relayed artwork = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(got, poster) {
		t.Errorf("relayed artwork is %d bytes, want the sharer's %d", len(got), len(poster))
	}
	fetched := f.rec.countPath("/api/v1/titles/")
	if fetched == 0 {
		t.Fatal("the poster was never fetched from the sharer")
	}

	// A second request is served from the cache: the sharer is asked once.
	again := authStream(t, f.home, path, f.homeAdmin, "")
	second, _ := io.ReadAll(again.Body)
	again.Body.Close()
	if again.StatusCode != http.StatusOK || !bytes.Equal(second, poster) {
		t.Fatalf("the cached artwork read back as %d / %d bytes", again.StatusCode, len(second))
	}
	if now := f.rec.countPath("/api/v1/titles/"); now != fetched {
		t.Errorf("the sharer was asked %d times for one poster, want %d (cached)", now, fetched)
	}
}

// listTitles is the browse list of one Library.
func listTitles(t *testing.T, srv *testharness.Server, token, libID string) titlesListResp {
	t.Helper()
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list); status != http.StatusOK {
		t.Fatalf("listing titles = %d; body: %s", status, body)
	}
	return list
}

// --- the relay route's own posture --------------------------------------------

// TestRelayRouteRefusals: the relay routes are media routes, and they refuse
// exactly as the media routes they mirror do.
func TestRelayRouteRefusals(t *testing.T) {
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d; body: %s", status, body)
	}
	remote := relayRemoteSessionOf(t, dec.StreamURL)

	// Another User's relay session is hidden, not forbidden.
	f.home.CreateUser(f.homeAdmin, "nosy", "hunter2hunter2", "member")
	other := f.home.LoginAs("nosy", "hunter2hunter2")
	if resp := authStream(t, f.home, dec.StreamURL, other, ""); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("another User's relay stream = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Anonymous is a 401, exactly like /sessions/{id}/stream.
	if resp := authStream(t, f.home, dec.StreamURL, "", ""); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Errorf("anonymous relay stream = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// A path that is not this session's is refused, whatever it names over there:
	// the relay is not a proxy into a friend's server.
	for _, tail := range []string{
		"libraries",
		"sessions/" + remote + "/../../libraries",
		"titles/" + titleID + "/artwork/poster",
		"sessions/00000000-0000-0000-0000-000000000000/stream",
	} {
		resp := authStream(t, f.home, "/api/v1/relay/"+dec.SessionID+"/"+tail, f.homeAdmin, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("relaying %q = %d, want 404", tail, resp.StatusCode)
		}
	}

	// The stream token reaches the same bytes (ADR-0039's third credential).
	if dec.StreamToken != "" {
		resp := authStream(t, f.home, "/api/v1/stream/"+dec.StreamToken+"/stream", "", "")
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(data) == 0 {
			t.Errorf("the stream token on a relay session = %d with %d bytes, want 200 with bytes",
				resp.StatusCode, len(data))
		}
	}
}
