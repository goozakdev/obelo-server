package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// Black-box tests for the Online source provider Extension point (ADR-0068, issue
// 01): an Installed Online source provider whose "source" is an httptest server
// answering rows() and resolve(), a fake https media host, and the endpoints a
// client reaches — GET /onlineSources, the source page, the thumbnail proxy and
// the playback start — plus the stream token that carries the bytes.
//
// The media host is an httptest TLS server whose client the Server is told to
// use, so the relay fetches over real https while trusting the test certificate.

const (
	onlineBase    = "/api/v1/onlineSources"
	onlineSlug    = "tube"
	onlineName    = "Test Tube"
	onlineItemMP4 = "v1"
)

// onlineMedia is the fake https media host. It serves one deterministic mp4-ish
// payload at /v1.mp4 with Range support, a PNG-signature thumbnail, and a path that
// redirects to a loopback address.
type onlineMedia struct {
	srv   *httptest.Server
	body  []byte
	mu    sync.Mutex
	hits  map[string]int
	refer string
	// slowStarted is closed when /slow.mp4 begins to trickle; slowEnded when its
	// handler returns, which it does once the Server drops the upstream request.
	slowStarted, slowEnded chan struct{}
	slowOnce               sync.Once
}

func newOnlineMedia(t *testing.T) *onlineMedia {
	t.Helper()
	m := &onlineMedia{hits: map[string]int{}, slowStarted: make(chan struct{}), slowEnded: make(chan struct{})}
	m.body = make([]byte, 300_000)
	for i := range m.body {
		m.body[i] = byte(i * 7)
	}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits[r.URL.Path]++
		if ref := r.Header.Get("Referer"); ref != "" {
			m.refer = ref
		}
		m.mu.Unlock()
		switch {
		case r.URL.Path == "/v1.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "v1.mp4", time.Time{}, bytes.NewReader(m.body))
		case r.URL.Path == "/expiring.mp4" && m.hitsOf(r.URL.Path) == 1:
			// A URL that has expired by the time the player asks: refused once, then
			// good again (the re-resolved answer names the same address).
			w.WriteHeader(http.StatusGone)
		case r.URL.Path == "/expiring.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "expiring.mp4", time.Time{}, bytes.NewReader(m.body))
		case r.URL.Path == "/dead.mp4":
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/slow.mp4":
			// A response that never finishes: a kilobyte every 20ms until the Server
			// drops the request.
			defer close(m.slowEnded)
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			m.slowOnce.Do(func() { close(m.slowStarted) })
			for {
				if _, err := w.Write(make([]byte, 1024)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		case r.URL.Path == "/redirect.mp4":
			http.Redirect(w, r, m.srv.URL+"/v1.mp4", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/thumb/"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(onlineThumbPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *onlineMedia) hitsOf(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

func (m *onlineMedia) hitCount(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

// onlineThumbPNG begins with the PNG signature, which is all the Server's content sniff
// asks of a thumbnail.
var onlineThumbPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 2048)...)

// onlineSource stands in for the remote service the Plugin talks to: it answers the
// guest's rows and resolve posts and records every call envelope.
type onlineSource struct {
	srv      *httptest.Server
	mu       sync.Mutex
	rowsCall []map[string]any
	rowCalls []map[string]any
	resolved []string
	hints    []map[string]any
	rows     func() []map[string]any
	row      func(rowID, cursor string) map[string]any
	// search answers search(); queries records what was asked.
	search  func(query string) []map[string]any
	queries []string
	// down, when set and true for a call's path, makes the source answer 500.
	down func(path string) bool
	// hang makes the source hold every call until the caller gives up.
	hang     atomic.Bool
	variants func(itemID string) []map[string]any
}

func newOnlineSource(t *testing.T, media *onlineMedia) *onlineSource {
	t.Helper()
	s := &onlineSource{}
	mp4 := func(res, path string) map[string]any {
		return map[string]any{
			"url": media.srv.URL + path, "container": "mp4", "codecs": []string{"h264", "aac"},
			"resolution": res, "headers": map[string]string{"Referer": "https://tube.example/"},
		}
	}
	s.rows = func() []map[string]any {
		return []map[string]any{{
			"id": "recent", "label": "Recently added",
			"items": []map[string]any{
				{"id": onlineItemMP4, "title": "A talk", "thumbnailUrl": media.srv.URL + "/thumb/v1.png",
					"durationMs": 61000, "description": "About things", "publishedAt": "2026-09-01T00:00:00Z"},
				{"id": "tall", "title": "Only in 1080p", "thumbnailUrl": media.srv.URL + "/thumb/tall.png", "durationMs": 1000},
				{"id": "redir", "title": "Redirects", "thumbnailUrl": media.srv.URL + "/thumb/redir.png", "durationMs": 1000},
				// Malformed entries the Server drops: a plain-http thumbnail and an id
				// outside the URL-safe set.
				{"id": "plainhttp", "title": "No", "thumbnailUrl": "http://insecure.example/x.png", "durationMs": 1},
				{"id": "bad/id", "title": "No", "thumbnailUrl": media.srv.URL + "/thumb/x.png", "durationMs": 1},
			},
		}}
	}
	s.search = func(query string) []map[string]any {
		return []map[string]any{{"id": "hit-" + query, "title": "Found " + query,
			"thumbnailUrl": media.srv.URL + "/thumb/v1.png", "durationMs": 1000}}
	}
	s.variants = func(itemID string) []map[string]any {
		switch itemID {
		case onlineItemMP4:
			return []map[string]any{mp4("720p", "/v1.mp4")}
		case "tall":
			return []map[string]any{mp4("1080p", "/v1.mp4")}
		case "redir":
			return []map[string]any{mp4("720p", "/redirect.mp4")}
		case "slow":
			return []map[string]any{mp4("720p", "/slow.mp4")}
		case "expiring":
			return []map[string]any{mp4("720p", "/expiring.mp4")}
		case "dead":
			return []map[string]any{mp4("720p", "/dead.mp4")}
		case "split":
			return []map[string]any{{
				"kind": "split", "videoUrl": media.srv.URL + "/v.mp4", "audioUrl": media.srv.URL + "/a.m4a",
				"container": "mp4", "codecs": []string{"h264", "aac"}, "resolution": "1080p",
				"headers": map[string]string{"Referer": "https://tube.example/", "User-Agent": "Tube/1"},
			}}
		case "manifest":
			return []map[string]any{{"kind": "manifest", "url": media.srv.URL + "/master.m3u8", "container": "hls", "resolution": "720p"}}
		case "plainhttp":
			return []map[string]any{{"url": "http://" + media.srv.Listener.Addr().String() + "/v1.mp4", "container": "mp4", "codecs": []string{"h264", "aac"}, "resolution": "720p"}}
		case "offlist":
			// A host the manifest does not allowlist: the same server by name, not by address.
			return []map[string]any{{"url": strings.Replace(media.srv.URL, "127.0.0.1", "localhost", 1) + "/v1.mp4", "container": "mp4", "codecs": []string{"h264", "aac"}, "resolution": "720p"}}
		}
		return nil
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call map[string]any
		_ = json.NewDecoder(r.Body).Decode(&call)
		if s.hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if s.down != nil && s.down(r.URL.Path) {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/search":
			req, _ := call["request"].(map[string]any)
			query, _ := req["query"].(string)
			s.queries = append(s.queries, query)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": s.search(query)})
		case "/rows":
			s.rowsCall = append(s.rowsCall, call)
			_ = json.NewEncoder(w).Encode(map[string]any{"rows": s.rows()})
		case "/row":
			req, _ := call["request"].(map[string]any)
			s.rowCalls = append(s.rowCalls, req)
			rowID, _ := req["rowId"].(string)
			cursor, _ := req["cursor"].(string)
			_ = json.NewEncoder(w).Encode(s.row(rowID, cursor))
		case "/resolve":
			id, _ := call["request"].(map[string]any)["itemId"].(string)
			s.resolved = append(s.resolved, id)
			hints, _ := call["request"].(map[string]any)["hints"].(map[string]any)
			s.hints = append(s.hints, hints)
			_ = json.NewEncoder(w).Encode(map[string]any{"variants": s.variants(id)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *onlineSource) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rowsCall) + len(s.rowCalls) + len(s.resolved) + len(s.queries)
}

// onlineServer installs the test Plugin, boots a server that relays through the
// media host's client, and returns it with an Admin token.
func onlineServer(t *testing.T, opts ...testharness.Option) (*testharness.Server, string, *onlineSource, *onlineMedia) {
	t.Helper()
	srv, admin, src, media, _ := onlineServerWithRunner(t, true, opts...)
	return srv, admin, src, media
}

// onlineServerWithRunner is onlineServer, also returning the fake ffmpeg an Online
// encode runs under. The manifest allowlists the media host (a loopback address,
// whose private-address check the harness is told to skip for exactly that
// host:port), and the Server believes it has an ffmpeg unless opts say otherwise.
func onlineServerWithRunner(t *testing.T, exemptMedia bool, opts ...testharness.Option) (*testharness.Server, string, *onlineSource, *onlineMedia, *fakeOnlineFFmpeg) {
	t.Helper()
	media := newOnlineMedia(t)
	src := newOnlineSource(t, media)
	runner := &fakeOnlineFFmpeg{}
	dataDir := t.TempDir()
	manifest := plugintest.OnlineSourceManifest(onlineSlug, onlineName, src.srv.URL)
	manifest.Network.Hosts = []string{"127.0.0.1"}
	plugintest.Install(t, dataDir, manifest)
	all := append([]testharness.Option{
		testharness.WithDataDir(dataDir),
		testharness.WithPluginFetchesExemptAt(src.srv.Listener.Addr().String()),
		testharness.WithOnlineSourceClient(media.srv.Client()),
		testharness.WithOnlineSourceRunner(runner),
		testharness.WithFFmpegAvailability(true),
	}, opts...)
	if exemptMedia {
		all = append(all, testharness.WithOnlineSourceMediaExemptAt(media.srv.Listener.Addr().String()))
	}
	srv := testharness.New(t, all...)
	return srv, adminToken(t, srv), src, media, runner
}

// fakeOnlineFFmpeg stands in for ffmpeg: it records every run's arguments and
// writes a playlist and a segment where the real one would, and runs until killed.
type fakeOnlineFFmpeg struct {
	mu   sync.Mutex
	runs [][]string
}

type fakeOnlineJob struct {
	done chan struct{}
	once sync.Once
}

func (j *fakeOnlineJob) Wait() error { <-j.done; return nil }
func (j *fakeOnlineJob) Kill() error { j.once.Do(func() { close(j.done) }); return nil }

func (f *fakeOnlineFFmpeg) Start(_ context.Context, args []string) (transcode.Job, error) {
	f.mu.Lock()
	f.runs = append(f.runs, args)
	f.mu.Unlock()
	out := args[len(args)-1]
	_ = os.WriteFile(out, []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.0,\nsegment000.ts\n"), 0o644)
	_ = os.WriteFile(filepath.Join(filepath.Dir(out), "segment000.ts"), []byte("fake-ts-bytes"), 0o644)
	return &fakeOnlineJob{done: make(chan struct{})}, nil
}

func (f *fakeOnlineFFmpeg) argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.runs...)
}

type onlineSourcesResp struct {
	Sources []struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		IconURL *string `json:"iconUrl"`
	} `json:"sources"`
}

type onlineRowsResp struct {
	Rows []struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		NextCursor *string `json:"nextCursor"`
		Items      []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			ThumbnailURL string `json:"thumbnailUrl"`
			DurationMs   int64  `json:"durationMs"`
			Description  string `json:"description"`
			PublishedAt  string `json:"publishedAt"`
		} `json:"items"`
	} `json:"rows"`
}

type onlinePlayResp struct {
	SessionID string `json:"sessionId"`
	StreamURL string `json:"streamUrl"`
	Format    string `json:"format"`
}

// canPlayMP4 is the Capability profile of a client that plays h264/aac in mp4.
func canPlayMP4(constraints map[string]any) map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":  []string{"mp4"},
			"videoCodecs": []map[string]any{{"codec": "h264"}},
			"audioCodecs": []string{"aac"},
		},
		"constraints": constraints,
	}
}

