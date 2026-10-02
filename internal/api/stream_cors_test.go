package api_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/api"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for CORS on the stream-token subtree (ticket 39 of the Android
// client; ADR-0066). A Google Cast receiver is a web page on a foreign origin, so
// it can read a response under GET /api/v1/stream/… only if that response says so.
//
// The scope is the point: EVERY response of the subtree carries the headers and
// nothing outside it does. A cookie route that echoed an Origin would let any web
// page the user visits read their media, so the "nothing else" half is tested as
// hard as the "everything" half.

const castOrigin = "https://receiver.example"

// corsResponse is one captured response of a /stream/ request.
type corsResponse struct {
	status int
	header http.Header
	body   []byte
}

// streamDo sends method to apiPath with an optional Origin and no credential of
// any kind, and captures the whole response.
func streamDo(t *testing.T, srv *testharness.Server, method, apiPath, origin string, extra http.Header) corsResponse {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL(apiPath), nil)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, apiPath, err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, apiPath, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return corsResponse{status: resp.StatusCode, header: resp.Header, body: body}
}

// assertStreamCORS requires the S1 headers on r, as sent to a request carrying
// origin ("" = no Origin header).
func assertStreamCORS(t *testing.T, what string, r corsResponse, origin string) {
	t.Helper()
	got, present := r.header["Access-Control-Allow-Origin"]
	switch {
	case origin == "" && present:
		t.Errorf("%s: Access-Control-Allow-Origin = %q with no request Origin, want none", what, got)
	case origin != "" && (len(got) != 1 || got[0] != origin):
		t.Errorf("%s: Access-Control-Allow-Origin = %q, want the request's %q echoed", what, got, origin)
	}
	if got := r.header.Values("Vary"); !containsFold(got, "Origin") {
		t.Errorf("%s: Vary = %q, want it to include Origin", what, got)
	}
	if got := r.header.Get("Access-Control-Expose-Headers"); got != "Content-Length, Content-Range, Accept-Ranges" {
		t.Errorf("%s: Access-Control-Expose-Headers = %q, want Content-Length, Content-Range, Accept-Ranges", what, got)
	}
	if _, ok := r.header["Access-Control-Allow-Credentials"]; ok {
		t.Errorf("%s: sent Access-Control-Allow-Credentials; these routes honour no credential and must never grant one", what)
	}
	if r.header.Get("Access-Control-Allow-Origin") == "*" {
		t.Errorf("%s: Access-Control-Allow-Origin is *; Cast refuses the wildcard and the echo is the contract", what)
	}
}

func containsFold(vs []string, want string) bool {
	for _, v := range vs {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
	}
	return false
}

// assertNoCORS requires that no Access-Control-* header (and no Vary: Origin,
// which only exists to qualify one) is on a response.
func assertNoCORS(t *testing.T, what string, r corsResponse) {
	t.Helper()
	for k := range r.header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s: carries %s = %q; only /api/v1/stream/… may answer CORS", what, k, r.header[k])
		}
	}
}

// sameResponse requires b to equal a in status, body and every header but Date.
func sameResponse(t *testing.T, what string, a, b corsResponse) {
	t.Helper()
	if a.status != b.status {
		t.Errorf("%s: status %d vs %d", what, a.status, b.status)
	}
	if !bytes.Equal(a.body, b.body) {
		t.Errorf("%s: body %q vs %q", what, a.body, b.body)
	}
	flat := func(h http.Header) []string {
		var out []string
		for k, vs := range h {
			if k == "Date" {
				continue
			}
			out = append(out, k+": "+strings.Join(vs, "\x00"))
		}
		sort.Strings(out)
		return out
	}
	fa, fb := flat(a.header), flat(b.header)
	if strings.Join(fa, "\n") != strings.Join(fb, "\n") {
		t.Errorf("%s: headers differ\n%v\nvs\n%v", what, fa, fb)
	}
}

