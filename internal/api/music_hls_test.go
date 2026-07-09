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

// Regression for the "music never plays on Safari — segments 404" bug. An
// audio-only Track that must transcode (FLAC/MP3 → AAC) is delivered as HLS. The
// transcode tier used a SERVER-OWNED synthesized playlist plus seek-realignment,
// machinery built for VIDEO (exact keyframe-aligned 4s segments). Audio segments
// are not keyframe-aligned, and a native-HLS client (Safari) requests segments
// out-of-order / in parallel, which made the realignment logic kill and restart
// ffmpeg repeatedly — leaving in-between segments unproduced, so the player got
// 404s and hung. The fix delivers audio-only transcode like the remux tier: serve
// ffmpeg's OWN playlist with NO realignment (audio encodes far faster than
// realtime, so the whole track is produced and every listed segment is served).

// generateLongAudioTrack writes a multi-second tagged audio file into a fresh
// Music library dir and returns the dir. The duration spans several 4s HLS
// segments (the silent-music bug only shows with more than one segment). Skips if
// ffmpeg cannot produce it.
func generateLongAudioTrack(t *testing.T, seconds string, codec string, ext string) string {
	t.Helper()
	dir := t.TempDir()
	albumDir := filepath.Join(dir, "Test Artist", "Long Album (2020)")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatalf("mkdir long-audio album dir: %v", err)
	}
	out := filepath.Join(albumDir, "01 - Long Track."+ext)
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds,
		"-c:a", codec,
		"-metadata", "artist=Test Artist",
		"-metadata", "album_artist=Test Artist",
		"-metadata", "album=Long Album",
		"-metadata", "title=Long Track",
		"-metadata", "track=1",
		"-metadata", "date=2020",
		out,
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg could not synthesize the long audio track: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs long-audio dir: %v", err)
	}
	return abs
}

// TestMusicTranscodeServesFfmpegPlaylistAndAllSegments: a multi-segment audio-only
// transcode is delivered with ffmpeg's OWN playlist (accurate, variable segment
// durations) and EVERY listed segment is fetchable. Before the fix the transcode
// tier served a SYNTHESIZED playlist of uniform 4.000s segments plus a
// realignment path built for video; this asserts the audio path now uses the
// remux-style delivery (ffmpeg's real playlist), which a native-HLS client (Safari)
// plays without the segment 404s the synthesized/realign path produced.
func TestMusicTranscodeServesFfmpegPlaylistAndAllSegments(t *testing.T) {
	requireFFmpeg(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)

	// A ~21.3s FLAC (won't divide evenly by 4 → a SHORT trailing segment), which
	// must transcode to AAC under an aac-only profile — the exact audio-only HLS path.
	root := generateLongAudioTrack(t, "21.3", "flac", "flac")
	libID := createMusicLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	artists := listArtists(t, srv, token, libID)
	artistID := findArtist(t, artists, "Test Artist")
	albums := artistAlbums(t, srv, token, artistID)
	if len(albums.Albums) == 0 {
		t.Fatalf("no albums scanned; have: %+v", albums.Albums)
	}
	tracks := albumTracks(t, srv, token, albums.Albums[0].ID)
	if len(tracks.Tracks) == 0 {
		t.Fatalf("no tracks scanned")
	}
	trackID := tracks.Tracks[0].ID

	// aac-only profile → the FLAC must transcode (audio-only AAC HLS).
	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4"},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var dec decisionResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+trackID+"/playback", token, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, raw)
	}
	if dec.Tier != "transcode" {
		t.Fatalf("tier = %q, want transcode; body: %s", dec.Tier, raw)
	}

	// Fetch the playlist.
	pl := authStream(t, srv, dec.StreamURL, token, "")
	manifest := string(readAllClose(t, pl))
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", pl.StatusCode)
	}
	segments := parseSegments(manifest)
	if len(segments) < 3 {
		t.Fatalf("playlist lists %d segments, want >= 3 for a multi-segment audio test:\n%s", len(segments), manifest)
	}

	// It must be ffmpeg's REAL playlist, not a synthesized uniform-4s one: audio
	// segments are not keyframe-aligned, so the real EXTINF durations vary (and the
	// trailing one is short). A playlist whose every EXTINF is exactly 4.000000 is
	// the old synthesized one — the bug.
	durs := parseExtinfDurations(manifest)
	if len(durs) == 0 {
		t.Fatalf("no #EXTINF durations in playlist:\n%s", manifest)
	}
	allFour := true
	for _, d := range durs {
		if d != "4.000000" {
			allFour = false
			break
		}
	}
	if allFour {
		t.Errorf("playlist has uniform 4.000000 segments (the synthesized playlist); want ffmpeg's real, variable durations:\n%s", manifest)
	}

	// Every listed segment is fetchable — out-of-order, mirroring a native-HLS
	// client's fan-out (no segment 404s).
	base := dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]
	for i := len(segments) - 1; i >= 0; i-- { // request last → first
		resp := authStream(t, srv, base+segments[i], token, "")
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusOK {
			t.Errorf("segment %s status = %d, want 200 (every listed segment must be served)", segments[i], code)
		}
	}
}

// parseExtinfDurations returns the duration field of each #EXTINF line, e.g.
// "4.017056" from "#EXTINF:4.017056,". Used to distinguish ffmpeg's real,
// variable-duration playlist from a synthesized uniform-4s one.
func parseExtinfDurations(manifest string) []string {
	var out []string
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		v := strings.TrimPrefix(line, "#EXTINF:")
		v = strings.TrimSuffix(strings.TrimSpace(v), ",")
		out = append(out, strings.TrimSuffix(v, ","))
	}
	return out
}

// readAllClose reads and closes an HTTP response body, failing on a read error.
func readAllClose(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return b
}