func playOnline(t *testing.T, srv *testharness.Server, token, item string, body map[string]any) (int, onlinePlayResp, []byte) {
	t.Helper()
	var out onlinePlayResp
	status, raw := srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/"+item+"/playback", token, body, &out)
	return status, out, raw
}

func getBytes(t *testing.T, srv *testharness.Server, path string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TestOnlineSourcesListIsServedFromTheRegistryAndCallsNoPlugin: one tile per
// enabled source from its own endpoint, with zero Plugin calls, and the home
// response carries nothing about sources.
func TestOnlineSourcesListIsServedFromTheRegistryAndCallsNoPlugin(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)

	var list onlineSourcesResp
	if status, body := srv.AuthGET(onlineBase, admin, &list); status != http.StatusOK {
		t.Fatalf("GET /onlineSources = %d, want 200; body: %s", status, body)
	}
	if len(list.Sources) != 1 || list.Sources[0].ID != onlineSlug || list.Sources[0].Name != onlineName {
		t.Fatalf("sources = %+v, want exactly the one enabled source %q named %q", list.Sources, onlineSlug, onlineName)
	}
	if list.Sources[0].IconURL != nil {
		t.Fatalf("iconUrl = %q, want null until the icon slice", *list.Sources[0].IconURL)
	}
	if n := src.calls(); n != 0 {
		t.Fatalf("listing the tiles made %d Plugin calls, want 0", n)
	}

	status, home := srv.AuthGET("/api/v1/home", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /home = %d; body: %s", status, home)
	}
	for _, leak := range []string{onlineSlug, onlineName, "onlineSources", "sources"} {
		if bytes.Contains(home, []byte(leak)) {
			t.Fatalf("the home response mentions %q: %s", leak, home)
		}
	}
	if n := src.calls(); n != 0 {
		t.Fatalf("loading home made %d Plugin calls, want 0", n)
	}
}