// TestStreamCORSPreflightIsTokenBlind: OPTIONS is answered in the method gate's
// position, before the token is examined, so a live token, a dead one and a
// malformed path get byte-identical responses. Any difference is a validity
// oracle (ADR-0039) — the same one the 405-before-404 order exists to prevent.
func TestStreamCORSPreflightIsTokenBlind(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)

	preflight := http.Header{
		"Access-Control-Request-Method":  {"GET"},
		"Access-Control-Request-Headers": {"range"},
	}
	live := streamDo(t, srv, http.MethodOptions, tokenStream(dec.StreamToken), castOrigin, preflight)

	if live.status != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204; body: %s", live.status, live.body)
	}
	if len(live.body) != 0 {
		t.Errorf("preflight carried a body: %q", live.body)
	}
	assertStreamCORS(t, "preflight", live, castOrigin)
	for header, want := range map[string]string{
		"Access-Control-Allow-Methods": "GET, HEAD",
		"Access-Control-Allow-Headers": "Content-Type, Accept-Encoding, Range",
		"Access-Control-Max-Age":       "3600",
	} {
		if got := live.header.Get(header); got != want {
			t.Errorf("preflight %s = %q, want %q", header, got, want)
		}
	}

	for name, path := range map[string]string{
		"dead token":          tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy"),
		"dead token, hls":     tokenHLS("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy", "index.m3u8"),
		"live token, hls":     tokenHLS(dec.StreamToken, "master.m3u8"),
		"live token, unknown": "/api/v1/stream/" + dec.StreamToken + "/nope",
		"no artifact":         "/api/v1/stream/" + dec.StreamToken,
		"malformed":           "/api/v1/stream/",
		"nested junk":         "/api/v1/stream/a/b/c/d",
	} {
		sameResponse(t, "OPTIONS "+name, live, streamDo(t, srv, http.MethodOptions, path, castOrigin, preflight))
	}
}

// TestStreamCORSHeadersOnEveryResponse: success, 206, every refusal, the 405 and
// the 204 all carry the headers — a receiver's script has to be able to read the
// status of a failure — with the Origin echoed, and with no Origin no
// Access-Control-Allow-Origin at all (Vary: Origin still, because the response
// depends on it).
func TestStreamCORSHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)
	rng := http.Header{"Range": {"bytes=0-9"}}

	cases := []struct {
		name, method, path string
		extra              http.Header
		status             int
	}{
		{"200 progressive", http.MethodGet, tokenStream(dec.StreamToken), nil, http.StatusOK},
		{"206 range", http.MethodGet, tokenStream(dec.StreamToken), rng, http.StatusPartialContent},
		{"404 dead token", http.MethodGet, tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy"), nil, http.StatusNotFound},
		{"404 live token, unknown artifact", http.MethodGet, "/api/v1/stream/" + dec.StreamToken + "/nope", nil, http.StatusNotFound},
		{"404 live token, hls on a direct-play session", http.MethodGet, tokenHLS(dec.StreamToken, "index.m3u8"), nil, http.StatusNotFound},
		{"404 malformed", http.MethodGet, "/api/v1/stream/", nil, http.StatusNotFound},
		{"405 POST", http.MethodPost, tokenStream(dec.StreamToken), nil, http.StatusMethodNotAllowed},
		{"405 DELETE dead token", http.MethodDelete, tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy"), nil, http.StatusMethodNotAllowed},
		{"204 preflight", http.MethodOptions, tokenStream(dec.StreamToken), nil, http.StatusNoContent},
		{"200 HEAD", http.MethodHead, tokenStream(dec.StreamToken), nil, http.StatusOK},
	}
	for _, c := range cases {
		withOrigin := streamDo(t, srv, c.method, c.path, castOrigin, c.extra)
		if withOrigin.status != c.status {
			t.Errorf("%s = %d, want %d; body: %s", c.name, withOrigin.status, c.status, withOrigin.body)
			continue
		}
		assertStreamCORS(t, c.name+" with Origin", withOrigin, castOrigin)

		noOrigin := streamDo(t, srv, c.method, c.path, "", c.extra)
		if noOrigin.status != c.status {
			t.Errorf("%s without Origin = %d, want %d", c.name, noOrigin.status, c.status)
		}
		assertStreamCORS(t, c.name+" without Origin", noOrigin, "")
	}

	// The echo is verbatim, not a fixed allow-list: another origin gets itself.
	other := streamDo(t, srv, http.MethodGet, tokenStream(dec.StreamToken), "http://192.168.1.50:8008", nil)
	assertStreamCORS(t, "another origin", other, "http://192.168.1.50:8008")
}

// TestStreamCORSRefusalIsUnchanged: CORS is ADDED to the refusal and nothing else
// about it moves — still the one 404 envelope, still no WWW-Authenticate.
func TestStreamCORSRefusalIsUnchanged(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)

	const want = `{"error":{"code":"NOT_FOUND","message":"session not found"}}`
	for name, path := range map[string]string{
		"dead token":  tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy"),
		"unknown art": "/api/v1/stream/" + dec.StreamToken + "/nope",
	} {
		r := streamDo(t, srv, http.MethodGet, path, castOrigin, nil)
		if r.status != http.StatusNotFound || strings.TrimSpace(string(r.body)) != want {
			t.Errorf("%s = %d %s, want the one 404 envelope", name, r.status, r.body)
		}
		if r.header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s sent WWW-Authenticate", name)
		}
	}
}

