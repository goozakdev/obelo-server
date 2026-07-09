package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the selectable video read path + capability-aware default
// (selectable-video/01, ADR-0025) — the pure logic the black-box API/ffprobe tests
// exercise end to end. They pin the selectable set, the capability-then-quality
// default (a client that cannot decode a co-packaged 4K HEVC Stream defaults to the
// 1080p H.264 it can play direct), the is_default disposition tiebreak, cover-art
// exclusion, and the `-map 0:v:N` index. Mirrors audio_select_test.go seam-for-seam.

// multiVideoFile is an mkv carrying two co-packaged video cuts that SHARE one audio
// Stream — the Spider Noir shape: a 4K HEVC cut (the container default, titled
// "Colour") and a 1080p H.264 cut ("Black & White"), plus an embedded cover-art still
// that must be excluded from the selectable set. File-level attributes describe the
// primary (4K HEVC), as a real scan would populate them.
func multiVideoFile() store.File {
	f := mkvFile("hevc", "aac", 2160, 40_000_000)
	f.Streams = []store.Stream{
		{ID: "v-4k", Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, IsDefault: true, Title: "Colour"},
		{ID: "v-1080", Kind: "video", Codec: "h264", Width: 1920, Height: 1080, Title: "Black & White"},
		{ID: "v-art", Kind: "video", Codec: "mjpeg", Width: 600, Height: 600}, // embedded cover art
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
	}
	return f
}

func multiVideoDetail() store.TitleDetail {
	return store.TitleDetail{
		Editions: []store.Edition{{ID: "e1", Name: "cuts", Files: []store.File{multiVideoFile()}}},
	}
}

// hevcAndH264Profile decodes BOTH the co-packaged 4K HEVC and the 1080p H.264 cuts —
// a fully-capable client.
func hevcAndH264Profile() DeviceProfile {
	return DeviceProfile{
		Containers: []string{"mp4", "mkv"},
		VideoCodecs: []VideoCodecSupport{
			{Codec: "h264", MaxResolution: "1080p"},
			{Codec: "hevc", MaxResolution: "2160p"},
		},
		AudioCodecs:      []string{"aac", "ac3"},
		MaxAudioChannels: 8,
	}
}

// TestSelectableVideoStreams: the selectable set is a File's non-cover-art video
// Streams — cover art excluded, order preserved.
func TestSelectableVideoStreams(t *testing.T) {
	got := SelectableVideoStreams(multiVideoFile())
	if len(got) != 2 {
		t.Fatalf("selectable video count = %d, want 2 (cover art excluded); got %+v", len(got), got)
	}
	if got[0].ID != "v-4k" || got[1].ID != "v-1080" {
		t.Errorf("selectable ids = %q,%q, want v-4k,v-1080", got[0].ID, got[1].ID)
	}

	// A single-video File → a one-element list, its Streams unchanged.
	if got := SelectableVideoStreams(mp4File(1080, 6_000_000)); len(got) != 1 {
		t.Errorf("single-video selectable count = %d, want 1", len(got))
	}
	// An audio-only File (only cover art) → empty.
	audioOnly := store.File{Streams: []store.Stream{
		{ID: "art", Kind: "video", Codec: "png"},
		{ID: "a", Kind: "audio", Codec: "flac"},
	}}
	if got := SelectableVideoStreams(audioOnly); len(got) != 0 {
		t.Errorf("cover-art-only selectable count = %d, want 0; got %+v", len(got), got)
	}
}