// TestAnAdminOpensASourcePageAndThumbnailsAreProxiedNotStored: the page returns the
// Plugin's rows and items (malformed ones dropped), each item's thumbnail points at
// the Server's proxy, and fetching it writes nothing to disk.
func TestAnAdminOpensASourcePageAndThumbnailsAreProxiedNotStored(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)

	var page onlineRowsResp
	status, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, &page)
	if status != http.StatusOK {
		t.Fatalf("GET rows = %d; body: %s", status, body)
	}
	if len(page.Rows) != 1 || page.Rows[0].ID != "recent" || page.Rows[0].Label != "Recently added" {
		t.Fatalf("rows = %+v, want the Plugin's one row", page.Rows)
	}
	var ids []string
	for _, it := range page.Rows[0].Items {
		ids = append(ids, it.ID)
	}
	if want := []string{onlineItemMP4, "tall", "redir"}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("item ids = %v, want %v (the plain-http thumbnail and the unsafe id are dropped)", ids, want)
	}
	first := page.Rows[0].Items[0]
	if first.Title != "A talk" || first.DurationMs != 61000 || first.Description != "About things" || first.PublishedAt != "2026-09-01T00:00:00Z" {
		t.Fatalf("item = %+v, want the Plugin's fields", first)
	}
	wantThumb := onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail"
	if first.ThumbnailURL != wantThumb {
		t.Fatalf("thumbnailUrl = %q, want the Server's proxy %q", first.ThumbnailURL, wantThumb)
	}
	if strings.Contains(string(body), media.srv.URL) {
		t.Fatalf("the page leaks the upstream address: %s", body)
	}
	// The Settings the host resolved ride the call envelope.
	if n := src.calls(); n != 1 {
		t.Fatalf("the page made %d Plugin calls, want 1", n)
	}

	before := fileSet(t, srv.DataDir)
	req, _ := http.NewRequest(http.MethodGet, srv.URL(wantThumb), nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, onlineThumbPNG) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("thumbnail = %d %q (%d bytes), want the media host's PNG", resp.StatusCode, resp.Header.Get("Content-Type"), len(got))
	}
	if after := fileSet(t, srv.DataDir); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("fetching a thumbnail changed the files under the data dir:\nbefore %v\nafter  %v", before, after)
	}

	// An item the page never offered has no thumbnail to proxy.
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/items/nope/thumbnail", admin, nil); st != http.StatusNotFound {
		t.Fatalf("thumbnail of an unknown item = %d, want 404", st)
	}
}

