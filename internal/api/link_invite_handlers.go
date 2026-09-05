package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/server"
)

// The sharing half of linking, over HTTP (ADR-0055 §1–§4): an Admin mints an
// invite for a `remote` User, and another household's Server redeems it once.
//
// One of the two endpoints is unauthenticated, and — as with the Device
// authorization grant — that is the design rather than an oversight: the
// redeeming Server has no credential on this one yet, which is the entire reason
// an invite exists. What stands in for auth is the shape of the secret: 256 bits
// of crypto/rand, stored only as a hash, valid for a day, and dead the moment it
// is spent.

// inviteScheme prefixes the one string an Admin sends (ADR-0055 §2). It is NOT
// an https:// URL, and deliberately: there is no hosted page to open it against,
// and the action it triggers belongs on the REDEEMING Server, which the string
// cannot name. A custom scheme is what lets a phone hand it to the right app
// instead of to a browser.
const inviteScheme = "obelo-link:"

// invitePayload is what the invite string carries, base64url-encoded
// (ADR-0055 §2). Nothing in it is a secret except Code, and the whole point of
// the design is that the string is safe in a chat: the origins are not secret,
// the identity is public over GET /server, and the code is single-use and
// expires in a day.
//
// The field names are the wire contract between two Servers and are short
// because the whole thing has to survive being pasted and shown as a QR code.
type invitePayload struct {
	// V is the link-protocol version (ADR-0055 §3). First field on purpose: the
	// redeeming Server reads it before it trusts anything else in here.
	V int `json:"v"`
	// ID and Name are the SHARER's Server identity (ADR-0034). The id is what lets
	// the home Server recognise the same Server later, so a re-key from a fresh
	// invite updates the existing Link instead of creating a second one.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Origins are the addresses to try, in order, as typed by the sharing Admin.
	// This Server does not know its own public address and never emits one
	// (ADR-0005), which is why they are an input rather than something derived.
	Origins []string `json:"origins"`
	// Code is the one-time secret. It exists in this response and nowhere else on
	// this Server — only its SHA-256 is stored.
	Code string `json:"code"`
	// Exp is the expiry, RFC 3339, so the redeeming side can say "this invite
	// expired yesterday" without a round trip.
	Exp string `json:"exp"`
}

// --- POST /users/{id}/invite (Admin) ---------------------------------------

type mintInviteRequest struct {
	// Origins are the addresses the sharing Admin typed. The dialog pre-fills the
	// MagicDNS origin when the Tailnet node is connected and offers a free-text
	// field for a public HTTPS origin (issue 04); this endpoint only validates and
	// carries them.
	Origins []string `json:"origins"`
}

type mintInviteResponse struct {
	// Invite is the whole `obelo-link:` string — one field, because "a hostname
	// and a code" is two fields, two mistakes and no room for a second address.
	Invite string `json:"invite"`
	// ExpiresAt is the same instant the payload's `exp` carries, hoisted out so
	// the dialog can say "expires tomorrow at 4pm" without decoding the string it
	// was just handed.
	ExpiresAt string `json:"expiresAt"`
}

// handleMintLinkInvite mints a one-time invite for a `remote` User (Admin
// scope). The raw code exists only in this response: minting again is how a
// lapsed or spent invite is replaced, and doing so kills whatever unspent invite
// this User had.
func handleMintLinkInvite(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req mintInviteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		origins, err := normalizeOrigins(req.Origins)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, codeInvalidOrigin, err.Error(), nil)
			return
		}

		invite, err := deps.Auth.MintLinkInvite(id)
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case errors.Is(err, auth.ErrNotRemoteUser):
			writeError(w, http.StatusUnprocessableEntity, codeNotRemoteUser,
				"an invite can only be minted for a linked server (role remote)", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not mint an invite", nil)
			return
		}

		identity := deps.Meta.Identity()
		expires := invite.ExpiresAt.UTC().Format(time.RFC3339)
		encoded, err := encodeInvite(invitePayload{
			V:       server.LinkProtocolVersion,
			ID:      identity.ID,
			Name:    identity.Name,
			Origins: origins,
			Code:    invite.Code,
			Exp:     expires,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not encode the invite", nil)
			return
		}
		writeJSON(w, http.StatusCreated, mintInviteResponse{
			Invite:    encoded,
			ExpiresAt: expires,
		})
	}
}

// encodeInvite renders the payload as `obelo-link:<base64url(JSON)>`.
//
// RawURLEncoding — no padding — because the string is pasted into chats and
// scanned from QR codes, and a trailing '=' is the character most likely to be
// eaten by a link-detector or a line wrap. The URL alphabet keeps the whole
// string free of '+' and '/', which some clients turn into %2B and spaces.
func encodeInvite(p invitePayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return inviteScheme + base64.RawURLEncoding.EncodeToString(raw), nil
}

