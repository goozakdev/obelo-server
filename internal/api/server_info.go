package api

import (
	"net/http"

	"github.com/marioquake/juicebox/internal/server"
)

// serverInfoResponse is the camelCase JSON shape of GET /api/v1/server defined
// in docs/api-contract.md: the Server identity, server version, supported API
// versions, a feature-flags map, and setupRequired.
type serverInfoResponse struct {
	// ID and Name are the Server identity (ADR-0034). Both are omitempty, which is
	// what makes them additive: a client written against this contract must treat
	// them as optional, and a server that cannot resolve an identity degrades to
	// the pre-ADR-0034 shape rather than advertising empty strings.
	//
	// Neither is a secret — this endpoint is [Unauthenticated], the id is an opaque
	// UUID granting nothing, and the name is chosen by the operator.
	ID                string          `json:"id,omitempty"`
	Name              string          `json:"name,omitempty"`
	Version           string          `json:"version"`
	SupportedVersions []int           `json:"supportedVersions"`
	Features          map[string]bool `json:"features"`
	SetupRequired     bool            `json:"setupRequired"`
}

// handleServerInfo serves the handshake. It is the one real endpoint in this
// slice; it lets a client of any age detect capabilities via feature flags
// rather than version-sniffing.
func handleServerInfo(meta *server.Metadata) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := meta.Info()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to assemble server info", nil)
			return
		}
		writeJSON(w, http.StatusOK, serverInfoResponse{
			ID:                info.Identity.ID,
			Name:              info.Identity.Name,
			Version:           info.Version,
			SupportedVersions: info.SupportedVersions,
			Features:          info.Features,
			SetupRequired:     info.SetupRequired,
		})
	}
}
