package playback

import (
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/transcode"
)

// Unit tests for the lean single-track remux (PRD remux-selected): the client
// forces a copy-only directStream — one video + one audio Stream — on a File that
// would otherwise directPlay, so a many-track container is trimmed to the bytes the
// viewer uses without paying for a transcode. They pin the pure escalation rule
// (applyRemuxSelectedOnly), the demux suppression, the forced single-track FFmpeg
// map, and the end-to-end Service.Negotiate outcome + cap-exemption.

// multiAudioMKVFile is an otherwise-directPlay mkv carrying one video + TWO aac
// audio Streams (English default + Japanese), the shape the trim is about: a
// remux-selected session keeps exactly one of them.
func multiAudioMKVFile() store.File {
	f := mkvFile("h264", "aac", 1080, 6_000_000)
	f.ID = "f1"
	f.DurationMs = 60_000
	f.Streams = []store.Stream{
		{ID: "v1", Kind: "video", Codec: "h264", Height: 1080, IsDefault: true},
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		{ID: "a-ja", Kind: "audio", Codec: "aac", Channels: 6, Language: "jpn"},
	}
	return f
}

func directPlayDecision() Decision {
	f := multiAudioMKVFile()
	return Decision{
		Tier:             TierDirectPlay,
		Edition:          store.Edition{ID: "e1", Files: []store.File{f}},
		File:             f,
		VideoStream:      f.Streams[0],
		AudioStream:      f.Streams[1],
		EstimatedBitrate: f.Bitrate,
	}
}

// TestApplyRemuxSelectedOnly pins the escalation rule: it keys on the File being
// "otherwise directPlay-capable", not on the current tier — so a directPlay Decision
// AND a directPlay-capable File already bumped to directStream by a non-default pick
// both trim to a flagged single-track directStream, while a container-mismatch remux,
// a transcode, an audio-only File, and a non-remuxable audio codec are left untouched.
func TestApplyRemuxSelectedOnly(t *testing.T) {
	prof := h264Profile() // supports mp4 + mkv, h264, aac/ac3 — the fixture direct-plays.
	cons := Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"}

	// directPlay → directStream, flag set, streams unchanged.
	got := applyRemuxSelectedOnly(prof, cons, directPlayDecision())
	if got.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream", got.Tier)
	}
	if !got.RemuxSelectedOnly {
		t.Error("RemuxSelectedOnly not set on the escalated Decision")
	}
	if got.AudioStream.ID != "a-en" || got.VideoStream.ID != "v1" {
		t.Errorf("resolved streams changed: v=%q a=%q, want v1/a-en", got.VideoStream.ID, got.AudioStream.ID)
	}

	// directStream from a NON-DEFAULT pick on a directPlay-capable File → still trims
	// (the pick is exactly what the trim honors, "those exact Streams retained"): here
	// the Japanese track was already re-pinned, and it survives as the single kept audio.
	pick := directPlayDecision()
	pick.Tier = TierDirectStream
	pick.AudioStream = pick.File.Streams[2] // a-ja
	out := applyRemuxSelectedOnly(prof, cons, pick)
	if out.Tier != TierDirectStream || !out.RemuxSelectedOnly {
		t.Errorf("stream-pick remux: tier=%q flag=%v, want directStream/true (trim honors the pick)", out.Tier, out.RemuxSelectedOnly)
	}
	if out.AudioStream.ID != "a-ja" {
		t.Errorf("stream-pick remux kept audio %q, want a-ja (the pick)", out.AudioStream.ID)
	}

	// directStream from a CONTAINER MISMATCH (File is NOT directPlay-capable) → no-op.
	mismatch := directPlayDecision()
	mismatch.Tier = TierDirectStream
	mp4Only := DeviceProfile{Containers: []string{"mp4"}, VideoCodecs: []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 8}
	if out := applyRemuxSelectedOnly(mp4Only, cons, mismatch); out.Tier != TierDirectStream || out.RemuxSelectedOnly {
		t.Errorf("container-mismatch remux: tier=%q flag=%v, want directStream/false (no-op)", out.Tier, out.RemuxSelectedOnly)
	}

	// Already transcode (a Quality cap, an AAC narrowing, a burn) → no-op.
	tc := directPlayDecision()
	tc.Tier = TierTranscode
	if out := applyRemuxSelectedOnly(prof, cons, tc); out.Tier != TierTranscode || out.RemuxSelectedOnly {
		t.Errorf("already-transcode: tier=%q flag=%v, want transcode/false (no-op)", out.Tier, out.RemuxSelectedOnly)
	}

	// Audio-only File (a Music Track) → no-op: nothing to trim, and it could break.
	ao := directPlayDecision()
	ao.AudioOnly = true
	if out := applyRemuxSelectedOnly(prof, cons, ao); out.Tier != TierDirectPlay || out.RemuxSelectedOnly {
		t.Errorf("audio-only: tier=%q flag=%v, want directPlay/false (no-op)", out.Tier, out.RemuxSelectedOnly)
	}

	// Resolved audio the remux container can't carry (FLAC) → no-op: a copy can't
	// honor it, so the working direct play stands rather than a broken remux. The
	// profile must decode FLAC so the File is still directPlay-capable (isolating the
	// remuxable-audio gate as the sole reason for the no-op).
	flacProf := prof
	flacProf.AudioCodecs = append(flacProf.AudioCodecs, "flac")
	flac := directPlayDecision()
	flac.File.Streams[1].Codec = "flac"
	flac.File.AudioCodec = "flac"
	flac.AudioStream = flac.File.Streams[1]
	if out := applyRemuxSelectedOnly(flacProf, cons, flac); out.Tier != TierDirectPlay || out.RemuxSelectedOnly {
		t.Errorf("flac audio: tier=%q flag=%v, want directPlay/false (copy-only can't remux flac)", out.Tier, out.RemuxSelectedOnly)
	}
}

