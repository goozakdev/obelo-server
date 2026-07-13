package scanner

import (
	"path/filepath"
	"regexp"
	"strings"
)

// This file holds the filename-detail grammar from docs/naming-convention.md
// "Editions, parts, and extras" and "Extras and ignored files": classifying a
// recognized file inside a movie folder into a main-video Edition/part, an
// extra, junk, artwork, or a sidecar subtitle. The folder name carries identity
// (identity.go); the filename carries only this detail.

// mediaExtensions is the recognized-media allowlist (naming-convention.md).
// Anything not on it is ignored entirely. Video and audio share one centralized
// allowlist: video for Movie/TV, audio (`.flac .mp3 .m4a .ogg .opus .wav`) for
// the Music kind (issue tv-music/03).
var mediaExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true,
	".mov": true, ".ts": true, ".webm": true,
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true, ".opus": true, ".wav": true,
}

// audioExtensions is the subset of the allowlist that is audio — the recognized
// media in a Music Library. A Music scan only files audio files (a stray video
// in a Music Library is ignored, like a stray non-media file).
var audioExtensions = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true, ".opus": true, ".wav": true,
}

// isAudio reports whether name has a recognized audio extension (the Music
// Library's recognized media).
func isAudio(name string) bool {
	return audioExtensions[strings.ToLower(filepath.Ext(name))]
}

// artworkPoster / artworkBackground map a co-located artwork basename (lower,
// extension stripped) to its role (naming-convention.md "Local artwork").
var (
	artworkPosterNames     = map[string]bool{"poster": true, "cover": true}
	artworkBackgroundNames = map[string]bool{"fanart": true, "backdrop": true}
)

// imageExtensions are the artwork image extensions we honor.
var imageExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}

// subtitleExtensions are the sidecar-subtitle extensions we discover (text +
// image-based). Delivery/burn-in is a later slice; we only record presence.
var subtitleExtensions = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".vtt": true,
	".sup": true, ".sub": true, ".idx": true,
}

// extraSubfolders are the named subfolders whose contents are Extras attached to
// the parent Title (naming-convention.md). Keyed by lower-cased folder name.
var extraSubfolders = map[string]string{
	"extras":            "other",
	"trailers":          "trailer",
	"behind the scenes": "behindthescenes",
	"featurettes":       "featurette",
	"deleted scenes":    "deleted",
	"interviews":        "interview",
	"shorts":            "short",
	"other":             "other",
}

// extraSuffixes maps a filename suffix (the part after the last "-", lowered) to
// an extra type. A file ending in one of these is an Extra, not a main video.
var extraSuffixes = map[string]string{
	"trailer":         "trailer",
	"behindthescenes": "behindthescenes",
	"deleted":         "deleted",
	"featurette":      "featurette",
	"interview":       "interview",
	"short":           "short",
	"scene":           "scene",
	"clip":            "clip",
	"other":           "other",
}

// sampleSizeFloor is the size below which a recognized media file is treated as
// junk (a teaser/sample sliver), in bytes. Real movies are far larger; the tiny
// ffmpeg fixtures sit above this but a deliberate `sample.*` is also matched by
// name so size is a backstop, not the primary signal.
const sampleSizeFloor = 1 << 10 // 1 KiB

// junkRe matches sample/junk filenames: a bare "sample" or a "-sample" /
// ".sample" suffix (naming-convention.md "Sample/junk").
var junkRe = regexp.MustCompile(`(?i)(^sample$|[-_. ]sample$)`)

// qualityTokenRe extracts a resolution/source quality token from a filename
// segment, e.g. "2160p", "1080p", "720p", "bluray", "remux". Used to
// auto-distinguish Editions when no explicit {edition-…} tag is present.
var qualityTokenRe = regexp.MustCompile(`(?i)\b(\d{3,4}p|2160p|1080p|720p|480p|bluray|blu-ray|remux|web-?dl|webrip|hdtv|dvd|uhd|4k)\b`)

// partRe matches a multi-part suffix: part1/part 1, cd1, pt1, disc1, disk1
// (naming-convention.md aliases). Returns the part number.
var partRe = regexp.MustCompile(`(?i)[-_. ](?:part|pt|cd|disc|disk)[ _]?(\d+)\b`)

