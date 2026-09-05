package access

import "strings"

// The Playback-ceiling vocabulary (CONTEXT.md "Playback ceiling", ADR-0054 §2).
// A ceiling says HOW a Title may play for one User — a maximum resolution, a
// maximum bitrate, and a maximum number of concurrent streams — and is clamped
// into the session's Constraints before negotiation, so it changes the tier a
// File plays at and never whether the Title exists.
//
// The resolution rung is a token, not a height, because that is the vocabulary
// the negotiator's Constraints already speak (internal/playback/profile.go's
// resolutionHeight): storing "1080p" means the clamp is a straight assignment
// into Constraints.MaxResolution rather than a conversion the two packages could
// disagree about.
//
// This package deliberately does NOT import playback to reuse that ladder:
// playback imports access (the Scope rides on every Request), so the dependency
// only goes one way. The settable set here is the narrow one an operator picks
// from — the same shape as the maturity ladder, which access also owns rather
// than borrowing from the catalog.
var playbackResolutions = map[string]int{
	"720p":  720,
	"1080p": 1080,
	"2160p": 2160,
}

// canonicalResolution folds a requested ceiling token to its canonical spelling
// and reports whether it is settable. Case and surrounding space are forgiven
// ("1080P" is the same rung); anything else — a height, a codec level, a rung we
// do not offer — is not settable and the caller rejects the whole write.
func canonicalResolution(token string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(token))
	if _, ok := playbackResolutions[t]; !ok {
		return "", false
	}
	return t, true
}
