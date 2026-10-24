package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// A multi-part Edition is read through the concat demuxer, which maps each LATER part's
// track at the index part 1 maps. A 1080p-capped User must never be delivered a segment
// above 1080p, locally or relayed: when a later part fails the checks the video is
// encoded and scaled (directplay-ceiling-bypass). Every delivered segment is ffprobed.

// mpcPart builds one part. layout lists the video tracks as "height" with a trailing
// "d" for the default disposition, e.g. {"2160", "720d"}.
func mpcPart(t *testing.T, dir, name string, layout ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-y", "-loglevel", "error"}
	var maps, disp []string
	for i, l := range layout {
		h := strings.TrimSuffix(l, "d")
		height, _ := strconv.Atoi(h)
		args = append(args, "-f", "lavfi", "-i", "testsrc=duration=8:size="+strconv.Itoa(height*16/9&^1)+"x"+h+":rate=2")
		maps = append(maps, "-map", strconv.Itoa(i)+":v")
		if strings.HasSuffix(l, "d") {
			disp = append(disp, "-disposition:v:"+strconv.Itoa(i), "default")
		} else {
			disp = append(disp, "-disposition:v:"+strconv.Itoa(i), "0")
		}
	}
	args = append(args, "-f", "lavfi", "-i", "sine=frequency=1000:duration=8")
	args = append(args, maps...)
	args = append(args, "-map", strconv.Itoa(len(layout))+":a", "-c:v", "libx264", "-preset", "ultrafast", "-g", "2", "-pix_fmt", "yuv420p")
	args = append(args, disp...)
	args = append(args, "-c:a", "aac", "-shortest", filepath.Join(dir, name))
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot build the fixture %s: %v: %s", name, err, b)
	}
}

func mpcLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(title string, p1, p2 []string) {
		dir := filepath.Join(root, title+" (2017)")
		mpcPart(t, dir, title+" (2017) {edition-Cut} - part1.mkv", p1...)
		mpcPart(t, dir, title+" (2017) {edition-Cut} - part2.mkv", p2...)
	}
	mk("Alpha", []string{"720d"}, []string{"2160", "720d"})
	mk("Bravo", []string{"720d", "480"}, []string{"2160", "720d"})
	mk("Charlie", []string{"720d"}, []string{"2160d"})
	mk("Delta", []string{"2160", "720d"}, []string{"720d", "2160"})
	mk("Echo", []string{"2160", "720d"}, []string{"2160", "720d"})
	return root
}

func mpcProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mkv", "mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxLevel": "5.2", "maxResolution": "2160p"}},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
	}
}

// mpcSegmentHeights fetches every video segment of the decision and returns the height
// of each, probed from the delivered bytes.
// A segment that never arrives within the wait is not delivered: lenient records it and
// stops, otherwise it fails the test.
func mpcSegmentHeights(t *testing.T, srv *testharness.Server, tok string, dec decisionResp, lenient bool) []int {
	t.Helper()
	media := videoMediaPlaylistURL(t, srv, dec.StreamURL, tok)
	pl := authStream(t, srv, media, tok, "")
	body, _ := io.ReadAll(pl.Body)
	pl.Body.Close()
	segs := parseSegments(string(body))
	if len(segs) == 0 {
		t.Fatalf("no segments in the media playlist: %.300s", body)
	}
	base := media[:strings.LastIndex(media, "/")+1]
	var init []byte
	if m := bytes.Index(body, []byte(`#EXT-X-MAP:URI="`)); m >= 0 {
		rest := body[m+len(`#EXT-X-MAP:URI="`):]
		r := authStream(t, srv, base+string(rest[:bytes.IndexByte(rest, '"')]), tok, "")
		init, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}
	var heights []int
	for _, s := range segs {
		var b []byte
		var code int
		req, _ := http.NewRequest(http.MethodGet, srv.URL(base+s), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err == nil {
			b, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			code = resp.StatusCode
		}
		if err != nil || code != http.StatusOK {
			if !lenient {
				t.Fatalf("segment %s = %d (err %v)", s, code, err)
			}
			t.Logf("segment %s was not delivered (status %d, err %v)", s, code, err)
			break
		}
		p := filepath.Join(t.TempDir(), "seg.bin")
		if err := os.WriteFile(p, append(append([]byte{}, init...), b...), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_streams", p).Output()
		if err != nil {
			t.Fatalf("ffprobe %s: %v", s, err)
		}
		var pr struct {
			Streams []struct {
				CodecType string `json:"codec_type"`
				Height    int    `json:"height"`
			} `json:"streams"`
		}
		_ = json.Unmarshal(out, &pr)
		for _, st := range pr.Streams {
			if st.CodecType == "video" {
				heights = append(heights, st.Height)
			}
		}
	}
	return heights
}

func mpcCheck(t *testing.T, srv *testharness.Server, tok, title string, titles titlesListResp, wantTier string, lenient bool) {
	t.Helper()
	var dec decisionResp
	st, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+findTitle(t, titles, title)+"/playback", tok, mpcProfile(), &dec)
	if st != http.StatusOK {
		t.Fatalf("%s: play = %d: %.300s", title, st, raw)
	}
	defer srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, tok, nil, nil)
	if dec.Tier != wantTier {
		t.Errorf("%s: tier %q, want %q", title, dec.Tier, wantTier)
	}
	heights := mpcSegmentHeights(t, srv, tok, dec, lenient)
	for _, h := range heights {
		if h > 1080 {
			t.Errorf("%s: delivered segment heights %v, want none above the 1080p ceiling", title, heights)
			break
		}
	}
}

