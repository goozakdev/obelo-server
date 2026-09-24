package api_test

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for Local lyrics: a Music Library whose tracks carry their
// words in each of the places the Scanner reads them — a sidecar .lrc, an
// embedded ID3 SYLT (synced) or USLT (plain) frame, a Vorbis LYRICS comment —
// and one that carries none, read back through GET /titles/{id}/lyrics.
//
// The ID3 tags are written by hand (lyricsID3Tag) because ffmpeg writes a
// "lyrics-xxx" key as a TXXX frame, never as USLT or SYLT, so an ffmpeg-tagged
// file cannot stand in for what a real tagger produces.

type lyricLineResp struct {
	StartMs int64  `json:"startMs"`
	Text    string `json:"text"`
}

type lyricsBodyResp struct {
	Kind   string          `json:"kind"`
	Source string          `json:"source"`
	Lines  []lyricLineResp `json:"lines"`
	Text   string          `json:"text"`
}

type lyricsResp struct {
	Lyrics *lyricsBodyResp `json:"lyrics"`
}

// The lyrics every fixture carries, in the shape each source spells them.
const sidecarLRC = "[ar:Lyric Band]\n[ti:From The Sidecar]\n[00:01.50]First sidecar line\n[00:03.25]Second sidecar line\n"

var syltLines = []lyricLineResp{
	{StartMs: 500, Text: "First embedded line"},
	{StartMs: 2750, Text: "Second embedded line"},
}

const usltText = "Plain words from USLT\nA second plain line"

const vorbisLyricsText = "Plain words from a LYRICS comment"

// lyricsFixture writes the Music Library root under a temp dir and returns it.
// It skips when ffmpeg is unavailable, as every synthesized-fixture suite does.
func lyricsFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	root := t.TempDir()
	album := filepath.Join(root, "Lyric Band", "Words (2024)")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatalf("creating album folder: %v", err)
	}
	tags := func(title, track string) map[string]string {
		return map[string]string{
			"artist": "Lyric Band", "album_artist": "Lyric Band", "album": "Words",
			"title": title, "track": track, "date": "2024",
		}
	}

	// 01: a FLAC with a sidecar .lrc beside it.
	encodeClip(t, filepath.Join(album, "01 - From The Sidecar.flac"), tags("From The Sidecar", "1"))
	if err := os.WriteFile(filepath.Join(album, "01 - From The Sidecar.lrc"), []byte(sidecarLRC), 0o644); err != nil {
		t.Fatalf("writing sidecar lrc: %v", err)
	}

	// 02: an MP3 whose only lyrics are an embedded SYLT frame.
	writeID3Clip(t, filepath.Join(album, "02 - Embedded Synced.mp3"), tags("Embedded Synced", "2"),
		syltFrame(syltLines))

	// 03: an MP3 whose only lyrics are an embedded USLT frame.
	writeID3Clip(t, filepath.Join(album, "03 - Embedded Plain.mp3"), tags("Embedded Plain", "3"),
		usltFrame(usltText))

	// 04: a FLAC whose only lyrics are a Vorbis LYRICS comment.
	vorbis := tags("Vorbis Plain", "4")
	vorbis["LYRICS"] = vorbisLyricsText
	encodeClip(t, filepath.Join(album, "04 - Vorbis Plain.flac"), vorbis)

	// 05: no lyrics of any kind.
	encodeClip(t, filepath.Join(album, "05 - Instrumental.flac"), tags("Instrumental", "5"))
	return root
}

// encodeClip synthesizes one second of tone at out, tagged through ffmpeg.
func encodeClip(t *testing.T, out string, tags map[string]string) {
	t.Helper()
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1"}
	for k, v := range tags {
		args = append(args, "-metadata", k+"="+v)
	}
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v: %s", out, err, b)
	}
}

// writeID3Clip synthesizes an untagged MP3 and prepends a hand-built ID3v2.3 tag
// carrying the identity tags plus the extra frames.
func writeID3Clip(t *testing.T, out string, tags map[string]string, extra ...[]byte) {
	t.Helper()
	raw := out + ".raw.mp3"
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-write_id3v2", "0", raw}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v: %s", raw, err, b)
	}
	audio, err := os.ReadFile(raw)
	if err != nil {
		t.Fatalf("reading raw mp3: %v", err)
	}
	if err := os.Remove(raw); err != nil {
		t.Fatalf("removing raw mp3: %v", err)
	}
	frames := [][]byte{
		textFrame("TPE1", tags["artist"]),
		textFrame("TPE2", tags["album_artist"]),
		textFrame("TALB", tags["album"]),
		textFrame("TIT2", tags["title"]),
		textFrame("TRCK", tags["track"]),
	}
	frames = append(frames, extra...)
	if err := os.WriteFile(out, append(lyricsID3Tag(frames...), audio...), 0o644); err != nil {
		t.Fatalf("writing %s: %v", out, err)
	}
}

