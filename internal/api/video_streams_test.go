package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue selectable-video/01 integration test: the selectable-video read path end to
// end. A fixture Movie carries TWO co-packaged video Streams sharing one audio — a
// "Colour" cut (the container default) and a "Black & White" cut. After a scan, GET
// /titles/{id} must expose each File's video Streams with the ready menu label (the
// embedded title tag), the resolution, and exactly one default flag — through the
// real ffprobe/scanner/store/API stack, never internals. Mirrors audio_streams_test.go.

const videoStreamsRootRel = "video-streams"

const videoMovieDir = "Video Movie (2021)"

var videoFixturesAvailable bool

func init() {
	videoFixturesAvailable = ensureVideoFixtures()
}

func requireVideoFixtures(t *testing.T) {
	t.Helper()
	if !videoFixturesAvailable {
		t.Skip("video fixtures unavailable (ffmpeg not on PATH)")
	}
}

// ensureVideoFixtures generates the Video Movie (2021) fixture if missing: an mkv
// muxing two video Streams (different resolutions, each title-tagged) and one shared
// audio Stream.
func ensureVideoFixtures() bool {
	dir := filepath.Join("testdata", videoStreamsRootRel, videoMovieDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	out := filepath.Join(dir, "Video Movie (2021).mkv")
	if fileExists(out) {
		return true
	}
	return generateMultiVideoClip(out)
}

// generateMultiVideoClip muxes a 1s clip with two video Streams and one audio Stream:
//
//	v:0 h264 320x240  title="Colour"          default
//	v:1 h264 160x120  title="Black & White"
//	a:0 aac  stereo
//
// Returns false if ffmpeg is unavailable or fails.
func generateMultiVideoClip(out string) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
		"-map", "0:v", "-map", "1:v", "-map", "2:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-metadata:s:v:0", "title=Colour", "-disposition:v:0", "default",
		"-metadata:s:v:1", "title=Black & White", "-disposition:v:1", "0",
		"-c:a", "aac",
		"-shortest", out)
	return cmd.Run() == nil
}

func videoStreamsRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", videoStreamsRootRel))
	if err != nil {
		t.Fatalf("resolving video-streams root: %v", err)
	}
	return abs
}

// --- wire shapes (only the video-stream surface) ----------------------------

type videoStreamResp struct {
	ID        string `json:"id"`
	Index     int    `json:"index"`
	Codec     string `json:"codec"`
	Language  string `json:"language"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	IsDefault bool   `json:"isDefault"`
	Label     string `json:"label"`
}

type videoFileResp struct {
	ID           string            `json:"id"`
	VideoStreams []videoStreamResp `json:"videoStreams"`
}

type videoEditionResp struct {
	Files []videoFileResp `json:"files"`
}

type videoDetailResp struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Editions []videoEditionResp `json:"editions"`
}

func scanVideoMovie(t *testing.T) videoDetailResp {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, videoStreamsRoot(t))
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Video Movie")

	var d videoDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	return d
}

// firstFileVideo returns the video Streams of the fixture's single File.
func firstFileVideo(t *testing.T, d videoDetailResp) []videoStreamResp {
	t.Helper()
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 {
		t.Fatalf("expected one edition with one file, got %+v", d.Editions)
	}
	return d.Editions[0].Files[0].VideoStreams
}

func TestVideoStreamsListed(t *testing.T) {
	requireVideoFixtures(t)
	d := scanVideoMovie(t)
	streams := firstFileVideo(t, d)

	if len(streams) != 2 {
		t.Fatalf("video stream count = %d, want 2; streams: %+v", len(streams), streams)
	}

	byLabel := map[string]videoStreamResp{}
	var defaults int
	for _, s := range streams {
		byLabel[s.Label] = s
		if s.ID == "" {
			t.Errorf("video stream missing id (needed as the videoStreamId selector): %+v", s)
		}
		if s.Codec != "h264" {
			t.Errorf("video stream codec = %q, want h264; %+v", s.Codec, s)
		}
		if s.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default video stream count = %d, want exactly 1", defaults)
	}

	// The title-tagged "Colour" cut is the container default and the taller Stream.
	if c, ok := byLabel["Colour"]; !ok {
		t.Errorf("missing 'Colour' track; got labels %v", videoLabels(streams))
	} else {
		if !c.IsDefault {
			t.Errorf("'Colour' should be the default video stream (container disposition)")
		}
		if c.Height != 240 {
			t.Errorf("'Colour' height = %d, want 240", c.Height)
		}
	}

	// The "Black & White" cut is present, non-default, and labeled by its title tag.
	if bw, ok := byLabel["Black & White"]; !ok {
		t.Errorf("missing 'Black & White' track; got labels %v", videoLabels(streams))
	} else if bw.IsDefault {
		t.Errorf("'Black & White' must not be the default")
	} else if bw.Height != 120 {
		t.Errorf("'Black & White' height = %d, want 120", bw.Height)
	}
}

// TestVideoStreamsRescanStable: an incremental rescan (nothing changed) leaves the
// video Stream list and labels unchanged.
func TestVideoStreamsRescanStable(t *testing.T) {
	requireVideoFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, videoStreamsRoot(t))

	scanLib(t, srv, token, libID, "")
	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Video Movie")

	var first videoDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &first)

	scanLib(t, srv, token, libID, "incremental")
	var second videoDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &second)

	v1, v2 := firstFileVideo(t, first), firstFileVideo(t, second)
	if len(v1) != len(v2) {
		t.Fatalf("video stream count changed across rescan: %d → %d", len(v1), len(v2))
	}
	if len(v1) == 0 {
		t.Fatal("expected video streams on the fixture, got none")
	}
	if videoLabelsKey(v1) != videoLabelsKey(v2) {
		t.Errorf("video stream labels changed across rescan:\n first:  %v\n second: %v", videoLabels(v1), videoLabels(v2))
	}
}

func videoLabels(streams []videoStreamResp) []string {
	out := make([]string, 0, len(streams))
	for _, s := range streams {
		out = append(out, s.Label)
	}
	return out
}

func videoLabelsKey(streams []videoStreamResp) string {
	key := ""
	for _, s := range streams {
		key += s.Label + "|"
	}
	return key
}
