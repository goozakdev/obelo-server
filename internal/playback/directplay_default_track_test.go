package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Direct play serves the File unchanged, so the player plays the container-default
// video track. When negotiation ranks and checks a DIFFERENT track, the Decision is a
// remux of that track (ADR-0025, amended) — never a directPlay of unchecked bytes.

func twoTrackMkv(defCodec string, defHeight int, altCodec string, altHeight int) store.File {
	f := mkvFile(defCodec, "aac", defHeight, 6_000_000)
	f.Streams = []store.Stream{
		{ID: "v0", Kind: "video", Codec: defCodec, Height: defHeight, IsDefault: true},
		{ID: "v1", Kind: "video", Codec: altCodec, Height: altHeight},
		{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, Language: "en", IsDefault: true},
	}
	return f
}

func TestUndecodableDefaultTrackRemuxesTheDecodableOne(t *testing.T) {
	f := twoTrackMkv("hevc", 1080, "h264", 1080)
	dec, unsup := Negotiate(h264Profile(), Constraints{}, store.Edition{ID: "e1", Files: []store.File{f}}, f)
	if unsup != nil {
		t.Fatalf("unexpectedly unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream (direct play would play the undecodable HEVC default)", dec.Tier)
	}
	if dec.VideoStream.ID != "v1" {
		t.Errorf("video = %q, want v1", dec.VideoStream.ID)
	}
	if idx := videoMapIndex(dec.File, dec.VideoStream); idx == nil || *idx != 1 {
		t.Errorf("map index = %v, want 0:v:1", idx)
	}
}

// With no cap and a fully capable client the taller alt (1080p) outranks the 720p
// container default, so it is the checked track and is remuxed.
func TestTallerNonDefaultTrackIsRemuxed(t *testing.T) {
	f := twoTrackMkv("h264", 720, "h264", 1080)
	dec, unsup := Negotiate(h264Profile(), Constraints{}, store.Edition{ID: "e1", Files: []store.File{f}}, f)
	if unsup != nil {
		t.Fatalf("unexpectedly unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "v1" {
		t.Errorf("got tier %q video %q, want directStream v1", dec.Tier, dec.VideoStream.ID)
	}
}

func TestCheckedDefaultTrackStillDirectPlays(t *testing.T) {
	// Default is the ranked track (tallest and decodable).
	f := twoTrackMkv("h264", 1080, "h264", 720)
	dec, unsup := Negotiate(h264Profile(), Constraints{}, store.Edition{ID: "e1", Files: []store.File{f}}, f)
	if unsup != nil {
		t.Fatalf("unexpectedly unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectPlay || dec.VideoStream.ID != "v0" {
		t.Errorf("got tier %q video %q, want directPlay v0", dec.Tier, dec.VideoStream.ID)
	}

	// Single-track File.
	m := mp4File(1080, 6_000_000)
	dec, unsup = Negotiate(h264Profile(), Constraints{}, store.Edition{ID: "e1", Files: []store.File{m}}, m)
	if unsup != nil || dec.Tier != TierDirectPlay {
		t.Errorf("single track: tier %q unsup %v, want directPlay", dec.Tier, unsup)
	}
}
