package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue audio-streams/05 integration tests: Remembered audio through the HTTP API
// (ADR-0023). These assert the EXTERNAL behavior the PRD names — memory that changes
// what the NEXT playback resolves — never the store internals: an explicit pick
// (audioStreamId, or an in-band pick reported through the progress surface) makes the
// next negotiation of the same Title resolve that Stream, an Episode's language pick
// bubbles up to sibling Episodes, a commentary pick stays quarantined, memory outranks
// preferredAudioLang, it re-resolves by trait after a file swap, and it is per-User.
//
// The fixture is a two-Episode dubbed Show whose Episodes each carry the same three
// audio Streams (English AAC stereo default, Japanese AC3 5.1, an English DTS
// commentary), so a pick on one Episode is meaningful on the next. It is generated
// lazily with ffmpeg under testdata/audio-memory/; the tests skip when ffmpeg is
// absent (as the other real-ffmpeg audio tests do).

const audioMemoryRootRel = "audio-memory"

var dubbedShowFixturesAvailable bool

func init() {
	dubbedShowFixturesAvailable = ensureDubbedShowFixtures()
}

func requireDubbedShowFixtures(t *testing.T) {
	t.Helper()
	if !dubbedShowFixturesAvailable {
		t.Skip("dubbed-show fixtures unavailable (ffmpeg not on PATH)")
	}
}

var dubbedShowEpisodes = []string{
	filepath.Join("Dubbed Show (2023)", "Season 01", "Dubbed Show (2023) - S01E01 - Pilot.mkv"),
	filepath.Join("Dubbed Show (2023)", "Season 01", "Dubbed Show (2023) - S01E02 - Return.mkv"),
}

// ensureDubbedShowFixtures generates the two-Episode dubbed Show if missing.
func ensureDubbedShowFixtures() bool {
	root := filepath.Join("testdata", audioMemoryRootRel)
	for _, rel := range dubbedShowEpisodes {
		out := filepath.Join(root, rel)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		if !generateDubbedEpisode(out, false) {
			return false
		}
	}
	return true
}

// generateDubbedEpisode muxes a 1s Episode clip with a video Stream and three audio
// Streams. In natural order:
//
//	a:0 AAC stereo eng default
//	a:1 AC3  5.1   jpn
//	a:2 DTS  stereo eng title="Director's Commentary", disposition comment
//
// shuffled=true swaps the first two (jpn 5.1 becomes a:0, English default a:1) so a
// re-scan re-orders the Streams and re-issues their ids — the file-swap the trait
// re-resolver must survive. DTS is experimental in ffmpeg, hence -strict -2.
func generateDubbedEpisode(out string, shuffled bool) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=24",
		"-f", "lavfi", "-i", "aevalsrc=0.1*sin(1000*t):duration=1:channel_layout=5.1",
		"-map", "0:v", "-map", "1:a", "-map", "1:a", "-map", "1:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
	}
	if shuffled {
		args = append(args, audioStreamArgs(0, "ac3", 6, "jpn", "", "0", false)...)
		args = append(args, audioStreamArgs(1, "aac", 2, "eng", "", "default", false)...)
	} else {
		args = append(args, audioStreamArgs(0, "aac", 2, "eng", "", "default", false)...)
		args = append(args, audioStreamArgs(1, "ac3", 6, "jpn", "", "0", false)...)
	}
	// The DTS English commentary always rides last (a:2) in both orders.
	args = append(args, audioStreamArgs(2, "dca", 2, "eng", "Director's Commentary", "comment", true)...)
	args = append(args, "-shortest", out)
	return exec.Command("ffmpeg", args...).Run() == nil
}

