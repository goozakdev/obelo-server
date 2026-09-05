package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The three calls this Server makes to another household's, and nothing else.
// Every one of them is to a documented path on the unified API (ADR-0010), and
// every response is read defensively: what comes back was written by a machine
// this operator does not administer.

// apiPrefix is where the peer serves its API. It is a constant rather than
// something discovered because there is nothing to discover: the contract
// versions via URL path, and a Server that does not serve /api/v1 is not a
// Server this one can link to.
const apiPrefix = "/api/v1"

// maxPeerBody bounds what is read from a peer. The three responses here are a
// handshake, a login response and an empty logout; a megabyte is orders of
// magnitude more than any of them and is the difference between "that origin
// answered something odd" and a server that fills its memory because somebody
// pointed it at a file server.
const maxPeerBody = 1 << 20

// Peer is what the handshake told us about the Server at an origin — the four
// facts that decide whether linking to it can work at all (ADR-0055 §3).
type Peer struct {
	ID                  string
	Name                string
	ServerLinking       bool
	LinkProtocolVersion int
}

// ErrNotObelo is an origin that answered with something that is not an Obelo
// handshake. It is an ORIGIN-level failure, not a link-level one: the operator
// may have typed one address wrong out of two, so the caller tries the next.
var ErrNotObelo = errors.New("link: that address did not answer as an Obelo server")

// ErrWrongServer is an origin that answered as a DIFFERENT Server than the
// invite names. Also origin-level: an invite listing a stale address that now
// points at somebody else's machine must not link to it, but the other origins
// in the same invite are still worth trying.
var ErrWrongServer = errors.New("link: that address is a different server than the invite names")

// ErrInviteRefused is the sharer answering INVALID_INVITE: the code is unknown,
// expired or already spent over there. Distinct from ErrBadInvite (this side
// could not read the string) because the operator's next move differs — this
// one means the string was fine and the CODE is gone.
var ErrInviteRefused = errors.New("link: the sharing server refused this invite; ask for a fresh one")

// ProtocolMismatch is the refusal ADR-0055 §3 requires: two Servers stamping
// different link-protocol versions, refused at paste time with a message naming
// WHICH side needs an upgrade, never mid-sync and never mid-film.
//
// Theirs is 0 for a Server that does not advertise features.serverLinking at
// all — it speaks no version of this protocol, which is the same conclusion and
// the same fix ("their server needs an upgrade"), so it is not a separate error.
type ProtocolMismatch struct {
	Theirs int
	Ours   int
}

func (e *ProtocolMismatch) Error() string {
	return fmt.Sprintf("link: the other server speaks link protocol %d, this one speaks %d",
		e.Theirs, e.Ours)
}

// Upgrade names the side that has to move: "theirs" when the other Server is
// behind, "ours" when this one is. It is computed here rather than in the API
// layer so the two sentences ADR-0055 §3 promises come from one place.
func (e *ProtocolMismatch) Upgrade() string {
	if e.Theirs < e.Ours {
		return "theirs"
	}
	return "ours"
}

// probe is the handshake read (GET /server) that must happen BEFORE anything is
// redeemed. It is what makes the version check cost the Admin nothing: an
// invite refused here is still live, and can be redeemed once both sides agree.
func (s *Service) probe(ctx context.Context, client *http.Client, origin string) (Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+apiPrefix+"/server", nil)
	if err != nil {
		return Peer{}, ErrNotObelo
	}
	resp, err := client.Do(req)
	if err != nil {
		return Peer{}, fmt.Errorf("%w: %v", ErrNotObelo, err)
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return Peer{}, fmt.Errorf("%w: it answered %d", ErrNotObelo, resp.StatusCode)
	}
	var body struct {
		ID                  string          `json:"id"`
		Name                string          `json:"name"`
		Features            map[string]bool `json:"features"`
		LinkProtocolVersion int             `json:"linkProtocolVersion"`
	}
	if err := decodeBody(resp, &body); err != nil {
		return Peer{}, ErrNotObelo
	}
	if body.ID == "" {
		// A Server predating ADR-0034 has no identity, and without one a re-key
		// could never find this Link again. That is not a version mismatch, it is
		// something this design cannot build on.
		return Peer{}, ErrNotObelo
	}
	return Peer{
		ID:                  body.ID,
		Name:                body.Name,
		ServerLinking:       body.Features["serverLinking"],
		LinkProtocolVersion: body.LinkProtocolVersion,
	}, nil
}

