// Package subtitle holds the pure, dependency-free helpers shared by the scanner
// (which persists Subtitle tracks) and the API (which lists them): ISO-639
// language normalization, subtitle-kind classification by codec, sidecar
// filename parsing, and human labels. Keeping them here — importing nothing
// internal — lets both the low-level scanner and the higher-level api package
// reuse one implementation without a layering cycle.
package subtitle

import (
	"path/filepath"
	"strings"
)

// iso6392to1 maps ISO-639-2 three-letter codes (both the bibliographic /B and
// terminological /T variants ffprobe may emit) to their ISO-639-1 two-letter
// code. Only the languages a household library realistically carries are listed;
// an unlisted code normalizes to "" (Unknown), never a wrong guess.
var iso6392to1 = map[string]string{
	"eng": "en",
	"fre": "fr", "fra": "fr",
	"ger": "de", "deu": "de",
	"spa": "es",
	"ita": "it",
	"por": "pt",
	"rus": "ru",
	"jpn": "ja",
	"chi": "zh", "zho": "zh",
	"kor": "ko",
	"dut": "nl", "nld": "nl",
	"swe": "sv",
	"nor": "no",
	"dan": "da",
	"fin": "fi",
	"pol": "pl",
	"ara": "ar",
	"heb": "he",
	"hin": "hi",
	"tur": "tr",
	"cze": "cs", "ces": "cs",
	"gre": "el", "ell": "el",
	"hun": "hu",
	"tha": "th",
	"vie": "vi",
	"ukr": "uk",
	"rum": "ro", "ron": "ro",
	"ind": "id",
	"may": "ms", "msa": "ms",
	"cat": "ca",
}

// nameToISO1 maps common English (and a few endonym) language names to their
// ISO-639-1 code, so a sidecar or tag written "English"/"French" still matches.
var nameToISO1 = map[string]string{
	"english": "en",
	"french":  "fr", "francais": "fr", "français": "fr",
	"german": "de", "deutsch": "de",
	"spanish": "es", "espanol": "es", "español": "es", "castellano": "es",
	"italian": "it", "italiano": "it",
	"portuguese": "pt", "portugues": "pt", "português": "pt",
	"russian":  "ru",
	"japanese": "ja",
	"chinese":  "zh", "mandarin": "zh",
	"korean":     "ko",
	"dutch":      "nl",
	"swedish":    "sv",
	"norwegian":  "no",
	"danish":     "da",
	"finnish":    "fi",
	"polish":     "pl",
	"arabic":     "ar",
	"hebrew":     "he",
	"hindi":      "hi",
	"turkish":    "tr",
	"czech":      "cs",
	"greek":      "el",
	"hungarian":  "hu",
	"thai":       "th",
	"vietnamese": "vi",
	"ukrainian":  "uk",
	"romanian":   "ro",
	"indonesian": "id",
	"malay":      "ms",
	"catalan":    "ca",
}

// iso1ToName is the canonical English display name per ISO-639-1 code, the
// inverse used to label a track. Built from the primary spelling in nameToISO1.
var iso1ToName = map[string]string{
	"en": "English", "fr": "French", "de": "German", "es": "Spanish",
	"it": "Italian", "pt": "Portuguese", "ru": "Russian", "ja": "Japanese",
	"zh": "Chinese", "ko": "Korean", "nl": "Dutch", "sv": "Swedish",
	"no": "Norwegian", "da": "Danish", "fi": "Finnish", "pl": "Polish",
	"ar": "Arabic", "he": "Hebrew", "hi": "Hindi", "tr": "Turkish",
	"cs": "Czech", "el": "Greek", "hu": "Hungarian", "th": "Thai",
	"vi": "Vietnamese", "uk": "Ukrainian", "ro": "Romanian", "id": "Indonesian",
	"ms": "Malay", "ca": "Catalan",
}

// iso6391 is the set of valid ISO-639-1 codes we recognize (the values above),
// used to validate a two-letter input for pass-through.
var iso6391 = func() map[string]bool {
	s := map[string]bool{}
	for _, c := range iso6392to1 {
		s[c] = true
	}
	return s
}()

// legacyISO1 maps deprecated ISO-639-1 codes to their current form (iw→he,
// in→id), so an old tag still folds to the modern code.
var legacyISO1 = map[string]string{
	"iw": "he", "in": "id",
}

