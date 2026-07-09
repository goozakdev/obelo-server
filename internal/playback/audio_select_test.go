package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the explicit audio-selection negotiation (audio-streams/02): the
// audio-relative -map index, id resolution, and the tier a chosen Stream resolves
// to. These pin the pure logic the black-box API/ffprobe tests exercise end to end.

// multiAudioFile is a video File carrying four audio Streams spanning the traits
// selection must handle: a default AAC stereo, a non-default AC3 5.1, a client-
// incompatible DTS commentary, and an untagged AAC mono — mirroring the shared
// checked-in fixture.
func multiAudioFile() store.File {
	f := mkvFile("h264", "aac", 1080, 6_000_000)
	f.Streams = []store.Stream{
		{ID: "v", Kind: "video", Codec: "h264", Height: 1080, IsDefault: true},
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		{ID: "a-ja", Kind: "audio", Codec: "ac3", Channels: 6, Language: "jpn"},
		{ID: "a-com", Kind: "audio", Codec: "dts", Channels: 2, Language: "eng", Commentary: true, Title: "Director's Commentary"},
		{ID: "a-und", Kind: "audio", Codec: "aac", Channels: 1},
	}
	return f
}

func multiAudioDetail() store.TitleDetail {
	return store.TitleDetail{
		Editions: []store.Edition{{ID: "e1", Name: "1080p", Files: []store.File{multiAudioFile()}}},
	}
}

// TestAudioMapIndex: the -map selector is the chosen Stream's audio-relative index
// on a multi-audio File, and nil (no explicit map, byte-identical args) for a
// single-audio File or an unresolvable Stream.
func TestAudioMapIndex(t *testing.T) {
	f := multiAudioFile()
	cases := map[string]int{"a-en": 0, "a-ja": 1, "a-com": 2, "a-und": 3}
	for id, want := range cases {
		got := audioMapIndex(f, store.Stream{ID: id})
		if got == nil || *got != want {
			t.Errorf("audioMapIndex(%q) = %v, want %d", id, got, want)
		}
	}

	// Single-audio File → nil (implicit selection is correct; args stay unchanged).
	single := mp4File(1080, 6_000_000)
	if got := audioMapIndex(single, single.Streams[1]); got != nil {
		t.Errorf("single-audio audioMapIndex = %v, want nil", got)
	}
	// An id that names no audio Stream of the File → nil.
	if got := audioMapIndex(f, store.Stream{ID: "nope"}); got != nil {
		t.Errorf("unknown-id audioMapIndex = %v, want nil", got)
	}
}

// TestResolveAudioStream: an audio-Stream id resolves to its owning Edition/File/
// Stream; an unknown id, a subtitle/video id, and the empty id all miss.
func TestResolveAudioStream(t *testing.T) {
	d := multiAudioDetail()
	ed, f, s, ok := resolveAudioStream(d, "a-ja")
	if !ok || ed.ID != "e1" || f.ID == "" || s.Codec != "ac3" || s.Channels != 6 {
		t.Fatalf("resolve a-ja = ed:%q file:%q stream:%+v ok:%v", ed.ID, f.ID, s, ok)
	}
	for _, bad := range []string{"", "does-not-exist", "v" /* a video Stream id */} {
		if _, _, _, ok := resolveAudioStream(d, bad); ok {
			t.Errorf("resolveAudioStream(%q) unexpectedly resolved", bad)
		}
	}
}

// TestAudioSelectionTier pins the tier a chosen Stream escalates to, per the
// deliver-the-selected-audio rules (ADR-0022).
func TestAudioSelectionTier(t *testing.T) {
	f := multiAudioFile()
	ed := store.Edition{ID: "e1", Files: []store.File{f}}
	cons := Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"}
	stream := func(id string) store.Stream {
		_, _, s, _ := resolveAudioStream(store.TitleDetail{Editions: []store.Edition{ed}}, id)
		return s
	}

	// A profile that can play the container + h264 + aac/ac3 within 8 channels.
	full := h264Profile()
	// Default AAC stereo → direct play (unchanged).
	if got := audioSelectionTier(full, cons, ed, f, stream("a-en")); got != TierDirectPlay {
		t.Errorf("default-audio tier = %q, want directPlay", got)
	}
	// Non-default AC3 5.1 (decodable, remuxable) → remux: direct play carries only
	// the default audio, so selecting another escalates one tier.
	if got := audioSelectionTier(full, cons, ed, f, stream("a-ja")); got != TierDirectStream {
		t.Errorf("non-default decodable tier = %q, want directStream", got)
	}
	// The DTS commentary: the client can't decode dts → transcode (re-encode to AAC).
	if got := audioSelectionTier(full, cons, ed, f, stream("a-com")); got != TierTranscode {
		t.Errorf("undecodable-codec tier = %q, want transcode", got)
	}

	// A profile that lacks the container (mp4 only) but decodes the codecs → even the
	// default audio can't direct-play, so a remux is the floor; a non-default pick
	// stays remux (not a needless transcode).
	remuxOnly := DeviceProfile{
		Containers:       []string{"mp4"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac", "ac3"},
		MaxAudioChannels: 8,
	}
	if got := audioSelectionTier(remuxOnly, cons, ed, f, stream("a-en")); got != TierDirectStream {
		t.Errorf("container-mismatch default tier = %q, want directStream", got)
	}
	if got := audioSelectionTier(remuxOnly, cons, ed, f, stream("a-ja")); got != TierDirectStream {
		t.Errorf("container-mismatch non-default tier = %q, want directStream", got)
	}

	// Channel cap: a 2-channel device forced to downmix the 5.1 must transcode it.
	stereoOnly := h264Profile()
	stereoOnly.MaxAudioChannels = 2
	if got := audioSelectionTier(stereoOnly, cons, ed, f, stream("a-ja")); got != TierTranscode {
		t.Errorf("over-channel-cap tier = %q, want transcode", got)
	}
}

// TestPickAudioNormalizesLang: a client hint of "ja" selects a Stream tagged the
// ISO-639-2 "jpn" (both sides normalized), so preferredAudioLang actually picks the
// delivered audio on a multi-audio File.
func TestPickAudioNormalizesLang(t *testing.T) {
	f := multiAudioFile()
	for _, pref := range []string{"ja", "jpn", "Japanese"} {
		a, ok := pickAudioStream(f, pref)
		if !ok || a.ID != "a-ja" {
			t.Errorf("pickAudioStream(pref=%q) = %+v (ok=%v), want a-ja", pref, a, ok)
		}
	}
	// No preference → the default disposition track.
	if a, _ := pickAudioStream(f, ""); a.ID != "a-en" {
		t.Errorf("no-pref pick = %q, want the default a-en", a.ID)
	}
}
