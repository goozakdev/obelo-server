// Package auth is the authentication spine (ADR-0013, ADR-0015): first-Admin
// bootstrap via a one-time claim token, password verification, opaque
// DB-backed bearer tokens, and Device lifecycle.
//
// It is deliberately transport-agnostic — it speaks Users, Devices, and tokens,
// not HTTP. The api package wraps it in thin handlers and a bearer-auth
// middleware (ADR-0006 modular-monolith seam). All secrets live here: tokens
// are generated and hashed in this package, passwords are hashed and verified
// here, and nothing in a returned value or error leaks a raw token or
// plaintext password.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/obelo-server/internal/store"
)

// Role values a User may hold (CONTEXT.md: Admin manages; Member browses/plays).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Store is the persistence the auth service needs. *store.DB satisfies it; the
// interface keeps the seam explicit and the service unit-testable.
type Store interface {
	CountUsers() (int, error)
	CreateAdmin(id, username, passwordHash string) (store.User, error)
	CreateUser(id, username, role, passwordHash string) (store.User, error)
	ListUsers() ([]store.User, error)
	UserByID(id string) (store.User, error)
	UserByUsername(username string) (store.User, error)
	CountAdmins() (int, error)
	SetUserPassword(id, passwordHash string) error
	DeleteUser(id string) error
	UpsertDevice(newID, userID, clientID, name, platform string) (store.Device, error)
	DevicesByUser(userID string) ([]store.Device, error)
	DeviceByID(id string) (store.Device, error)
	DeleteDevice(id string) error
	InsertToken(tokenHash, deviceID, userID string) error
	LookupToken(tokenHash string) (store.TokenIdentity, error)
	DeleteToken(tokenHash string) error
	// Device authorization grant (ADR-0036).
	InsertDeviceAuthRequest(req store.DeviceAuthRequest) error
	DeviceAuthByUserCode(userCode string) (store.DeviceAuthRequest, error)
	DeviceAuthByCodeHash(hash string) (store.DeviceAuthRequest, error)
	ApproveDeviceAuth(userCode, userID, now string) error
	RedeemDeviceAuth(hash, now string) (store.DeviceAuthRequest, error)
	TouchDeviceAuthPoll(hash, now string) (string, error)
	CountLiveDeviceAuthRequests(now string) (int, error)
	DeleteExpiredDeviceAuthRequests(now string) error
	// Session stream tokens (.scratch/session-stream-tokens). A SEPARATE namespace
	// from InsertToken/LookupToken above, and deliberately so: nothing here reads
	// or writes auth_tokens, so a stream token can never authenticate as a bearer
	// and a bearer can never authenticate as a stream token.
	InsertStreamToken(t store.StreamToken) error
	LiveStreamToken(hash, now string) (store.StreamToken, error)
	LiveStreamTokenForSession(hash, sessionID, now string) (store.StreamToken, error)
	DeleteStreamTokensForSession(sessionID string) error
	DeleteExpiredStreamTokens(now string) error
}

// Common service errors, mapped to HTTP envelopes by the api layer. They are
// intentionally coarse so that login failures cannot be probed apart (an
// unknown username and a wrong password both surface as ErrInvalidCredentials).
var (
	ErrSetupClosed        = errors.New("auth: setup already completed")
	ErrInvalidClaimToken  = errors.New("auth: invalid claim token")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrInvalidToken       = errors.New("auth: invalid or revoked token")
	ErrDeviceNotFound     = errors.New("auth: device not found")
	ErrForbidden          = errors.New("auth: not permitted")
	// User-management errors (Admin-scope /users surface).
	ErrUserNotFound  = errors.New("auth: user not found")
	ErrUsernameTaken = errors.New("auth: username already taken")
	ErrLastAdmin     = errors.New("auth: cannot remove the last admin")
	ErrInvalidUser   = errors.New("auth: invalid user input")
)

