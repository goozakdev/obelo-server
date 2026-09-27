package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The password flow of a Sign-in provider (ADR-0063 decisions 3, 6 and 7), as
// the auth service sees it. The Plugins, their order and the host's judgment on
// what they answer live in internal/signin; this file only knows that there is an
// ordered list of things that can say "yes, that is subject S", and what the
// Server does with a yes.
//
// Three rules, and they are the whole of it:
//
//   - An accepted answer resolves by (provider id, subject), NEVER by the
//     username on the form or the one the provider reports. A returning identity
//     signs in as the User that holds it, whatever it is called today.
//   - An identity seen for the first time becomes a NEW Member granted nothing,
//     with no Local password. Not an Admin, not a merge, not a library.
//   - If that new Member's username is already taken here, nothing is created
//     and nothing is linked: ErrUsernameCollision, which — unlike a refusal — is
//     allowed to say what happened, because the credential was good.

// ExternalAnswer is one Sign-in provider's accepted answer: who the source says
// the person is. A provider that rejects answers ok=false instead, with no
// ExternalAnswer at all.
type ExternalAnswer struct {
	Subject  string
	Username string
	Groups   []string
	// RefreshToken is what a redirect provider handed back to re-check the
	// identity later without the person present; "" when it handed none. It is
	// stored on the identity and never a password.
	RefreshToken string
}

// PasswordProvider is one enabled password-flow Sign-in provider, already judged
// by the host: an answer it hands back has a subject and a username.
type PasswordProvider interface {
	// ID is the provider's Plugin id — the first half of every External
	// identity it vouches for.
	ID() string
	// CheckPassword asks the provider about one credential. ok=false is a
	// rejection; a provider that failed answers ok=false too, because a failing
	// provider is never a reason to sign anyone in.
	CheckPassword(ctx context.Context, username, password string) (answer ExternalAnswer, ok bool)
}

// SignInProviders is the ordered list of password-flow Sign-in providers a login
// asks after the Local password, first-asked first.
type SignInProviders interface {
	PasswordProviders() []PasswordProvider
}

// ErrUsernameCollision: a Sign-in provider accepted the credential, the identity
// is new here, and the username it would be created under already belongs to a
// User of this server. ADR-0063 decision 7: refused, not merged — no suffix, no
// link, no queue.
var ErrUsernameCollision = errors.New("auth: a new external identity's username is already taken")

// ErrExternalUsernameInvalid: a Sign-in provider accepted the credential, the
// identity is new here, and the username it names breaks the rule every
// username is held to (username.go). Nobody can be created under it, so it is a
// refusal: nothing was created and no session was issued.
var ErrExternalUsernameInvalid = errors.New("auth: a new external identity's username breaks the username rule")

// ErrSignInProviderGone: the provider answered, but its Plugin was uninstalled
// before the sign-in could write anything (ADR-0063 decision 10). A refusal, like
// any other: nothing was created and no session was issued.
var ErrSignInProviderGone = errors.New("auth: the sign-in provider was uninstalled")

// signIn holds the Sign-in providers a Service consults. Set once at composition,
// read on every login; the mutex is for the tests that build a Service and then
// hand it providers.
type signIn struct {
	mu        sync.RWMutex
	providers SignInProviders
	answered  func(pluginID, subject string)
}

// OnExternalSignIn tells the Service whom to tell when a provider signs in an
// identity it already holds: that provider has just answered for it, so a
// failure a re-check remembers of that identity no longer stands.
func (s *Service) OnExternalSignIn(fn func(pluginID, subject string)) {
	s.signIn.mu.Lock()
	defer s.signIn.mu.Unlock()
	s.signIn.answered = fn
}

// UseSignInProviders tells the Service which password-flow Sign-in providers to
// ask, in order, when the Local password does not sign someone in. Nil (the
// default) means there are none, and login is exactly the Local password.
func (s *Service) UseSignInProviders(p SignInProviders) {
	s.signIn.mu.Lock()
	defer s.signIn.mu.Unlock()
	s.signIn.providers = p
}

func (s *Service) passwordProviders() []PasswordProvider {
	s.signIn.mu.RLock()
	p := s.signIn.providers
	s.signIn.mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.PasswordProviders()
}

// errNoProviderAccepted is the external half of a refusal: every provider was
// asked and none accepted. Login turns it into the one refusal every other
// refusal is.
var errNoProviderAccepted = errors.New("auth: no sign-in provider accepted")