// fileSet is every file path under dir, sorted. The database changes in place, so
// a thumbnail written to disk shows as a new path, not as a changed one.
func fileSet(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestTheThumbnailRouteAcceptsTheMediaCookie: a browser <img> carries the cookie
// and no Authorization header, so the thumbnail leaf accepts it; no credential at
// all is still a 401, and the cookie opens no other Online route.
func TestTheThumbnailRouteAcceptsTheMediaCookie(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := onlineServer(t)
	// onlineServer's adminToken already created the Admin; sign in again to get the cookie.
	token, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", token, nil); st != http.StatusOK {
		t.Fatalf("rows = %d, want 200", st)
	}

	thumb := onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail"
	resp, b := getBytes(t, srv, thumb, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, onlineThumbPNG) {
		t.Fatalf("thumbnail with the media cookie = %d, want 200 and the PNG", resp.StatusCode)
	}
	if resp, _ = getBytes(t, srv, thumb, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("thumbnail with no credential = %d, want 401", resp.StatusCode)
	}
	resp, _ = getBytes(t, srv, onlineBase+"/"+onlineSlug+"/rows", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rows with only the media cookie = %d, want 401: the cookie opens the media GET alone", resp.StatusCode)
	}
}

// TestAMuxedVariantIsRelayedByteForByteThroughAStreamToken: the playback start runs
// resolve() once, hands back a stream-token URL that never names the upstream, and
// the relayed bytes (whole, and by Range) are the media host's own.
func TestAMuxedVariantIsRelayedByteForByteThroughAStreamToken(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)

	status, play, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil))
	if status != http.StatusOK || play.SessionID == "" {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	if !strings.HasPrefix(play.StreamURL, "/api/v1/stream/") || strings.Contains(string(raw), media.srv.URL) ||
		strings.Contains(string(raw), "127.0.0.1") {
		t.Fatalf("playback start = %s, want a stream-token URL that never names the upstream", raw)
	}
	if len(src.resolved) != 1 {
		t.Fatalf("resolve ran %d times, want once", len(src.resolved))
	}

	// No Authorization header and no cookie: the token in the path is the credential.
	resp, body := getBytes(t, srv, play.StreamURL, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, media.body) {
		t.Fatalf("stream = %d (%d bytes), want 200 and the media host's %d bytes", resp.StatusCode, len(body), len(media.body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content type = %q, want video/mp4", ct)
	}
	media.mu.Lock()
	refer := media.refer
	media.mu.Unlock()
	if refer != "https://tube.example/" {
		t.Fatalf("the media host saw Referer %q, want the variant's header", refer)
	}

	resp, body = getBytes(t, srv, play.StreamURL, map[string]string{"Range": "bytes=1000-1999"})
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, media.body[1000:2000]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-1999/%d", len(media.body)) {
		t.Fatalf("range = %d %q (%d bytes), want 206 and bytes 1000-1999", resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
	}
}

// TestThePlaybackCeilingAppliesToAnOnlineItem: a variant over the client's resolution
// cap, or in a container it cannot play, is the same "a transcode would be required"
// refusal a Title gives — and no session is opened.
func TestThePlaybackCeilingAppliesToAnOnlineItem(t *testing.T) {
	t.Parallel()
	// A Server with no ffmpeg: what it cannot relay it cannot play. With one, the
	// same variants are encoded (TestAnOnlineItemTheClientCannotRelayIsEncodedByFFmpeg).
	srv, admin, _, _ := onlineServer(t, testharness.WithFFmpegAvailability(false))

	var env errorEnvelope
	status, _ := srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/tall/playback", admin,
		canPlayMP4(map[string]any{"maxResolution": "720p"}), &env)
	if status != http.StatusNotImplemented || env.Error.Code != "TRANSCODE_REQUIRED" {
		t.Fatalf("1080p under a 720p cap = %d %s, want 501 TRANSCODE_REQUIRED", status, env.Error.Code)
	}
	if status, _, _ = playOnline(t, srv, admin, "tall", canPlayMP4(map[string]any{"maxResolution": "1080p"})); status != http.StatusOK {
		t.Fatalf("1080p under a 1080p cap = %d, want 200", status)
	}

	env = errorEnvelope{}
	noMP4 := canPlayMP4(nil)
	noMP4["deviceProfile"].(map[string]any)["containers"] = []string{"webm"}
	status, _ = srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/"+onlineItemMP4+"/playback", admin, noMP4, &env)
	if status != http.StatusNotImplemented || env.Error.Code != "TRANSCODE_REQUIRED" {
		t.Fatalf("mp4 for a webm-only client = %d %s, want 501 TRANSCODE_REQUIRED", status, env.Error.Code)
	}
}

// TestAnOnlineSessionRecordsNoProgressAndNoWatchState: playing to the end and
// stopping writes no resume position and no watch state, ending the session revokes
// its stream token, and the session is not a Title session (it has no Edition or
// File to report progress against).
func TestAnOnlineSessionRecordsNoProgressAndNoWatchState(t *testing.T) {
	t.Parallel()
	srv, admin, _, media := onlineServer(t)
	userID := adminUserID(t, srv, admin)

	status, play, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	if resp, b := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusOK || !bytes.Equal(b, media.body) {
		t.Fatalf("stream = %d, want the whole file", resp.StatusCode)
	}
	if n := srv.CountStreamTokensForSession(play.SessionID); n != 1 {
		t.Fatalf("stream tokens for the session = %d, want 1", n)
	}

	// The player's keepalive, at the very end.
	var prog struct {
		ResumePositionMs int64 `json:"resumePositionMs"`
		Watched          bool  `json:"watched"`
	}
	st, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
		map[string]any{"positionMs": 61000, "state": "playing"}, &prog)
	if st != http.StatusOK || prog.Watched || prog.ResumePositionMs != 0 {
		t.Fatalf("progress at the end = %d %s, want 200 with nothing resumed or watched", st, body)
	}
	if st, body = srv.JSON(http.MethodDelete, "/api/v1/sessions/"+play.SessionID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("ending the session = %d; body: %s", st, body)
	}

	ws, audio, video := srv.CountWatchRowsForUser(userID)
	if ws != 0 || audio != 0 || video != 0 {
		t.Fatalf("watch rows after an Online play = %d/%d/%d, want none", ws, audio, video)
	}
	if n := srv.CountStreamTokensForSession(play.SessionID); n != 0 {
		t.Fatalf("stream tokens after the session ended = %d, want 0", n)
	}
	if resp, _ := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stream after the session ended = %d, want 404", resp.StatusCode)
	}
	if st, _ = srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
		map[string]any{"positionMs": 1}, nil); st != http.StatusNotFound {
		t.Fatalf("progress after the session ended = %d, want 404", st)
	}
}