// Service implements the authentication operations. It holds the one-time claim
// token in memory (see NewService) — there is no on-disk claim-token state.
type Service struct {
	store Store

	// claimToken is the one-time bootstrap secret (ADR-0013). It is held only in
	// memory: regenerated fresh on each boot while zero Users exist, and cleared
	// once the first Admin is created. Because it is never persisted, a restart
	// before setup rotates it (the operator reads the new value from the logs),
	// and after setup the zero-users state is unreachable without wiping the data
	// dir — so the token can never be reused.
	mu         sync.RWMutex
	claimToken string

	// now is the clock. It exists as a field only so tests can move time: the
	// Device authorization grant (ADR-0036) turns on expiry windows and a rate
	// limiter, and a test that had to sleep for five real minutes to watch a code
	// expire would never be written, which means the expiry would never be tested.
	now func() time.Time

	// The per-boot brute-force counters. All three are the same fixed-window
	// failureLimiter (failure_limiter.go); what differs is what they are keyed by
	// and how generous they are.
	//
	//   approveFails   — device-code approve, keyed by User (that endpoint is
	//                    authenticated, so there is always one). See device_auth.go.
	//   loginUserFails — password login, keyed by the submitted username.
	//   loginIPFails   — password login, keyed by client IP.
	//
	// Login checks BOTH of its counters and refuses if either is over; see
	// login_limit.go for why one without the other is not a control.
	approveFails   *failureLimiter
	loginUserFails *failureLimiter
	loginIPFails   *failureLimiter
}

// Option configures the Service. Present for the clock seam; NewService's
// zero-option form is the production path.
type Option func(*Service)

// WithClock replaces the service's clock. Tests use it to expire codes and open
// rate-limit windows without sleeping.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService builds the auth service. If the database has zero Users it
// generates a fresh one-time claim token (returned via ClaimToken for the
// bootstrap to log); otherwise the claim token is empty and setup is closed.
func NewService(s Store, opts ...Option) (*Service, error) {
	svc := &Service{
		store:          s,
		now:            time.Now,
		approveFails:   newFailureLimiter(approveFailureLimit, approveFailureWindow),
		loginUserFails: newFailureLimiter(loginUserFailureLimit, loginFailureWindow),
		loginIPFails:   newFailureLimiter(loginIPFailureLimit, loginFailureWindow),
	}
	for _, opt := range opts {
		opt(svc)
	}
	n, err := s.CountUsers()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		tok, err := generateClaimToken()
		if err != nil {
			return nil, err
		}
		svc.claimToken = tok
	}
	return svc, nil
}

// generateClaimToken returns a high-entropy, human-typable claim token.
func generateClaimToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generating claim token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ClaimToken returns the current one-time claim token, or "" if setup is closed
// (an Admin already exists). The caller (bootstrap) logs it; it is never
// returned over the API.
func (s *Service) ClaimToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.claimToken
}

// SetupRequired reports whether the first Admin still needs bootstrapping.
func (s *Service) SetupRequired() (bool, error) {
	n, err := s.store.CountUsers()
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// Setup creates the first Admin, given the correct claim token. It is refused
// once any User exists (ErrSetupClosed) or if the token is wrong/absent
// (ErrInvalidClaimToken). On success the in-memory claim token is cleared so it
// cannot be reused. The comparison is constant-time.
//
// ctx is threaded in for the KDF meter (password.go), and only reaches it after
// the claim token has already been checked — a caller with the wrong token never
// gets as far as costing us a derivation.
func (s *Service) Setup(ctx context.Context, claimToken, username, password string) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-check user count under the lock so two concurrent setups can't both win.
	n, err := s.store.CountUsers()
	if err != nil {
		return store.User{}, err
	}
	if n > 0 || s.claimToken == "" {
		return store.User{}, ErrSetupClosed
	}
	if claimToken == "" || subtle.ConstantTimeCompare([]byte(claimToken), []byte(s.claimToken)) != 1 {
		return store.User{}, ErrInvalidClaimToken
	}
	if username == "" || password == "" {
		return store.User{}, fmt.Errorf("auth: username and password are required")
	}

	hash, err := HashPasswordContext(ctx, password)
	if err != nil {
		return store.User{}, err
	}
	user, err := s.store.CreateAdmin(uuid.NewString(), username, hash)
	if err != nil {
		return store.User{}, err
	}

	// First Admin exists now; close setup permanently for this process.
	s.claimToken = ""
	return user, nil
}