// editionTagRe is reused from identity.go for {edition-Name}.

// isMedia reports whether name has a recognized media (video) extension.
func isMedia(name string) bool {
	return mediaExtensions[strings.ToLower(filepath.Ext(name))]
}

// isHidden reports whether a directory entry is a hidden/dotfile that must never
// be treated as content: any name starting with ".". Most important are the "._"
// AppleDouble sidecars macOS/smbfs leaves next to real files on a network share —
// a resource-fork blob that carries the real file's extension (so isMedia accepts
// it) but is not playable media; also .DS_Store, .Trashes, .Spotlight-V100, etc.
// Filtered at the directory-read boundary (readDirResilient) so no classifier
// downstream (isMedia / isAudio / artworkRole / ParseEpisodeToken) ever sees them.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// isSubtitle reports whether name is a sidecar-subtitle file.
func isSubtitle(name string) bool {
	return subtitleExtensions[strings.ToLower(filepath.Ext(name))]
}

// artworkRole classifies an image file by basename into "poster", "background",
// or "" (not recognized artwork). It also honors a "<basename>-poster" suffix.
func artworkRole(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if !imageExtensions[ext] {
		return ""
	}
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	switch {
	case artworkPosterNames[base] || strings.HasSuffix(base, "-poster"):
		return "poster"
	case artworkBackgroundNames[base]:
		return "background"
	default:
		return ""
	}
}

// isJunk reports whether a recognized media file is sample/junk and must be
// ignored entirely (not even an extra). A sub-floor size is also junk.
func isJunk(name string, size int64) bool {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if junkRe.MatchString(base) {
		return true
	}
	if size > 0 && size < sampleSizeFloor {
		return true
	}
	return false
}

// extraTypeFromSuffix returns the extra type when the filename ends in a
// recognized extra suffix (e.g. "Movie-trailer"), else "".
func extraTypeFromSuffix(name string) string {
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	idx := strings.LastIndexAny(base, "-_")
	if idx < 0 {
		return ""
	}
	suffix := base[idx+1:]
	return extraSuffixes[suffix]
}

// extraTypeFromFolder returns the extra type when dir (the immediate parent
// folder name) is a recognized extras subfolder, else "".
func extraTypeFromFolder(dir string) string {
	return extraSubfolders[strings.ToLower(strings.TrimSpace(dir))]
}

// partNumber returns the multi-part number parsed from a filename (1-based), or
// 0 when the file is not a part.
func partNumber(name string) int {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if m := partRe.FindStringSubmatch(base); m != nil {
		n := 0
		for _, r := range m[1] {
			n = n*10 + int(r-'0')
		}
		return n
	}
	return 0
}

// editionName derives the Edition label for a main video file. An explicit
// {edition-Name} tag wins; otherwise a quality token (resolution/source) labels
// the Edition; otherwise "" (the single unnamed Edition). The result is also the
// per-file Edition discriminator: two files with the same editionName (and not
// parts) collide → ambiguous.
func editionName(name string, videoHeight int) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if m := editionTagRe.FindStringSubmatch(base); m != nil {
		return strings.TrimSpace(m[1])
	}
	if tok := qualityTokenRe.FindString(base); tok != "" {
		return canonicalQuality(tok)
	}
	// No filename signal: fall back to the ffprobed resolution so a folder with a
	// 4K and a 1080p rip (named only by resolution-less detail) still splits.
	return resolutionLabel(videoHeight)
}

// canonicalQuality normalizes a parsed quality token to a stable label.
func canonicalQuality(tok string) string {
	t := strings.ToLower(tok)
	switch t {
	case "blu-ray":
		return "bluray"
	case "web-dl", "webdl":
		return "webdl"
	case "uhd", "4k":
		return "2160p"
	default:
		return t
	}
}

// resolutionLabel maps a video height to a resolution label, "" when unknown.
func resolutionLabel(height int) string {
	switch {
	case height >= 2160:
		return "2160p"
	case height >= 1080:
		return "1080p"
	case height >= 720:
		return "720p"
	case height > 0:
		return "SD"
	default:
		return ""
	}
}
