package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue audio-streams/01 integration test: the audio-Stream read path end to end.
// A fixture Movie carries four embedded audio Streams — English AAC stereo
// (default), Japanese AC3 5.1, an English DTS (client-incompatible codec)
// commentary tagged "Director's Commentary" with the comment disposition, and an
// untagged AAC mono track. After a scan, GET /titles/{id} must expose each File's
// audio Streams with a normalized language, the familiar channel layout, the
// default flag, and the ready menu label — through the real ffprobe/scanner/store/
// API stack, never internals.
//
// This is the shared multi-audio fixture the later audio slices (delivery,
// selection, memory) reuse. It lives under testdata/audio-streams/ (its own root),
// is generated with ffmpeg lazily, and the test skips if ffmpeg is absent.

const audioStreamsRootRel = "audio-streams"

const audioMovieDir = "Audio Movie (2021)"

var audioFixturesAvailable bool

func init() {
	audioFixturesAvailable = ensureAudioFixtures()
}

func requireAudioFixtures(t *testing.T) {
	t.Helper()
	if !audioFixturesAvailable {
		t.Skip("audio fixtures unavailable (ffmpeg not on PATH)")
	}
}

// ensureAudioFixtures generates the Audio Movie (2021) fixture if missing: an mkv
// muxing one video and four embedded audio Streams spanning the traits the read
// path must surface (a default, a 5.1 layout, a commentary with a title tag on a
// client-incompatible codec, and an untagged track).
func ensureAudioFixtures() bool {
	dir := filepath.Join("testdata", audioStreamsRootRel, audioMovieDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	out := filepath.Join(dir, "Audio Movie (2021).mkv")
	if fileExists(out) {
		return true
	}
	return generateMultiAudioClip(out)
}

// generateMultiAudioClip muxes a 1s clip with a video Stream and four audio
// Streams:
//
//	a:0 AAC stereo  eng  default
//	a:1 AC3  5.1    jpn
//	a:2 DTS  stereo eng  title="Director's Commentary", disposition comment
//	a:3 AAC  mono   und  (untagged language → Unknown)
//
// DTS encoding is experimental in ffmpeg, hence -strict -2. Returns false if
// ffmpeg is unavailable or fails.
func generateMultiAudioClip(out string) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=24",
		"-f", "lavfi", "-i", "aevalsrc=0.1*sin(1000*t):duration=1:channel_layout=5.1",
		"-map", "0:v", "-map", "1:a", "-map", "1:a", "-map", "1:a", "-map", "1:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a:0", "aac", "-ac:a:0", "2", "-metadata:s:a:0", "language=eng", "-disposition:a:0", "default",
		"-c:a:1", "ac3", "-ac:a:1", "6", "-metadata:s:a:1", "language=jpn", "-disposition:a:1", "0",
		"-c:a:2", "dca", "-strict", "-2", "-ac:a:2", "2",
		"-metadata:s:a:2", "language=eng", "-metadata:s:a:2", "title=Director's Commentary", "-disposition:a:2", "comment",
		"-c:a:3", "aac", "-ac:a:3", "1", "-metadata:s:a:3", "language=und",
		"-shortest", out)
	return cmd.Run() == nil
}

func audioStreamsRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", audioStreamsRootRel))
	if err != nil {
		t.Fatalf("resolving audio-streams root: %v", err)
	}
	return abs
}

// --- wire shapes (only the audio-stream surface) ----------------------------

type audioStreamResp struct {
	ID         string `json:"id"`
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	Language   string `json:"language"`
	Channels   int    `json:"channels"`
	Layout     string `json:"layout"`
	IsDefault  bool   `json:"isDefault"`
	Commentary bool   `json:"commentary"`
	Label      string `json:"label"`
}

type audioFileResp struct {
	ID           string            `json:"id"`
	AudioStreams []audioStreamResp `json:"audioStreams"`
}

type audioEditionResp struct {
	Files []audioFileResp `json:"files"`
}

type audioDetailResp struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Editions []audioEditionResp `json:"editions"`
}

func scanAudioMovie(t *testing.T) audioDetailResp {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, audioStreamsRoot(t))
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Audio Movie")

	var d audioDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	return d
}

// firstFileAudio returns the audio Streams of the fixture's single File.
func firstFileAudio(t *testing.T, d audioDetailResp) []audioStreamResp {
	t.Helper()
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 {
		t.Fatalf("expected one edition with one file, got %+v", d.Editions)
	}
	return d.Editions[0].Files[0].AudioStreams
}