// LoginResult bundles what a successful login returns: the raw bearer token
// (shown once), the User, and the resolved Device.
type LoginResult struct {
	Token  string
	User   store.User
	Device store.Device
}

// DeviceInput is the client-supplied Device descriptor on login.
type DeviceInput struct {
	Name     string
	Platform string
	ClientID string
}

// Login verifies credentials, reuses/refreshes the Device for the stable
// clientId (no duplicates), mints a fresh opaque token, stores only its hash,
// and returns the raw token to the caller. A bad username or password both
// yield ErrInvalidCredentials.
//
// clientIP is the caller's source address, and the auth package takes it as a
// plain string precisely so it can stay transport-agnostic — deriving it is the
// api layer's job (see clientIP there for why it is RemoteAddr and never
// X-Forwarded-For). Pass "" if there genuinely is none; those callers then share
// one bucket in the per-IP counter, which is stricter, not looser.
//
// Too many recent FAILURES from this username or this address yield
// ErrTooManyLoginAttempts, returned before any credential work — see
// login_limit.go. A successful login charges nothing, so a household that types
// its passwords correctly never meets the limiter at all.
func (s *Service) Login(ctx context.Context, username, password string, dev DeviceInput, clientIP string) (LoginResult, error) {
	if dev.ClientID == "" {
		return LoginResult{}, fmt.Errorf("auth: device.clientId is required")
	}

	// Before the user lookup, before the KDF, before anything that could differ
	// between a real username and a made-up one. This ordering is the whole reason
	// the refusal leaks nothing.
	if err := s.refuseLogin(username, clientIP); err != nil {
		return LoginResult{}, err
	}

	user, err := s.store.UserByUsername(username)
	if errors.Is(err, store.ErrNotFound) {
		// Run a verification anyway to keep timing roughly uniform, then fail. It
		// goes through VerifyPasswordContext like the real one, so it queues on the
		// same KDF semaphore: a dummy verify that skipped the queue would return
		// instantly under load while a real one waited, which is the timing
		// difference this call exists to erase, reintroduced by the fix for a
		// different problem.
		if verr := VerifyPasswordContext(ctx, dummyHash, password); kdfAbandoned(verr) {
			return LoginResult{}, verr
		}
		s.chargeLoginFailure(username, clientIP)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		// A store failure is our fault, not the caller's; charging it would let a
		// sick database lock out the people trying to use it.
		return LoginResult{}, err
	}
	if verr := VerifyPasswordContext(ctx, user.PasswordHash, password); verr != nil {
		if kdfAbandoned(verr) {
			return LoginResult{}, verr
		}
		s.chargeLoginFailure(username, clientIP)
		return LoginResult{}, ErrInvalidCredentials
	}

	// Everything past "the password was right" is shared with the device-code
	// grant (ADR-0036), which authenticates differently but must produce an
	// identical session. issueSession is that shared tail; see device_auth.go.
	return s.issueSession(user, dev)
}