// mpcWorld serves root from a sharer and, linked to it, a home Server, each with a
// Member capped at 1080p; it returns both servers, the Members' tokens and title lists.
func mpcWorld(t *testing.T, root string) (sharer *testharness.Server, kTok string, sl titlesListResp, home *testharness.Server, hkTok string, ml titlesListResp) {
	t.Helper()
	sharer = testharness.New(t)
	sAdmin := adminToken(t, sharer)
	srcLib := createMovieLibrary(t, sharer, sAdmin, root)
	scanLib(t, sharer, sAdmin, srcLib, "")
	kid := sharer.CreateUser(sAdmin, "kid", "memberpass123", "member")
	grantLibraries(t, sharer, sAdmin, kid, srcLib)
	setPlaybackCeiling(t, sharer, sAdmin, kid, map[string]any{"maxResolution": "1080p"})
	kTok = sharer.LoginAs("kid", "memberpass123")
	sharer.AuthGET("/api/v1/libraries/"+srcLib+"/titles", sAdmin, &sl)

	remote := createRemoteUser(t, sharer, sAdmin, "other household")
	grantLibraries(t, sharer, sAdmin, remote, srcLib)
	proxy := httptest.NewServer(sharer.Handler())
	t.Cleanup(proxy.Close)
	home = testharness.New(t)
	hAdmin := adminToken(t, home)
	var l linkResp
	if status, body := postLink(t, home, hAdmin, mintInvite(t, sharer, sAdmin, remote, proxy.URL).Invite, &l); status != http.StatusCreated {
		t.Fatalf("link %d %s", status, body)
	}
	mirror := l.Libraries[0].ID
	hk := home.CreateUser(hAdmin, "hkid", "memberpass123", "member")
	grantLibraries(t, home, hAdmin, hk, mirror)
	setPlaybackCeiling(t, home, hAdmin, hk, map[string]any{"maxResolution": "1080p"})
	hkTok = home.LoginAs("hkid", "memberpass123")
	home.AuthGET("/api/v1/libraries/"+mirror+"/titles", hAdmin, &ml)
	return
}

func TestMultiPartDeliversNothingAboveTheCeiling(t *testing.T) {
	requireFFmpeg(t)
	sharer, kTok, sl, home, hkTok, ml := mpcWorld(t, mpcLibrary(t))

	// Alpha's parts lay their streams out differently (part 1: video, audio; part 2: video,
	// video, audio), which the concat demuxer cannot join by stream index whatever the
	// tier, so ffmpeg may stop early; what is delivered must still be within the ceiling.
	for _, c := range []struct {
		title, tier string
		lenient     bool
	}{
		{"Delta", "transcode", false}, {"Bravo", "transcode", false}, {"Charlie", "transcode", false},
		{"Alpha", "transcode", true}, {"Echo", "directStream", false},
	} {
		t.Run(c.title+"/local", func(t *testing.T) { mpcCheck(t, sharer, kTok, c.title, sl, c.tier, c.lenient) })
		t.Run(c.title+"/relay", func(t *testing.T) { mpcCheck(t, home, hkTok, c.title, ml, c.tier, c.lenient) })
	}
}

// mpcTSPart builds an MPEG-TS part. layout lists the streams in order: "d" is a data
// stream (the scanner drops it, so it takes an ffmpeg index no stored Stream has) and a
// number is an h264 track of that height.
func mpcTSPart(t *testing.T, dir, name string, layout ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(blob, []byte("obelo"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(blob)
	args := []string{"-nostdin", "-y", "-loglevel", "error"}
	var maps []string
	n := 0
	for _, l := range layout {
		if l == "d" {
			args = append(args, "-f", "data", "-i", blob)
		} else {
			height, _ := strconv.Atoi(l)
			args = append(args, "-f", "lavfi", "-i", "testsrc=duration=8:size="+strconv.Itoa(height*16/9&^1)+"x"+l+":rate=2")
		}
		maps = append(maps, "-map", strconv.Itoa(n))
		n++
	}
	args = append(args, "-f", "lavfi", "-i", "sine=frequency=1000:duration=8", "-map", strconv.Itoa(n))
	args = append(args, maps...)
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-g", "2", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:d", "copy", "-t", "8", filepath.Join(dir, name))
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot build the fixture %s: %v: %s", name, err, b)
	}
}

// The scanner stores the real ffmpeg Stream.Index but drops data streams, so a stream's
// position in the stored list is not its ffmpeg index. Part 1 [0 data, 1 h264 720,
// 2 h264 480, 3 aac] and part 2 [0 h264 720, 1 h264 2160, 2 aac] look alike by position
// (video, video, audio) yet ffmpeg index 1 is the 720p track in one and the 2160p in the
// other. That must be encoded; the same layout in both parts, data stream included, stays
// a remux.
func TestMultiPartShiftedDataStreamDeliversNothingAboveTheCeiling(t *testing.T) {
	requireFFmpeg(t)
	root := t.TempDir()
	mk := func(title string, p1, p2 []string) {
		dir := filepath.Join(root, title+" (2017)")
		mpcTSPart(t, dir, title+" (2017) {edition-Cut} - part1.ts", p1...)
		mpcTSPart(t, dir, title+" (2017) {edition-Cut} - part2.ts", p2...)
	}
	mk("Foxtrot", []string{"d", "720", "480"}, []string{"720", "2160"})
	mk("Golf", []string{"d", "720"}, []string{"d", "720"})
	sharer, kTok, sl, home, hkTok, ml := mpcWorld(t, root)
	for _, c := range []struct{ title, tier string }{{"Foxtrot", "transcode"}, {"Golf", "directStream"}} {
		t.Run(c.title+"/local", func(t *testing.T) { mpcCheck(t, sharer, kTok, c.title, sl, c.tier, false) })
		t.Run(c.title+"/relay", func(t *testing.T) { mpcCheck(t, home, hkTok, c.title, ml, c.tier, false) })
	}
}
