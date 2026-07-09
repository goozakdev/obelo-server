// Package audio holds the pure, dependency-free helpers for presenting a File's
// embedded audio Streams to a viewer: ISO-639 language normalization (reused from
// the subtitle work), channel-count → familiar layout labeling ("Stereo"/"5.1"),
// and the human menu label a Stream carries (language + layout, or a title tag
// like "Director's Commentary"). Per CONTEXT.md the embedded audio Stream is
// itself the selectable unit — there is no coined "Audio track" concept — so this
// package speaks only of Streams. Kept free of any internal dependency except the
// subtitle ISO-639 tables, so the scanner and the API can both reuse it without a
// layering cycle. Later slices (rendition delivery, Remembered audio) grow it.
package audio

import (
	"fmt"
	"strings"

	"github.com/marioquake/juicebox/internal/subtitle"
)

// NormalizeLang folds a raw audio-stream language tag to one canonical ISO-639-1
// code, reusing the exact machinery the subtitle read path built (so `jpn`, `ja`,
// and `Japanese` all collapse to "ja", and an undetermined/unknown tag returns ""
// → surfaced as "Unknown"). A thin alias, kept here so callers speak in audio
// terms and the reuse is a single, documented seam.
func NormalizeLang(raw string) string { return subtitle.NormalizeLang(raw) }

// ChannelLayout renders an ffprobe channel count as the familiar surround label a
// viewer expects ("Stereo", "5.1", "7.1"), rather than a bare number. 0 channels
// (unknown) returns "" so the label simply omits a layout; an unusual count falls
// back to "N ch" so nothing is silently dropped.
func ChannelLayout(channels int) string {
	switch channels {
	case 0:
		return ""
	case 1:
		return "Mono"
	case 2:
		return "Stereo"
	case 3:
		return "2.1"
	case 4:
		return "4.0"
	case 5:
		return "5.0"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	default:
		return fmt.Sprintf("%d ch", channels)
	}
}

// Label builds the human menu string for one audio Stream from its normalized
// language, channel count, embedded title tag, and commentary disposition. The
// language name always leads ("English", or "Unknown" for an untagged Stream). A
// title tag is the most descriptive signal, so it wins outright ("English
// Director's Commentary"); otherwise the familiar channel layout is appended
// ("English 5.1"), plus a trailing "Commentary" when the disposition flags it but
// the file carried no title. So "English 5.1" reads distinct from "English
// Director's Commentary" (PRD user stories 2–4).
func Label(lang string, channels int, title string, commentary bool) string {
	parts := []string{subtitle.DisplayLang(lang)}
	if t := strings.TrimSpace(title); t != "" {
		parts = append(parts, t)
	} else {
		if layout := ChannelLayout(channels); layout != "" {
			parts = append(parts, layout)
		}
		if commentary {
			parts = append(parts, "Commentary")
		}
	}
	return strings.Join(parts, " ")
}