// TestIsDemuxedSuppressedByRemuxSelected: a multi-audio remux-selected Decision
// stays MUXED — demuxing would deliver every audio Stream as a rendition, the
// opposite of the trim — while the same multi-audio Decision without the flag
// demuxes as usual.
func TestIsDemuxedSuppressedByRemuxSelected(t *testing.T) {
	dec := directPlayDecision()
	dec.Tier = TierDirectStream // a plain multi-audio remux demuxes.
	if !IsDemuxed(dec) {
		t.Fatal("multi-audio directStream should demux without the flag (guard for the negative below)")
	}
	dec.RemuxSelectedOnly = true
	if IsDemuxed(dec) {
		t.Error("remux-selected multi-audio Decision must stay muxed (single kept audio), got demuxed")
	}
}

// TestForcedMapIndices: the forced selectors always emit an explicit index — the
// chosen Stream's relative index, 0 when it can't be located — unlike the ordinary
// nil-for-single-stream helpers.
func TestForcedMapIndices(t *testing.T) {
	f := multiAudioMKVFile()
	// a-ja is the 2nd audio Stream (0-based index 1); v1 is the only video (0).
	if got := forcedAudioMapIndex(f, store.Stream{ID: "a-ja"}); got == nil || *got != 1 {
		t.Errorf("forcedAudioMapIndex(a-ja) = %v, want 1", got)
	}
	if got := forcedVideoMapIndex(f, store.Stream{ID: "v1"}); got == nil || *got != 0 {
		t.Errorf("forcedVideoMapIndex(v1) = %v, want 0", got)
	}
	// An unresolvable Stream still pins index 0 (never nil) so a map is always emitted.
	if got := forcedAudioMapIndex(f, store.Stream{ID: "nope"}); got == nil || *got != 0 {
		t.Errorf("forcedAudioMapIndex(unknown) = %v, want 0 (non-nil fallback)", got)
	}
}

// TestRemuxSelectedHLSArgsSingleTrack: the args builder for a remux-selected
// multi-audio Decision copies EXACTLY the selected video + audio (`-map 0:v:0 -map
// 0:a:1 -c copy`), drops the other audio, and exposes NO audio-rendition builder
// (muxed, not demuxed). Contrast a plain multi-audio remux, which is demuxed.
func TestRemuxSelectedHLSArgsSingleTrack(t *testing.T) {
	svc := NewService(fakeSubStore{}, nil, "", Governance{})
	f := multiAudioMKVFile()
	dec := Decision{
		Tier: TierDirectStream, Edition: store.Edition{ID: "e1"}, File: f,
		VideoStream: f.Streams[0], AudioStream: f.Streams[2], // the Japanese track (0:a:1)
		RemuxSelectedOnly: true,
	}

	primary, cpuFallback, audioRendition := svc.hlsArgsBuilders(h264Profile(), Constraints{}, dec, nil)
	if audioRendition != nil {
		t.Error("remux-selected session exposed an audio-rendition builder; it must stay muxed (single kept audio)")
	}
	if cpuFallback != nil {
		t.Error("remux (copy) has no CPU fallback; got a non-nil builder")
	}
	args := strings.Join(primary("/scratch", transcode.SeekOffset{}), " ")
	for _, want := range []string{"-map 0:v:0", "-map 0:a:1", "-c copy"} {
		if !strings.Contains(args, want) {
			t.Errorf("remux-selected args missing %q; got: %s", want, args)
		}
	}
	// The other audio Stream (0:a:0) must NOT be mapped — that is the whole point.
	if strings.Contains(args, "0:a:0") {
		t.Errorf("remux-selected args mapped the dropped audio (0:a:0); got: %s", args)
	}
}

// remuxSelStore is a minimal TitleStore + WatchStateStore over one Title detail,
// for the end-to-end Service.Negotiate test.
type remuxSelStore struct{ detail store.TitleDetail }

func (s remuxSelStore) TitleByID(id string) (store.TitleDetail, error) {
	if id != s.detail.ID {
		return store.TitleDetail{}, store.ErrNotFound
	}
	return s.detail, nil
}
func (s remuxSelStore) WatchStateFor(userID, titleID string) (store.WatchState, error) {
	return store.WatchState{}, nil
}
func (s remuxSelStore) SaveWatchState(userID, titleID string, resumeMs int64, watched, played bool) error {
	return nil
}

