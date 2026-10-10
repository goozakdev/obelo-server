package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Safari refuses a copied-HEVC fMP4 session whose HLS master lacks an honest variant
// description. A SINGLE-audio, subtitle-less HDR10 Main 10 File is the case that used to
// get no master at all (or a bare variant): the page loaded index.m3u8 + init.mp4 and
// then never requested a segment. The master must always be served for fMP4 HEVC and
// must carry the real profile/level, the muxed audio codec, RESOLUTION, FRAME-RATE and
// VIDEO-RANGE.
func TestHDRSingleAudioFMP4ServesHonestMaster(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg has no libx265; the HDR10 fixture cannot be built")
	}

	root := t.TempDir()
	dir := filepath.Join(root, "HDR Movie (2019)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24000/1001",
		"-f", "lavfi", "-i", "aevalsrc=0.1*sin(1000*t):duration=2:channel_layout=5.1",
		"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:log-level=none",
		"-c:a", "eac3", "-ac", "6", "-shortest", filepath.Join(dir, "HDR Movie (2019).mkv"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build HDR fixture: %v: %s", err, out)
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "HDR Movie")

	// Plays HEVC + E-AC3 but not mkv: a remux (directStream) that copies video and audio.
	dec := negotiateAudio(t, srv, token, id, map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"videoCodecs":      []map[string]any{{"codec": "hevc", "maxResolution": "2160p"}},
			"audioCodecs":      []string{"aac", "eac3"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000, "maxResolution": "2160p"},
	})
	if dec.Tier != "directStream" {
		t.Fatalf("tier = %q, want directStream (a copied remux)", dec.Tier)
	}
	if !strings.HasSuffix(dec.StreamURL, "/master.m3u8") {
		t.Fatalf("streamUrl = %q, want the master playlist so VIDEO-RANGE reaches the player", dec.StreamURL)
	}

	master := fetchText(t, srv, dec.StreamURL, token)
	if !regexp.MustCompile(`CODECS="hvc1\.2\.4\.L\d+\.B0,ec-3"`).MatchString(master) {
		t.Errorf("master CODECS is not Main 10 + ec-3:\n%s", master)
	}
	for _, want := range []string{"RESOLUTION=320x240", "FRAME-RATE=23.976", "VIDEO-RANGE=PQ", "BANDWIDTH="} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %s:\n%s", want, master)
		}
	}

	// The audio is muxed in the variant: the master must not advertise an AUDIO group
	// (its audio_<id>.m3u8 has no builder and 404s), and nothing it references may 404.
	if strings.Contains(master, "TYPE=AUDIO") || strings.Contains(master, "AUDIO=") {
		t.Errorf("master advertises an AUDIO group for muxed audio:\n%s", master)
	}
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	for _, line := range strings.Split(master, "\n") {
		line = strings.TrimSpace(line)
		uri := line
		if i := strings.Index(line, `URI="`); i >= 0 {
			uri = line[i+5:]
			uri = uri[:strings.Index(uri, `"`)]
		} else if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		resp := authStream(t, srv, base+uri, token, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("master URI %q -> %d, want 200", uri, resp.StatusCode)
		}
	}
}
