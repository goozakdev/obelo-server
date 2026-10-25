package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A codec tag names the codec a rung DELIVERS, never the File's default: an h264 encode of an
// hevc File is "h264", and a remux of the File's h264 ALT track is "h264".
func TestRungCodecIsTheDeliveredCodec(t *testing.T) {
	f := store.File{ID: "f", VideoCodec: "hevc", AudioCodec: "aac", Height: 2160}
	alt := store.Stream{ID: "alt", Kind: "video", Codec: "h264", Height: 720}
	main := store.Stream{ID: "main", Kind: "video", Codec: "hevc", Height: 2160, IsDefault: true}
	p := uhdProfile()
	p.VideoCodecs = append(p.VideoCodecs, VideoCodecSupport{Codec: "hevc", MaxResolution: "2160p"})
	for _, tc := range []struct {
		name string
		dec  Decision
		c    Constraints
		want string
	}{
		{"encode of an hevc File", Decision{Tier: TierTranscode, File: f, VideoStream: main}, Constraints{MaxResolution: "720p", MaxBitrate: 4_000_000}, "h264"},
		{"encode of the ALT track", Decision{Tier: TierTranscode, File: f, VideoStream: alt, Burn: &BurnSubtitle{}}, Constraints{}, "h264"},
		{"remux of the ALT track", Decision{Tier: TierDirectStream, File: f, VideoStream: alt}, Constraints{}, "h264"},
		{"remux of the default track", Decision{Tier: TierDirectStream, File: f, VideoStream: main}, Constraints{}, "hevc"},
		{"video copy", Decision{Tier: TierTranscode, VideoCopy: true, File: f, VideoStream: main}, Constraints{}, "hevc"},
		{"direct play", Decision{Tier: TierDirectPlay, File: f, VideoStream: alt}, Constraints{}, "hevc"},
	} {
		r := describeRung(p, tc.c, tc.dec, videoNaming)
		if r.codec != tc.want {
			t.Errorf("%s: codec %q, want %q", tc.name, r.codec, tc.want)
		}
	}
}

// Names are trimmed before use; a rung with no Name and no known height is "Original", never
// an internal id.
func TestEditionLabelTrimsNamesAndNeverShowsAnId(t *testing.T) {
	for _, tc := range []struct {
		name   string
		height int
		want   string
	}{
		{"  Director's Cut ", 720, "Director's Cut · 720p"},
		{" 2160p ", 2160, "2160p"},
		{"  4K\t", 2160, "2160p"},
		{"   ", 1080, "1080p"},
		{"   ", 0, "Original"},
		{"", 0, "Original"},
		{"  Extended ", 0, "Extended"},
	} {
		got := editionLabel(store.Edition{ID: "ed-internal", Name: tc.name}, store.File{Height: tc.height})
		if got != tc.want {
			t.Errorf("Name %q height %d: label %q, want %q", tc.name, tc.height, got, tc.want)
		}
	}
	if got := audioEditionLabel(store.Edition{ID: "x", Name: "  FLAC  "}, store.File{}); got != "FLAC" {
		t.Errorf("audio label = %q, want the trimmed Name", got)
	}
	if got := audioEditionLabel(store.Edition{ID: "x", Name: "  "}, store.File{}); got != "Original" {
		t.Errorf("audio label = %q, want Original for a blank Name", got)
	}
}

// Online's top rung carries its height when it is known.
func TestOnlineOriginalCarriesItsHeight(t *testing.T) {
	got := onlineLadder(t, Constraints{}, true, mp4("1080p"))
	if got[0].Label != "Original · 1080p" {
		t.Errorf("top rung label = %q, want Original · 1080p", got[0].Label)
	}
	noHeight := mp4("")
	if got := onlineLadder(t, Constraints{}, true, noHeight); got[0].Label != "Original" {
		t.Errorf("heightless top rung label = %q, want Original", got[0].Label)
	}
}