func TestAudioStreamsListed(t *testing.T) {
	requireAudioFixtures(t)
	d := scanAudioMovie(t)
	streams := firstFileAudio(t, d)

	if len(streams) != 4 {
		t.Fatalf("audio stream count = %d, want 4; streams: %+v", len(streams), streams)
	}

	// Index by codec+language so we don't depend on the exact serialization order.
	byLabel := map[string]audioStreamResp{}
	var defaults int
	for _, s := range streams {
		byLabel[s.Label] = s
		if s.ID == "" {
			t.Errorf("audio stream missing id (needed as the audioStreamId selector): %+v", s)
		}
		if s.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default audio stream count = %d, want exactly 1", defaults)
	}

	// English AAC stereo, the default track → "English Stereo".
	if en, ok := byLabel["English Stereo"]; !ok {
		t.Errorf("missing 'English Stereo' track; got labels %v", labels(streams))
	} else {
		if en.Language != "en" || en.Layout != "Stereo" || en.Channels != 2 {
			t.Errorf("English Stereo: lang=%q layout=%q ch=%d, want en/Stereo/2", en.Language, en.Layout, en.Channels)
		}
		if !en.IsDefault {
			t.Errorf("English Stereo should be the default audio stream")
		}
	}

	// Japanese AC3 5.1 → normalized jpn→ja, 6 channels rendered "5.1".
	if ja, ok := byLabel["Japanese 5.1"]; !ok {
		t.Errorf("missing 'Japanese 5.1' track; got labels %v", labels(streams))
	} else if ja.Language != "ja" || ja.Layout != "5.1" || ja.Channels != 6 {
		t.Errorf("Japanese 5.1: lang=%q layout=%q ch=%d, want ja/5.1/6", ja.Language, ja.Layout, ja.Channels)
	}

	// English DTS commentary: the title tag wins the label and the commentary
	// disposition is surfaced; the client-incompatible codec is listed verbatim.
	if c, ok := byLabel["English Director's Commentary"]; !ok {
		t.Errorf("missing 'English Director's Commentary' track; got labels %v", labels(streams))
	} else {
		if c.Codec != "dts" {
			t.Errorf("commentary codec = %q, want dts (the client-incompatible fixture codec)", c.Codec)
		}
		if !c.Commentary {
			t.Errorf("commentary track: Commentary = false, want true")
		}
		if c.Language != "en" {
			t.Errorf("commentary language = %q, want en", c.Language)
		}
	}

	// Untagged language surfaces as "Unknown" (never a bogus code).
	if u, ok := byLabel["Unknown Mono"]; !ok {
		t.Errorf("missing 'Unknown Mono' track (untagged language → Unknown); got labels %v", labels(streams))
	} else if u.Language != "" || u.Layout != "Mono" {
		t.Errorf("Unknown Mono: lang=%q layout=%q, want ''/Mono", u.Language, u.Layout)
	}
}

// TestAudioStreamsRescanStable: an incremental rescan (nothing changed on disk)
// leaves the audio Stream list unchanged — a rescan refreshes rows without
// disturbing them (or the watch state, which lives in another table).
func TestAudioStreamsRescanStable(t *testing.T) {
	requireAudioFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, audioStreamsRoot(t))

	scanLib(t, srv, token, libID, "")
	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Audio Movie")

	var first audioDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &first)

	scanLib(t, srv, token, libID, "incremental")
	var second audioDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &second)

	a1, a2 := firstFileAudio(t, first), firstFileAudio(t, second)
	if len(a1) != len(a2) {
		t.Fatalf("audio stream count changed across rescan: %d → %d", len(a1), len(a2))
	}
	if len(a1) == 0 {
		t.Fatal("expected audio streams on the fixture, got none")
	}
	if labelsKey(a1) != labelsKey(a2) {
		t.Errorf("audio stream labels changed across rescan:\n first:  %v\n second: %v", labels(a1), labels(a2))
	}
}

func labels(streams []audioStreamResp) []string {
	out := make([]string, 0, len(streams))
	for _, s := range streams {
		out = append(out, s.Label)
	}
	return out
}

func labelsKey(streams []audioStreamResp) string {
	key := ""
	for _, s := range streams {
		key += s.Label + "|"
	}
	return key
}