// undetermined are the tag values that explicitly mean "no known language";
// they normalize to "" (Unknown) rather than a bogus code.
var undetermined = map[string]bool{
	"und": true, "undetermined": true, "unknown": true,
	"mul": true, "zxx": true, "mis": true,
}

// NormalizeLang folds a raw language tag from any source — an ffprobe stream tag
// ("eng"), a two-letter code ("en"), a BCP-47 tag ("en-US"), or an English name
// ("English") — to one canonical ISO-639-1 code ("en"). An empty, undetermined,
// or unrecognized value returns "" (surfaced as "Unknown"), so a rescan never
// invents a language it can't justify.
func NormalizeLang(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	// Drop a region/script subtag: en-US, pt_BR, zh-Hans → primary subtag.
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	if undetermined[s] {
		return ""
	}
	if c, ok := legacyISO1[s]; ok {
		return c
	}
	switch len(s) {
	case 2:
		if iso6391[s] {
			return s
		}
		return ""
	case 3:
		if c, ok := iso6392to1[s]; ok {
			return c
		}
		return ""
	}
	if c, ok := nameToISO1[s]; ok {
		return c
	}
	return ""
}

// DisplayLang returns the English display name for a normalized ISO-639-1 code,
// or "Unknown" for an empty/unrecognized code.
func DisplayLang(code string) string {
	if name, ok := iso1ToName[code]; ok {
		return name
	}
	return "Unknown"
}

// imageCodecs are the bitmap subtitle formats (embedded codec names and sidecar
// format tokens) that cannot become text and must be burned in on transcode.
var imageCodecs = map[string]bool{
	"hdmv_pgs_subtitle": true, "pgssub": true, "pgs": true, "sup": true,
	"dvd_subtitle": true, "dvdsub": true, "vobsub": true,
	"dvb_subtitle": true, "dvbsub": true,
	"xsub": true,
}

// KindForCodec classifies a subtitle by its codec/format token into "text"
// (selectable, WebVTT-convertible) or "image" (bitmap, burn-in only). Anything
// not known to be a bitmap format is treated as text — the common, safe default.
func KindForCodec(codec string) string {
	if imageCodecs[strings.ToLower(strings.TrimSpace(codec))] {
		return "image"
	}
	return "text"
}

// subFlags are the disposition-ish flags a sidecar filename can carry after the
// language code (Movie.en.forced.srt, Movie.en.sdh.srt). Note "hi" (a
// hearing-impaired abbreviation) is deliberately NOT here: it collides with the
// ISO-639-1 code for Hindi, and a bare "Movie.hi.srt" overwhelmingly means the
// Hindi language, so it is parsed as a language, not a flag.
var subFlags = map[string]bool{
	"forced": true, "sdh": true, "cc": true,
}

// ParseSidecar splits a sidecar subtitle's filename into its media stem and the
// language + forced flag encoded as dotted suffixes: "Movie.en.forced.srt" →
// stem "Movie", lang "en", forced true; "Movie.srt" → stem "Movie", lang "",
// forced false. It walks trailing tokens right-to-left, consuming flags and
// language codes until a token that is neither — that token (and everything
// left of it) is the stem, so a dotted title word (Some.Movie) is never
// mistaken for a language. Either ordering (Movie.en.forced / Movie.forced.en)
// is tolerated; the rightmost language token wins; parts[0] is always stem.
// The stem lets a caller tell a title sidecar from an extra's (Movie-trailer).
func ParseSidecar(name string) (stem, lang string, forced bool) {
	base := name
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	parts := strings.Split(base, ".")
	end := len(parts) // exclusive index of the first consumed suffix token
	for i := len(parts) - 1; i >= 1; i-- {
		tok := strings.ToLower(strings.TrimSpace(parts[i]))
		if subFlags[tok] {
			if tok == "forced" {
				forced = true
			}
			end = i
			continue
		}
		if code := NormalizeLang(tok); code != "" {
			if lang == "" {
				lang = code
			}
			end = i
			continue
		}
		break // the stem — stop consuming
	}
	return strings.Join(parts[:end], "."), lang, forced
}

// ParseSidecarName is ParseSidecar without the stem, for callers that only need
// the language and forced flag.
func ParseSidecarName(name string) (lang string, forced bool) {
	_, lang, forced = ParseSidecar(name)
	return lang, forced
}

// Label builds the human menu label for a track from its (normalized) language
// and forced flag: "English", "English (Forced)", "Unknown".
func Label(lang string, forced bool) string {
	label := DisplayLang(lang)
	if forced {
		label += " (Forced)"
	}
	return label
}