// TestNegotiateRemuxSelectedFlipsAndIsUnmetered: a remuxSelectedOnly request on an
// otherwise-directPlay multi-audio MKV negotiates a flagged directStream — and,
// being copy-only, consumes NO transcode cap slot even with the cap exhausted-tight
// (acceptance: does not increment transcode load). Omitting the flag keeps direct
// play (no regression).
func TestNegotiateRemuxSelectedFlipsAndIsUnmetered(t *testing.T) {
	detail := store.TitleDetail{Editions: []store.Edition{{ID: "e1", Name: "1080p", Files: []store.File{multiAudioMKVFile()}}}}
	detail.Title.ID = "t1"
	svc := NewService(remuxSelStore{detail: detail}, nil, "", Governance{MaxConcurrentTranscodes: 1})
	// Exhaust the single transcode slot so a metered escalation would be rejected;
	// a copy-only remux must sail past it.
	if _, err := svc.Sessions().CreateGoverned(CreateInput{UserID: "filler"}, transcodeDecision("x")); err != nil {
		t.Fatalf("filling transcode slot: %v", err)
	}

	base := Request{
		UserID: "u1", TitleID: "t1", Profile: h264Profile(),
		Constraints: Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"},
		Scope:       access.Scope{AllLibraries: true},
	}

	// Without the flag: plain direct play, unchanged.
	plain := base
	dec, _, unsup, busy, err := svc.Negotiate(plain)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("plain negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierDirectPlay || dec.RemuxSelectedOnly {
		t.Errorf("no-flag decision: tier=%q flag=%v, want directPlay/false", dec.Tier, dec.RemuxSelectedOnly)
	}

	// With the flag: a flagged directStream, delivered despite the full cap.
	sel := base
	sel.RemuxSelectedOnly = true
	dec, sess, unsup, busy, err := svc.Negotiate(sel)
	if err != nil || unsup != nil {
		t.Fatalf("remux-selected negotiate: err=%v unsup=%v", err, unsup)
	}
	if busy != nil {
		t.Fatalf("remux-selected was rejected as busy (%+v); a copy-only remux must not be metered", busy)
	}
	if dec.Tier != TierDirectStream || !dec.RemuxSelectedOnly {
		t.Errorf("flagged decision: tier=%q flag=%v, want directStream/true", dec.Tier, dec.RemuxSelectedOnly)
	}
	if sess.ID == "" {
		t.Error("remux-selected created no session")
	}
	// The remux did not consume a transcode slot: only the filler transcode counts.
	if got := svc.Sessions().ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after the remux, want 1 (the remux is unmetered)", got)
	}
}

// TestNegotiateRemuxSelectedHonoursNonDefaultPick: remuxSelectedOnly alongside a
// NON-DEFAULT audioStreamId keeps EXACTLY that Stream (acceptance: "those exact
// Streams are the ones retained"). The non-default pick already bumps the tier to
// directStream on its own; the trim must still fire (the File is directPlay-capable)
// and pin the copy to the picked audio, dropping the default — not fall into the
// demuxed keep-all-audio layout.
func TestNegotiateRemuxSelectedHonoursNonDefaultPick(t *testing.T) {
	detail := store.TitleDetail{Editions: []store.Edition{{ID: "e1", Name: "1080p", Files: []store.File{multiAudioMKVFile()}}}}
	detail.Title.ID = "t1"
	svc := NewService(remuxSelStore{detail: detail}, nil, "", Governance{})

	dec, _, unsup, busy, err := svc.Negotiate(Request{
		UserID: "u1", TitleID: "t1", Profile: h264Profile(),
		Constraints:       Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"},
		AudioStreamID:     "a-ja", // the non-default Japanese track
		RemuxSelectedOnly: true,
		Scope:             access.Scope{AllLibraries: true},
	})
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierDirectStream || !dec.RemuxSelectedOnly {
		t.Errorf("tier=%q flag=%v, want directStream/true", dec.Tier, dec.RemuxSelectedOnly)
	}
	if dec.AudioStream.ID != "a-ja" {
		t.Errorf("retained audio = %q, want a-ja (the exact pick)", dec.AudioStream.ID)
	}
	if IsDemuxed(dec) {
		t.Error("remux-selected + non-default pick demuxed (would keep all audio); must stay muxed single-track")
	}
	// The copied output maps exactly the picked audio (0:a:1), dropping the default.
	primary, _, _ := svc.hlsArgsBuilders(h264Profile(), Constraints{}, dec, nil)
	args := strings.Join(primary("/scratch", transcode.SeekOffset{}), " ")
	if !strings.Contains(args, "-map 0:a:1") || strings.Contains(args, "0:a:0") {
		t.Errorf("args did not pin only the picked audio (0:a:1); got: %s", args)
	}
}
