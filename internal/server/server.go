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
	users UserCounter
}

// NewMetadata builds a Metadata backed by the given user counter.
func NewMetadata(users UserCounter) *Metadata {
	return &Metadata{users: users}
}

// Features returns the advertised feature-flags map. Clients branch on these
// rather than version strings. As later slices land, flags flip to true.
func (m *Metadata) Features() map[string]bool {
	return map[string]bool{
		"auth":           true,
		"libraries":      true,
		"scanner":        true,
		"directPlay":     true,
		"watchState":     true,
		"home":           true,
		"transcode":      false,
		"search":         false,
		"collections":    false,
		"playlists":      false,
		"realtimeEvents": false,
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
		Version:           Version,
		SupportedVersions: SupportedAPIVersions,
		Features:          m.Features(),
		SetupRequired:     n == 0,
	}, nil
}