// dummyHash is a valid hash of a random value, used to equalize login timing
// when the username is unknown so attackers can't distinguish "no such user"
// from "wrong password" by response time.
//
// It is derived once, at package initialization, via the uncancellable
// HashPassword — there is no request to hang up on here, and the KDF semaphore it
// takes is uncontended because nothing is serving yet. Do not make this lazy: a
// first-use derivation would make the very first unknown-username login slower
// than every one after it, which is a timing difference in the function whose job
// is not having one.
var dummyHash = func() string {
	h, err := HashPassword("dummy-password-for-timing-equalization")
	if err != nil {
		// Falling back to a fixed well-formed hash keeps VerifyPassword on the
		// same code path; correctness of the value is irrelevant (it never matches).
		return "pbkdf2-sha256$210000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	return h
}()

// Authenticate resolves a raw bearer token to its identity, or ErrInvalidToken
// if the token is unknown/revoked. The middleware calls this per request.
func (s *Service) Authenticate(rawToken string) (store.TokenIdentity, error) {
	if rawToken == "" {
		return store.TokenIdentity{}, ErrInvalidToken
	}
	id, err := s.store.LookupToken(hashToken(rawToken))
	if errors.Is(err, store.ErrNotFound) {
		return store.TokenIdentity{}, ErrInvalidToken
	}
	if err != nil {
		return store.TokenIdentity{}, err
	}
	return id, nil
}

// Logout revokes the current token by deleting it. Idempotent.
func (s *Service) Logout(rawToken string) error {
	return s.store.DeleteToken(hashToken(rawToken))
}

// Devices lists the given User's Devices.
func (s *Service) Devices(userID string) ([]store.Device, error) {
	return s.store.DevicesByUser(userID)
}

// DeleteDevice removes a Device (and cascades to its tokens, revoking access
// immediately). The caller must be the Device's owner or an Admin; otherwise
// ErrForbidden. A missing Device yields ErrDeviceNotFound.
func (s *Service) DeleteDevice(caller store.User, deviceID string) error {
	device, err := s.store.DeviceByID(deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrDeviceNotFound
	}
	if err != nil {
		return err
	}
	if device.UserID != caller.ID && caller.Role != "admin" {
		return ErrForbidden
	}
	if err := s.store.DeleteDevice(deviceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrDeviceNotFound
		}
		return err
	}
	return nil
}

// --- User management (Admin scope) -----------------------------------------
//
// These power the /users surface an Admin uses to manage Members (and further
// Admins) after the first-Admin bootstrap. Access enforcement (what a Member
// can browse/play) is a separate slice; this one only manages the User records.

// CreateUser mints a User with the given role (defaulting to Member when role
// is empty), hashing the password here so no plaintext leaves the caller. A
// duplicate username yields ErrUsernameTaken; an empty username/password or an
// unknown role yields ErrInvalidUser. ctx is threaded in for the KDF meter
// (password.go): this is an Admin-only endpoint, but it hashes, and every path
// into argon2 queues in the same line.
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (store.User, error) {
	if username == "" || password == "" {
		return store.User{}, ErrInvalidUser
	}
	if role == "" {
		role = RoleMember
	}
	if role != RoleAdmin && role != RoleMember {
		return store.User{}, ErrInvalidUser
	}
	hash, err := HashPasswordContext(ctx, password)
	if err != nil {
		return store.User{}, err
	}
	user, err := s.store.CreateUser(uuid.NewString(), username, role, hash)
	if err != nil {
		if isUniqueViolation(err) {
			return store.User{}, ErrUsernameTaken
		}
		return store.User{}, err
	}
	return user, nil
}

// Users lists every User (for the Admin user-management view).
func (s *Service) Users() ([]store.User, error) {
	return s.store.ListUsers()
}

// User returns one User by id, or ErrUserNotFound.
func (s *Service) User(id string) (store.User, error) {
	u, err := s.store.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrUserNotFound
	}
	return u, err
}

// SetPassword resets a User's password (Admin recovery). ErrInvalidUser for an
// empty password; ErrUserNotFound for an unknown User. ctx is threaded in for the
// KDF meter (password.go), same as CreateUser.
func (s *Service) SetPassword(ctx context.Context, id, password string) error {
	if password == "" {
		return ErrInvalidUser
	}
	hash, err := HashPasswordContext(ctx, password)
	if err != nil {
		return err
	}
	if err := s.store.SetUserPassword(id, hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

// DeleteUser removes a User, cascading their Devices, tokens, and watch state.
// It refuses to delete the final Admin (ErrLastAdmin) so the server can never be
// orphaned. ErrUserNotFound for an unknown User.
func (s *Service) DeleteUser(id string) error {
	u, err := s.store.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if u.Role == RoleAdmin {
		n, err := s.store.CountAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	if err := s.store.DeleteUser(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure
// (the username collision). The driver surfaces it only as a message, so we
// match on it — the same pragmatic check the setup handler already uses.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}