func adminUserID(t *testing.T, srv *testharness.Server, admin string) string {
	t.Helper()
	var users usersListResp
	if st, body := srv.AuthGET("/api/v1/users", admin, &users); st != http.StatusOK {
		t.Fatalf("GET /users = %d; body: %s", st, body)
	}
	for _, u := range users.Users {
		if u.Role == "admin" {
			return u.ID
		}
	}
	t.Fatal("no Admin among the Users")
	return ""
}

// TestMembersAndRemoteCallersSeeNoOnlineSources: a Member holding no grant and a
// Remote-role caller each get an empty tile list and a 404 from rows, thumbnail and
// playback, and the Plugin is never called (online_source_access_test.go covers the
// granted Member).
func TestMembersAndRemoteCallersSeeNoOnlineSources(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	var remoteOut struct {
		ID string `json:"id"`
	}
	if st, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "peer", "role": "remote"}, &remoteOut); st != http.StatusCreated {
		t.Fatalf("creating the remote User = %d; body: %s", st, body)
	}
	remote := srv.IssueTokenForUser(remoteOut.ID, "peer-server-id")

	for name, token := range map[string]string{"member": member, "remote": remote} {
		var list onlineSourcesResp
		if st, body := srv.AuthGET(onlineBase, token, &list); st != http.StatusOK || len(list.Sources) != 0 {
			t.Fatalf("%s: GET /onlineSources = %d %s, want 200 and an empty list", name, st, body)
		}
		if !bytes.Contains(mustGet(t, srv, onlineBase, token), []byte(`"sources":[]`)) {
			t.Fatalf("%s: sources is not an empty array", name)
		}
		for _, path := range []string{
			onlineBase + "/" + onlineSlug + "/rows",
			onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail",
		} {
			if st, _ := srv.AuthGET(path, token, nil); st != http.StatusNotFound {
				t.Fatalf("%s: GET %s = %d, want 404", name, path, st)
			}
		}
		if st, _, _ := playOnline(t, srv, token, onlineItemMP4, canPlayMP4(nil)); st != http.StatusNotFound {
			t.Fatalf("%s: playback = %d, want 404", name, st)
		}
	}
	if n := src.calls(); n != 0 {
		t.Fatalf("a refused caller caused %d Plugin calls, want 0", n)
	}
}

