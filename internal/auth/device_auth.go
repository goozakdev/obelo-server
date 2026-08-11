package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/obelo-server/internal/store"
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

	// maxLiveDeviceAuthRequests caps concurrent unexpired requests, and exists
	// because a 4-char code space can be crowded: as it fills, generation degrades
	// into a retry loop and then fails outright, and a real TV cannot sign in.
	//
	// It was 32, on the stated grounds that 30^4 is "small enough to crowd". Do the
	// arithmetic and that number is not defending anything. The space is 810,000.
	// A fresh code collides with the live set with probability live/810000, and
	// userCodeAttempts re-rolls, so a START fails only if all 8 rolls collide:
	//
	//     live   occupancy   1st-roll collision   8 consecutive (a failed start)
	//       32     0.004%          1 in 25,313        ~6e-36
	//      256     0.032%          1 in  3,164        ~1e-28
	//     1024     0.126%          1 in    791        ~6e-24
	//
	// At 1024 live codes one start in 791 pays for a second roll and nothing else
	// changes. Crowding is not the binding constraint anywhere near here; a cap in
	// the tens of thousands would be, and this is nowhere near that.
	//
	// What 32 WAS doing was handing an unauthenticated caller a household outage
	// for 32 requests every 5 minutes — one request every 9 seconds, indefinitely,
	// indistinguishable from a slightly eager client. This endpoint takes no
	// credential by design (ADR-0036: a TV that could authenticate would not need
	// the grant), so nothing else stood in the way. At 1024 the same outage costs a
	// sustained 3.4 starts per second forever, which is a flood somebody notices
	// and which the per-source quota below then splits across four addresses.
	//
	// Raising it is not free, and the cost is worth naming: with 1024 hostile codes
	// parked in the space, a household member who mistypes their real code has a
	// 0.126% chance of landing on one and signing a stranger's device into their
	// own account, against 0.004% before — a 32x rise in an already-remote path.
	//
	// Be precise about what bounds that, because the obvious answer is wrong: the
	// approve endpoint's limiter does NOT. It charges FAILURES, and a typo that
	// lands on a LIVE code is a success — never charged, so the 10-wrong-codes-per-
	// 5-minutes ceiling never engages. That limiter bounds an attacker WALKING the
	// space, which is a different attack. What bounds this one is that the mistype
	// has to produce a well-formed code at all (NormalizeUserCode refuses anything
	// else), that the human is shown the device before approving — though note the
	// name and platform on that screen are whatever the party that STARTED the flow
	// declared, so a hostile entry is free to call itself "Living Room Apple TV" —
	// and that DELETE /devices/{id} revokes the resulting session immediately
	// (ADR-0015), which is the recourse this grant already leans on.
	//
	// If this ever needs to move again, move it up. The direction that hurts is
	// down: every reduction makes the lockout cheaper, and 810,000 is not the
	// reason to make one.
	maxLiveDeviceAuthRequests = 1024

	// maxDeviceAuthStartsPerSource is how many codes ONE client address may
	// successfully obtain per deviceAuthStartWindow. It counts successes, not
	// failures — a start that succeeded is precisely what consumed a slot.
	//
	// The number is 128 and every digit of it is about the reverse-proxy
	// deployment (ADR-0005), so read this before tightening it. The client address
	// comes from clientIP in the api package, which reads RemoteAddr and never
	// X-Forwarded-For, because there is no trusted-proxy configuration to make the
	// header safe. BEHIND A PROXY, THEREFORE, EVERY REQUEST IN THE WORLD SHARES ONE
	// KEY, and this per-source quota IS the global cap — 1024 becomes unreachable
	// and 128 per 5 minutes is what the whole household gets.
	//
	// That is the trap in this control, and the only thing that makes it safe is
	// that 128 is comfortably ABOVE the 32 it replaces:
	//
	//   Behind a proxy — an attacker locks the household out by burning the shared
	//   128 before it does, which costs 128 starts per 5 minutes instead of 32. The
	//   shape of the outage is unchanged and its price is 4x. Strictly better than
	//   today, and strictly worse than the 1024 a proxy-aware deployment would get,
	//   which is the price of not reading a header we cannot trust.
	//
	//   Directly exposed (LAN, or a port-forward) — one address gets 128 per
	//   window, so at most 256 codes live at once counting a window straddle, out
	//   of 1024. A single source can no longer lock anybody out at all: it takes
	//   four addresses timed across a boundary, or eight without. Meanwhile the
	//   household's own TVs each hold their own budget and never interact.
	//
	// Anything BELOW 32 turns this into a regression delivered as a security fix:
	// behind a proxy it would be a global cap tighter than the one it replaced, and
	// legitimate TV sign-in would get harder than it is today. Do not tune this
	// down without re-deriving that comparison. The number to move first is
	// maxLiveDeviceAuthRequests, upward.
	//
	// Household headroom, for the record: 128 successful starts inside five minutes
	// from every device behind the proxy combined. A household does single digits a
	// week. The one legitimate client that could reach it is a TV app retry-looping
	// the start endpoint, which is a bug, and which the 429 this produces tells it
	// to stop doing — see the api layer.
	maxDeviceAuthStartsPerSource = 128

	// deviceAuthStartWindow matches deviceAuthTTL on purpose: the quota is meant to
	// approximate "codes this source is holding right now", and a code's life is
	// exactly one TTL. A per-source LIVE count would be the honest version, but it
	// would mean storing the source address on the row, which is a schema change
	// and a new piece of retained network metadata (ADR-0001) for an accuracy this
	// does not need. The fixed window over-counts — a code the source already
	// redeemed still occupies its quota — and over-counting is the strict
	// direction.
	deviceAuthStartWindow = deviceAuthTTL

	// userCodeAttempts bounds the collision re-roll. See the table on
	// maxLiveDeviceAuthRequests: even with the space at its cap the odds of 8
	// consecutive collisions are ~6e-24. This is a guard against an infinite loop,
	// not a real code path.
	userCodeAttempts = 8

	// The approve endpoint's brute-force limit: a User gets approveFailureLimit
	// wrong codes per approveFailureWindow before being refused outright.
	//
	// This is the one control standing between a 4-char code and enumeration. It
	// counts FAILURES only, so a household approving real codes never meets it,
	// and it is keyed by User (the endpoint is authenticated, so there is always
	// one) rather than by IP — an IP is shared by every device behind the NAT and
	// spoofable besides. Password login (login_limit.go) reaches the opposite
	// conclusion about IP because it is UNAUTHENTICATED and therefore has no User
	// to key on; the two are not in disagreement.
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
	ErrDeviceCodeUnknown   = errors.New("auth: unknown device code")
	ErrDeviceCodePending   = errors.New("auth: device code not yet approved")
	ErrDeviceCodeExpired   = errors.New("auth: device code expired")
	ErrDeviceCodeSlowDown  = errors.New("auth: polling too fast")
	ErrUserCodeUnknown     = errors.New("auth: unknown user code")
	ErrTooManyAttempts     = errors.New("auth: too many failed attempts")
	ErrDeviceAuthBusy      = errors.New("auth: too many device authorizations in flight")
	ErrDeviceAuthThrottled = errors.New("auth: too many device authorizations started from this address")
)

