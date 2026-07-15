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

// Metadata assembles handshake Info. It is the single source of truth for what
// the server advertises.
type Metadata struct {
	users    UserCounter
	identity Identity
}

// NewMetadata builds a Metadata backed by the given user counter, advertising the
// given Identity (ADR-0034). A zero Identity is legal — the handshake simply omits
// the fields, which is what a client sees from a server predating ADR-0034.
func NewMetadata(users UserCounter, identity Identity) *Metadata {
	return &Metadata{users: users, identity: identity}
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
		// transcode is not a route-existence flag: /transcoding is only the
		// admin observability snapshot (ADR-0029). It advertises the transcode
		// delivery tier, which depends on a resolved ffmpeg backend, so it stays
		// false until it is computed from that backend rather than hardcoded.
		"transcode": false,
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
