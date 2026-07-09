package playback

import (
	"strings"

	"github.com/marioquake/juicebox/internal/audio"
	"github.com/marioquake/juicebox/internal/store"
)

// Remembered audio (audio-streams/05, ADR-0023): an explicit audio pick is stored
// per (User, Title) — and, for an Episode, bubbles up as the (User, Show) default —
// as the pick's MEANING (normalized language + distinguishing traits), never a
// stream index. This file holds the PURE re-resolution: given a remembered meaning
// and a File's current audio Streams, pick the Stream the meaning now refers to.
// The store reads/writes and the resolution-order orchestration live in service.go;
// keeping the matcher pure (no store, no DB) makes it directly unit-testable and is
// what closes the ADR-0014 "keyed to identity, not to file internals" gap for audio.

// audioMemoryOf captures the MEANING of a chosen audio Stream for storage: its
// normalized ISO-639-1 language plus the distinguishing traits the re-resolver
// matches on (the embedded title-tag label, the channel count, the commentary
// disposition). Language is normalized here so both write and match compare
// canonical codes (jpn/ja/Japanese all collapse to "ja").
func audioMemoryOf(s store.Stream) store.RememberedAudio {
	return store.RememberedAudio{
		Language:   audio.NormalizeLang(s.Language),
		Label:      strings.TrimSpace(s.Title),
		Channels:   s.Channels,
		Commentary: s.Commentary,
	}
}

// resolveRememberedAudioStream re-resolves a remembered pick against a File's
// current audio Streams by MEANING (ADR-0023), tolerating a re-rip or Edition
// switch that shuffled stream order or changed indexes:
//
//   - an EXACT-TRAIT match (same normalized language, title-tag label, channel
//     count, and commentary disposition) wins — the same track, wherever it now
//     sits;
//   - else the first LANGUAGE match (same normalized, non-empty language) — the
//     closest surviving track when the exact pick is gone;
//   - else no match (found=false), and the caller falls through to the next
//     resolution level (Show memory, then preferredAudioLang -> default -> first).
//
// It never errors: a stale memory degrades to no-match, never blocks playback. A
// remembered "unknown language" pick (Language=="") only ever matches by exact
// traits — an empty language does not language-match every untagged Stream.
func resolveRememberedAudioStream(streams []store.Stream, mem store.RememberedAudio) (store.Stream, bool) {
	var langMatch *store.Stream
	for i := range streams {
		s := &streams[i]
		if s.Kind != "audio" {
			continue
		}
		if audioTraitsMatch(*s, mem) {
			return *s, true
		}
		if langMatch == nil && mem.Language != "" && audio.NormalizeLang(s.Language) == mem.Language {
			langMatch = s
		}
	}
	if langMatch != nil {
		return *langMatch, true
	}
	return store.Stream{}, false
}

// audioTraitsMatch reports whether an audio Stream is an EXACT-trait match for a
// remembered pick: the same normalized language, embedded title-tag label, channel
// count, and commentary disposition — the tuple that distinguishes "English 5.1"
// from "English Director's Commentary" from "English Stereo".
func audioTraitsMatch(s store.Stream, mem store.RememberedAudio) bool {
	return audio.NormalizeLang(s.Language) == mem.Language &&
		strings.TrimSpace(s.Title) == mem.Label &&
		s.Channels == mem.Channels &&
		s.Commentary == mem.Commentary
}