// signInExternally asks each password-flow Sign-in provider in order and signs
// in on the FIRST acceptance. It is reached only once the Local password has not
// signed anyone in, and it is reached on every such path — a wrong Local
// password, an unknown username, a User with no Local password — so the work a
// refusal does is the same whichever refusal it is.
func (s *Service) signInExternally(ctx context.Context, username, password string, dev DeviceInput) (LoginResult, error) {
	for _, p := range s.passwordProviders() {
		answer, ok := p.CheckPassword(ctx, username, password)
		if !ok {
			continue
		}
		user, err := s.resolveExternalIdentity(p.ID(), answer)
		if errors.Is(err, ErrSignInProviderGone) || errors.Is(err, ErrExternalUsernameInvalid) {
			// Uninstalled since it answered, or naming a new identity by a
			// username nobody may hold: an answer the Server cannot act on signs
			// nobody in, exactly as a rejection does.
			continue
		}
		if err != nil {
			return LoginResult{}, err
		}
		return s.issueSession(user, dev)
	}
	return LoginResult{}, errNoProviderAccepted
}

// SignInExternal signs in the person a redirect-flow Sign-in provider vouched
// for, once the host has judged the answer (internal/signin). It resolves exactly
// as the password flow does — by (providerID, subject), never by username, with
// the same new-Member rule and the same ErrUsernameCollision — and issues the
// same session a login does.
func (s *Service) SignInExternal(providerID string, answer ExternalAnswer, dev DeviceInput) (LoginResult, error) {
	if dev.ClientID == "" {
		return LoginResult{}, fmt.Errorf("auth: device.clientId is required")
	}
	user, err := s.resolveExternalIdentity(providerID, answer)
	if err != nil {
		return LoginResult{}, err
	}
	return s.issueSession(user, dev)
}

// resolveExternalIdentity turns an accepted answer into the User it signs in as —
// the holder of (providerID, subject) if there is one, otherwise a new Member —
// and syncs that User's Group mapping from the groups just answered, so the
// session it signs in with already carries the role the mapping gives.
func (s *Service) resolveExternalIdentity(providerID string, answer ExternalAnswer) (store.User, error) {
	user, err := s.holderOf(providerID, answer)
	if err != nil {
		return store.User{}, err
	}
	if err := s.store.SetExternalRefreshToken(providerID, answer.Subject, answer.RefreshToken); err != nil {
		return store.User{}, err
	}
	if err := s.SyncGroupMapping(user.ID); err != nil {
		return store.User{}, err
	}
	return s.store.UserByID(user.ID)
}

// holderOf is the holder of (providerID, subject), recording what the provider
// just said about it, or a new Member holding it.
func (s *Service) holderOf(providerID string, answer ExternalAnswer) (store.User, error) {
	user, err := s.store.ExternalIdentityUser(providerID, answer.Subject)
	if err == nil {
		err := s.store.RecordExternalSignIn(providerID, answer.Subject, answer.Username, answer.Groups)
		if errors.Is(err, store.ErrSignInProviderUninstalled) {
			return store.User{}, ErrSignInProviderGone
		}
		if err != nil {
			return store.User{}, err
		}
		s.signIn.mu.RLock()
		answered := s.signIn.answered
		s.signIn.mu.RUnlock()
		if answered != nil {
			answered(providerID, answer.Subject)
		}
		return user, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}

	// First time this identity has been seen. The new User is a Member with no
	// Local password — the relaxed rule is CreateUser's own, asked with the one
	// fact only this path can state.
	if !passwordRuleAdmits(RoleMember, "", true) {
		return store.User{}, ErrInvalidUser
	}
	// The provider's username is trimmed and held to the rule a local one is, and
	// stored the same way; what the provider said is kept on the identity as it
	// said it.
	username, ok := normalizeUsername(providerUsername(answer.Username))
	if !ok {
		return store.User{}, ErrExternalUsernameInvalid
	}
	user, err = s.store.CreateExternalMember(uuid.NewString(), username,
		providerID, answer.Subject, answer.Username, answer.Groups)
	if err != nil {
		// A concurrent first sign-in of the same identity may have won the insert —
		// on the identity row, or first on the username it was minting. It is the
		// same person, so whoever holds the identity now is the answer, and only
		// when nobody does is a taken username a collision.
		if held, lookupErr := s.store.ExternalIdentityUser(providerID, answer.Subject); lookupErr == nil {
			return held, nil
		}
	}
	switch {
	case errors.Is(err, store.ErrExternalIdentityTaken):
		return s.store.ExternalIdentityUser(providerID, answer.Subject)
	case errors.Is(err, store.ErrSignInProviderUninstalled):
		return store.User{}, ErrSignInProviderGone
	case errors.Is(err, store.ErrUsernameHeld), isUniqueViolation(err):
		return store.User{}, ErrUsernameCollision
	case err != nil:
		return store.User{}, err
	}
	return user, nil
}