// audioStreamArgs builds the per-stream ffmpeg flags for one audio Stream at the
// given audio-relative index (codec, channels, language, optional title tag,
// disposition). DTS (dca) needs the experimental strict flag.
func audioStreamArgs(idx int, codec string, channels int, lang, title, disposition string, dts bool) []string {
	s := itoaTest(idx)
	out := []string{"-c:a:" + s, codec}
	if dts {
		out = append(out, "-strict", "-2")
	}
	out = append(out,
		"-ac:a:"+s, itoaTest(channels),
		"-metadata:s:a:"+s, "language="+lang)
	if title != "" {
		out = append(out, "-metadata:s:a:"+s, "title="+title)
	}
	out = append(out, "-disposition:a:"+s, disposition)
	return out
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- helpers ----------------------------------------------------------------

// scanDubbedShow scans the checked-in two-Episode dubbed Show and returns
// (server, admin token, libraryID).
func scanDubbedShow(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, audioMemoryRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

func audioMemoryRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", audioMemoryRootRel))
	if err != nil {
		t.Fatalf("resolving audio-memory root: %v", err)
	}
	return abs
}

// dubbedEpisodeID resolves the "Dubbed Show" Episode with the given episode number
// to its Title id via the shows → seasons → episodes browse path.
func dubbedEpisodeID(t *testing.T, srv *testharness.Server, token, libID string, epNum int) string {
	t.Helper()
	showID := findShow(t, listShows(t, srv, token, libID), "Dubbed Show")
	seasons := showSeasons(t, srv, token, showID)
	var seasonID string
	for _, s := range seasons.Seasons {
		if s.SeasonNumber == 1 {
			seasonID = s.ID
		}
	}
	if seasonID == "" {
		t.Fatalf("Dubbed Show has no Season 1; seasons: %+v", seasons.Seasons)
	}
	for _, ep := range seasonEpisodes(t, srv, token, seasonID).Episodes {
		if ep.EpisodeNumber == epNum {
			return ep.ID
		}
	}
	t.Fatalf("Dubbed Show S01E%02d not found", epNum)
	return ""
}

// audioStreamIDByLang finds a decision audio-Stream entry by its normalized ISO-639-1
// language and returns its id (the audioStreamId selector).
func audioStreamIDByLang(t *testing.T, dec decisionResp, lang string) string {
	t.Helper()
	for _, a := range dec.AudioStreams {
		if a.Language == lang {
			return a.ID
		}
	}
	t.Fatalf("no audio stream with language %q in decision; got %+v", lang, dec.AudioStreams)
	return ""
}

// resolvedCodec returns the decision's resolved audioStream codec (the audio the
// delivery carries), failing when the decision reports no resolved audio.
func resolvedCodec(t *testing.T, dec decisionResp) string {
	t.Helper()
	if dec.AudioStream == nil {
		t.Fatalf("decision reports no resolved audioStream: %+v", dec)
	}
	return dec.AudioStream.Codec
}

// --- tests ------------------------------------------------------------------

// TestRememberedAudioReplayResolvesPick: an explicit audioStreamId pick on a Title is
// Remembered, so replaying the SAME Title (no audioStreamId) resolves that Stream —
// the core memory promise. Default resolution is the English AAC stereo; after picking
// Japanese, the replay resolves ac3 (Japanese 5.1) and escalates off direct play.
func TestRememberedAudioReplayResolvesPick(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, token, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)

	// Default resolution → English AAC stereo, direct play.
	base := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, base); got != "aac" {
		t.Fatalf("default resolved codec = %q, want aac (English default)", got)
	}

	// Explicit pick of the Japanese track → written to memory.
	jpnID := audioStreamIDByLang(t, base, "ja")
	pick := negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))
	if got := resolvedCodec(t, pick); got != "ac3" {
		t.Fatalf("explicit pick resolved codec = %q, want ac3 (Japanese)", got)
	}

	// Replay with NO audioStreamId → memory resolves the Japanese track again.
	replay := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, replay); got != "ac3" {
		t.Fatalf("replay resolved codec = %q, want ac3 (remembered Japanese)", got)
	}
	if replay.Tier == "directPlay" {
		t.Errorf("replay tier = directPlay, want an escalated HLS tier (a non-default remembered pick can't direct-play)")
	}
}

// TestRememberedAudioBubblesUpToSiblingEpisode: a language pick on S01E01 becomes the
// Show's default, so S01E02 — which has no pick of its own — opens in that language.
func TestRememberedAudioBubblesUpToSiblingEpisode(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, token, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)
	ep2 := dubbedEpisodeID(t, srv, token, libID, 2)

	base1 := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	jpnID := audioStreamIDByLang(t, base1, "ja")
	negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))

	// S01E02, never picked, inherits the Show's Japanese default via the bubble-up.
	ep2dec := negotiateAudio(t, srv, token, ep2, mkvMultiAudioProfile())
	if got := resolvedCodec(t, ep2dec); got != "ac3" {
		t.Fatalf("sibling episode resolved codec = %q, want ac3 (inherited Japanese)", got)
	}
}

// TestRememberedCommentaryQuarantined: a commentary pick on one Episode stays on that
// Episode (its replay resolves the commentary) while the rest of the Show keeps the
// language pick (the sibling still resolves Japanese) — the quarantine rule.
func TestRememberedCommentaryQuarantined(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, token, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)
	ep2 := dubbedEpisodeID(t, srv, token, libID, 2)

	base1 := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	// First set the Show default to Japanese via a language pick on E1.
	jpnID := audioStreamIDByLang(t, base1, "ja")
	negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))
	// Now pick the commentary on E1 — this must NOT overwrite the Show default.
	comID := audioStreamByLabel(t, base1, "English Director's Commentary").ID
	negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), comID))

	// E1 replay resolves its own commentary pick (DTS).
	replay1 := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, replay1); got != "dts" {
		t.Fatalf("E1 replay resolved codec = %q, want dts (remembered commentary)", got)
	}
	// E2 still inherits the LANGUAGE (Japanese), not the quarantined commentary.
	ep2dec := negotiateAudio(t, srv, token, ep2, mkvMultiAudioProfile())
	if got := resolvedCodec(t, ep2dec); got != "ac3" {
		t.Fatalf("sibling resolved codec = %q, want ac3 (Show keeps Japanese, commentary quarantined)", got)
	}
}

