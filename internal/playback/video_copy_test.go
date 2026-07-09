package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the direct-stream-video + transcode-audio path (ADR-0024): the
// negotiation copies a client-decodable video codec (HEVC) instead of re-encoding
// it, marks the Decision VideoCopy (so the session is unmetered with an ffmpeg-owned
// playlist), and selects fMP4 delivery — pinning the pure decision the API/ffprobe
// tests exercise end to end.

// hevcSafariProfile plays HEVC up to 4K (like Safari on Apple silicon) but only
// h264 up to 1080p, decodes aac (not ac3/eac3/truehd), and caps at stereo — the
// profile the fixed web client sends.
func hevcSafariProfile() DeviceProfile {
	return DeviceProfile{
		Containers: []string{"mp4"}, // mkv NOT supported → container forces remux/transcode
		VideoCodecs: []VideoCodecSupport{
			{Codec: "h264", MaxResolution: "1080p"},
			{Codec: "hevc", MaxResolution: "2160p"},
		},
		AudioCodecs:      []string{"aac"},
		MaxAudioChannels: 2,
	}
}

func hevcFile(audioCodec string, audioChannels, audioStreams int) store.File {
	f := store.File{
		ID: "f", Path: "/movies/x.mkv", Container: "matroska,webm",
		VideoCodec: "hevc", AudioCodec: audioCodec, Width: 3840, Height: 2160, Bitrate: 60_000_000, Present: true,
		Streams: []store.Stream{
			{ID: "v", Kind: "video", Codec: "hevc", Height: 2160, IsDefault: true},
		},
	}
	for i := 0; i < audioStreams; i++ {
		f.Streams = append(f.Streams, store.Stream{
			ID: "a" + string(rune('0'+i)), Kind: "audio", Codec: audioCodec,
			Channels: audioChannels, Language: "en", IsDefault: i == 0,
		})
	}
	return f
}

func negotiateFile(profile DeviceProfile, f store.File) (Decision, *Unsupported) {
	cons := Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}
	return SelectEdition(profile, cons, []store.Edition{{ID: "e", Files: []store.File{f}}}, "")
}

// TestVideoCopyForHEVCWithUndecodableAudio: a 4K HEVC + TrueHD mkv (the Back to the
// Future case) transcodes ONLY the audio and COPIES the HEVC video — VideoCopy set,
// fMP4 selected — rather than re-encoding 4K HEVC to h264.
func TestVideoCopyForHEVCWithUndecodableAudio(t *testing.T) {
	dec, unsup := negotiateFile(hevcSafariProfile(), hevcFile("truehd", 8, 3))
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Fatalf("tier = %s, want transcode (TrueHD forces it)", dec.Tier)
	}
	// The Decision must mark VideoCopy (the Service stamps it BEFORE building the
	// job plan — transcodeJobPlan's FMP4 reads dec.UsesFMP4(), the single container
	// authority) so the session is unmetered with an ffmpeg-owned playlist.
	dec.VideoCopy = planVideoFor(hevcSafariProfile(), Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}, dec).Copy
	if !dec.VideoCopy {
		t.Errorf("dec.VideoCopy = false, want true")
	}
	if !dec.UsesFMP4() {
		t.Errorf("dec.UsesFMP4() = false, want true (copied HEVC transcode)")
	}
	// The plan copies the video and re-encodes the audio.
	plan := transcodeJobPlan(hevcSafariProfile(), Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}, dec)
	if !plan.Video.Copy {
		t.Errorf("video plan Copy = false, want true (HEVC copied, not re-encoded to h264)")
	}
	if plan.Video.Codec != "hevc" {
		t.Errorf("video plan Codec = %q, want hevc", plan.Video.Codec)
	}
	if plan.Audio.Copy {
		t.Errorf("audio plan Copy = true, want re-encode (TrueHD → AAC)")
	}
	if !plan.FMP4 {
		t.Errorf("plan FMP4 = false, want true (copied HEVC needs fMP4)")
	}
}

// TestRemuxHEVCUsesFMP4: a HEVC + AAC mkv remuxes (container-only mismatch) — the
// video is copied by the remux, so it must be delivered as fMP4, not HEVC-in-TS which
// Safari can't play.
func TestRemuxHEVCUsesFMP4(t *testing.T) {
	dec, unsup := negotiateFile(hevcSafariProfile(), hevcFile("aac", 2, 1))
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectStream {
		t.Fatalf("tier = %s, want directStream (HEVC+AAC mkv remuxes)", dec.Tier)
	}
	if !dec.UsesFMP4() {
		t.Errorf("dec.UsesFMP4() = false, want true (remuxed HEVC needs fMP4)")
	}
	// Remux is not a transcode, so VideoCopy stays false (it's unmetered/ffmpeg-owned
	// by tier already) — UsesFMP4 derives fMP4 from the directStream tier + codec.
	if dec.VideoCopy {
		t.Errorf("dec.VideoCopy = true on a remux, want false")
	}
}

