package api

import (
	"net/http"

	"github.com/marioquake/juicebox/internal/server"
)

// serverInfoResponse is the camelCase JSON shape of GET /api/v1/server defined
// in docs/api-contract.md: server version, supported API versions, a
// feature-flags map, and setupRequired.
type serverInfoResponse struct {
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
			Version:           info.Version,
			SupportedVersions: info.SupportedVersions,
			Features:          info.Features,
			SetupRequired:     info.SetupRequired,
		})
	}
}
