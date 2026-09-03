// Package link is the HOME half of linking (ADR-0055, ADR-0056): the side that
// holds a credential for another household's Server, decides how to reach it,
// and speaks to it.
//
// The sharing half lives in internal/auth (minting and redeeming the invite) and
// internal/store (the export). Nothing here runs on a sharing Server; nothing
// there runs on a home one. Both halves ship in the same binary because every
// Server can be either side, but they meet only over HTTP.
//
// # What this package owns
//
//   - Parsing the one string an Admin pastes (invite.go).
//   - Choosing between ADR-0055 §5's two dialers per origin (dialer.go).
//   - Talking to the peer: the handshake probe, the redemption, the logout
//     (peer.go).
//   - The Link's life: create, re-key, unlink, list (service.go).
//
// # What it deliberately does not own
//
// The MIRROR. Creating the linked Libraries and pulling the export are issues 07
// and 08; this package leaves them a named seam (Service.OnLinked /
// Service.OnUnlinked) and nothing more. A Link that exists with no Libraries
// behind it is the correct intermediate state, not a half-built one.
package link

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// InviteScheme prefixes the one string a sharing Admin sends (ADR-0055 §2). It
// is not an https:// URL: there is no hosted page to open it against, and the
// action it triggers belongs on THIS Server, which the string cannot name.
const InviteScheme = "obelo-link:"

// Invite is a decoded `obelo-link:` string. It is the sharer's half of the
// contract — everything this Server needs to find that one and prove it was
// invited — and nothing in it is trusted until the probe confirms it: the
// server id is checked against the handshake, and the origins are addresses to
// TRY, not addresses to believe.
type Invite struct {
	// Version is the link-protocol version the sharer stamped (ADR-0055 §3),
	// checked for equality before anything is dialed. It is read first and the
	// rest of the payload is only meaningful if it matches.
	Version int
	// ServerID and ServerName are the sharer's identity (ADR-0034). The id is what
	// makes a re-key a re-key: a fresh invite carrying an id already on file
	// updates that Link rather than creating a second one.
	ServerID   string
	ServerName string
	// Origins are the addresses to try, IN ORDER, as typed by the sharing Admin.
	Origins []string
	// Code is the one-time secret, spent exactly once at the other end.
	Code string
	// ExpiresAt is when the code dies over there. Checked here too, so an invite
	// somebody opened next week fails on this side with a sentence about the
	// expiry rather than on theirs with a generic refusal.
	ExpiresAt time.Time
}

// ErrBadInvite is every shape failure of the string: the wrong scheme, base64
// that does not decode, JSON that does not parse, a field missing or nonsense.
// ONE error for all of them, because the operator's move is identical in every
// case — ask for the string again — and because a paste that lost its last
// character should not produce a different sentence than one that lost its
// first.
var ErrBadInvite = errors.New("link: this is not a valid invite")

// ErrInviteExpired is a well-formed invite whose 24 hours have run out
// (ADR-0055 §1). It is deliberately NOT ErrBadInvite: the string is fine, the
// friend simply sent it yesterday, and the fix is to ask for a fresh one rather
// than to re-copy this one.
var ErrInviteExpired = errors.New("link: this invite has expired")

// ParseInvite decodes the string an Admin pasted. It validates SHAPE only —
// every semantic check (the version, whether the id matches what answers, the
// expiry) belongs to the caller, which has a clock and a network.
func ParseInvite(s string) (Invite, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), InviteScheme)
	if !ok {
		return Invite{}, ErrBadInvite
	}
	// Padding is tolerated on the way IN though it is never emitted: the string
	// travels through chat clients and QR readers, and an encoder somewhere else
	// that pads is not the operator's mistake to debug.
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(rest, "="))
	if err != nil {
		return Invite{}, ErrBadInvite
	}

	var p struct {
		V       int      `json:"v"`
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Origins []string `json:"origins"`
		Code    string   `json:"code"`
		Exp     string   `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// Unknown fields are ACCEPTED, unlike every request body this server decodes.
	// A newer sharer may stamp a field this build has never heard of, and the
	// version — which is checked — is what decides whether the two can talk. A
	// strict decode here would turn every additive change into a parse error on
	// the far side, which is the one thing the version exists to prevent.
	if err := dec.Decode(&p); err != nil {
		return Invite{}, ErrBadInvite
	}

	if p.V <= 0 || p.ID == "" || p.Code == "" || len(p.Origins) == 0 {
		return Invite{}, ErrBadInvite
	}
	exp, err := time.Parse(time.RFC3339, p.Exp)
	if err != nil {
		return Invite{}, ErrBadInvite
	}

	origins := make([]string, 0, len(p.Origins))
	for _, o := range p.Origins {
		origin, ok := cleanOrigin(o)
		if !ok {
			return Invite{}, ErrBadInvite
		}
		origins = append(origins, origin)
	}

	return Invite{
		Version:    p.V,
		ServerID:   p.ID,
		ServerName: p.Name,
		Origins:    origins,
		Code:       p.Code,
		ExpiresAt:  exp,
	}, nil
}

// Expired reports whether the invite's 24 hours have run out.
func (i Invite) Expired(now time.Time) bool { return !now.Before(i.ExpiresAt) }

// cleanOrigin accepts a bare absolute http(s) origin and returns it
// canonicalized (a lone trailing "/" folded away, the host lowercased).
//
// It is a SECOND, deliberately independent check of what the minting side
// already validated (api.normalizeOrigin). The two are not shared code because
// they are not the same rule read from the same side: over there an Admin is
// typing and a refusal means "fix your input"; here a string arrived from
// another household and a refusal means "do not dial this". A home Server that
// trusted the far side's validation would be trusting the far side, which is
// exactly what an origin list is not entitled to.
func cleanOrigin(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Host == "" || u.User != nil {
		return "", false
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), true
}
