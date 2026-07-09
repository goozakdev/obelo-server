package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the Remembered-video trait re-resolver (selectable-video/04, ADR-0025)
// as a pure function: exact-trait match → label match → no-match. These pin the
// meaning-keyed resolution the API tests exercise end to end — the logic that lets a
// pick survive a re-rip or remux that shuffled stream order, mirroring
// audio_memory_test.go.

// TestResolveRememberedVideoExactTrait: a pick re-resolves to the Stream carrying the
// same title tag + resolution + codec even when the stream ORDER has shuffled (indexes
// changed) — and the traits keep two co-packaged cuts apart.
func TestResolveRememberedVideoExactTrait(t *testing.T) {
	// The two-cut fixture, reordered: the Black & White cut now sits first.
	shuffled := []store.Stream{
		{ID: "v-bw", Kind: "video", Codec: "h264", Width: 160, Height: 120, Title: "Black & White"},
		{ID: "v-col", Kind: "video", Codec: "h264", Width: 320, Height: 240, Title: "Colour", IsDefault: true},
	}

	// A pick of the Colour cut (stored as its meaning) re-resolves to v-col despite the
	// reorder — not the same-codec Black & White cut.
	colMem := videoMemoryOf(store.Stream{Codec: "h264", Width: 320, Height: 240, Title: "Colour"})
	if got, ok := resolveRememberedVideoStream(shuffled, colMem); !ok || got.ID != "v-col" {
		t.Fatalf("colour resolve = %+v (ok=%v), want v-col", got, ok)
	}

	// The Black & White pick resolves to v-bw, distinguished by its label + resolution.
	bwMem := videoMemoryOf(store.Stream{Codec: "h264", Width: 160, Height: 120, Title: "Black & White"})
	if got, ok := resolveRememberedVideoStream(shuffled, bwMem); !ok || got.ID != "v-bw" {
		t.Fatalf("b&w resolve = %+v (ok=%v), want v-bw", got, ok)
	}
}

// TestResolveRememberedVideoLabelFallback: when no Stream matches the exact traits (a
// re-encode changed the Black & White cut's resolution/codec) the pick degrades to the
// first same-LABEL Stream rather than erroring — the title tag is the durable meaning.
func TestResolveRememberedVideoLabelFallback(t *testing.T) {
	streams := []store.Stream{
		{ID: "v-col", Kind: "video", Codec: "h264", Width: 320, Height: 240, Title: "Colour", IsDefault: true},
		{ID: "v-bw2", Kind: "video", Codec: "hevc", Width: 320, Height: 240, Title: "Black & White"}, // re-encoded
	}
	// Remembered a 160x120 h264 Black & White that no longer exists as such → label match.
	mem := videoMemoryOf(store.Stream{Codec: "h264", Width: 160, Height: 120, Title: "Black & White"})
	got, ok := resolveRememberedVideoStream(streams, mem)
	if !ok || got.ID != "v-bw2" {
		t.Fatalf("label-fallback resolve = %+v (ok=%v), want v-bw2", got, ok)
	}
}

// TestResolveRememberedVideoNoMatch: a remembered cut with no surviving Stream at all
// yields no-match (the caller then falls through to the next resolution level — Show
// memory, then the capability default → disposition → first), never an error.
func TestResolveRememberedVideoNoMatch(t *testing.T) {
	streams := []store.Stream{
		{ID: "v-col", Kind: "video", Codec: "h264", Width: 320, Height: 240, Title: "Colour", IsDefault: true},
	}
	mem := videoMemoryOf(store.Stream{Codec: "h264", Width: 160, Height: 120, Title: "Black & White"})
	if got, ok := resolveRememberedVideoStream(streams, mem); ok {
		t.Fatalf("no-match resolve = %+v (ok=%v), want no match", got, ok)
	}

	// A remembered UNTITLED pick ("") only ever exact-matches; it must NOT label-match
	// every untagged Stream. An untitled Stream is present but its resolution differs, so
	// it is no-match (an untitled multi-res set re-resolves strictly by resolution).
	untitledMem := videoMemoryOf(store.Stream{Codec: "h264", Width: 1920, Height: 1080})
	if _, ok := resolveRememberedVideoStream([]store.Stream{{ID: "v-und", Kind: "video", Codec: "h264", Width: 640, Height: 360}}, untitledMem); ok {
		t.Fatal("untitled pick label-matched an untagged Stream, want no match")
	}
}

// TestResolveRememberedVideoSkipsCoverArt: a remembered pick never resolves to an
// embedded cover-art still, even one that (degenerately) carries a matching label —
// cover art is not in the selectable set, so it is excluded from re-resolution.
func TestResolveRememberedVideoSkipsCoverArt(t *testing.T) {
	streams := []store.Stream{
		{ID: "cover", Kind: "video", Codec: "mjpeg", Width: 600, Height: 600, Title: "Colour"},
		{ID: "v-col", Kind: "video", Codec: "h264", Width: 320, Height: 240, Title: "Colour", IsDefault: true},
	}
	mem := videoMemoryOf(store.Stream{Codec: "h264", Width: 320, Height: 240, Title: "Colour"})
	if got, ok := resolveRememberedVideoStream(streams, mem); !ok || got.ID != "v-col" {
		t.Fatalf("resolve = %+v (ok=%v), want v-col (cover art skipped)", got, ok)
	}
}

// TestVideoMemoryOfNormalizesCodec: the stored meaning trims the label and lowercases
// the codec so a pick matches a differently-cased codec Stream on the next play.
func TestVideoMemoryOfNormalizesCodec(t *testing.T) {
	mem := videoMemoryOf(store.Stream{Codec: "H264", Width: 320, Height: 240, Title: "  Colour  "})
	if mem.Codec != "h264" {
		t.Errorf("normalized codec = %q, want h264", mem.Codec)
	}
	if mem.Label != "Colour" {
		t.Errorf("label = %q, want trimmed \"Colour\"", mem.Label)
	}
	// A differently-cased same codec resolves against it.
	streams := []store.Stream{{ID: "v-col", Kind: "video", Codec: "h264", Width: 320, Height: 240, Title: "Colour"}}
	if got, ok := resolveRememberedVideoStream(streams, mem); !ok || got.ID != "v-col" {
		t.Fatalf("cross-case resolve = %+v (ok=%v), want v-col", got, ok)
	}
}