func mustGet(t *testing.T, srv *testharness.Server, path, token string) []byte {
	t.Helper()
	_, body := srv.AuthGET(path, token, nil)
	return body
}

// TestARelayedPlayWhoseUpstreamRedirectsToALoopbackAddressIsRefused: the media host
// answers the first request with a redirect to an https address on loopback. The
// relay fetches through safefetch, so that hop is refused and nothing is served.
func TestARelayedPlayWhoseUpstreamRedirectsToALoopbackAddressIsRefused(t *testing.T) {
	t.Parallel()
	srv, admin, _, media := onlineServer(t)

	status, play, raw := playOnline(t, srv, admin, "redir", canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	resp, body := getBytes(t, srv, play.StreamURL, nil)
	if resp.StatusCode == http.StatusOK || bytes.Contains(body, media.body[:64]) {
		t.Fatalf("a redirect into loopback was followed: %d, %d bytes", resp.StatusCode, len(body))
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a refused hop", resp.StatusCode)
	}
	if n := media.hitCount("/v1.mp4"); n != 0 {
		t.Fatalf("the redirect target was fetched %d times, want 0", n)
	}
}

// TestAPluginInstalledButNotEnabledHasNoTileAndNoRouteData: switching the Plugin off
// removes its tile and every route behind it, and nothing is asked of the Plugin.
func TestAPluginInstalledButNotEnabledHasNoTileAndNoRouteData(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/disable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("disable = %d; body: %s", st, body)
	}

	var list onlineSourcesResp
	if st, body := srv.AuthGET(onlineBase, admin, &list); st != http.StatusOK || len(list.Sources) != 0 {
		t.Fatalf("GET /onlineSources = %d %s, want an empty list", st, body)
	}
	for _, path := range []string{
		onlineBase + "/" + onlineSlug + "/rows",
		onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail",
	} {
		if st, _ := srv.AuthGET(path, admin, nil); st != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, st)
		}
	}
	if st, _, _ := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil)); st != http.StatusNotFound {
		t.Fatalf("playback = %d, want 404", st)
	}
	if n := src.calls(); n != 0 {
		t.Fatalf("a disabled source was called %d times", n)
	}
}

// TestSettingsTheAdminEntersReachThePluginCall: the server-wide settings an Admin
// saves on the existing Plugins screen arrive in the call envelope, for rows and
// for resolve alike.
func TestSettingsTheAdminEntersReachThePluginCall(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	if st, body := saveDeclaredSettings(t, srv, admin, onlineSlug, map[string]any{"region": "eu-west"}); st != http.StatusOK {
		t.Fatalf("saving settings = %d; body: %s", st, body)
	}
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil); st != http.StatusOK {
		t.Fatalf("rows = %d", st)
	}
	if st, _, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK {
		t.Fatalf("playback = %d; body: %s", st, raw)
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	settings, _ := src.rowsCall[0]["settings"].(map[string]any)
	values, _ := settings["values"].(map[string]any)
	if values["region"] != "eu-west" {
		t.Fatalf("the rows call carried settings %v, want region eu-west", settings)
	}
}

