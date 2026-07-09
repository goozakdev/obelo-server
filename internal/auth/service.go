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
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/google/uuid"
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
}

// NewService builds the auth service. If the database has zero Users it
// generates a fresh one-time claim token (returned via ClaimToken for the
// bootstrap to log); otherwise the claim token is empty and setup is closed.
func NewService(s Store) (*Service, error) {
	svc := &Service{store: s}
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
func (s *Service) Setup(claimToken, username, password string) (store.User, error) {
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

	hash, err := HashPassword(password)
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
func (s *Service) Login(username, password string, dev DeviceInput) (LoginResult, error) {
	if dev.ClientID == "" {
		return LoginResult{}, fmt.Errorf("auth: device.clientId is required")
	}

	user, err := s.store.UserByUsername(username)
	if errors.Is(err, store.ErrNotFound) {
		// Run a verification anyway to keep timing roughly uniform, then fail.
		_ = VerifyPassword(dummyHash, password)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}
	if err := VerifyPassword(user.PasswordHash, password); err != nil {
		return LoginResult{}, ErrInvalidCredentials
	}

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

// dummyHash is a valid hash of a random value, used to equalize login timing
// when the username is unknown so attackers can't distinguish "no such user"
// from "wrong password" by response time.
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
// unknown role yields ErrInvalidUser.
func (s *Service) CreateUser(username, password, role string) (store.User, error) {
	if username == "" || password == "" {
		return store.User{}, ErrInvalidUser
	}
	if role == "" {
		role = RoleMember
	}
	if role != RoleAdmin && role != RoleMember {
		return store.User{}, ErrInvalidUser
	}
	hash, err := HashPassword(password)
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
// empty password; ErrUserNotFound for an unknown User.
func (s *Service) SetPassword(id, password string) error {
	if password == "" {
		return ErrInvalidUser
	}
	hash, err := HashPassword(password)
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