// TestHevcInMpegTSClientSkipsFMP4: an hls.js client (profile HevcInMpegTS) takes a
// copied HEVC over the MPEG-TS pipeline — hls.js ≥ 1.6 demuxes HEVC-in-TS itself, and
// the TS path's dictated cuts give the exact synthesized playlists strict MSE playback
// needs (the fMP4 hls-muxer grid can drift and stall hls.js). Only Apple's native
// player (no flag) keeps fMP4.
func TestHevcInMpegTSClientSkipsFMP4(t *testing.T) {
	d := Decision{
		Tier:        TierTranscode,
		VideoCopy:   true,
		File:        store.File{VideoCodec: "hevc"},
		VideoStream: store.Stream{Codec: "hevc"},
	}
	if !d.UsesFMP4() {
		t.Fatalf("baseline: copied HEVC without the flag must be fMP4")
	}
	d.HevcInMpegTS = true
	if d.UsesFMP4() {
		t.Errorf("dec.UsesFMP4() = true for an hls.js (HevcInMpegTS) client, want false (MPEG-TS)")
	}
}

// TestH264NeverFMP4: an h264 File (copied or re-encoded) always stays MPEG-TS — fMP4
// is only for a copied non-h264 codec.
func TestH264NeverFMP4(t *testing.T) {
	// h264 + TrueHD mkv → transcode (TrueHD), video is h264 so it's COPYABLE (client
	// supports h264 within 1080p... but this is 4K h264, over the h264 ceiling → the
	// video re-encodes to 1080p h264). Either way: not fMP4.
	f := hevcFile("truehd", 8, 1)
	f.VideoCodec = "h264"
	f.Streams[0].Codec = "h264"
	dec, unsup := negotiateFile(hevcSafariProfile(), f)
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.UsesFMP4() {
		t.Errorf("dec.UsesFMP4() = true for an h264 File, want false (h264 rides MPEG-TS)")
	}
}

// videoCopyTranscodeDecision is a transcode Decision whose video is COPIED and audio
// transcoded (ADR-0024) — the shape a client-decodable video + undecodable audio
// produces (an h264 file whose AC3/DTS audio must transcode, or a HEVC-capable client
// on a 4K HEVC file).
func videoCopyTranscodeDecision(id string) Decision {
	d := transcodeDecision(id)
	d.VideoCopy = true
	return d
}

// TestVideoCopySessionIsUnmeteredAndServesFFmpegPlaylist pins the two session
// consequences of ADR-0024 that ALSO fix the "h264 file, flaky first play / breaks on
// audio switch" bug (cases 1 & 2): a video-copy transcode
//   (a) does NOT take a transcode cap slot (no video encode — unmetered like remux), and
//   (b) serves ffmpeg's OWN media playlist, not the synthesized uniform-4s one — a
//       copied stream has no forced-keyframe grid, so a synthesized playlist would
//       mislist its segments and 404/stall the player (the documented no-keyframe-grid
//       failure). Before this change a copy-video transcode wrongly did both.
func TestVideoCopySessionIsUnmeteredAndServesFFmpegPlaylist(t *testing.T) {
	root := t.TempDir()
	m := NewRemuxManager(&fakeRunner{}, root)
	m.SetTranscodeCap(1) // exactly one video-encode slot

	// A video-copy transcode session.
	s, err := m.CreateGoverned(CreateInput{UserID: "u1", TitleID: "t1"}, videoCopyTranscodeDecision("a"))
	if err != nil {
		t.Fatalf("create video-copy session: %v", err)
	}
	// (a) Unmetered: it took no slot, so a GENUINE video transcode still fits at cap=1.
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes after a video-copy session = %d, want 0 (unmetered)", got)
	}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2", TitleID: "t2"}, transcodeDecision("b")); err != nil {
		t.Errorf("a real transcode was rejected though the video-copy session took no slot: %v", err)
	}
	// (b) It serves ffmpeg's own playlist (ownsPlaylist false), like remux.
	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no runtime for video-copy session")
	}
	if rt.ownsPlaylist {
		t.Errorf("video-copy runtime ownsPlaylist = true, want false (a copied stream has no 4s keyframe grid → must serve ffmpeg's playlist)")
	}
	// Contrast: a normal (re-encoding) transcode DOES own its playlist (fresh manager,
	// so the cap above doesn't reject it).
	m2 := NewRemuxManager(&fakeRunner{}, t.TempDir())
	s2, err := m2.CreateGoverned(CreateInput{UserID: "u3", TitleID: "t3"}, transcodeDecision("c"))
	if err != nil {
		t.Fatalf("create re-encoding session: %v", err)
	}
	rt2, _ := m2.remuxRuntimeFor(s2.ID)
	if !rt2.ownsPlaylist {
		t.Errorf("re-encoding transcode ownsPlaylist = false, want true")
	}
}