// TestRememberedAudioOutranksPreferredLang: a remembered pick beats the client-sent
// preferredAudioLang (ADR-0023 resolution order, story 20). After remembering Japanese,
// a request that prefers English still resolves Japanese.
func TestRememberedAudioOutranksPreferredLang(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, token, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)

	base := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	jpnID := audioStreamIDByLang(t, base, "ja")
	negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))

	// Client now prefers English — memory (Japanese) must still win.
	dec := negotiateAudio(t, srv, token, ep1, withPreferredAudioLang(mkvMultiAudioProfile(), "en"))
	if got := resolvedCodec(t, dec); got != "ac3" {
		t.Fatalf("resolved codec = %q, want ac3 (memory outranks preferredAudioLang=en)", got)
	}
}

// TestRememberedAudioViaInBandProgress: an in-band pick reported through the progress
// surface (audioStreamId on POST /sessions/{id}/progress — the web player's in-band
// switch path) is Remembered, so a later play resolves it. This pins the write-back
// that has no re-negotiation of its own.
func TestRememberedAudioViaInBandProgress(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, token, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)

	dec := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	jpnID := audioStreamIDByLang(t, dec, "ja")

	// Report an in-band pick alongside a progress tick (what the player does on an
	// in-band HLS audio switch — no re-negotiation).
	status, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/progress", token,
		map[string]any{"positionMs": 1000, "state": "playing", "audioStreamId": jpnID}, nil)
	if status != http.StatusOK {
		t.Fatalf("progress+audio status = %d, want 200; body: %s", status, body)
	}

	// A later play resolves the Remembered Japanese track.
	replay := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, replay); got != "ac3" {
		t.Fatalf("replay resolved codec = %q, want ac3 (remembered via progress write-back)", got)
	}
}

// TestRememberedAudioIsPerUser: one User's pick never changes another User's
// resolution of the same Title.
func TestRememberedAudioIsPerUser(t *testing.T) {
	requireDubbedShowFixtures(t)
	srv, admin, libID := scanDubbedShow(t)
	ep1 := dubbedEpisodeID(t, srv, admin, libID, 1)

	base := negotiateAudio(t, srv, admin, ep1, mkvMultiAudioProfile())
	jpnID := audioStreamIDByLang(t, base, "ja")
	negotiateAudio(t, srv, admin, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))

	// A second User (granted the Library) has no memory → default English.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libID)
	member := srv.LoginAs("kid", "memberpass123")

	dec := negotiateAudio(t, srv, member, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, dec); got != "aac" {
		t.Fatalf("member resolved codec = %q, want aac (unaffected by admin's Japanese pick)", got)
	}
}

// TestRememberedAudioReResolvesAfterFileSwap: after the File is replaced by one whose
// audio Streams are re-ordered (and re-issued fresh ids by the rescan), the remembered
// pick re-resolves by MEANING (Japanese 5.1) — memory keyed to traits, not to a stream
// index, survives a re-rip. Uses a writable temp Library so the File can be swapped.
func TestRememberedAudioReResolvesAfterFileSwap(t *testing.T) {
	requireDubbedShowFixtures(t)
	root := t.TempDir()
	epPath := filepath.Join(root, "Dubbed Show (2023)", "Season 01", "Dubbed Show (2023) - S01E01 - Pilot.mkv")
	if err := os.MkdirAll(filepath.Dir(epPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !generateDubbedEpisode(epPath, false) {
		t.Skip("ffmpeg unavailable for regen")
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	ep1 := dubbedEpisodeID(t, srv, token, libID, 1)

	base := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	jpnID := audioStreamIDByLang(t, base, "ja")
	negotiateAudio(t, srv, token, ep1, withAudioStreamId(mkvMultiAudioProfile(), jpnID))

	// Swap in the shuffled-order File (Japanese 5.1 now the first audio Stream) and
	// re-derive its Streams with a full rescan — new stream ids, new order.
	if !generateDubbedEpisode(epPath, true) {
		t.Fatal("failed to regenerate shuffled fixture")
	}
	scanLib(t, srv, token, libID, "full")

	// The Title identity is stable across the swap, so memory still applies — and it
	// re-resolves the Japanese track by traits despite its new id/index.
	swapEp := dubbedEpisodeID(t, srv, token, libID, 1)
	if swapEp != ep1 {
		t.Fatalf("episode id changed across file swap (%q -> %q); memory keys on identity", ep1, swapEp)
	}
	replay := negotiateAudio(t, srv, token, ep1, mkvMultiAudioProfile())
	if got := resolvedCodec(t, replay); got != "ac3" {
		t.Fatalf("post-swap resolved codec = %q, want ac3 (Japanese re-resolved by traits)", got)
	}
}
