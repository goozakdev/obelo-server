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

// signIn holds the Sign-in providers a Service consults. Set once at composition,
// read on every login; the mutex is for the tests that build a Service and then
// hand it providers.
type signIn struct {
	mu        sync.RWMutex
	providers SignInProviders
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

// resolveExternalIdentity turns an accepted answer into the User it signs in as:
// the holder of (providerID, subject) if there is one, otherwise a new Member.
func (s *Service) resolveExternalIdentity(providerID string, answer ExternalAnswer) (store.User, error) {
	user, err := s.store.ExternalIdentityUser(providerID, answer.Subject)
	if err == nil {
		if err := s.store.RecordExternalSignIn(providerID, answer.Subject, answer.Username, answer.Groups); err != nil {
			return store.User{}, err
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
	user, err = s.store.CreateExternalMember(uuid.NewString(), answer.Username,
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
	case isUniqueViolation(err):
		return store.User{}, ErrUsernameCollision
	case err != nil:
		return store.User{}, err
	}
	return user, nil
}