// redemption is what a spent invite bought: an ordinary Device-bound bearer
// (ADR-0055 §1). Nothing link-specific comes back, and that is the design.
type redemption struct {
	Token string
	// DeviceID is the Device the sharer created for this Server (ADR-0055 §4). It
	// is kept for one purpose — the unlink, which deletes it so no ghost with a
	// last-seen is left behind. Empty from a Server that does not report one.
	DeviceID string
}

// redeem spends the code at the origin that answered, presenting this Server's
// own identity as the Device (ADR-0055 §4).
func (s *Service) redeem(ctx context.Context, client *http.Client, origin string, inv Invite) (redemption, error) {
	self := s.self()
	body, err := json.Marshal(map[string]any{
		"code":                inv.Code,
		"linkProtocolVersion": s.version,
		"server": map[string]any{
			"id":   self.ID,
			"name": self.Name,
		},
	})
	if err != nil {
		return redemption{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		origin+apiPrefix+"/auth/link/redeem", bytes.NewReader(body))
	if err != nil {
		return redemption{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return redemption{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return redemption{}, s.redeemFailure(resp)
	}
	var out struct {
		Token  string `json:"token"`
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
		LinkProtocolVersion int `json:"linkProtocolVersion"`
	}
	if err := decodeBody(resp, &out); err != nil || out.Token == "" {
		return redemption{}, fmt.Errorf("%w: it accepted the invite but returned no token", ErrNotObelo)
	}
	return redemption{Token: out.Token, DeviceID: out.Device.ID}, nil
}

// redeemFailure turns the sharer's error envelope into one of this package's
// errors. The two that carry meaning are the version conflict and the refused
// invite; anything else is reported as the transport failure it effectively is,
// with the peer's own status in the message so an operator has something to
// take to the other household.
func (s *Service) redeemFailure(resp *http.Response) error {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = decodeBody(resp, &env)

	switch {
	case resp.StatusCode == http.StatusConflict && env.Error.Code == "LINK_PROTOCOL":
		// The sharer names the versions from ITS side — it "supports" one and we
		// "requested" another (issue 03) — so supported is theirs.
		theirs := s.version
		if n, ok := env.Error.Details["supported"].(float64); ok {
			theirs = int(n)
		}
		return &ProtocolMismatch{Theirs: theirs, Ours: s.version}
	case resp.StatusCode == http.StatusBadRequest && env.Error.Code == "INVALID_INVITE":
		return ErrInviteRefused
	}
	msg := env.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("status %d", resp.StatusCode)
	}
	return fmt.Errorf("%w: the sharing server refused the redemption (%s)", ErrUnreachable, msg)
}

// surrender hands the credential back on unlink (ADR-0055, Consequences): a
// best-effort call so the sharer's Device row disappears instead of lingering as
// a ghost with a last-seen.
//
// IT IS `DELETE /devices/{id}` AND NOT THE `POST /auth/logout` THE ADR NAMES,
// and the difference is the whole requirement. Logout deletes a TOKEN; the
// Device row survives it, still listed on the sharer's Users page with the date
// this household last called. Deleting the Device removes both — its tokens
// cascade — which is the outcome the ADR describes in the same sentence. The
// logout is kept as the fallback for a Link made before the Device id was
// recorded, so the credential is surrendered either way.
//
// Every error here is returned for LOGGING and never acted on. A friend whose
// server is off must not be able to prevent this household from unlinking — the
// whole point of unlink is that this side is done.
func (s *Service) surrender(ctx context.Context, client *http.Client, l store.Link) error {
	if l.ActiveOrigin == "" || l.Token == "" {
		return nil
	}
	if l.DeviceID != "" {
		return s.call(ctx, client, http.MethodDelete,
			l.ActiveOrigin+apiPrefix+"/devices/"+url.PathEscape(l.DeviceID), l.Token)
	}
	return s.call(ctx, client, http.MethodPost,
		l.ActiveOrigin+apiPrefix+"/auth/logout", l.Token)
}

func (s *Service) call(ctx context.Context, client *http.Client, method, target, token string) error {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer closeBody(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("link: the sharing server answered %d", resp.StatusCode)
	}
	return nil
}

// decodeBody reads a bounded JSON body. Content-Type is checked loosely: an
// origin that is not an Obelo at all is far more likely to answer HTML than to
// answer JSON that happens to parse, and reading a 404 page as a handshake
// produces a confusing failure two steps later.
func decodeBody(resp *http.Response, dst any) error {
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(strings.ToLower(ct), "json") {
		return errors.New("link: the response was not JSON")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxPeerBody)).Decode(dst)
}

// closeBody drains and closes, so the connection can be reused for the very next
// call to the same peer instead of being torn down between the probe and the
// redemption.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPeerBody))
	_ = resp.Body.Close()
}
