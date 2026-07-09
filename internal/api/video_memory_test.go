package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue selectable-video/04 integration tests: Remembered video through the HTTP API
// (ADR-0025, ADR-0023 mirrored). These assert the EXTERNAL behavior the PRD names —
// memory that changes what the NEXT playback resolves — never the store internals: an
// explicit videoStreamId pick makes the next negotiation of the same Title resolve that
// cut, an Episode's pick bubbles up to sibling Episodes, a one-off pick on a sibling with
// its own memory is unaffected, memory re-resolves by MEANING after a file swap, an
// absent remembered cut falls back to the default with no error, and memory is per-User.
//
// The fixture is a two-Episode Show whose Episodes each carry the same two co-packaged
// video Streams sharing one audio (a "Colour" cut — the container default + taller
// Stream — and a "Black & White" cut), so a pick on one Episode is meaningful on the
// next. It is generated lazily with ffmpeg under testdata/video-memory/; the tests skip
// when ffmpeg is absent (as the other real-ffmpeg video tests do). Mirrors
// audio_memory_test.go seam-for-seam.

const videoMemoryRootRel = "video-memory"

var multiVideoShowFixturesAvailable bool

func init() {
	multiVideoShowFixturesAvailable = ensureMultiVideoShowFixtures()
}

func requireMultiVideoShowFixtures(t *testing.T) {
	t.Helper()
	if !multiVideoShowFixturesAvailable {
		t.Skip("multi-video show fixtures unavailable (ffmpeg not on PATH)")
	}
}

var multiVideoShowEpisodes = []string{
	filepath.Join("Video Show (2022)", "Season 01", "Video Show (2022) - S01E01 - Pilot.mkv"),
	filepath.Join("Video Show (2022)", "Season 01", "Video Show (2022) - S01E02 - Return.mkv"),
}

// ensureMultiVideoShowFixtures generates the two-Episode multi-video Show if missing.
func ensureMultiVideoShowFixtures() bool {
	root := filepath.Join("testdata", videoMemoryRootRel)
	for _, rel := range multiVideoShowEpisodes {
		out := filepath.Join(root, rel)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		if !generateMultiVideoEpisode(out, false) {
			return false
		}
	}
	return true
}

// generateMultiVideoEpisode muxes a 1s Episode clip with two video Streams and one audio
// Stream. In natural order:
//
//	v:0 h264 320x240 title="Colour"         default
//	v:1 h264 160x120 title="Black & White"
//	a:0 aac  stereo
//
// shuffled=true swaps the two video Streams' map order (Black & White becomes v:0, Colour
// v:1) so a re-scan re-orders the Streams and re-issues their ids — the file-swap the
// trait re-resolver must survive. Each cut keeps its title tag; Colour keeps the default
// disposition wherever it lands.
func generateMultiVideoEpisode(out string, shuffled bool) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
	}
	if shuffled {
		// Black & White (input 1) mapped first, Colour (input 0) second.
		args = append(args, "-map", "1:v", "-map", "0:v", "-map", "2:a",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-metadata:s:v:0", "title=Black & White", "-disposition:v:0", "0",
			"-metadata:s:v:1", "title=Colour", "-disposition:v:1", "default")
	} else {
		args = append(args, "-map", "0:v", "-map", "1:v", "-map", "2:a",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-metadata:s:v:0", "title=Colour", "-disposition:v:0", "default",
			"-metadata:s:v:1", "title=Black & White", "-disposition:v:1", "0")
	}
	args = append(args, "-c:a", "aac", "-shortest", out)
	return exec.Command("ffmpeg", args...).Run() == nil
}

// generateSingleVideoEpisode muxes a 1s Episode with a SINGLE Colour video Stream and one
// audio Stream — the "re-ripped without the Black & White cut" file the absent-Stream
// fallback test swaps in.
func generateSingleVideoEpisode(out string) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-metadata:s:v:0", "title=Colour", "-disposition:v:0", "default",
		"-c:a", "aac", "-shortest", out)
	return cmd.Run() == nil
}

// --- helpers ----------------------------------------------------------------

func videoMemoryRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", videoMemoryRootRel))
	if err != nil {
		t.Fatalf("resolving video-memory root: %v", err)
	}
	return abs
}

