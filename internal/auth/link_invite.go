package auth

import (
	"errors"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The sharing half of linking (ADR-0055 §1, §4): an Admin mints a one-time
// invite for a `remote` User, and another Server redeems it once for an ordinary
// Device-bound bearer.
//
// This is the Device authorization grant with the roles reversed. There, the
// device that wanted a session minted the code and a person approved it. Here
// the SHARER mints and a MACHINE redeems, which removes both of the things the
// grant needed a second, human-sized code for: there is no screen to read a code
// off and no person to approve. What is left is one secret, one expiry, and a
// compare-and-swap — and the tail is identical, because a redeemed invite lands
// in issueSession exactly as a redeemed device code does. A linked Server holds
// nothing more exotic than a Device token.
//
// Like the rest of the package this file is transport-agnostic: it never sees
// the `obelo-link:` string, which is the api layer's encoding of the sharer's
// identity, its origins and this code (ADR-0055 §2).

const (
	// linkInviteTTL is 24 hours, where the device code's is five minutes, and the
	// difference is entirely about who is standing where. The TV flow has a person
	// at both ends of the same room; here the sharing Admin sends a message and the
	// friend may open it tomorrow (ADR-0055 §1). A day is the shortest window that
	// does not turn "I'll do it tonight" into a support request.
	//
	// The cost of the longer window is bounded by what the code IS: 256 bits of
	// crypto/rand, stored only as a hash, single-use, and useless without also
	// knowing an origin that answers. Unlike the four-character user code there is
	// nothing here to guess at, so the TTL is not defending a small space — it is
	// only limiting how long a code left in a chat log stays live.
	linkInviteTTL = 24 * time.Hour

	// LinkDevicePlatform is the Device platform a redeeming Server presents
	// (ADR-0055 §4). It is set HERE and never taken from the request, so a linked
	// Server cannot describe itself as a phone on somebody else's Devices list.
	LinkDevicePlatform = "server"

	// The per-address limit on redemption attempts. This endpoint is
	// unauthenticated by necessity — the redeeming Server has no credential yet,
	// which is the entire point of the invite — so, exactly like password login
	// (login_limit.go), the only thing that can be keyed on is the source address.
	//
	// It counts FAILURES, not successes: a redemption that worked spent a real
	// invite, and a household linking two Servers does that once. Fifteen failed
	// attempts from one address in a quarter hour is already far past a
	// mistyped-paste, and nowhere near enough to walk a 256-bit space — which
	// nothing can, and which is why this limiter is a nuisance-bound rather than
	// the security property. The security property is the entropy of the code.
	//
	// Its own counter rather than a share of loginIPFails, deliberately: a stranger
	// guessing at invites must not be able to lock the household out of password
	// login, and an operator fumbling their password must not consume a friend's
	// budget for redeeming an invite they were just sent.
	linkRedeemFailureLimit  = 15
	linkRedeemFailureWindow = 15 * time.Minute
)

var (
	// ErrNotRemoteUser: an invite was requested for a User that is not a linked
	// Server (→ 422). An invite is the `remote` role's ONLY credential and no
	// other role has any use for one — minting for a Member would hand out a
	// passwordless second way into a person's account (ADR-0054, ADR-0055 §1).
	ErrNotRemoteUser = errors.New("auth: an invite can only be minted for a remote user")

	// ErrInvalidInvite is the ONE answer for unknown, expired and already-redeemed
	// (→ 400 INVALID_INVITE). The collapse is deliberate and matches the device
	// grant's treatment of a user code: a caller who could tell "expired" from
	// "never existed" from "already spent" would learn which codes are live, and
	// there is nothing a legitimate redeemer does differently in the three cases
	// anyway — all three mean "ask for a fresh invite".
	ErrInvalidInvite = errors.New("auth: invalid, expired or already redeemed invite")

	// ErrLinkRedeemThrottled: this source address is over linkRedeemFailureLimit.
	ErrLinkRedeemThrottled = errors.New("auth: too many invite redemptions attempted from this address")
)

// LinkRedeemThrottledError is what RedeemLinkInvite returns when it refuses on
// the rate limit. It carries what is left of the window so the api layer can
// answer with Retry-After; a caller that does not care matches
// errors.Is(err, ErrLinkRedeemThrottled) and never learns this type exists.
//
// The same shape as LoginThrottledError and DeviceAuthThrottledError rather than
// a type shared with either: three unrelated limits on three unrelated
// endpoints, and one type would mean a handler could not tell which it held.
type LinkRedeemThrottledError struct {
	// RetryAfter is the time remaining on the tripped window — an upper bound,
	// since the window is fixed and reopens whole.
	RetryAfter time.Duration
}

func (e *LinkRedeemThrottledError) Error() string { return ErrLinkRedeemThrottled.Error() }

// Unwrap makes errors.Is(err, ErrLinkRedeemThrottled) true, so handlers switch on
// the sentinel like every other auth error and the carried duration is additive.
func (e *LinkRedeemThrottledError) Unwrap() error { return ErrLinkRedeemThrottled }

// LinkInvite is a freshly minted invite. Code is the raw secret — returned
// exactly once, here, and never again, since only its hash is stored. The api
// layer packs it into the `obelo-link:` string and hands that to the Admin; this
// server has no way to recover it afterwards, which is the same promise
// ADR-0015 makes about a bearer token.
type LinkInvite struct {
	Code      string
	ExpiresAt time.Time
}

// MintLinkInvite creates a one-time invite for the given `remote` User,
// invalidating any unspent invite it already had.
//
// Re-minting replaces rather than accumulates (ADR-0055 §1: "a lapsed or spent
// Invite is replaced by minting a new one"). An Admin who mints twice because
// the first message went astray must not leave two live codes behind — the one
// they just copied is the one that works, and any older string in a chat log is
// dead the moment this returns.
//
// It refuses every role but `remote` with ErrNotRemoteUser, and an unknown User
// with ErrUserNotFound.
func (s *Service) MintLinkInvite(userID string) (LinkInvite, error) {
	user, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return LinkInvite{}, ErrUserNotFound
	}
	if err != nil {
		return LinkInvite{}, err
	}
	if user.Role != RoleRemote {
		return LinkInvite{}, ErrNotRemoteUser
	}

	now := s.now()
	nowStr := formatTime(now)

	// The reaper, on the device grant's cadence: sweep at mint time, so the table
	// stays tidy without a background goroutine. Nothing depends on it having run
	// — expiry is enforced in RedeemLinkInvite's WHERE clause.
	if err := s.store.DeleteExpiredLinkInvites(nowStr); err != nil {
		return LinkInvite{}, err
	}
	if err := s.store.DeleteUnredeemedLinkInvites(userID); err != nil {
		return LinkInvite{}, err
	}

	code, err := newToken()
	if err != nil {
		return LinkInvite{}, err
	}
	expires := now.Add(linkInviteTTL)
	if err := s.store.InsertLinkInvite(store.LinkInvite{
		CodeHash:  hashToken(code),
		UserID:    userID,
		CreatedAt: nowStr,
		ExpiresAt: formatTime(expires),
	}); err != nil {
		return LinkInvite{}, err
	}
	return LinkInvite{Code: code, ExpiresAt: expires}, nil
}