// normalizeOrigins validates the Admin-typed addresses and returns them
// canonicalized, in the order given — the order IS meaningful (ADR-0055 §2: the
// home Server tries them in order and remembers the one that answered), so
// nothing here sorts or dedupes beyond dropping exact repeats.
//
// An origin is a scheme and an authority and NOTHING else. A path is refused
// rather than trimmed because "https://example.org/obelo" almost always means
// the operator is running behind a path-prefixing proxy, which this server does
// not support (ADR-0005) — silently dropping the prefix would produce an invite
// that fails later with an unexplainable 404 on the other household's machine.
// A trailing "/" alone is not a path and is folded away, since that is what a
// browser's address bar hands you when you copy an origin.
func normalizeOrigins(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		origin, err := normalizeOrigin(raw)
		if err != nil {
			return nil, err
		}
		if seen[origin] {
			continue
		}
		seen[origin] = true
		out = append(out, origin)
	}
	if len(out) == 0 {
		// An invite with no address names no Server to redeem against, so it could
		// only ever fail on the other side — with nothing on THIS side to explain
		// why. Refuse it here, where the Admin is looking at the dialog.
		return nil, errors.New("at least one origin is required")
	}
	return out, nil
}

func normalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("an origin must not be empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("origin " + strconv.Quote(s) + " is not a valid URL")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", errors.New("origin " + strconv.Quote(s) +
			" must be an absolute http:// or https:// address")
	}
	if u.Host == "" {
		return "", errors.New("origin " + strconv.Quote(s) + " has no host")
	}
	if u.User != nil {
		return "", errors.New("origin " + strconv.Quote(s) + " must not carry credentials")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("origin " + strconv.Quote(s) +
			" must be a bare origin with no path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin " + strconv.Quote(s) +
			" must not carry a query or fragment")
	}
	// Host is lowercased (a hostname is case-insensitive and the port is digits),
	// so two spellings of the same address dedupe.
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// --- POST /auth/link/redeem (unauthenticated) ------------------------------

type redeemInviteRequest struct {
	Code string `json:"code"`
	// LinkProtocolVersion is the REDEEMING Server's version, checked for equality
	// before anything is spent (ADR-0055 §3).
	LinkProtocolVersion int `json:"linkProtocolVersion"`
	// Server is the redeeming Server's own identity (ADR-0034), which becomes the
	// Device row on this side (ADR-0055 §4). There is no `platform` field: it is
	// always "server", set by the auth layer, so a linked Server cannot present
	// itself as a phone.
	Server struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"server"`
}

// redeemInviteResponse is a LoginResponse plus the version. The first three
// fields are byte-identical to POST /auth/login's — that is the contract, and
// the reason this returns a Device and a bearer rather than anything
// link-specific: what a Link holds IS an ordinary Device-bound token.
//
// The media cookie is deliberately NOT set here. Every other session-minting
// endpoint sets it for the browser that will byte-serve with it; the caller here
// is a Go process on another household's machine, which has no cookie jar and no
// use for one.
type redeemInviteResponse struct {
	Token               string     `json:"token"`
	User                userJSON   `json:"user"`
	Device              deviceJSON `json:"device"`
	LinkProtocolVersion int        `json:"linkProtocolVersion"`
}

// handleRedeemLinkInvite spends an invite for the Server that presents it.
//
// Unauthenticated and rate-limited per client IP, like the rest of the
// unauthenticated auth surface. Gated by features.serverLinking in the sense
// that matters to a client: the flag is route existence, this is the route, and
// TestFeaturesMatchRoutes ties the two together so the flag cannot advertise a
// route this server does not serve (or hide one it does).
func handleRedeemLinkInvite(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req redeemInviteRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// The version is checked BEFORE anything is redeemed (ADR-0055 §3), which is
		// why it comes ahead of every other validation here. A mismatch must cost
		// the Admin nothing: the invite they sent is still live, and re-minting
		// would not have helped anyway.
		if req.LinkProtocolVersion != server.LinkProtocolVersion {
			writeError(w, http.StatusConflict, codeLinkProtocol,
				"the two servers speak different link protocol versions; "+
					"whichever is lower needs an upgrade",
				map[string]any{
					// Named from the answering side, which is the only stable frame: this
					// server SUPPORTS one version and the caller REQUESTED another. The
					// redeeming Server compares them to decide which of the two messages
					// ADR-0055 §3 names — "their server needs an upgrade" or "yours does".
					"supported": server.LinkProtocolVersion,
					"requested": req.LinkProtocolVersion,
				})
			return
		}

		res, err := svc.RedeemLinkInvite(req.Code, req.Server.ID, req.Server.Name, clientIP(r))
		switch {
		case errors.Is(err, auth.ErrLinkRedeemThrottled):
			var throttled *auth.LinkRedeemThrottledError
			if errors.As(err, &throttled) {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(throttled.RetryAfter)))
			}
			writeError(w, http.StatusTooManyRequests, codeTooManyAttempts,
				"too many invites redeemed from this address; wait and try again", nil)
			return
		case errors.Is(err, auth.ErrInvalidInvite):
			// ONE answer for unknown, expired, already-spent, and a missing server id.
			// Telling them apart would let a caller map which invites are live, and no
			// legitimate redeemer acts differently on the difference: every one of them
			// means "ask for a fresh invite".
			writeError(w, http.StatusBadRequest, codeInvalidInvite,
				"this invite is not valid; ask for a fresh one", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not redeem the invite", nil)
			return
		}

		writeJSON(w, http.StatusOK, redeemInviteResponse{
			Token:               res.Token,
			User:                toUserJSON(res.User),
			Device:              toDeviceJSON(res.Device),
			LinkProtocolVersion: server.LinkProtocolVersion,
		})
	}
}