// TestDefaultVideoStreamCapability: the default is the cheapest-playable-then-most-
// pixels pick. A fully-capable client gets the 4K HEVC; a client that cannot decode
// HEVC (or is resolution-capped below 4K) gets the co-packaged 1080p H.264 it can
// play direct — the load-bearing capability-then-quality rule.
func TestDefaultVideoStreamCapability(t *testing.T) {
	f := multiVideoFile()
	noCap := Constraints{}

	// Fully-capable: both decodable → most pixels → the 4K HEVC.
	if got, ok := defaultVideoStream(hevcAndH264Profile(), noCap, f); !ok || got.ID != "v-4k" {
		t.Errorf("capable default = %q (ok=%v), want v-4k", got.ID, ok)
	}

	// Cannot decode HEVC (h264-only device): the 4K HEVC is undecodable, the 1080p
	// H.264 is → the decodable Stream wins even though it has fewer pixels.
	if got, ok := defaultVideoStream(h264Profile(), noCap, f); !ok || got.ID != "v-1080" {
		t.Errorf("h264-only default = %q (ok=%v), want v-1080 (the co-packaged direct-playable cut)", got.ID, ok)
	}

	// Resolution-capped below 4K: even a HEVC-capable client can't take the 2160p cut
	// under a 1080p session cap → the 1080p H.264 is the cheapest playable Stream.
	if got, ok := defaultVideoStream(hevcAndH264Profile(), Constraints{MaxResolution: "1080p"}, f); !ok || got.ID != "v-1080" {
		t.Errorf("res-capped default = %q (ok=%v), want v-1080", got.ID, ok)
	}

	// Audio-only File → no default (ok=false), exactly like the old pickVideoStream.
	if _, ok := defaultVideoStream(h264Profile(), noCap, store.File{Streams: []store.Stream{{Kind: "audio", Codec: "aac"}}}); ok {
		t.Errorf("audio-only default unexpectedly ok")
	}
}

