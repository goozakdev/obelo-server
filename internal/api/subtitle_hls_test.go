package api_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue subtitles/03 integration test: in-band TEXT subtitle delivery on the
// remux/transcode (HLS) tiers through the real API (ADR-0020, the ADR-0004
// master-playlist amendment). A wrong-container profile against an h264/aac mkv
// that carries a sidecar English subtitle → directStream → the decision's
// streamUrl is a MASTER playlist that references the video media playlist plus an
// #EXT-X-MEDIA:TYPE=SUBTITLES rendition; the rendition's media playlist lists
// segmented WebVTT, and a LATER segment stays time-mapped (carries the right cue
// + an X-TIMESTAMP-MAP). Uses real ffmpeg to build the clip + remux it, so it
// skips when ffmpeg is absent.

// generateSubtitledRemuxClip writes a ~12s h264/aac mkv (so a wrong-container
// profile REMUXES it, container-only) into a fresh library dir alongside an
// English sidecar SubRip whose cues fall in distinct 4s HLS segments (1s, 5s,
// 9s), and returns the dir. 12s → three 4s segments, so "a later segment" (index
// 2, [8s,12s)) is well-defined. Skips the test if ffmpeg can't produce it.
func generateSubtitledRemuxClip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	movieDir := filepath.Join(dir, "Subbed Movie (2003)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatalf("mkdir subbed movie dir: %v", err)
	}
	out := filepath.Join(movieDir, "Subbed Movie (2003).mkv")
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=12",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", out,
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg could not synthesize the subtitled remux clip: %v", err)
	}
	// One cue per segment window so each segment's content is distinctive.
	srt := "1\n00:00:01,000 --> 00:00:03,000\nOpening line\n\n" +
		"2\n00:00:05,000 --> 00:00:07,000\nMiddle line\n\n" +
		"3\n00:00:09,000 --> 00:00:11,000\nClosing line\n"
	if err := os.WriteFile(filepath.Join(movieDir, "Subbed Movie (2003).en.srt"), []byte(srt), 0o644); err != nil {
		t.Fatalf("writing sidecar srt: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs subbed-clip dir: %v", err)
	}
	return abs
}