// transcodeCache is every file under the Server's transcode cache directory.
func transcodeCache(t *testing.T, srv *testharness.Server) []string {
	t.Helper()
	root := filepath.Join(srv.DataDir, "transcode")
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

const onlineWhitelist = "-protocol_whitelist https,tls,tcp,crypto"

// TestAnOnlineItemTheClientCannotRelayIsEncodedByFFmpeg: a split variant, a manifest,
// and a variant above the User's resolution cap each come back as an HLS stream
// token URL; ffmpeg ran once with the whitelist on every input, the variant's
// headers and the cap; the playlist and a segment are served through the token; the
// client never learns an upstream address; and the Plugin was told the cap.
func TestAnOnlineItemTheClientCannotRelayIsEncodedByFFmpeg(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		item       string
		constraint map[string]any
		inputs     int
		want       []string
	}{
		"split":        {"split", nil, 2, []string{"-user_agent Tube/1", "Referer: https://tube.example/", "-map 0:v:0 -map 1:a:0?", "/v.mp4", "/a.m4a"}},
		"manifest":     {"manifest", nil, 1, []string{"/master.m3u8"}},
		"over-ceiling": {"tall", map[string]any{"maxResolution": "720p", "maxBitrate": 2_000_000}, 1, []string{"min(720,ih)", "-maxrate 2000000", "/v1.mp4"}},
		// The first URL passed the check; where it redirects is ffmpeg's business and
		// is not looked at (ADR-0068 decision 9, the accepted residual risk).
		"redirecting first URL": {"redir", map[string]any{"maxResolution": "480p"}, 1, []string{"/redirect.mp4"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, admin, src, media, runner := onlineServerWithRunner(t, true)
			status, play, raw := playOnline(t, srv, admin, tc.item, canPlayMP4(tc.constraint))
			if status != http.StatusOK || play.Format != "hls" {
				t.Fatalf("playback start = %d format %q; body: %s", status, play.Format, raw)
			}
			if !strings.HasPrefix(play.StreamURL, "/api/v1/stream/") || !strings.HasSuffix(play.StreamURL, "/hls/index.m3u8") ||
				strings.Contains(string(raw), media.srv.URL) || strings.Contains(string(raw), "127.0.0.1") {
				t.Fatalf("playback start = %s, want an HLS stream-token URL that never names the upstream", raw)
			}
			runs := runner.argv()
			if len(runs) != 1 {
				t.Fatalf("ffmpeg ran %d times, want once", len(runs))
			}
			args := strings.Join(runs[0], " ")
			if n := strings.Count(args, onlineWhitelist); n != tc.inputs {
				t.Errorf("%d inputs carry %q, want %d; args: %s", n, onlineWhitelist, tc.inputs, args)
			}
			for _, want := range tc.want {
				if !strings.Contains(args, want) {
					t.Errorf("ffmpeg args lack %q: %s", want, args)
				}
			}
			if strings.Contains(args, "min(") && tc.constraint == nil {
				t.Errorf("an uncapped play got a scale cap: %s", args)
			}
			if n := media.hitCount("/redirect.mp4") + media.hitCount("/v1.mp4"); n != 0 {
				t.Errorf("the Server fetched the media itself %d times; ffmpeg reads it", n)
			}

			resp, body := getBytes(t, srv, play.StreamURL, nil)
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "#EXTM3U") ||
				resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
				t.Fatalf("playlist = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			seg := strings.TrimSuffix(play.StreamURL, "index.m3u8") + "segment000.ts"
			if resp, body = getBytes(t, srv, seg, nil); resp.StatusCode != http.StatusOK || string(body) != "fake-ts-bytes" {
				t.Fatalf("segment = %d %q", resp.StatusCode, body)
			}
			for _, bad := range []string{"secret.txt", "..%2Findex.m3u8", "segment0.ts"} {
				if resp, _ = getBytes(t, srv, strings.TrimSuffix(play.StreamURL, "index.m3u8")+bad, nil); resp.StatusCode == http.StatusOK {
					t.Errorf("hls/%s was served", bad)
				}
			}
			// The progressive artifact belongs to a relayed session alone.
			if resp, _ = getBytes(t, srv, strings.TrimSuffix(play.StreamURL, "hls/index.m3u8")+"stream", nil); resp.StatusCode != http.StatusNotFound {
				t.Errorf("progressive artifact of an encoded session = %d, want 404", resp.StatusCode)
			}

			src.mu.Lock()
			defer src.mu.Unlock()
			if tc.constraint != nil && (len(src.hints) != 1 || src.hints[0]["maxHeight"] != float64(720) && tc.item == "tall") {
				t.Errorf("resolve() hints = %v, want the cap", src.hints)
			}
		})
	}
}

// TestACapabilityHintReachesResolve: the User's resolution cap is told to the Plugin
// so it can offer a variant that fits.
func TestACapabilityHintReachesResolve(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	if status, _, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(map[string]any{"maxResolution": "720p"})); status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.hints) != 1 || src.hints[0]["maxHeight"] != float64(720) {
		t.Fatalf("resolve() hints = %v, want maxHeight 720", src.hints)
	}
}