// RedeemLinkInvite spends an invite for the redeeming Server, returning an
// ordinary session — the same LoginResult a password login or a device-code
// redemption produces, because a linked Server's credential is an ordinary
// Device-bound bearer and nothing more (ADR-0055 §1).
//
// serverID and serverName are the REDEEMING Server's own identity (ADR-0034),
// presented as the Device clientId and name (ADR-0055 §4). Keying the Device on
// the server id is what makes re-linking from the same household update one row
// instead of leaving a trail of dead ones, and what lets the sharer's Users page
// say "Brandon's server" with a real last-seen. The platform is not taken from
// the caller — see LinkDevicePlatform.
//
// clientIP is the caller's source address, taken as a plain string for the same
// reason Login takes one: this package stays transport-agnostic. Pass "" if
// there genuinely is none; those callers share one bucket, which is stricter,
// not looser.
//
// Every refusal that is about the code is ErrInvalidInvite — see its comment for
// why unknown, expired and spent are one answer.
func (s *Service) RedeemLinkInvite(code, serverID, serverName, clientIP string) (LoginResult, error) {
	now := s.now()
	if ok, retryAfter := s.linkRedeemFails.allow(clientIP, now); !ok {
		return LoginResult{}, &LinkRedeemThrottledError{RetryAfter: retryAfter}
	}
	if code == "" || serverID == "" {
		s.linkRedeemFails.charge(clientIP, now)
		return LoginResult{}, ErrInvalidInvite
	}

	// The compare-and-swap IS the single-use rule; there is no read-then-write to
	// race against. It also collapses expiry into the same miss, which is why an
	// expired invite and a spent one are indistinguishable from here on.
	inv, err := s.store.RedeemLinkInvite(hashToken(code), formatTime(now))
	if errors.Is(err, store.ErrNotFound) {
		s.linkRedeemFails.charge(clientIP, now)
		return LoginResult{}, ErrInvalidInvite
	}
	if err != nil {
		// A store failure is our fault, not the caller's; charging it would let a
		// sick database lock out the Server trying to link.
		return LoginResult{}, err
	}

	user, err := s.store.UserByID(inv.UserID)
	if errors.Is(err, store.ErrNotFound) {
		// The User was deleted between mint and redeem. The FK cascade normally
		// takes the invite with it, so this is the narrow window rather than the
		// normal path — and it answers as an invalid invite, which is exactly what
		// it now is.
		return LoginResult{}, ErrInvalidInvite
	}
	if err != nil {
		return LoginResult{}, err
	}
	if user.Role != RoleRemote {
		// Belt and braces. MintLinkInvite refuses every other role and
		// CheckRoleChange forbids crossing the `remote` boundary in either
		// direction (ADR-0054), so nothing should reach here — but a token is about
		// to be minted, and the guard that stops a person's account being handed out
		// passwordlessly is worth stating twice. The invite is already spent by now,
		// which is the safe direction: a code that reached an impossible state does
		// not get a second try.
		return LoginResult{}, ErrInvalidInvite
	}

	// The shared tail: password login, the device grant and this all land here, so
	// the three ways of signing in cannot drift apart in what they produce.
	return s.issueSession(user, DeviceInput{
		Name:     serverName,
		Platform: LinkDevicePlatform,
		ClientID: serverID,
	})
}
