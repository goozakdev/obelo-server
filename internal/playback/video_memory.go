package playback

import (
	"strings"

	"github.com/marioquake/juicebox/internal/store"
)

// Remembered video (selectable-video/04, ADR-0025, ADR-0023 mirrored): an explicit
// video pick is stored per (User, Title) — and, for an Episode, bubbles up as the
// (User, Show) default — as the pick's MEANING (the embedded title tag, falling back
// to the resolution/codec traits), never a stream index. This file holds the PURE
// re-resolution: given a remembered meaning and a File's current video Streams, pick
// the Stream the meaning now refers to. The store reads/writes and the resolution-order
// orchestration live in service.go; keeping the matcher pure (no store, no DB) makes it
// directly unit-testable and is the direct mirror of resolveRememberedAudioStream.

// videoMemoryOf captures the MEANING of a chosen video Stream for storage: its embedded
// title-tag label plus the distinguishing traits the re-resolver matches on (the
// normalized video codec and the resolution). The label is the primary identity — a
// titled cut ("Black & White") survives a re-rip that changed its resolution — with the
// codec/resolution traits distinguishing an untitled multi-bitrate/multi-res set. Codec
// is lowercased here so both write and match compare canonically.
func videoMemoryOf(s store.Stream) store.RememberedVideo {
	return store.RememberedVideo{
		Label:  strings.TrimSpace(s.Title),
		Codec:  strings.ToLower(strings.TrimSpace(s.Codec)),
		Width:  s.Width,
		Height: s.Height,
	}
}

// resolveRememberedVideoStream re-resolves a remembered pick against a File's current
// selectable (non-cover-art) video Streams by MEANING (ADR-0025), tolerating a re-rip
// or remux that shuffled stream order or changed indexes:
//
//   - an EXACT-TRAIT match (same title-tag label, normalized codec, and resolution)
//     wins — the same cut, wherever it now sits;
//   - else the first LABEL match (same non-empty title tag) — the surviving cut when a
//     re-encode changed its resolution or codec but kept its "Black & White" tag;
//   - else no match (found=false), and the caller falls through to the next resolution
//     level (Show memory, then the capability-then-quality default -> disposition -> first).
//
// It never errors: a stale memory degrades to no-match, never blocks playback. A
// remembered UNTITLED pick (Label=="") only ever matches by exact traits — an empty
// label does not label-match every untagged Stream — so an untitled multi-res set is
// re-resolved strictly by resolution/codec.
func resolveRememberedVideoStream(streams []store.Stream, mem store.RememberedVideo) (store.Stream, bool) {
	var labelMatch *store.Stream
	for i := range streams {
		s := &streams[i]
		if s.Kind != "video" || isCoverArtStream(*s) {
			continue
		}
		if videoTraitsMatch(*s, mem) {
			return *s, true
		}
		if labelMatch == nil && mem.Label != "" && strings.TrimSpace(s.Title) == mem.Label {
			labelMatch = s
		}
	}
	if labelMatch != nil {
		return *labelMatch, true
	}
	return store.Stream{}, false
}

// videoTraitsMatch reports whether a video Stream is an EXACT-trait match for a
// remembered pick: the same embedded title-tag label, normalized codec, and resolution
// — the tuple that distinguishes "Colour" (320x240 h264) from "Black & White"
// (160x120 h264) from a co-packaged lower-bitrate cut of the same picture.
func videoTraitsMatch(s store.Stream, mem store.RememberedVideo) bool {
	return strings.TrimSpace(s.Title) == mem.Label &&
		strings.ToLower(strings.TrimSpace(s.Codec)) == mem.Codec &&
		s.Width == mem.Width &&
		s.Height == mem.Height
}