// TestDefaultVideoStreamIsDefaultTiebreak: among equally-playable, equal-resolution
// Streams the container is_default disposition decides — independent of scan order.
func TestDefaultVideoStreamIsDefaultTiebreak(t *testing.T) {
	f := mkvFile("h264", "aac", 1080, 6_000_000)
	f.Streams = []store.Stream{
		{ID: "v-a", Kind: "video", Codec: "h264", Width: 1920, Height: 1080},
		{ID: "v-b", Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
		{ID: "a", Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
	}
	if got, ok := defaultVideoStream(h264Profile(), Constraints{}, f); !ok || got.ID != "v-b" {
		t.Errorf("tiebreak default = %q (ok=%v), want v-b (the is_default disposition)", got.ID, ok)
	}
}

// TestNegotiateMultiVideoDefault: negotiation reports (and gates on) the capability
// default, so the reported video Stream is the one that will be delivered. A client
// that can't decode the 4K HEVC still DIRECT-PLAYS the co-packaged 1080p H.264.
func TestNegotiateMultiVideoDefault(t *testing.T) {
	ed := multiVideoDetail().Editions[0]
	f := ed.Files[0]

	// h264-only client: defaults to the 1080p H.264 and direct-plays it (not a
	// transcode of the 4K HEVC).
	dec, unsup := Negotiate(h264Profile(), Constraints{}, ed, f)
	if unsup != nil {
		t.Fatalf("h264 client unexpectedly unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectPlay {
		t.Errorf("tier = %q, want directPlay", dec.Tier)
	}
	if dec.VideoStream.ID != "v-1080" || dec.VideoStream.Codec != "h264" {
		t.Errorf("reported video = %q/%q, want v-1080/h264", dec.VideoStream.ID, dec.VideoStream.Codec)
	}

	// Fully-capable client: gets the 4K HEVC, direct-played.
	dec, unsup = Negotiate(hevcAndH264Profile(), Constraints{}, ed, f)
	if unsup != nil {
		t.Fatalf("capable client unexpectedly unsupported: %v", unsup)
	}
	if dec.VideoStream.ID != "v-4k" {
		t.Errorf("capable reported video = %q, want v-4k", dec.VideoStream.ID)
	}
}

// TestVideoMapIndex: the -map selector is the chosen Stream's video-relative index —
// counting ALL video Streams (cover art included, matching ffmpeg's 0:v:N ordinal) —
// and nil for a single-video File or an unresolvable Stream so those args stay
// byte-for-byte unchanged.
func TestVideoMapIndex(t *testing.T) {
	f := multiVideoFile()
	// v-4k is 0:v:0, v-1080 is 0:v:1, the cover-art still is 0:v:2 (ffmpeg counts it).
	cases := map[string]int{"v-4k": 0, "v-1080": 1, "v-art": 2}
	for id, want := range cases {
		got := videoMapIndex(f, store.Stream{ID: id})
		if got == nil || *got != want {
			t.Errorf("videoMapIndex(%q) = %v, want %d", id, got, want)
		}
	}

	// Single-video File → nil (implicit selection is correct; args stay unchanged).
	single := mp4File(1080, 6_000_000)
	single.Streams[0].ID = "only-v"
	if got := videoMapIndex(single, single.Streams[0]); got != nil {
		t.Errorf("single-video videoMapIndex = %v, want nil", got)
	}
	// An id that names no video Stream of the File → nil.
	if got := videoMapIndex(f, store.Stream{ID: "nope"}); got != nil {
		t.Errorf("unknown-id videoMapIndex = %v, want nil", got)
	}
}

// Unit tests for the explicit video-selection negotiation (selectable-video/02): id
// resolution, the tier floor a chosen Stream escalates to, and the escalation's
// preservation of the audio/subtitle/burn picks — the pure logic the black-box
// API/ffprobe tests exercise end to end. Mirrors audio_select_test.go seam-for-seam.

// multiVideoMultiAudioDetail is the multi-video fixture PLUS a second audio Stream, so
// the video-switch preservation tests can assert a non-default audio pick survives the
// switch (the shared audio is by construction untouched — this makes that observable).
func multiVideoMultiAudioDetail() store.TitleDetail {
	f := mkvFile("hevc", "aac", 2160, 40_000_000)
	f.ID = "fmv"
	f.Streams = []store.Stream{
		{ID: "v-4k", Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, IsDefault: true, Title: "Colour"},
		{ID: "v-1080", Kind: "video", Codec: "h264", Width: 1920, Height: 1080, Title: "Black & White"},
		{ID: "v-art", Kind: "video", Codec: "mjpeg", Width: 600, Height: 600}, // embedded cover art
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		{ID: "a-ja", Kind: "audio", Codec: "ac3", Channels: 6, Language: "jpn"},
	}
	return store.TitleDetail{Editions: []store.Edition{{ID: "e1", Files: []store.File{f}}}}
}

// TestResolveVideoStream: a video-Stream id resolves to its owning Edition/File/
// Stream; an unknown id, an audio id, an embedded cover-art id, and the empty id all
// miss (cover art is never in the client-facing selectable set).
func TestResolveVideoStream(t *testing.T) {
	d := multiVideoMultiAudioDetail()
	ed, f, s, ok := resolveVideoStream(d, "v-1080")
	if !ok || ed.ID != "e1" || f.ID != "fmv" || s.Codec != "h264" || s.Height != 1080 {
		t.Fatalf("resolve v-1080 = ed:%q file:%q stream:%+v ok:%v", ed.ID, f.ID, s, ok)
	}
	for _, bad := range []string{"", "does-not-exist", "a-en" /* audio */, "v-art" /* cover art */} {
		if _, _, _, ok := resolveVideoStream(d, bad); ok {
			t.Errorf("resolveVideoStream(%q) unexpectedly resolved", bad)
		}
	}
}

// TestVideoSelectionFloor pins the minimum tier a chosen video Stream escalates to,
// per the deliver-the-selected-video rules (ADR-0025): the default imposes no floor,
// a decodable non-default escalates to remux (direct play carries only the default
// video), and one the client can't decode forces a transcode.
func TestVideoSelectionFloor(t *testing.T) {
	d := multiVideoMultiAudioDetail()
	ed := d.Editions[0]
	f := ed.Files[0]
	cons := Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}
	stream := func(id string) store.Stream {
		_, _, s, _ := resolveVideoStream(d, id)
		return s
	}

	// Fully-capable client: default = v-4k (most pixels, decodable).
	full := hevcAndH264Profile()
	def, ok := defaultVideoStream(full, cons, f)
	if !ok || def.ID != "v-4k" {
		t.Fatalf("capable default = %q (ok=%v), want v-4k", def.ID, ok)
	}
	// The default itself imposes no floor (direct play stands).
	if got := videoSelectionFloor(full, cons, ed, f, stream("v-4k"), def); got != TierDirectPlay {
		t.Errorf("default-video floor = %q, want directPlay", got)
	}
	// Non-default but decodable h264 cut → remux: direct play carries only the default.
	if got := videoSelectionFloor(full, cons, ed, f, stream("v-1080"), def); got != TierDirectStream {
		t.Errorf("non-default decodable floor = %q, want directStream", got)
	}

	// h264-only client: default = v-1080 (the decodable co-packaged cut); selecting the
	// 4K HEVC it can't decode forces a transcode of THAT Stream, not a fallback.
	h264 := h264Profile()
	hdef, _ := defaultVideoStream(h264, cons, f)
	if hdef.ID != "v-1080" {
		t.Fatalf("h264 default = %q, want v-1080", hdef.ID)
	}
	if got := videoSelectionFloor(h264, cons, ed, f, stream("v-4k"), hdef); got != TierTranscode {
		t.Errorf("undecodable-video floor = %q, want transcode", got)
	}

	// Container-mismatch client (decodes h264 but not mkv) on a TWO-h264 File: even the
	// default can't direct-play, so a decodable non-default pick floors at remux (a remux
	// fixes exactly the container) rather than a needless transcode.
	twoH264 := mkvFile("h264", "aac", 1080, 6_000_000)
	twoH264.Streams = []store.Stream{
		{ID: "hi", Kind: "video", Codec: "h264", Height: 1080, IsDefault: true},
		{ID: "lo", Kind: "video", Codec: "h264", Height: 720},
		{ID: "a", Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
	}
	tEd := store.Edition{ID: "e2", Files: []store.File{twoH264}}
	tDetail := store.TitleDetail{Editions: []store.Edition{tEd}}
	remuxOnly := DeviceProfile{
		Containers:       []string{"mp4"}, // NOT mkv → the default can't direct-play
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac"},
		MaxAudioChannels: 8,
	}
	rdef, _ := defaultVideoStream(remuxOnly, cons, twoH264)
	if rdef.ID != "hi" {
		t.Fatalf("two-h264 default = %q, want hi", rdef.ID)
	}
	_, _, loStream, _ := resolveVideoStream(tDetail, "lo")
	if got := videoSelectionFloor(remuxOnly, cons, tEd, twoH264, loStream, rdef); got != TierDirectStream {
		t.Errorf("container-mismatch non-default floor = %q, want directStream", got)
	}
}

// TestEscalateForVideoPreservesPicks: a video switch swaps only the video Stream and
// re-tiers, carrying the base Decision's audio/subtitle/burn picks and resume through
// unchanged (acceptance: the switch preserves audioStreamId/burnSubtitleId/position).
func TestEscalateForVideoPreservesPicks(t *testing.T) {
	d := multiVideoMultiAudioDetail()
	f := d.Editions[0].Files[0]
	req := Request{Profile: hevcAndH264Profile(), Constraints: Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}}
	def, _ := defaultVideoStream(req.Profile, req.Constraints, f)

	// Base: a remux that already re-pinned the non-default Japanese AC3 audio (as
	// escalateForAudio would). base.VideoStream is the capability default (v-4k).
	_, _, ja, _ := resolveAudioStream(d, "a-ja")
	subs := buildSubtitleTracks(f, d.Subtitles)
	base := Decision{
		Tier: TierDirectStream, Edition: d.Editions[0], File: f,
		VideoStream: def, AudioStream: ja, Subtitles: subs,
	}

	got, err := escalateForVideo(req, d, base, "v-1080")
	if err != nil {
		t.Fatalf("escalateForVideo: %v", err)
	}
	if got.VideoStream.ID != "v-1080" {
		t.Errorf("switched video = %q, want v-1080", got.VideoStream.ID)
	}
	if got.AudioStream.ID != "a-ja" {
		t.Errorf("audio pick not preserved: %q, want a-ja", got.AudioStream.ID)
	}
	if len(got.Subtitles) != len(subs) {
		t.Errorf("subtitle list not preserved: %d, want %d", len(got.Subtitles), len(subs))
	}
	// v-1080 is decodable + non-default → remux; the base was already remux → remux.
	if got.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream", got.Tier)
	}

	// Composes with a burn: a burn Decision stays a transcode (you cannot overlay onto a
	// copied Stream), the Burn target survives, and the video is re-pinned.
	burn := &BurnSubtitle{Path: f.Path, StreamIndex: 0}
	burnBase := Decision{Tier: TierTranscode, Edition: d.Editions[0], File: f, VideoStream: def, AudioStream: ja, Burn: burn}
	gotBurn, err := escalateForVideo(req, d, burnBase, "v-1080")
	if err != nil {
		t.Fatalf("escalateForVideo (burn): %v", err)
	}
	if gotBurn.Burn != burn {
		t.Errorf("burn target not preserved across a video switch")
	}
	if gotBurn.Tier != TierTranscode {
		t.Errorf("burn+switch tier = %q, want transcode", gotBurn.Tier)
	}
	if gotBurn.VideoStream.ID != "v-1080" {
		t.Errorf("burn+switch video = %q, want v-1080", gotBurn.VideoStream.ID)
	}
}

// TestEscalateForVideoDefaultIsNoEscalation: selecting the resolved default by id does
// not force an escalation off the base tier (acceptance: a videoStreamId equal to the
// default keeps direct play).
func TestEscalateForVideoDefaultIsNoEscalation(t *testing.T) {
	d := multiVideoMultiAudioDetail()
	f := d.Editions[0].Files[0]
	req := Request{Profile: hevcAndH264Profile(), Constraints: Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}}
	def, _ := defaultVideoStream(req.Profile, req.Constraints, f)
	base := Decision{Tier: TierDirectPlay, Edition: d.Editions[0], File: f, VideoStream: def}

	got, err := escalateForVideo(req, d, base, def.ID)
	if err != nil {
		t.Fatalf("escalateForVideo: %v", err)
	}
	if got.Tier != TierDirectPlay {
		t.Errorf("default-video-id tier = %q, want directPlay (no needless escalation)", got.Tier)
	}
}

// TestEscalateForVideoUnknownAndForeign: an unknown id (and one from another File)
// fails structurally rather than silently defaulting.
func TestEscalateForVideoUnknownAndForeign(t *testing.T) {
	d := multiVideoMultiAudioDetail()
	f := d.Editions[0].Files[0]
	req := Request{Profile: hevcAndH264Profile(), Constraints: Constraints{MaxBitrate: 100_000_000, MaxResolution: "2160p"}}
	def, _ := defaultVideoStream(req.Profile, req.Constraints, f)
	base := Decision{Tier: TierDirectPlay, Edition: d.Editions[0], File: f, VideoStream: def}

	if _, err := escalateForVideo(req, d, base, "does-not-exist"); err != ErrVideoStreamNotFound {
		t.Errorf("unknown videoStreamId err = %v, want ErrVideoStreamNotFound", err)
	}
	// A base pinned to a DIFFERENT File than the pick's File → not found (same-container).
	otherBase := base
	otherBase.File = store.File{ID: "other"}
	if _, err := escalateForVideo(req, d, otherBase, "v-1080"); err != ErrVideoStreamNotFound {
		t.Errorf("cross-file videoStreamId err = %v, want ErrVideoStreamNotFound", err)
	}
}
