package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/discovery"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// Admin-scope Tailnet remote-access settings (ADR-0043), the shape the
// metadata-provider subtree established: a GET that joins the persisted settings
// with live state, a PUT that validates everything before it writes anything, and
// — new here — three imperative verbs, because connecting to somebody else's
// network is an ACTION with a moment, not a field with a value.
//
// Every route is Admin-only (wired behind requireAuth + requireAdmin at
// /settings/ in api.go). Remote access is the operator's configuration; a Member
// gets the same refusal here as on any other admin route.
//
// The MagicDNS name appears in this response and no other. It is an authenticated
// admin surface; publishing the name on the unauthenticated handshake is issue 05
// and is deliberately gated, because an unauthenticated scanner hitting a
// port-forward must not learn that a Tailnet exists or what it is called.

// TailnetSettingsStore is the persistence the Tailnet settings handlers read and
// write. *store.DB satisfies it; the narrow interface keeps the HTTP layer
// testable (mirroring ProviderSettingsStore).
type TailnetSettingsStore interface {
	TailnetSettings() (store.TailnetSettings, error)
	SetTailnetSettings(u store.TailnetSettingsUpsert) error
}

// --- Wire shapes ------------------------------------------------------------

// tailnetStatusJSON is the live condition of the node — never persisted, always
// read from the running state machine.
type tailnetStatusJSON struct {
	State string `json:"state"`
	// FQDN / Addresses are how a client reaches this Server from away, empty until
	// the node is up.
	FQDN      string   `json:"fqdn,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// KeyExpiry is when this node's membership lapses, and it is deliberately NOT
	// omitempty: null means "no expiry" (a tagged node), which is a genuinely
	// different — and better — state than "expires soon", and a field that vanishes
	// leaves the UI unable to tell it from a field it forgot to send.
	KeyExpiry *string `json:"keyExpiry"`
	// LoginURL is the interactive-join link, present while the node is waiting on a
	// human (and again once a key has lapsed).
	LoginURL string `json:"loginURL,omitempty"`
	// LastError is why the node is in the error state, in words for a person. It is
	// a warning surfaced, never a request that failed.
	LastError string `json:"lastError,omitempty"`
	// HTTPSBound is whether tailnet :443 is accepting connections RIGHT NOW, and it
	// is the field the displayed address depends on. It is NOT httpsEnabled above:
	// that one is the operator's request, this one is what the server achieved, and
	// they come apart whenever the two Tailscale console prerequisites are missing.
	// A client that renders the request as though it were the outcome sends people
	// to an https:// address that refuses connections.
	//
	// NOT omitempty, unlike every other optional field here: false is the whole
	// point of it, and a field that vanishes when it is false cannot be told from a
	// server that never sends it.
	HTTPSBound bool `json:"httpsBound"`
	// HTTPSError is why it is not bound, in the server's own words — the same
	// paragraph the log carries, naming both console settings. Verbatim on the
	// client, like LastError: it is the only actionable part.
	HTTPSError string `json:"httpsError,omitempty"`
}

// tailnetResponse is the GET/PUT/verb body: the persisted settings plus the live
// status. One shape for all five routes, so a client that has just connected does
// not need a second request to learn what happened.
type tailnetResponse struct {
	Enabled      bool              `json:"enabled"`
	Hostname     string            `json:"hostname"`
	ControlURL   string            `json:"controlURL"`
	HTTPSEnabled bool              `json:"httpsEnabled"`
	Status       tailnetStatusJSON `json:"status"`
}

// updateTailnetRequest is the PUT body. Every field is a pointer so "omitted"
// (nil) is distinguishable from an explicit value — the metadata-provider rule,
// and the reason a client can save a hostname without also having to restate
// whether the node should be up.
type updateTailnetRequest struct {
	Enabled      *bool   `json:"enabled,omitempty"`
	Hostname     *string `json:"hostname,omitempty"`
	ControlURL   *string `json:"controlURL,omitempty"`
	HTTPSEnabled *bool   `json:"httpsEnabled,omitempty"`
}

// --- Routing ----------------------------------------------------------------

// handleTailnetSubtree dispatches the /settings/tailscale subtree:
//
//	GET  /settings/tailscale            → settings + live status (the source of truth)
//	PUT  /settings/tailscale            → update settings, applied with no restart
//	POST /settings/tailscale/connect    → bring the node up
//	POST /settings/tailscale/disconnect → take it down, KEEPING its state
//	POST /settings/tailscale/forget     → take it down and WIPE its state
//
// Disconnect and forget are separate routes rather than a flag on one, because
// they mean different things to the operator: "not right now" and "not this
// Tailnet ever again". An operator who meant the first and got the second finds
// out when they try to come back.
//
// rest is the path with the /settings/ prefix already stripped.
func handleTailnetSubtree(deps Deps, rest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(strings.TrimPrefix(rest, "tailscale"), "/")
		switch action {
		case "":
			switch r.Method {
			case http.MethodGet:
				handleGetTailnet(deps)(w, r)
			case http.MethodPut:
				handleUpdateTailnet(deps)(w, r)
			default:
				w.Header().Set("Allow", "GET, PUT")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
					"method not allowed", nil)
			}
		case "connect":
			requireMethod(http.MethodPost, handleTailnetVerb(deps, tailnetConnect))(w, r)
		case "disconnect":
			requireMethod(http.MethodPost, handleTailnetVerb(deps, tailnetDisconnect))(w, r)
		case "forget":
			requireMethod(http.MethodPost, handleTailnetVerb(deps, tailnetForget))(w, r)
		default:
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		}
	}
}

// --- GET --------------------------------------------------------------------

func handleGetTailnet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TailnetSettings == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"remote access is not available on this server", nil)
			return
		}
		resp, err := buildTailnetResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read remote access settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- PUT --------------------------------------------------------------------

func handleUpdateTailnet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TailnetSettings == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"remote access is not available on this server", nil)
			return
		}
		var req updateTailnetRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// Resolve the partial update against what is on file and validate the WHOLE
		// desired state before persisting any of it, so a rejected update leaves the
		// settings exactly as they were (handleUpdateProviders' discipline).
		cur, err := deps.TailnetSettings.TailnetSettings()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read remote access settings", nil)
			return
		}
		desired := store.TailnetSettingsUpsert{
			Enabled:      cur.Enabled,
			Hostname:     cur.Hostname,
			ControlURL:   cur.ControlURL,
			HTTPSEnabled: cur.HTTPSEnabled,
		}
		if req.Enabled != nil {
			desired.Enabled = *req.Enabled
		}
		if req.HTTPSEnabled != nil {
			desired.HTTPSEnabled = *req.HTTPSEnabled
		}
		if req.Hostname != nil {
			h := strings.TrimSpace(*req.Hostname)
			// Strict, and this is the field to be strict about. It becomes a DNS label
			// at join time, where a bad one fails inside the coordination client with a
			// message written for a different audience — and the operator's own name for
			// their server would come back mangled or rejected long after they typed it.
			// Rejecting here, against the same sanitizer that produces the .local name,
			// is the only point where the message can name the field and the rule.
			if h == "" || discovery.DNSLabel(h) != h {
				writeError(w, http.StatusUnprocessableEntity, codeTailnetInvalidHostname,
					"hostname must be a single DNS label: letters, digits and hyphens only, "+
						"not starting or ending with a hyphen, at most 63 characters (e.g. \"obelo\")", nil)
				return
			}
			desired.Hostname = h
		}
		if req.ControlURL != nil { // omitted = unchanged; "" = Tailscale's own; value = a Headscale
			desired.ControlURL = strings.TrimSpace(*req.ControlURL)
			if desired.ControlURL != "" && !validBaseURL(desired.ControlURL) {
				writeError(w, http.StatusUnprocessableEntity, codeTailnetInvalidControlURL,
					"control URL must be an absolute http(s) URL, or empty to use Tailscale's coordination server", nil)
				return
			}
		}
		// A hostname was never seeded (a settings row that predates this feature, or a
		// narrow install) and none was supplied: fill the default rather than joining
		// under "", which the coordination server would reject at the worst moment.
		if desired.Hostname == "" {
			desired.Hostname = tailnet.FallbackHostname
		}

		if err := deps.TailnetSettings.SetTailnetSettings(desired); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to save remote access settings", nil)
			return
		}
		// Apply with no restart: enabling connects, disabling disconnects, and a
		// changed hostname re-joins under the new name. Its error is a SETTINGS-READ
		// failure only — a node that will not start is a warning already logged and
		// already visible in the status below, never an HTTP failure (ADR-0043).
		if deps.Tailnet != nil {
			if err := deps.Tailnet.Apply(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to apply remote access settings", nil)
				return
			}
		}

		resp, err := buildTailnetResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read remote access settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- POST connect / disconnect / forget --------------------------------------

// tailnetVerb is one of the three imperative state-machine transitions.
type tailnetVerb int

const (
	tailnetConnect tailnetVerb = iota
	tailnetDisconnect
	tailnetForget
)

// handleTailnetVerb drives one transition and answers with the same body the GET
// returns, so the caller sees the resulting state without a second request.
//
// A node that fails to come up is answered 200 with a status of "error" and the
// reason in lastError — NOT an HTTP error. That is deliberate and it is ADR-0043's
// posture: a coordination server that is down is the world being unavailable, not
// the operator's mistake, and a 500 here would be the server claiming a fault it
// did not have while the panel had nothing to show for it. The failures that DO
// come back as errors are the ones the operator can act on: a database that will
// not write, and a state directory Forget could not wipe.
func handleTailnetVerb(deps Deps, verb tailnetVerb) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TailnetSettings == nil || deps.Tailnet == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"remote access is not available on this server", nil)
			return
		}
		var err error
		switch verb {
		case tailnetConnect:
			err = deps.Tailnet.Connect(r.Context())
		case tailnetDisconnect:
			err = deps.Tailnet.Disconnect(r.Context())
		case tailnetForget:
			err = deps.Tailnet.Forget(r.Context())
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to change remote access state", nil)
			return
		}
		resp, buildErr := buildTailnetResponse(deps)
		if buildErr != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read remote access settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- Shared view builder ----------------------------------------------------

// buildTailnetResponse joins the persisted settings with the live node status.
// With no state machine wired (a narrow unit test, or a build with no Tailnet
// support) the status is the honest resting one — stopped — rather than an
// omitted field the UI would have to guess at.
func buildTailnetResponse(deps Deps) (tailnetResponse, error) {
	s, err := deps.TailnetSettings.TailnetSettings()
	if err != nil {
		return tailnetResponse{}, err
	}
	status := tailnet.Status{State: tailnet.StateStopped}
	if deps.Tailnet != nil {
		status = deps.Tailnet.Status()
	}
	return tailnetResponse{
		Enabled:      s.Enabled,
		Hostname:     s.Hostname,
		ControlURL:   s.ControlURL,
		HTTPSEnabled: s.HTTPSEnabled,
		Status:       statusJSON(status),
	}, nil
}

// statusJSON shapes a live Status onto the wire. KeyExpiry crosses as a nullable
// RFC3339 string: nil stays null (no expiry — a tagged node), a value renders in
// UTC like every other timestamp this API emits.
func statusJSON(s tailnet.Status) tailnetStatusJSON {
	out := tailnetStatusJSON{
		State:     string(s.State),
		FQDN:      s.FQDN,
		LoginURL:  s.LoginURL,
		LastError: s.LastError,
		// Straight through from the state machine's report of an OBSERVED bind. There
		// is deliberately no view logic here — no defaulting from the settings row, no
		// "well, HTTPS is enabled, so": that inference is the bug.
		HTTPSBound: s.HTTPSBound,
		HTTPSError: s.HTTPSError,
	}
	for _, a := range s.Addresses {
		out.Addresses = append(out.Addresses, a.String())
	}
	if s.KeyExpiry != nil {
		at := s.KeyExpiry.UTC().Format(time.RFC3339)
		out.KeyExpiry = &at
	}
	return out
}
