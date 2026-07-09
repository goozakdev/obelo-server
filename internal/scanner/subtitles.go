package scanner

import (
	"path/filepath"
	"strings"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
	"github.com/google/uuid"
)

// buildSidecarSubtitles turns the sidecar subtitle files discovered beside a
// Title's media into store.Subtitle tracks (source="sidecar"). It parses each
// filename for language + forced flags, classifies text vs image by extension,
// and folds a VOBSUB .idx/.sub pair into a single image track. names must be the
// bare filenames within dir, in a stable (sorted) order.
func buildSidecarSubtitles(dir string, names []string) []store.Subtitle {
	// A VOBSUB subtitle is an .idx (index) + .sub (bitmaps) pair sharing a base
	// name; record which bases have an .idx so the .sub partner is absorbed into
	// that one track rather than recorded twice.
	hasIdx := map[string]bool{}
	for _, n := range names {
		if strings.EqualFold(filepath.Ext(n), ".idx") {
			hasIdx[subtitleBase(n)] = true
		}
	}

	var out []store.Subtitle
	for _, n := range names {
		ext := strings.ToLower(filepath.Ext(n))
		if ext == ".sub" && hasIdx[subtitleBase(n)] {
			continue // absorbed into its sibling .idx track (one VOBSUB track)
		}
		stem, lang, forced := subtitle.ParseSidecar(n)
		// A sidecar whose stem names an Extra (Movie-trailer.en.srt) or junk
		// (sample) belongs to that clip, not the Title — don't surface it as a
		// selectable Title subtitle. (Size-based junk detection is skipped: a
		// subtitle file is legitimately tiny.)
		if extraSuffixType(stem) != "" || junkRe.MatchString(stem) {
			continue
		}
		codec := sidecarCodec(ext, hasIdx[subtitleBase(n)])
		out = append(out, store.Subtitle{
			ID:       uuid.NewString(),
			Source:   "sidecar",
			Kind:     subtitle.KindForCodec(codec),
			Language: lang,
			Forced:   forced,
			Codec:    codec,
			Path:     filepath.Join(dir, n),
		})
	}
	return out
}

// extraSuffixType reports the Extra type named by a sidecar stem's trailing
// -suffix ("Movie-trailer" → "trailer"), or "" when the stem is a main-video
// stem. Unlike extraTypeFromSuffix it takes an already-extension-stripped stem,
// so a dotted title (Some.Movie) is not mangled by a filepath.Ext strip.
func extraSuffixType(stem string) string {
	idx := strings.LastIndexAny(stem, "-_")
	if idx < 0 {
		return ""
	}
	return extraSuffixes[strings.ToLower(strings.TrimSpace(stem[idx+1:]))]
}

// subtitleBase is a sidecar filename minus its subtitle extension, lower-cased —
// the pairing key for VOBSUB .idx/.sub siblings.
func subtitleBase(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
}

// sidecarCodec maps a sidecar file extension to the codec/format token that
// drives kind classification here and the WebVTT-conversion / burn-in paths in
// later slices. A .sub is ambiguous: paired with an .idx it is VOBSUB (a bitmap
// image sub), but a standalone .sub is MicroDVD, a text format — so paired
// reports whether this .sub has an .idx sibling.
func sidecarCodec(ext string, paired bool) string {
	switch strings.ToLower(ext) {
	case ".srt":
		return "srt"
	case ".ass":
		return "ass"
	case ".ssa":
		return "ssa"
	case ".vtt":
		return "webvtt"
	case ".sup":
		return "sup"
	case ".idx":
		return "vobsub"
	case ".sub":
		if paired {
			return "vobsub" // has an .idx sibling → VOBSUB bitmap
		}
		return "microdvd" // standalone → MicroDVD text
	default:
		return strings.TrimPrefix(strings.ToLower(ext), ".")
	}
}
