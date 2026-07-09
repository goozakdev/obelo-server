package api

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// White-box unit tests for the video-Stream API projection (selectable-video/01):
// the cover-art exclusion, the single-video one-element list, the label fallback
// (title tag → resolution), and the default-flag re-marking (disposition for browse,
// resolved id for the playback decision). Fast and deterministic — no ffmpeg.

func TestToVideoStreamsProjection(t *testing.T) {
	streams := []store.Stream{
		{ID: "v-4k", Index: 0, Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, IsDefault: true, Title: "Colour"},
		{ID: "v-1080", Index: 1, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}, // untitled → resolution label
		{ID: "v-art", Index: 2, Kind: "video", Codec: "mjpeg", Width: 600, Height: 600},   // cover art, excluded
		{ID: "a", Index: 3, Kind: "audio", Codec: "aac", Channels: 2},
	}

	// Browse path (resolvedID ""): cover art excluded, default = container disposition.
	got := toVideoStreams(streams, "")
	if len(got) != 2 {
		t.Fatalf("video stream count = %d, want 2 (cover art excluded); %+v", len(got), got)
	}
	if got[0].ID != "v-4k" || got[1].ID != "v-1080" {
		t.Errorf("ids = %q,%q, want v-4k,v-1080", got[0].ID, got[1].ID)
	}
	if got[0].Label != "Colour" {
		t.Errorf("titled label = %q, want 'Colour'", got[0].Label)
	}
	if got[1].Label != "1080p" {
		t.Errorf("untitled label = %q, want the resolution fallback '1080p'", got[1].Label)
	}
	if !got[0].IsDefault || got[1].IsDefault {
		t.Errorf("browse default flags = %v,%v, want true,false (container disposition)", got[0].IsDefault, got[1].IsDefault)
	}

	// Playback decision path (resolvedID = the capability pick): the default is
	// re-marked to the resolved Stream, which may differ from the disposition.
	dec := toVideoStreams(streams, "v-1080")
	if dec[0].IsDefault || !dec[1].IsDefault {
		t.Errorf("resolved default flags = %v,%v, want false,true (v-1080 is the pick)", dec[0].IsDefault, dec[1].IsDefault)
	}

	// A single-video File → a one-element list.
	single := toVideoStreams([]store.Stream{
		{ID: "only", Kind: "video", Codec: "h264", Height: 720},
		{ID: "a", Kind: "audio", Codec: "aac"},
	}, "")
	if len(single) != 1 || single[0].ID != "only" {
		t.Fatalf("single-video projection = %+v, want one element 'only'", single)
	}

	// Always non-nil, even for an audio-only File (empty list, never null JSON).
	if got := toVideoStreams([]store.Stream{{Kind: "audio", Codec: "flac"}}, ""); got == nil {
		t.Errorf("toVideoStreams must never return nil")
	}
}

func TestVideoStreamLabel(t *testing.T) {
	cases := []struct {
		name string
		s    store.Stream
		want string
	}{
		{"title tag wins", store.Stream{Title: "Director's Cut", Height: 1080, Codec: "h264"}, "Director's Cut"},
		{"uhd → 4K", store.Stream{Height: 2160, Codec: "hevc"}, "4K"},
		{"1080 → 1080p", store.Stream{Height: 1080, Codec: "h264"}, "1080p"},
		{"720 → 720p", store.Stream{Height: 720, Codec: "h264"}, "720p"},
		{"no height → codec", store.Stream{Codec: "h264"}, "h264"},
		{"nothing → Video", store.Stream{}, "Video"},
	}
	for _, tc := range cases {
		if got := videoStreamLabel(tc.s); got != tc.want {
			t.Errorf("%s: videoStreamLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}