// TestStreamCORSIsScopedToTheStreamSubtree: the bearer media route, the cookie
// media route, a JSON route and an artwork route get NO Access-Control-* header
// even when asked with an Origin. The cookie route is the one that matters: a
// browser attaches ms_media ambiently, so an echoed Origin there would let any
// page the user visits read their media cross-site.
func TestStreamCORSIsScopedToTheStreamSubtree(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	root := generateSubtitledRemuxClip(t)
	list := scanLibraryAt(t, srv, bearer, root)
	titleID := findTitle(t, list, "Subbed Movie")
	dec := negotiateRemuxDecision(t, srv, bearer, titleID)

	poster := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("poster", 40))
	if st := uploadArtwork(t, srv, bearer,
		"/api/v1/titles/"+titleID+"/artworkUpload?role=poster", "image/png", poster, nil); st != http.StatusOK {
		t.Fatalf("uploading a poster = %d", st)
	}

	get := func(path string, h http.Header) corsResponse {
		return streamDo(t, srv, http.MethodGet, path, castOrigin, h)
	}
	bearerH := http.Header{"Authorization": {"Bearer " + bearer}}
	cookieH := http.Header{"Cookie": {mediaCookieName + "=" + bearer}}

	cases := []struct {
		name, path string
		h          http.Header
		status     int
	}{
		{"bearer hls master", "/api/v1/sessions/" + dec.SessionID + "/hls/master.m3u8", bearerH, http.StatusOK},
		{"cookie hls master", "/api/v1/sessions/" + dec.SessionID + "/hls/master.m3u8", cookieH, http.StatusOK},
		{"bearer progressive", "/api/v1/sessions/" + dec.SessionID + "/stream", bearerH, 0},
		{"JSON route", "/api/v1/server", nil, http.StatusOK},
		{"artwork route", "/api/v1/titles/" + titleID + "/artwork/poster", cookieH, http.StatusOK},
	}
	for _, c := range cases {
		r := get(c.path, c.h)
		if c.status != 0 && r.status != c.status {
			t.Errorf("%s = %d, want %d (the route must have answered for the check to mean anything)", c.name, r.status, c.status)
		}
		assertNoCORS(t, c.name, r)
	}

	// Their preflights are not answered either: no route outside the subtree
	// grants a cross-origin read.
	for _, path := range []string{
		"/api/v1/sessions/" + dec.SessionID + "/hls/master.m3u8",
		"/api/v1/server",
	} {
		assertNoCORS(t, "OPTIONS "+path, streamDo(t, srv, http.MethodOptions, path, castOrigin,
			http.Header{"Access-Control-Request-Method": {"GET"}}))
	}
}