// scanVideoShow scans the checked-in two-Episode multi-video Show and returns
// (server, admin token, libraryID).
func scanVideoShow(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, videoMemoryRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

// videoShowEpisodeID resolves the "Video Show" Episode with the given episode number to
// its Title id via the shows → seasons → episodes browse path.
func videoShowEpisodeID(t *testing.T, srv *testharness.Server, token, libID string, epNum int) string {
	t.Helper()
	showID := findShow(t, listShows(t, srv, token, libID), "Video Show")
	seasons := showSeasons(t, srv, token, showID)
	var seasonID string
	for _, s := range seasons.Seasons {
		if s.SeasonNumber == 1 {
			seasonID = s.ID
		}
	}
	if seasonID == "" {
		t.Fatalf("Video Show has no Season 1; seasons: %+v", seasons.Seasons)
	}
	for _, ep := range seasonEpisodes(t, srv, token, seasonID).Episodes {
		if ep.EpisodeNumber == epNum {
			return ep.ID
		}
	}
	t.Fatalf("Video Show S01E%02d not found", epNum)
	return ""
}

// resolvedVideoHeight returns the decision's resolved videoStream height — the cut the
// delivery carries (Colour is 240, Black & White is 120), the video analogue of
// resolvedCodec.
func resolvedVideoHeight(t *testing.T, dec decisionResp) int {
	t.Helper()
	if dec.VideoStream.Height == 0 {
		t.Fatalf("decision reports no resolved videoStream height: %+v", dec)
	}
	return dec.VideoStream.Height
}

// --- tests ------------------------------------------------------------------

// TestRememberedVideoReplayResolvesPick: an explicit videoStreamId pick on a Title is
// Remembered, so replaying the SAME Title (no videoStreamId) resolves that cut — the core
// memory promise. Default resolution is the Colour cut (taller, direct play); after
// picking Black & White, the replay resolves the 120px cut and escalates off direct play.
func TestRememberedVideoReplayResolvesPick(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	srv, token, libID := scanVideoShow(t)
	ep1 := videoShowEpisodeID(t, srv, token, libID, 1)

	// Default resolution → Colour (240), direct play.
	base := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	if got := resolvedVideoHeight(t, base); got != 240 {
		t.Fatalf("default resolved height = %d, want 240 (Colour default)", got)
	}

	// Explicit pick of the Black & White cut → written to memory.
	bwID := videoStreamByLabel(t, base, "Black & White").ID
	pick := negotiateVideo(t, srv, token, ep1, withVideoStreamId(mkvVideoProfile(), bwID))
	if got := resolvedVideoHeight(t, pick); got != 120 {
		t.Fatalf("explicit pick resolved height = %d, want 120 (Black & White)", got)
	}

	// Replay with NO videoStreamId → memory resolves the Black & White cut again.
	replay := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	if got := resolvedVideoHeight(t, replay); got != 120 {
		t.Fatalf("replay resolved height = %d, want 120 (remembered Black & White)", got)
	}
	if replay.Tier == "directPlay" {
		t.Errorf("replay tier = directPlay, want an escalated HLS tier (a non-default remembered pick can't direct-play)")
	}
}

// TestRememberedVideoBubblesUpToSiblingEpisode: a cut pick on S01E01 becomes the Show's
// default, so S01E02 — which has no pick of its own — opens in that cut.
func TestRememberedVideoBubblesUpToSiblingEpisode(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	srv, token, libID := scanVideoShow(t)
	ep1 := videoShowEpisodeID(t, srv, token, libID, 1)
	ep2 := videoShowEpisodeID(t, srv, token, libID, 2)

	base1 := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	bwID := videoStreamByLabel(t, base1, "Black & White").ID
	negotiateVideo(t, srv, token, ep1, withVideoStreamId(mkvVideoProfile(), bwID))

	// S01E02, never picked, inherits the Show's Black & White default via the bubble-up.
	ep2dec := negotiateVideo(t, srv, token, ep2, mkvVideoProfile())
	if got := resolvedVideoHeight(t, ep2dec); got != 120 {
		t.Fatalf("sibling episode resolved height = %d, want 120 (inherited Black & White)", got)
	}
}

// TestRememberedVideoSiblingOwnPickUnaffected: a one-off pick that sets a sibling's own
// Title memory is not overwritten by a later pick on a different Episode — the Title level
// wins over the Show bubble-up. E2 is pinned to Colour, then E1 is set to Black & White
// (which becomes the Show default); E2 still resolves its own Colour pick.
func TestRememberedVideoSiblingOwnPickUnaffected(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	srv, token, libID := scanVideoShow(t)
	ep1 := videoShowEpisodeID(t, srv, token, libID, 1)
	ep2 := videoShowEpisodeID(t, srv, token, libID, 2)

	// Stream ids are per-File, so each Episode's pick must name its OWN File's Stream.
	base1 := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	bwID1 := videoStreamByLabel(t, base1, "Black & White").ID
	base2 := negotiateVideo(t, srv, token, ep2, mkvVideoProfile())
	colID2 := videoStreamByLabel(t, base2, "Colour").ID

	// E2 gets its OWN explicit Colour pick (its Title memory).
	negotiateVideo(t, srv, token, ep2, withVideoStreamId(mkvVideoProfile(), colID2))
	// E1 is set to Black & White → that becomes the Show's bubble-up default.
	negotiateVideo(t, srv, token, ep1, withVideoStreamId(mkvVideoProfile(), bwID1))

	// E2 replay resolves its OWN Colour pick, not the Show's Black & White default.
	replay2 := negotiateVideo(t, srv, token, ep2, mkvVideoProfile())
	if got := resolvedVideoHeight(t, replay2); got != 240 {
		t.Fatalf("sibling with own pick resolved height = %d, want 240 (own Colour pick wins over Show default)", got)
	}
}

// TestRememberedVideoIsPerUser: one User's pick never changes another User's resolution of
// the same Title.
func TestRememberedVideoIsPerUser(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	srv, admin, libID := scanVideoShow(t)
	ep1 := videoShowEpisodeID(t, srv, admin, libID, 1)

	base := negotiateVideo(t, srv, admin, ep1, mkvVideoProfile())
	bwID := videoStreamByLabel(t, base, "Black & White").ID
	negotiateVideo(t, srv, admin, ep1, withVideoStreamId(mkvVideoProfile(), bwID))

	// A second User (granted the Library) has no memory → default Colour.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libID)
	member := srv.LoginAs("kid", "memberpass123")

	dec := negotiateVideo(t, srv, member, ep1, mkvVideoProfile())
	if got := resolvedVideoHeight(t, dec); got != 240 {
		t.Fatalf("member resolved height = %d, want 240 (unaffected by admin's Black & White pick)", got)
	}
}

// TestRememberedVideoReResolvesAfterFileSwap: after the File is replaced by one whose
// video Streams are re-ordered (and re-issued fresh ids by the rescan), the remembered
// pick re-resolves by MEANING (the Black & White cut's title tag + resolution) — memory
// keyed to traits, not to a stream index, survives a re-rip. Uses a writable temp Library
// so the File can be swapped.
func TestRememberedVideoReResolvesAfterFileSwap(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	root := t.TempDir()
	epPath := filepath.Join(root, "Video Show (2022)", "Season 01", "Video Show (2022) - S01E01 - Pilot.mkv")
	if err := os.MkdirAll(filepath.Dir(epPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !generateMultiVideoEpisode(epPath, false) {
		t.Skip("ffmpeg unavailable for regen")
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	ep1 := videoShowEpisodeID(t, srv, token, libID, 1)

	base := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	bwID := videoStreamByLabel(t, base, "Black & White").ID
	negotiateVideo(t, srv, token, ep1, withVideoStreamId(mkvVideoProfile(), bwID))

	// Swap in the shuffled-order File (Black & White now the first video Stream) and
	// re-derive its Streams with a full rescan — new stream ids, new order.
	if !generateMultiVideoEpisode(epPath, true) {
		t.Fatal("failed to regenerate shuffled fixture")
	}
	scanLib(t, srv, token, libID, "full")

	// The Title identity is stable across the swap, so memory still applies — and it
	// re-resolves the Black & White cut by traits despite its new id/index.
	swapEp := videoShowEpisodeID(t, srv, token, libID, 1)
	if swapEp != ep1 {
		t.Fatalf("episode id changed across file swap (%q -> %q); memory keys on identity", ep1, swapEp)
	}
	replay := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	if got := resolvedVideoHeight(t, replay); got != 120 {
		t.Fatalf("post-swap resolved height = %d, want 120 (Black & White re-resolved by traits)", got)
	}
}

// TestRememberedVideoAbsentStreamFallsBack: when the remembered cut's meaning matches no
// Stream in the current File (a re-rip dropped the Black & White cut, leaving only
// Colour), negotiation falls back to the default with no error — the graceful-degradation
// promise (story 14, ADR-0014).
func TestRememberedVideoAbsentStreamFallsBack(t *testing.T) {
	requireMultiVideoShowFixtures(t)
	root := t.TempDir()
	epPath := filepath.Join(root, "Video Show (2022)", "Season 01", "Video Show (2022) - S01E01 - Pilot.mkv")
	if err := os.MkdirAll(filepath.Dir(epPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !generateMultiVideoEpisode(epPath, false) {
		t.Skip("ffmpeg unavailable for regen")
	}

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	ep1 := videoShowEpisodeID(t, srv, token, libID, 1)

	base := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	bwID := videoStreamByLabel(t, base, "Black & White").ID
	negotiateVideo(t, srv, token, ep1, withVideoStreamId(mkvVideoProfile(), bwID))

	// Re-rip WITHOUT the Black & White cut — only the Colour cut survives.
	if !generateSingleVideoEpisode(epPath) {
		t.Fatal("failed to regenerate single-video fixture")
	}
	scanLib(t, srv, token, libID, "full")

	// The remembered Black & White cut no longer exists → fall back to the default (Colour,
	// 240) with a clean 200, direct play (nothing forced the escalation).
	replay := negotiateVideo(t, srv, token, ep1, mkvVideoProfile())
	if got := resolvedVideoHeight(t, replay); got != 240 {
		t.Fatalf("post-drop resolved height = %d, want 240 (fell back to Colour default)", got)
	}
	if replay.Tier != "directPlay" {
		t.Errorf("fallback tier = %q, want directPlay (no remembered cut to escalate for)", replay.Tier)
	}
}