// lyricsID3Tag wraps frames in an ID3v2.3 header (syncsafe tag size).
func lyricsID3Tag(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	n := len(body)
	h := []byte{'I', 'D', '3', 3, 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(h, body...)
}

// id3Frame is one ID3v2.3 frame: id, big-endian size, no flags.
func id3Frame(id string, body []byte) []byte {
	f := []byte(id)
	f = binary.BigEndian.AppendUint32(f, uint32(len(body)))
	f = append(f, 0, 0)
	return append(f, body...)
}

// textFrame is a UTF-8 (encoding 3) text frame. ID3v2.3 predates UTF-8, but
// taggers write it and ffprobe reads it; the lyrics frames below use Latin-1.
func textFrame(id, text string) []byte {
	return id3Frame(id, append([]byte{3}, text...))
}

// usltFrame is a Latin-1 USLT frame in English with an empty descriptor.
func usltFrame(text string) []byte {
	body := []byte{0, 'e', 'n', 'g', 0}
	return id3Frame("USLT", append(body, text...))
}

// syltFrame is a Latin-1 SYLT frame in English, timed in milliseconds, content
// type "lyrics", with one entry per line.
func syltFrame(lines []lyricLineResp) []byte {
	body := []byte{0, 'e', 'n', 'g', 2, 1, 0}
	for _, l := range lines {
		body = append(body, l.Text...)
		body = append(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(l.StartMs))
	}
	return id3Frame("SYLT", body)
}

// lyricsServer scans the fixture library and returns the server, an Admin token,
// the Library, and the track ids keyed by title.
func lyricsServer(t *testing.T) (*testharness.Server, string, string, map[string]string) {
	t.Helper()
	return lyricsServerAt(t, lyricsFixture(t))
}

// lyricsServerAt is lyricsServer over a fixture library already written at root.
func lyricsServerAt(t *testing.T, root string) (*testharness.Server, string, string, map[string]string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	artistID := findArtist(t, listArtists(t, srv, token, libID), "Lyric Band")
	albums := artistAlbums(t, srv, token, artistID)
	if len(albums.Albums) != 1 {
		t.Fatalf("albums = %+v, want the one fixture album", albums.Albums)
	}
	tracks := map[string]string{}
	for _, tr := range albumTracks(t, srv, token, albums.Albums[0].ID).Tracks {
		tracks[tr.Title] = tr.ID
	}
	if len(tracks) != 5 {
		t.Fatalf("tracks = %v, want all five fixtures filed", tracks)
	}
	return srv, token, libID, tracks
}

func readLyrics(t *testing.T, srv *testharness.Server, token, titleID string) lyricsResp {
	t.Helper()
	var resp lyricsResp
	status, body := srv.AuthGET("/api/v1/titles/"+titleID+"/lyrics", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET lyrics status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(string(body), `"lyrics":`) {
		t.Fatalf("GET lyrics body = %s, want a lyrics key", body)
	}
	return resp
}

func assertSynced(t *testing.T, got lyricsResp, want []lyricLineResp) {
	t.Helper()
	if got.Lyrics == nil {
		t.Fatalf("lyrics = null, want Synced lyrics")
	}
	if got.Lyrics.Kind != "synced" || got.Lyrics.Source != "local" {
		t.Fatalf("lyrics kind/source = %q/%q, want synced/local", got.Lyrics.Kind, got.Lyrics.Source)
	}
	if len(got.Lyrics.Lines) != len(want) {
		t.Fatalf("lines = %+v, want %+v", got.Lyrics.Lines, want)
	}
	for i := range want {
		if got.Lyrics.Lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, got.Lyrics.Lines[i], want[i])
		}
	}
	if got.Lyrics.Text != "" {
		t.Fatalf("synced lyrics carry text %q, want none", got.Lyrics.Text)
	}
}

func assertPlain(t *testing.T, got lyricsResp, want string) {
	t.Helper()
	if got.Lyrics == nil {
		t.Fatalf("lyrics = null, want Plain lyrics")
	}
	if got.Lyrics.Kind != "plain" || got.Lyrics.Source != "local" {
		t.Fatalf("lyrics kind/source = %q/%q, want plain/local", got.Lyrics.Kind, got.Lyrics.Source)
	}
	if got.Lyrics.Text != want {
		t.Fatalf("text = %q, want %q", got.Lyrics.Text, want)
	}
	if len(got.Lyrics.Lines) != 0 {
		t.Fatalf("plain lyrics carry lines %+v, want none", got.Lyrics.Lines)
	}
}