// TestAnEncodedSessionLeavesNoSegmentsInTheCacheWhenItEnds: Q2. While it lives the
// cache holds the playlist and segments; after the client stops it, they are gone;
// a relayed play never put anything there.
func TestAnEncodedSessionLeavesNoSegmentsInTheCacheWhenItEnds(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, _ := onlineServerWithRunner(t, true)

	if st, relay, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK {
		t.Fatalf("relay playback = %d; body: %s", st, raw)
	} else if got := transcodeCache(t, srv); len(got) != 0 {
		t.Fatalf("a relayed play wrote to the cache: %v (session %s)", got, relay.SessionID)
	}

	status, play, raw := playOnline(t, srv, admin, "split", canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("playback start = %d; body: %s", status, raw)
	}
	if got := transcodeCache(t, srv); len(got) != 2 {
		t.Fatalf("cache while playing = %v, want the playlist and one segment", got)
	}
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+play.SessionID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("ending the session = %d; body: %s", st, body)
	}
	if got := transcodeCache(t, srv); len(got) != 0 {
		t.Fatalf("segments left in the cache after the session ended: %v", got)
	}
	if resp, _ := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("playlist after the session ended = %d, want 404", resp.StatusCode)
	}
}

// TestTheTranscodeCapRejectsAnOnlineEncodeAsItDoesATitle: with a cap of 1 the second
// encode is 503 SERVER_BUSY with the Title shape (retryable, a suggested bitrate);
// ending the first frees the slot; a relay is never metered.
func TestTheTranscodeCapRejectsAnOnlineEncodeAsItDoesATitle(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, runner := onlineServerWithRunner(t, true, testharness.WithTranscodeCap(1))

	status, first, raw := playOnline(t, srv, admin, "split", canPlayMP4(nil))
	if status != http.StatusOK {
		t.Fatalf("first encode = %d; body: %s", status, raw)
	}
	var env errorEnvelope
	status, _ = srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/manifest/playback", admin,
		canPlayMP4(map[string]any{"maxBitrate": 4_000_000}), &env)
	if status != http.StatusServiceUnavailable || env.Error.Code != "SERVER_BUSY" ||
		env.Error.Details["retryable"] != true || env.Error.Details["suggestedMaxBitrate"] != float64(2_000_000) {
		t.Fatalf("second encode = %d %s %v, want 503 SERVER_BUSY retryable with half the asked bitrate", status, env.Error.Code, env.Error.Details)
	}
	if n := len(runner.argv()); n != 1 {
		t.Fatalf("ffmpeg ran %d times, want 1: a refused encode starts nothing", n)
	}
	if status, _, raw = playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil)); status != http.StatusOK {
		t.Fatalf("a relay at a full cap = %d, want it unmetered; body: %s", status, raw)
	}
	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("ending the first encode = %d", st)
	}
	if status, _, raw = playOnline(t, srv, admin, "manifest", canPlayMP4(nil)); status != http.StatusOK {
		t.Fatalf("encode after the slot was freed = %d; body: %s", status, raw)
	}
}

// TestTheFirstURLIsJudgedBeforeAnythingIsPlayed: a plain-http URL, a host off the
// manifest allowlist, and an allowlisted host that resolves to loopback each leave
// the source "not responding" and start no encode, on the relay path and the
// ffmpeg path alike.
func TestTheFirstURLIsJudgedBeforeAnythingIsPlayed(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, runner := onlineServerWithRunner(t, true)
	for _, item := range []string{"plainhttp", "offlist"} {
		for _, constraint := range []map[string]any{nil, {"maxResolution": "480p"}} {
			var env errorEnvelope
			status, _ := srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/"+item+"/playback", admin, canPlayMP4(constraint), &env)
			if status != http.StatusBadGateway || env.Error.Code != "SOURCE_UNAVAILABLE" {
				t.Errorf("%s (%v) = %d %s, want 502 SOURCE_UNAVAILABLE", item, constraint, status, env.Error.Code)
			}
		}
	}

	// The allowlisted media host IS loopback here, and this Server is not told to
	// overlook that.
	srv2, admin2, _, _, runner2 := onlineServerWithRunner(t, false)
	for _, item := range []string{onlineItemMP4, "split"} {
		var env errorEnvelope
		status, _ := srv2.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/"+item+"/playback", admin2, canPlayMP4(nil), &env)
		if status != http.StatusBadGateway || env.Error.Code != "SOURCE_UNAVAILABLE" {
			t.Errorf("loopback media %s = %d %s, want 502 SOURCE_UNAVAILABLE", item, status, env.Error.Code)
		}
	}
	if len(runner.argv())+len(runner2.argv()) != 0 {
		t.Fatalf("a refused first URL still started ffmpeg")
	}
}
