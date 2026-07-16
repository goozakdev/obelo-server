package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/juicebox/internal/store"
)

// The Device authorization grant (ADR-0036): a TV asks for a code, a phone
// approves it, the TV polls and collects a session. Modelled on RFC 8628, whose
// state machine this reproduces; the wire spelling is this API's own (the api
// layer maps these errors onto SCREAMING_SNAKE envelope codes).
//
// This file stays transport-agnostic like the rest of the package: it never
// builds the verification URL, because that needs the inbound request's host and
// scheme. The api layer owns that.

const (
	// deviceAuthTTL is how long a code is good for. Short, because the whole
	// window is "walk to your phone and scan": minutes, not hours. It is also the
	// blast radius of a guessed user code, so there is no reason to be generous.
	deviceAuthTTL = 5 * time.Minute

	// deviceAuthPollInterval is the poll cadence handed to the TV and enforced by
	// the slow-down rule. 2s makes approval feel immediate on screen while costing
	// the server ~150 lookups over a code's whole life.
	deviceAuthPollInterval = 2 * time.Second

	// slowDownGrace is the jitter the pacing rule forgives. A client that sleeps
	// exactly deviceAuthPollInterval between polls is OBEYING the interval, but
	// whether the server observes 2.001s or 1.999s comes down to scheduling and
	// clock resolution. Enforcing the interval to the nanosecond would punish a
	// correct client for noise it cannot control, so the rule is "meaningfully
	// faster than the interval", not "faster than the interval". A client actually
	// hammering the endpoint is orders of magnitude inside this, and still caught.
	slowDownGrace = 200 * time.Millisecond

	// maxLiveDeviceAuthRequests caps concurrent unexpired requests. A 4-char code
	// space (30^4) is small enough that an unbounded flood would crowd it: first
	// generation degrades into a retry loop, then it fails outright and a real TV
	// cannot sign in. A household needs single digits; 32 is far above real use
	// and far below the space.
	maxLiveDeviceAuthRequests = 32

	// userCodeAttempts bounds the collision re-roll. With the live cap above, the
	// odds of 8 consecutive collisions are indistinguishable from zero — this is a
	// guard against an infinite loop, not a real code path.
	userCodeAttempts = 8

	// The approve endpoint's brute-force limit: a User gets approveFailureLimit
	// wrong codes per approveFailureWindow before being refused outright.
	//
	// This is the one control standing between a 4-char code and enumeration. It
	// counts FAILURES only, so a household approving real codes never meets it,
	// and it is keyed by User (the endpoint is authenticated, so there is always
	// one) rather than by IP — an IP is shared by every device behind the NAT and
	// spoofable besides.
	approveFailureLimit  = 10
	approveFailureWindow = 5 * time.Minute
)

// Device-authorization errors. The api layer maps each to an envelope code; they
// are distinct because the TV shows different words for each, and "your code
// expired" versus "someone denied this" is not a distinction to collapse.
// There is deliberately no "denied" error, and no deny operation. Approval here
// is immediate on code entry — there is no confirmation screen to say no on, so
// nothing could ever reach a denied state. A user who signs in a TV they did not
// mean to already has recourse, and it is a better one: DELETE /devices/{id}
// revokes that Device's token instantly (ADR-0015). RFC 8628's access_denied is
// absent for the same reason; if a confirm step is ever added, it comes back
// with it.
var (
	ErrDeviceCodeUnknown  = errors.New("auth: unknown device code")
	ErrDeviceCodePending  = errors.New("auth: device code not yet approved")
	ErrDeviceCodeExpired  = errors.New("auth: device code expired")
	ErrDeviceCodeSlowDown = errors.New("auth: polling too fast")
	ErrUserCodeUnknown    = errors.New("auth: unknown user code")
	ErrTooManyAttempts    = errors.New("auth: too many failed attempts")
	ErrDeviceAuthBusy     = errors.New("auth: too many device authorizations in flight")
)

// DeviceAuthStart is what a TV receives when it begins a flow. DeviceCode is the
// raw poll secret — returned exactly once here and never again, since only its
// hash is stored.
type DeviceAuthStart struct {
	DeviceCode string
	UserCode   string
	ExpiresIn  time.Duration
	Interval   time.Duration
}

