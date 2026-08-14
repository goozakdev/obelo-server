package api

import (
	"net/http"

	"github.com/marioquake/obelo-server/internal/auth"
	"github.com/marioquake/obelo-server/internal/server"
	"github.com/marioquake/obelo-server/internal/tailnet"
)

// serverInfoResponse is the camelCase JSON shape of GET /api/v1/server defined
// in docs/api-contract.md: the Server identity, server version, supported API
// versions, a feature-flags map, setupRequired, and the Tailnet address.
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

	// TailnetURL is the ORIGIN a signed-in client should use to reach this Server
	// from away — "https://obelo.tail1a2b.ts.net" — or null (ADR-0043). It is the
	// field that makes "no addresses to remember" true: a client that paired on the
	// LAN learns it here and falls back to it when the LAN address stops answering,
	// which works because a Device's token is bound to a Device row and not to an
	// address (ADR-0015).
	//
	// A POINTER, and NEVER omitempty, matching KeyExpiry's reasoning on the
	// settings payload: null is this server SAYING it has no Tailnet address right
	// now, which is a different and more useful statement than a field that simply
	// is not there.
	//
	// **Null or absent OBLIGES a client to CLEAR its stored address.** That is the
	// contract and not the consumer's choice: an operator who turns the feature off
	// must not leave every paired client probing a dead name on every cold start
	// forever. The obligation is documented in the imperative in
	// docs/api-contract.md because the natural implementation — keep the last value
	// you saw — is silently wrong in exactly the case that matters.
	//
	// See tailnetURLFor for who gets a value and who gets null.
	TailnetURL *string `json:"tailnetURL"`
}

// handleServerInfo serves the handshake. It is the one real endpoint in this
// slice; it lets a client of any age detect capabilities via feature flags
// rather than version-sniffing.
//
// It is [Unauthenticated] and STAYS SO. It reads a bearer when one is offered,
// for the single purpose of deciding whether to include TailnetURL — see
// tailnetURLFor for why that must never become a 401.
func handleServerInfo(meta *server.Metadata, authSvc *auth.Service, node *tailnet.Manager) http.HandlerFunc {
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
			TailnetURL:        tailnetURLFor(r, authSvc, node),
		})
	}
}

// tailnetURLFor returns the Tailnet origin for this request, or nil.
//
// THE FIELD IS GATED, NOT THE ROUTE, AND THAT DISTINCTION IS LOAD-BEARING.
//
// The obvious implementation of "authenticated only" is to wrap the route in
// requireAuth. It is a bug. requireAuth answers a missing OR invalid bearer with
// 401, and the Apple client drives token-drop from that status everywhere — so a
// handshake that started 401-ing would present to every client in the house as a
// revoked token and log people out. The handshake must answer 200 to anyone who
// can reach the port, exactly as it always has; authentication decides only
// whether this one field carries a value.
//
// So: no bearer, a malformed one, an expired one, a revoked one — all return nil
// here, and the response is 200 with "tailnetURL": null. There is no input to
// this function that can produce a non-200. TestServerInfoNeverAnswers401 pins
// that, and it is named for the reason rather than for the mechanism.
//
// The cost of gating the field rather than the route lands on the client and is
// documented in ADR-0043: "no bearer" and "valid bearer, no Tailnet" are the same
// null on the wire, so a client cannot clear its stored address on a null without
// first knowing its own request carried a known-good token.
func tailnetURLFor(r *http.Request, authSvc *auth.Service, node *tailnet.Manager) *string {
	if authSvc == nil || node == nil {
		return nil
	}
	raw, ok := bearerToken(r)
	if !ok {
		return nil
	}
	if _, err := authSvc.Authenticate(raw); err != nil {
		// Deliberately indistinguishable from "no bearer". See the doc comment: the
		// alternative is a 401 that reads as revocation.
		return nil
	}
	origin, ok := tailnetOrigin(node.Status())
	if !ok {
		return nil
	}
	return &origin
}

// tailnetOrigin renders a Status as the origin a client should dial, reporting
// whether there is one at all.
//
// Two rules, both settled with the client team that consumes this:
//
//   - THE NAME ONLY, never the node's Tailnet IPs. The IPs are the thing the name
//     exists to replace, and they move; worse, on Apple platforms no certificate
//     can match an IP literal and App Transport Security refuses cleartext to one
//     regardless, so an IP would fail on the media channel while JSON worked.
//   - THE SCHEME ACHIEVED, never the one requested. HTTPSBound is an observed
//     bind; HTTPSEnabled is what the operator asked for, and the two come apart
//     whenever the Tailscale console prerequisites are missing. Sending the
//     requested scheme is how the admin UI once told operators to use an https://
//     address that refused connections.
//
// No port is ever emitted: the Tailnet listeners are :80 and :443, which are the
// defaults for their schemes, so an origin with a port would be wrong as well as
// noisy.
func tailnetOrigin(s tailnet.Status) (string, bool) {
	if s.FQDN == "" {
		return "", false
	}
	if s.HTTPSBound {
		return "https://" + s.FQDN, true
	}
	return "http://" + s.FQDN, true
}
