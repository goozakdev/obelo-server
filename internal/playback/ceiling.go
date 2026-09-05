package playback

import "github.com/goozakdev/obelo-server/internal/access"

// The per-User Playback ceiling (CONTEXT.md, ADR-0054 §2) meets negotiation
// here, and only here: the User's maximum resolution and bitrate are CLAMPED
// INTO the request's Constraints before anything is negotiated, so the existing
// tiering does what it always does. A 1080p Edition under a 1080p ceiling still
// direct-plays; a 4K-only File transcodes down under ADR-0009 governance and is
// refused with SERVER_BUSY when the budget is full. Nothing about the ceiling
// reaches the browse/read surface — a ceiling changes how a Title plays, never
// whether it exists.

// clampToCeiling returns c tightened to the Scope's Playback ceiling, and
// whether the ceiling actually bound (i.e. it, and not the client's own
// Constraints, is what limits this session — the userCeiling marker the Decision
// and Session carry for observability).
//
// The rule per dimension is "the stricter of the two wins, and zero means the
// other one": zero/"" on either side is an absence of opinion, not a limit of
// zero, so a client that declares nothing gets the User's ceiling and a client
// that asks for less than the ceiling keeps its own smaller number. An
// unrecognized ceiling token (height 0) is treated as no ceiling rather than as
// a hard block — a User is never made unable to play by a value the negotiator
// cannot read.
func clampToCeiling(c Constraints, s access.Scope) (Constraints, bool) {
	bound := false
	if ceiling := resolutionHeight(s.MaxResolution); ceiling > 0 {
		if asked := resolutionHeight(c.MaxResolution); asked == 0 || ceiling < asked {
			c.MaxResolution = s.MaxResolution
			bound = true
		}
	}
	if s.MaxBitrate > 0 && (c.MaxBitrate <= 0 || s.MaxBitrate < c.MaxBitrate) {
		c.MaxBitrate = s.MaxBitrate
		bound = true
	}
	return c, bound
}