// StartDeviceAuth mints a pending request for a Device that wants to be signed
// in. It requires no credentials — that is the point of the grant — so the only
// thing standing behind it is the live-request cap.
func (s *Service) StartDeviceAuth(dev DeviceInput) (DeviceAuthStart, error) {
	if dev.ClientID == "" {
		return DeviceAuthStart{}, fmt.Errorf("auth: device.clientId is required")
	}
	now := s.now()
	nowStr := formatTime(now)

	// Sweep before counting and before generating: expired rows hold user_codes
	// hostage (the column is UNIQUE across all rows, not just live ones), so the
	// sweep is what keeps the small code space actually available.
	if err := s.store.DeleteExpiredDeviceAuthRequests(nowStr); err != nil {
		return DeviceAuthStart{}, err
	}
	live, err := s.store.CountLiveDeviceAuthRequests(nowStr)
	if err != nil {
		return DeviceAuthStart{}, err
	}
	if live >= maxLiveDeviceAuthRequests {
		return DeviceAuthStart{}, ErrDeviceAuthBusy
	}

	deviceCode, err := newDeviceCode()
	if err != nil {
		return DeviceAuthStart{}, err
	}

	for attempt := 0; attempt < userCodeAttempts; attempt++ {
		userCode, err := newUserCode()
		if err != nil {
			return DeviceAuthStart{}, err
		}
		err = s.store.InsertDeviceAuthRequest(store.DeviceAuthRequest{
			DeviceCodeHash: hashToken(deviceCode),
			UserCode:       userCode,
			ClientID:       dev.ClientID,
			DeviceName:     dev.Name,
			DevicePlatform: dev.Platform,
			CreatedAt:      nowStr,
			ExpiresAt:      formatTime(now.Add(deviceAuthTTL)),
		})
		if errors.Is(err, store.ErrUserCodeTaken) {
			continue // re-roll; see userCodeAttempts
		}
		if err != nil {
			return DeviceAuthStart{}, err
		}
		return DeviceAuthStart{
			DeviceCode: deviceCode,
			UserCode:   userCode,
			ExpiresIn:  deviceAuthTTL,
			Interval:   deviceAuthPollInterval,
		}, nil
	}
	return DeviceAuthStart{}, ErrDeviceAuthBusy
}

// ApproveDeviceCode authorizes a pending request on behalf of userID, and
// returns the request so the caller can tell the human what they just signed in.
//
// Every failure here is charged against the User's rate limit, because from the
// limiter's side "unknown code" and "guess" are the same event.
func (s *Service) ApproveDeviceCode(userCode, userID string) (store.DeviceAuthRequest, error) {
	req, err := s.resolveUserCode(userCode, userID)
	if err != nil {
		return store.DeviceAuthRequest{}, err
	}
	if err := s.store.ApproveDeviceAuth(req.UserCode, userID, formatTime(s.now())); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The row moved between our read and this write — another phone got
			// there first, or it expired in the gap.
			return store.DeviceAuthRequest{}, ErrUserCodeUnknown
		}
		return store.DeviceAuthRequest{}, err
	}
	req.State = store.DeviceAuthApproved
	req.ApprovedUserID = userID
	return req, nil
}

// resolveUserCode normalizes, rate-limits, and looks up a human-typed code,
// collapsing every "no" into ErrUserCodeUnknown. The collapse is deliberate: a
// caller who could tell "expired" from "never existed" from "already approved"
// could map the live code space by watching the difference.
func (s *Service) resolveUserCode(userCode, userID string) (store.DeviceAuthRequest, error) {
	if !s.allowApproveAttempt(userID) {
		return store.DeviceAuthRequest{}, ErrTooManyAttempts
	}
	normalized := NormalizeUserCode(userCode)
	if normalized == "" {
		s.chargeApproveFailure(userID)
		return store.DeviceAuthRequest{}, ErrUserCodeUnknown
	}

	req, err := s.store.DeviceAuthByUserCode(normalized)
	if errors.Is(err, store.ErrNotFound) {
		s.chargeApproveFailure(userID)
		return store.DeviceAuthRequest{}, ErrUserCodeUnknown
	}
	if err != nil {
		return store.DeviceAuthRequest{}, err
	}
	if req.State != store.DeviceAuthPending || !s.now().Before(mustParseTime(req.ExpiresAt)) {
		s.chargeApproveFailure(userID)
		return store.DeviceAuthRequest{}, ErrUserCodeUnknown
	}
	return req, nil
}