// fetchText GETs an HLS artifact with the bearer token and returns its body,
// failing on a non-2xx.
func fetchText(t *testing.T, srv *testharness.Server, url, token string) string {
	t.Helper()
	resp := authStream(t, srv, url, token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestHLSInBandSubtitleMasterPlaylist(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	root := generateSubtitledRemuxClip(t)
	list := scanLibraryAt(t, srv, token, root)
	titleID := findTitle(t, list, "Subbed Movie")

	// A wrong-container profile (mp4 only) against the h264/aac mkv → directStream.
	dec := negotiateRemuxDecision(t, srv, token, titleID)

	// The streamUrl is the MASTER playlist (subtitles present), not the bare media
	// playlist.
	if !strings.HasSuffix(dec.StreamURL, "/hls/master.m3u8") {
		t.Fatalf("streamUrl = %q, want a .../hls/master.m3u8 (subtitles present)", dec.StreamURL)
	}
	// The decision lists the sidecar English text track (with an out-of-band url too
	// — that endpoint stays valid; the HLS path is additive).
	var haveText bool
	for _, s := range dec.Subtitles {
		if s.Kind == "text" && s.Language == "en" {
			haveText = true
		}
	}
	if !haveText {
		t.Fatalf("decision has no English text subtitle; got %+v", dec.Subtitles)
	}

	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]

	// The master references the video rendition + a SUBTITLES rendition.
	master := fetchText(t, srv, dec.StreamURL, token)
	if !strings.Contains(master, "#EXT-X-MEDIA:TYPE=SUBTITLES") {
		t.Fatalf("master playlist carries no SUBTITLES rendition:\n%s", master)
	}
	if !strings.Contains(master, `GROUP-ID="subs"`) || !strings.Contains(master, `SUBTITLES="subs"`) {
		t.Errorf("master playlist SUBTITLES group not wired to the video rendition:\n%s", master)
	}
	if !strings.Contains(master, "index.m3u8") {
		t.Errorf("master playlist does not reference the video media playlist:\n%s", master)
	}
	// The video media playlist is still reachable (unchanged runtime path).
	videoPL := fetchText(t, srv, base+"index.m3u8", token)
	if !strings.HasPrefix(strings.TrimSpace(videoPL), "#EXTM3U") {
		t.Errorf("video media playlist malformed:\n%s", videoPL)
	}

	// Extract the subtitle rendition's URI from the master and fetch its playlist.
	subURI := mediaAttr(t, master, "URI")
	subPL := fetchText(t, srv, base+subURI, token)
	if !strings.Contains(subPL, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(subPL, "#EXT-X-ENDLIST") {
		t.Errorf("subtitle rendition playlist is not a VOD playlist:\n%s", subPL)
	}
	segNames := parseSegments(subPL)
	if len(segNames) < 3 {
		t.Fatalf("subtitle rendition lists %d segments, want >= 3 (12s @ 4s):\n%s", len(segNames), subPL)
	}
	for _, n := range segNames {
		if !strings.HasSuffix(n, ".vtt") {
			t.Errorf("subtitle rendition segment %q is not a .vtt", n)
		}
	}

	// Segment 0 ([0,4s)) carries the opening cue + an X-TIMESTAMP-MAP.
	seg0 := fetchText(t, srv, base+segNames[0], token)
	if !strings.HasPrefix(seg0, "WEBVTT") || !strings.Contains(seg0, "X-TIMESTAMP-MAP=MPEGTS:") {
		t.Fatalf("segment 0 is not a time-mapped WebVTT:\n%s", seg0)
	}
	if !strings.Contains(seg0, "Opening line") {
		t.Errorf("segment 0 missing the 1s cue:\n%s", seg0)
	}

	// A LATER segment (index 2, [8s,12s)) stays time-mapped: it carries the closing
	// cue at its absolute 9s time under the same X-TIMESTAMP-MAP header. (Segments
	// are listed in index order; index 2 is the third — a clip a hair over 12s can
	// add an empty trailing segment, so target index 2 explicitly.)
	later := segNames[2]
	seg2 := fetchText(t, srv, base+later, token)
	if !strings.HasPrefix(seg2, "WEBVTT") || !strings.Contains(seg2, "X-TIMESTAMP-MAP=MPEGTS:") {
		t.Fatalf("later segment %q is not a time-mapped WebVTT:\n%s", later, seg2)
	}
	if !strings.Contains(seg2, "Closing line") || !strings.Contains(seg2, "00:00:09.000 --> 00:00:11.000") {
		t.Errorf("later segment missing its absolute-timed 9s cue:\n%s", seg2)
	}
	// And it does not leak the opening cue (segmentation actually partitions).
	if strings.Contains(seg2, "Opening line") {
		t.Errorf("later segment leaked the 1s cue (segmentation not applied):\n%s", seg2)
	}

	// Content types are correct for the player.
	assertContentType(t, srv, dec.StreamURL, token, "mpegurl")
	assertContentType(t, srv, base+subURI, token, "mpegurl")
	assertContentType(t, srv, base+segNames[0], token, "text/vtt")
}

// mediaAttr pulls a quoted attribute value (e.g. URI="...") off the first
// #EXT-X-MEDIA line of a master playlist.
func mediaAttr(t *testing.T, master, attr string) string {
	t.Helper()
	for _, line := range strings.Split(master, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		key := attr + `="`
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		rest := line[i+len(key):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	t.Fatalf("attribute %s not found on any EXT-X-MEDIA line:\n%s", attr, master)
	return ""
}

// assertContentType fetches url and checks its Content-Type contains want.
func assertContentType(t *testing.T, srv *testharness.Server, url, token, want string) {
	t.Helper()
	resp := authStream(t, srv, url, token, "")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, want) {
		t.Errorf("GET %s Content-Type = %q, want it to contain %q", url, ct, want)
	}
}