// Attaching an External identity (ADR-0063 decision 3): the ONE way an existing
// User gains one. The User is the caller, signed in as themselves; the identity
// is whatever a Sign-in provider just vouched for, by either flow. It never mints
// a Member and is never the username collision, because nothing resolves by
// username here — and it never moves an identity: one another User holds is
// ErrExternalIdentityHeld, and neither User's identities change. One whose
// provider was uninstalled since it answered is ErrSignInProviderGone, and
// nothing is attached.

// ErrExternalIdentityHeld: the identity being attached already belongs to a
// different User. An External identity is held by exactly one User, and an
// attach neither reassigns nor shares it.
var ErrExternalIdentityHeld = errors.New("auth: that external identity is held by another user")

// ErrUnknownSignInProvider: an attach named no enabled password-flow Sign-in
// provider.
var ErrUnknownSignInProvider = errors.New("auth: no such password sign-in provider")

// AttachExternal links the answer providerID vouched for to the existing User
// userID. A `remote` User is a linked Server, which may never sign in as a person
// (ADR-0054), so it may not hold an identity that would let it: ErrForbidden.
func (s *Service) AttachExternal(userID, providerID string, answer ExternalAnswer) error {
	user, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if user.Role == RoleRemote {
		return ErrForbidden
	}
	err = s.store.AttachExternalIdentity(user.ID, providerID, answer.Subject, answer.Username, answer.Groups)
	if errors.Is(err, store.ErrExternalIdentityTaken) {
		return ErrExternalIdentityHeld
	}
	if errors.Is(err, store.ErrSignInProviderUninstalled) {
		return ErrSignInProviderGone
	}
	if err != nil {
		return err
	}
	if err := s.store.SetExternalRefreshToken(providerID, answer.Subject, answer.RefreshToken); err != nil {
		return err
	}
	// An attach is a provider vouching like any sign-in, so the mapping syncs —
	// which, for the User with a Local password, is nothing at all.
	return s.SyncGroupMapping(user.ID)
}

// ExternalIdentities lists the External identities userID holds, oldest first.
func (s *Service) ExternalIdentities(userID string) ([]store.ExternalIdentity, error) {
	return s.store.ExternalIdentitiesByUser(userID)
}

// AttachWithPassword asks the one password-flow Sign-in provider providerID
// about a credential and, on an accepted answer, attaches it to userID. It
// needs proof first — see reauth.go — and only then puts the password to the
// directory, exactly as a login does, so it is limited and charged exactly as a
// login is: the same counters, keyed the same way, or it would be the way round
// them. A rejection is ErrInvalidCredentials.
func (s *Service) AttachWithPassword(ctx context.Context, userID, session string, proof Reauth, providerID, username, password, clientIP string) (ExternalAnswer, error) {
	if err := s.CheckReauth(ctx, userID, session, proof, clientIP); err != nil {
		return ExternalAnswer{}, err
	}
	answer, err := s.checkWithProvider(ctx, providerID, username, password, clientIP)
	if err != nil {
		return ExternalAnswer{}, err
	}
	if err := s.AttachExternal(userID, providerID, answer); err != nil {
		return ExternalAnswer{}, err
	}
	return answer, nil
}

// checkWithProvider asks the one password-flow Sign-in provider providerID
// about a credential, limited and charged as a login is.
func (s *Service) checkWithProvider(ctx context.Context, providerID, username, password, clientIP string) (ExternalAnswer, error) {
	var provider PasswordProvider
	for _, p := range s.passwordProviders() {
		if p.ID() == providerID {
			provider = p
			break
		}
	}
	if provider == nil {
		return ExternalAnswer{}, ErrUnknownSignInProvider
	}
	if err := s.refuseLogin(username, clientIP); err != nil {
		return ExternalAnswer{}, err
	}
	answer, ok := provider.CheckPassword(ctx, username, password)
	if !ok {
		s.chargeLoginFailure(username, clientIP)
		return ExternalAnswer{}, ErrInvalidCredentials
	}
	return answer, nil
}