// RedeemDeviceCode is the TV's poll. On success it mints and returns a session
// exactly as a password login would — same LoginResult, so the client reuses one
// code path for both ways of signing in.
func (s *Service) RedeemDeviceCode(deviceCode string) (LoginResult, error) {
	if deviceCode == "" {
		return LoginResult{}, ErrDeviceCodeUnknown
	}
	hash := hashToken(deviceCode)
	req, err := s.store.DeviceAuthByCodeHash(hash)
	if errors.Is(err, store.ErrNotFound) {
		return LoginResult{}, ErrDeviceCodeUnknown
	}
	if err != nil {
		return LoginResult{}, err
	}

	now := s.now()
	nowStr := formatTime(now)

	// Expiry outranks the slow-down check: a client polling a dead code needs to
	// be told the code is dead, not to try the same dead code more slowly.
	if !now.Before(mustParseTime(req.ExpiresAt)) {
		return LoginResult{}, ErrDeviceCodeExpired
	}

	prev, err := s.store.TouchDeviceAuthPoll(hash, nowStr)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return LoginResult{}, err
	}
	if prev != "" && now.Sub(mustParseTime(prev)) < deviceAuthPollInterval-slowDownGrace {
		return LoginResult{}, ErrDeviceCodeSlowDown
	}

	switch req.State {
	case store.DeviceAuthPending:
		return LoginResult{}, ErrDeviceCodePending
	case store.DeviceAuthRedeemed:
		// One-shot. A second collection is not "pending", it is over.
		return LoginResult{}, ErrDeviceCodeUnknown
	}

	// Compare-and-swap: whoever wins this UPDATE is the only caller that mints.
	claimed, err := s.store.RedeemDeviceAuth(hash, nowStr)
	if errors.Is(err, store.ErrNotFound) {
		return LoginResult{}, ErrDeviceCodeUnknown
	}
	if err != nil {
		return LoginResult{}, err
	}

	user, err := s.store.UserByID(claimed.ApprovedUserID)
	if err != nil {
		return LoginResult{}, err
	}
	// The Device descriptor comes from the ROW — what the TV declared when it
	// started the flow and what the phone was shown before approving — never from
	// the poll body. The poll carries only the device code, so there is no way to
	// swap in a different identity after a human has already approved one.
	return s.issueSession(user, DeviceInput{
		Name:     claimed.DeviceName,
		Platform: claimed.DevicePlatform,
		ClientID: claimed.ClientID,
	})
}

// NormalizeUserCode canonicalizes a human-typed code: upcase, and drop the
// spaces and hyphens people insert when copying a grouped code off a screen.
// It returns "" if what is left is not a well-formed code, so an ill-formed
// guess costs the caller an attempt without costing the database a lookup.
//
// It does NOT try to repair confusable characters. A typed O or 1 cannot be
// mapped back — the alphabet excludes 0/O and 1/I/L precisely so those glyphs
// are never minted, which means a code containing one was misread, and guessing
// which of two characters the human meant would authorize the wrong request.
func NormalizeUserCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch {
		case r == ' ' || r == '-' || r == '_':
			continue
		case strings.ContainsRune(userCodeAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	if b.Len() != userCodeLength {
		return ""
	}
	return b.String()
}

// issueSession mints a Device row and a fresh opaque token for user. It is the
// single place a session is created: password login and device-code redemption
// both land here, so the two ways of signing in cannot drift apart in what they
// produce.
func (s *Service) issueSession(user store.User, dev DeviceInput) (LoginResult, error) {
	device, err := s.store.UpsertDevice(uuid.NewString(), user.ID, dev.ClientID, dev.Name, dev.Platform)
	if err != nil {
		return LoginResult{}, err
	}
	raw, err := newToken()
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.store.InsertToken(hashToken(raw), device.ID, user.ID); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: raw, User: user, Device: device}, nil
}

// --- approve rate limiting -------------------------------------------------

// approveAttempts is the in-memory failure counter behind the approve endpoint.
// In memory, like the claim token (ADR-0013), and for the same reason: this is
// per-boot safety state, not a record of anything. A restart clears it, which is
// acceptable — restarting the server is not an attack primitive a household
// attacker has, and persisting it would buy a table and a sweeper for nothing.
type approveAttempts struct {
	count       int
	windowStart time.Time
}

func (s *Service) allowApproveAttempt(userID string) bool {
	s.approveMu.Lock()
	defer s.approveMu.Unlock()
	a, ok := s.approveFails[userID]
	if !ok {
		return true
	}
	if s.now().Sub(a.windowStart) >= approveFailureWindow {
		delete(s.approveFails, userID)
		return true
	}
	return a.count < approveFailureLimit
}

func (s *Service) chargeApproveFailure(userID string) {
	s.approveMu.Lock()
	defer s.approveMu.Unlock()
	if s.approveFails == nil {
		s.approveFails = map[string]*approveAttempts{}
	}
	a, ok := s.approveFails[userID]
	if !ok || s.now().Sub(a.windowStart) >= approveFailureWindow {
		s.approveFails[userID] = &approveAttempts{count: 1, windowStart: s.now()}
		return
	}
	a.count++
}

// --- time ------------------------------------------------------------------

// formatTime renders a timestamp the one way this feature stores them. See
// migrations/0041_device_auth.sql: expiry is compared in SQL, and RFC3339 does
// not compare against SQLite's datetime('now') format.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// mustParseTime reads a timestamp this package wrote. A parse failure means the
// row was written by something that is not this code — there is no sensible
// recovery, and treating an unreadable expiry as "not expired" would be the
// worst possible guess, so we fall back to the zero time, which is always in the
// past and therefore always expired.
func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