// TestStreamTokenHLSPlaylistAnswersCORS: a 200 HLS playlist GET carrying an Origin
// gets the S1 headers — the Cast receiver's own manifest fetch — and the playlist
// bytes are the ones it gets without an Origin.
func TestStreamTokenHLSPlaylistAnswersCORS(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	root := generateSubtitledRemuxClip(t)
	list := scanLibraryAt(t, srv, bearer, root)
	titleID := findTitle(t, list, "Subbed Movie")
	dec := negotiateRemuxDecision(t, srv, bearer, titleID)

	path := tokenHLS(dec.StreamToken, "master.m3u8")
	with := streamDo(t, srv, http.MethodGet, path, castOrigin, nil)
	if with.status != http.StatusOK {
		t.Fatalf("GET master.m3u8 with Origin = %d, want 200; body: %s", with.status, with.body)
	}
	assertStreamCORS(t, "hls playlist GET with Origin", with, castOrigin)
	without := streamDo(t, srv, http.MethodGet, path, "", nil)
	assertStreamCORS(t, "hls playlist GET without Origin", without, "")
	if string(with.body) != string(without.body) {
		t.Errorf("playlist bytes differ with an Origin:\n%s\nvs\n%s", with.body, without.body)
	}
}

// TestStreamTokenHeadAnswersHeadersWithNoBody (S2): HEAD is admitted in the method
// gate's position and then runs exactly as GET — headers, no body — on the
// progressive route (http.ServeContent), an HLS playlist and a segment
// (hlsArtifactHandler); and a dead token's HEAD answers what its GET answers.
func TestStreamTokenHeadAnswersHeadersWithNoBody(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	srv := testharness.New(t)
	bearer := adminToken(t, srv)
	root := generateSubtitledRemuxClip(t)
	list := scanLibraryAt(t, srv, bearer, root)
	titleID := findTitle(t, list, "Subbed Movie")
	dec := negotiateRemuxDecision(t, srv, bearer, titleID)

	playlist := anonText(t, srv, tokenHLS(dec.StreamToken, "index.m3u8"))
	segs := parseSegments(playlist)
	if len(segs) == 0 {
		t.Fatalf("no segments in the playlist:\n%s", playlist)
	}
	for _, file := range []string{"master.m3u8", "index.m3u8", segs[0]} {
		path := tokenHLS(dec.StreamToken, file)
		get := streamDo(t, srv, http.MethodGet, path, "", nil)
		head := streamDo(t, srv, http.MethodHead, path, "", nil)
		if head.status != http.StatusOK || get.status != http.StatusOK {
			t.Fatalf("%s: GET %d, HEAD %d, want 200 for both", file, get.status, head.status)
		}
		if len(head.body) != 0 {
			t.Errorf("HEAD %s carried a %d byte body", file, len(head.body))
		}
		if len(get.body) == 0 {
			t.Errorf("GET %s carried no body, so the HEAD check proves nothing", file)
		}
		if got, want := head.header.Get("Content-Type"), get.header.Get("Content-Type"); got != want {
			t.Errorf("HEAD %s Content-Type = %q, GET says %q", file, got, want)
		}
	}

	// Progressive, through http.ServeContent: Content-Length and Accept-Ranges are
	// the GET's, the body is absent.
	prog := testharness.New(t)
	_, _, ddec := duneSession(t, prog)
	getP := streamDo(t, prog, http.MethodGet, tokenStream(ddec.StreamToken), "", nil)
	headP := streamDo(t, prog, http.MethodHead, tokenStream(ddec.StreamToken), "", nil)
	if headP.status != http.StatusOK || len(headP.body) != 0 {
		t.Fatalf("HEAD progressive = %d with %d body bytes, want 200 and none", headP.status, len(headP.body))
	}
	if got, want := headP.header.Get("Content-Length"), getP.header.Get("Content-Length"); got == "" || got != want {
		t.Errorf("HEAD Content-Length = %q, GET says %q", got, want)
	}
	if got := headP.header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("HEAD Accept-Ranges = %q, want bytes", got)
	}

	// A dead token's HEAD is its GET's refusal (status and headers; HEAD has no body).
	dead := tokenStream("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFy")
	getD := streamDo(t, prog, http.MethodGet, dead, castOrigin, nil)
	headD := streamDo(t, prog, http.MethodHead, dead, castOrigin, nil)
	if headD.status != http.StatusNotFound || getD.status != http.StatusNotFound {
		t.Fatalf("dead token: GET %d, HEAD %d, want 404 for both", getD.status, headD.status)
	}
	getD.body = nil // the one legitimate difference
	sameResponse(t, "dead-token HEAD vs GET", getD, headD)
}

