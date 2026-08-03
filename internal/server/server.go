// Package server holds server-wide metadata and feature advertisement: the
// data behind the GET /api/v1/server handshake (ADR-0010, api-contract.md).
//
// Keeping this separate from the HTTP layer lets the api package stay a thin
// transport over domain values, and lets later slices grow feature flags and
// version negotiation without touching routing code.
package server

// Version is the server build version. A real build would stamp this via
// -ldflags; for the skeleton a constant is sufficient.
const Version = "0.1.0"

// SupportedAPIVersions lists the API major versions this server can serve.
// The contract versions via URL path (/api/v1); clients branch on this list
// and the feature flags, not on the server version string.
var SupportedAPIVersions = []int{1}

// Info is the payload behind the handshake. The api layer serializes it to the
// camelCase JSON shape defined in docs/api-contract.md.
type Info struct {
	// Identity is the Server's stable id + display name (ADR-0034). Additive to
	// the handshake, so it never bumps the API major version; a client written
	// against an older server must treat both as optional.
	Identity          Identity
	Version           string
	SupportedVersions []int
	Features          map[string]bool
	SetupRequired     bool
}

// UserCounter reports how many Users exist. *store.DB satisfies it; tests can
// substitute a fake. This is the only dependency server metadata has on the
// store, keeping the seam clean.
type UserCounter interface {
	CountUsers() (int, error)
}

// Capabilities are the startup-resolved facts about THIS deployment that the
// handshake advertises but that cannot be answered by "does the binary serve this
// route" — the ones that depend on the host the server happens to be running on.
// They are resolved once, before Metadata is built, and only read from here after:
// Info() is the unauthenticated endpoint every client hits on connect, so it must
// never probe anything (ADR-0009's setup-time posture, applied to the handshake).
//
// A zero Capabilities is the honest answer for a deployment where nothing
// host-dependent resolved — every flag here is false-by-default and false means
// "not available", never "unknown".
type Capabilities struct {
	// Transcode reports whether a usable ffmpeg was resolved at startup, and so
	// whether the remux/transcode delivery tiers of ADR-0003 can run here at all.
	// It comes from transcode.Availability; the server package deliberately takes
	// the resolved bool rather than importing the transcode domain, keeping the
	// handshake a thin advertiser of decisions made elsewhere (ADR-0006).
	Transcode bool
}

// Metadata assembles handshake Info. It is the single source of truth for what
// the server advertises.
type Metadata struct {
	users    UserCounter
	identity Identity
	caps     Capabilities
}

// NewMetadata builds a Metadata backed by the given user counter, advertising the
// given Identity (ADR-0034) and the given startup-resolved Capabilities. A zero
// Identity is legal — the handshake simply omits the fields, which is what a
// client sees from a server predating ADR-0034.
func NewMetadata(users UserCounter, identity Identity, caps Capabilities) *Metadata {
	return &Metadata{users: users, identity: identity, caps: caps}
}

// Identity returns the Server identity this metadata advertises. The mDNS
// advertiser reads it from here so there is exactly one source of truth for what
// the handshake and the TXT record claim.
func (m *Metadata) Identity() Identity { return m.identity }

// Features returns the advertised feature-flags map. Clients branch on these
// rather than version strings, so a flag that lags its routes is a bug: it tells
// every client to hide a feature this server serves. Flip the flag in the same
// commit that lands the route, and keep TestFeaturesMatchRoutes honest.
func (m *Metadata) Features() map[string]bool {
	return map[string]bool{
		"auth":           true,
		"libraries":      true,
		"scanner":        true,
		"directPlay":     true,
		"watchState":     true,
		"home":           true,
		"search":         true,
		"collections":    true,
		"playlists":      true,
		"realtimeEvents": true,
		// deviceAuth advertises the Device authorization grant (ADR-0036) — the
		// QR/one-time-code sign-in. A client MUST branch on this rather than on a
		// version: an older server has no /auth/device/* routes, and the correct
		// behaviour there is to fall back to the username/password form, which
		// every client keeps anyway as the manual path.
		"deviceAuth": true,
		// remuxSelectedOnly advertises the playback route's remuxSelectedOnly request
		// field (PRD remux-selected): the client may force a lean, copy-only directStream
		// (one video + one audio Stream) on an otherwise-directPlay File. A client MUST
		// branch on this rather than a version — an older server rejects the unknown field
		// with 400 — and grey the "Force Remux on Server" affordance when it is absent.
		"remuxSelectedOnly": true,
		// mediaCookieRefresh advertises POST /auth/media-cookie: a bearer-authenticated
		// re-issue of the HttpOnly ms_media cookie carrying the CURRENT bearer's token
		// (appletv-parity/12). The web instant user switch swaps the bearer from JS but
		// cannot touch the HttpOnly cookie, so it calls this to flip browser byte-serving
		// to the switched-in identity. A client MUST branch on this rather than a version:
		// an older server has no such route, and the correct behaviour there is to skip the
		// refresh (media falls back to today's behaviour until the next real login).
		"mediaCookieRefresh": true,
		// streamToken advertises the path-carried media routes
		// (GET /stream/{streamToken}/stream and .../hls/{file}, ADR-0039) plus the
		// streamToken/streamTokenExpiresAt fields on a Decision and
		// POST /sessions/{id}/stream-token. A client MUST branch on this rather than a
		// version: an older server 404s those paths, and the correct behaviour there is
		// to hide any affordance that hands a media URL to somebody else (AirPlay video,
		// a cast receiver) — because the failure surfaces as a television going black,
		// not as an error the app can catch. Every other credential path is unchanged,
		// so an absent flag costs nothing else.
		"streamToken": true,
		// transcode is the one flag that is NOT route-existence — which is why
		// TestFeaturesMatchRoutes excludes it: /transcoding is only the admin
		// observability snapshot (ADR-0029) and is served either way. It advertises the
		// transcode DELIVERY TIER (ADR-0003), computed at startup from whether a usable
		// ffmpeg was resolved on this host (transcode.Availability). True means the
		// remux/transcode half of negotiation can actually run here; false means this
		// deployment has no working ffmpeg, so every playback that is not direct play
		// will fail, and the correct client behaviour is to HIDE the affordance rather
		// than offer it and collect an error. A client that cannot direct-play a File
		// (an AVPlayer profile facing Matroska, say) should treat a false flag as "this
		// Title is unplayable here" rather than attempting negotiation.
		"transcode": m.caps.Transcode,
	}
}

// Info computes the current handshake payload, including setupRequired, which
// is true while zero Users exist (the server still needs its first Admin
// bootstrapped — ADR-0013).
func (m *Metadata) Info() (Info, error) {
	n, err := m.users.CountUsers()
	if err != nil {
		return Info{}, err
	}
	return Info{
		Identity:          m.identity,
		Version:           Version,
		SupportedVersions: SupportedAPIVersions,
		Features:          m.Features(),
		SetupRequired:     n == 0,
	}, nil
}