// TestASidecarLRCIsStoredAsSyncedLyrics: the .lrc beside a track is its Synced
// lyrics, each line at its own time; the [ar:]/[ti:] header tags are not lines.
func TestASidecarLRCIsStoredAsSyncedLyrics(t *testing.T) {
	srv, token, _, tracks := lyricsServer(t)
	assertSynced(t, readLyrics(t, srv, token, tracks["From The Sidecar"]), []lyricLineResp{
		{StartMs: 1500, Text: "First sidecar line"},
		{StartMs: 3250, Text: "Second sidecar line"},
	})
}

// TestAnEmbeddedSYLTIsStoredAsSyncedLyrics: with no sidecar, an embedded SYLT
// frame gives the track Synced lyrics exactly as a sidecar would.
func TestAnEmbeddedSYLTIsStoredAsSyncedLyrics(t *testing.T) {
	srv, token, _, tracks := lyricsServer(t)
	assertSynced(t, readLyrics(t, srv, token, tracks["Embedded Synced"]), syltLines)
}

// TestAnEmbeddedUSLTIsStoredAsPlainLyrics: an embedded USLT frame is Plain
// lyrics — the text as written, untimed.
func TestAnEmbeddedUSLTIsStoredAsPlainLyrics(t *testing.T) {
	srv, token, _, tracks := lyricsServer(t)
	assertPlain(t, readLyrics(t, srv, token, tracks["Embedded Plain"]), usltText)
}

// TestAnEmbeddedLYRICSTagIsStoredAsPlainLyrics: a LYRICS comment is Plain lyrics.
func TestAnEmbeddedLYRICSTagIsStoredAsPlainLyrics(t *testing.T) {
	srv, token, _, tracks := lyricsServer(t)
	assertPlain(t, readLyrics(t, srv, token, tracks["Vorbis Plain"]), vorbisLyricsText)
}

// TestATrackWithNoLocalLyricsAnswersNone: a track with no lyrics anywhere answers
// 200 with "lyrics": null — an empty state, never an error.
func TestATrackWithNoLocalLyricsAnswersNone(t *testing.T) {
	srv, token, _, tracks := lyricsServer(t)
	if got := readLyrics(t, srv, token, tracks["Instrumental"]); got.Lyrics != nil {
		t.Fatalf("lyrics = %+v, want null", got.Lyrics)
	}
}

// TestLyricsAreReadableByAMemberAndHiddenOutsideTheirLibraries: a Member granted
// the Library reads the same lyrics an Admin does; one not granted it gets the
// existence-hiding 404, as for every other read of the Title.
func TestLyricsAreReadableByAMemberAndHiddenOutsideTheirLibraries(t *testing.T) {
	srv, admin, libID, tracks := lyricsServer(t)
	id := tracks["Embedded Plain"]

	grantedID := srv.CreateUser(admin, "listener", "memberpass123", "member")
	grantLibraries(t, srv, admin, grantedID, libID)
	granted := srv.LoginAs("listener", "memberpass123")
	assertPlain(t, readLyrics(t, srv, granted, id), usltText)

	srv.CreateUser(admin, "outsider", "memberpass123", "member")
	outsider := srv.LoginAs("outsider", "memberpass123")
	if status, body := srv.AuthGET("/api/v1/titles/"+id+"/lyrics", outsider, nil); status != http.StatusNotFound {
		t.Fatalf("ungranted Member GET lyrics status = %d, want 404; body: %s", status, body)
	}
}

// TestARescanKeepsOneCopyOfLocalLyrics: scanning again rewrites the Local lyrics
// rather than adding a second copy or dropping them.
func TestARescanKeepsOneCopyOfLocalLyrics(t *testing.T) {
	srv, token, libID, tracks := lyricsServer(t)
	scanLib(t, srv, token, libID, "full")
	assertSynced(t, readLyrics(t, srv, token, tracks["Embedded Synced"]), syltLines)
	assertPlain(t, readLyrics(t, srv, token, tracks["Embedded Plain"]), usltText)
}

// TestLyricsRemovedFromDiskAreClearedOnRescan: when the sidecar a track's lyrics
// came from is deleted, the next scan clears them rather than keeping the stale
// copy.
func TestLyricsRemovedFromDiskAreClearedOnRescan(t *testing.T) {
	root := lyricsFixture(t)
	srv, token, libID, tracks := lyricsServerAt(t, root)
	id := tracks["From The Sidecar"]
	if got := readLyrics(t, srv, token, id); got.Lyrics == nil {
		t.Fatalf("lyrics = null before the sidecar is removed, want Synced lyrics")
	}

	if err := os.Remove(filepath.Join(root, "Lyric Band", "Words (2024)", "01 - From The Sidecar.lrc")); err != nil {
		t.Fatalf("removing sidecar lrc: %v", err)
	}
	scanLib(t, srv, token, libID, "full")
	if got := readLyrics(t, srv, token, id); got.Lyrics != nil {
		t.Fatalf("lyrics = %+v after the sidecar was removed, want null", got.Lyrics)
	}
}