// TestStreamCORSOnTheRelayBranch: a relay session reached through
// /stream/<token>/… is the same subtree, so its responses carry the headers too —
// the success, the preflight and the HEAD.
func TestStreamCORSOnTheRelayBranch(t *testing.T) {
	t.Parallel()
	f := linkForRelay(t)
	titleID := f.mirroredTitle(t, "Dune")
	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK || dec.StreamToken == "" {
		t.Fatalf("relayed playback = %d, token %q; body: %s", status, dec.StreamToken, body)
	}

	get := streamDo(t, f.home, http.MethodGet, tokenStream(dec.StreamToken), castOrigin, http.Header{"Range": {"bytes=0-99"}})
	if get.status != http.StatusPartialContent || len(get.body) != 100 {
		t.Fatalf("relay GET through the token = %d with %d bytes, want 206 with 100", get.status, len(get.body))
	}
	assertStreamCORS(t, "relay GET", get, castOrigin)

	head := streamDo(t, f.home, http.MethodHead, tokenStream(dec.StreamToken), castOrigin, nil)
	if head.status != http.StatusOK || len(head.body) != 0 {
		t.Fatalf("relay HEAD = %d with %d bytes, want 200 and none", head.status, len(head.body))
	}
	assertStreamCORS(t, "relay HEAD", head, castOrigin)

	opt := streamDo(t, f.home, http.MethodOptions, tokenStream(dec.StreamToken), castOrigin, nil)
	if opt.status != http.StatusNoContent {
		t.Fatalf("relay preflight = %d, want 204", opt.status)
	}
	assertStreamCORS(t, "relay preflight", opt, castOrigin)

	unknown := streamDo(t, f.home, http.MethodGet, "/api/v1/stream/"+dec.StreamToken+"/nope", castOrigin, nil)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("relay unknown artifact = %d, want 404", unknown.status)
	}
	assertStreamCORS(t, "relay refusal", unknown, castOrigin)
}

// TestStreamTokenNeverReachesTheAccessLogOnOptionsOrHead: OPTIONS and HEAD go
// through LogRequests -> RedactPath like GET, so the token is in no line.
func TestStreamTokenNeverReachesTheAccessLogOnOptionsOrHead(t *testing.T) {
	// Not parallel: it captures the process-wide log output.
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)

	var logged syncBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	ts := httptest.NewServer(api.LogRequests(srv.Handler()))
	defer ts.Close()

	for _, method := range []string{http.MethodOptions, http.MethodHead} {
		req, err := http.NewRequest(method, ts.URL+tokenStream(dec.StreamToken), nil)
		if err != nil {
			t.Fatalf("building %s: %v", method, err)
		}
		req.Header.Set("Origin", castOrigin)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	out := logged.String()
	if strings.Contains(out, dec.StreamToken) {
		t.Fatalf("the access log printed the stream token:\n%s", out)
	}
	for _, method := range []string{"OPTIONS", "HEAD"} {
		if !strings.Contains(out, "obelo: http: "+method+" /api/v1/stream/[redacted]/stream") {
			t.Errorf("no redacted %s line in the access log:\n%s", method, out)
		}
	}
}