// DeviceAuthThrottledError is what StartDeviceAuth returns when the CALLER is
// over maxDeviceAuthStartsPerSource, as distinct from ErrDeviceAuthBusy, which
// says the SERVER is out of slots. It carries what is left of the window so the
// api layer can answer with Retry-After; a caller that does not care matches
// errors.Is(err, ErrDeviceAuthThrottled) and never learns this type exists.
//
// Same shape as LoginThrottledError (login_limit.go) rather than a shared type:
// the two are unrelated limits on unrelated endpoints, and collapsing them would
// mean a handler could not tell which one it was holding.
type DeviceAuthThrottledError struct {
	// RetryAfter is the time remaining on the tripped window — an upper bound on
	// how long the caller must wait, since the window is fixed and reopens whole.
	RetryAfter time.Duration
}

func (e *DeviceAuthThrottledError) Error() string { return ErrDeviceAuthThrottled.Error() }

// Unwrap makes errors.Is(err, ErrDeviceAuthThrottled) true, so handlers switch on
// the sentinel like every other auth error and the carried duration is additive.
func (e *DeviceAuthThrottledError) Unwrap() error { return ErrDeviceAuthThrottled }

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
// things standing behind it are the two caps: how many codes may be live at all
// (maxLiveDeviceAuthRequests), and how many one address may hold
// (maxDeviceAuthStartsPerSource).
//
// clientIP is the caller's source address, taken as a plain string for the same
// reason Login takes one: this package stays transport-agnostic, and deriving the
// address is the api layer's job (see clientIP there, and read what it says about
// reverse proxies before touching either cap). Pass "" if there genuinely is
// none; those callers then share one bucket, which is stricter, not looser.
func (s *Service) StartDeviceAuth(dev DeviceInput, clientIP string) (DeviceAuthStart, error) {
	if dev.ClientID == "" {
		return DeviceAuthStart{}, fmt.Errorf("auth: device.clientId is required")
	}
	now := s.now()
	nowStr := formatTime(now)

	// The per-source quota is checked BEFORE the global cap, and the ordering is a
	// decision rather than an accident. A caller over its own budget is told that,
	// which is the only refusal it can act on — being told "the server is busy"
	// instead would send a client that is itself the flood back to retry, deepening
	// the problem it caused. It also means the pool's occupancy is only ever
	// reported to callers who are within their own budget, so filling the space is
	// not a way to watch it fill.
	if ok, retryAfter := s.deviceStartQuota.allow(clientIP, now); !ok {
		return DeviceAuthStart{}, &DeviceAuthThrottledError{RetryAfter: retryAfter}
	}

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
		// Charged here, on the row that actually exists, and nowhere else. A caller
		// refused by the global cap or lost to a store error got no code and holds no
		// slot, so charging it would let one flood spend a bystander's quota — and
		// behind a proxy every caller is a bystander sharing one key.
		s.deviceStartQuota.charge(clientIP, now)
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

// The counter itself is fixedWindowLimiter (fixed_window_limiter.go), shared with
// password login and with the start quota above, which all need the identical
// fixed-window shape. These two wrappers stay because they are what the flow above
// reads like — resolveUserCode asking "may this User try?" rather than reaching
// into a field — and because the Service owns the clock the limiter is
// deliberately without.
//
// The approve endpoint ignores the retry-after the limiter can report: its 429
// carries no Retry-After header (the phone's advice is "wait a few minutes and
// try again"), and adding one would be a contract change, not a cleanup.
func (s *Service) allowApproveAttempt(userID string) bool {
	ok, _ := s.approveFails.allow(userID, s.now())
	return ok
}

func (s *Service) chargeApproveFailure(userID string) {
	s.approveFails.charge(userID, s.now())
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
