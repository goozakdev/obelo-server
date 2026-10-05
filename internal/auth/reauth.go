package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Re-authentication for attaching an External identity. Attaching one is a new
// way into the account for good, so a session alone never does it — a session
// can be a borrowed laptop, a stolen cookie, a phone left unlocked. The attach
// itself must carry proof that the person at the keyboard is the User:
//
//   - A User with a Local password puts it in the attach. It is checked,
//     limited and charged exactly as a login's is, keyed by the User's own
//     username, or the attach would be a way to guess it round the limiter.
//   - A User without one (a Member minted from an External identity) signs in
//     again through an identity they already hold, just before. That sign-in
//     answers a re-auth grant: a secret good ONCE, for ReauthGrantTTL, only for
//     that User and only from that session, which the attach then presents.
//
// A grant is never a substitute for a Local password: the User who has one
// must type it.

// ReauthGrantTTL is how long a re-auth grant lives: long enough to go from
// "confirm it is you" to pressing Attach, including one redirect round trip,
// and short enough that it cannot be found later and used.
const ReauthGrantTTL = 5 * time.Minute

// ErrReauthRequired: an attach did not prove the caller is the User — no Local
// password, a wrong one, or no live grant of theirs from this session — or a
// re-auth came back as an identity the caller does not hold.
var ErrReauthRequired = errors.New("auth: re-authentication required")

// Reauth is the proof an attach carries. LocalPassword is for a User who has
// one; Grant is for a User who does not.
type Reauth struct {
	LocalPassword string
	Grant         string
}

// ReauthGrant is a minted grant: the raw secret, handed out once, and how long
// it lives.
type ReauthGrant struct {
	Grant     string
	ExpiresIn time.Duration
}

// reauthGrant is a live grant, stored under its hash.
type reauthGrant struct {
	userID  string
	session string // hashToken of the bearer token it was minted on
	expires time.Time
}

// reauthGrants is the Service's live grants. In memory on purpose: a grant
// lives minutes, and a restart that forgets them costs one more re-auth.
type reauthGrants struct {
	mu     sync.Mutex
	grants map[string]reauthGrant
}

// CheckReauth answers whether proof shows the caller behind session is the User
// userID, and spends a grant it presents. nil means yes; ErrReauthRequired
// means no; a `remote` User is ErrForbidden, since it may not attach at all.
func (s *Service) CheckReauth(ctx context.Context, userID, session string, proof Reauth, clientIP string) error {
	_, err := s.CheckReauthRefundable(ctx, userID, session, proof, clientIP)
	return err
}

// CheckReauthRefundable is CheckReauth for a caller that may fail for a reason
// that is not the caller's fault after the proof is accepted. refund puts a spent
// grant back, with its original expiry; it does nothing for a password proof or
// a refused one, and is always safe to call.
func (s *Service) CheckReauthRefundable(ctx context.Context, userID, session string, proof Reauth, clientIP string) (refund func(), err error) {
	refund = func() {}
	user, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return refund, ErrUserNotFound
	}
	if err != nil {
		return refund, err
	}
	if user.Role == RoleRemote {
		return refund, ErrForbidden
	}
	if user.PasswordHash == "" {
		restore, ok := s.takeReauthGrant(proof.Grant, user.ID, session)
		if !ok {
			return refund, ErrReauthRequired
		}
		return restore, nil
	}
	if proof.LocalPassword == "" {
		return refund, ErrReauthRequired
	}
	if err := s.refuseLogin(user.Username, clientIP); err != nil {
		return refund, err
	}
	verr := VerifyPasswordContext(ctx, user.PasswordHash, proof.LocalPassword)
	if kdfAbandoned(verr) {
		return refund, verr
	}
	if verr != nil {
		s.chargeLoginFailure(user.Username, clientIP)
		return refund, ErrReauthRequired
	}
	return refund, nil
}

// ReauthWithPassword re-authenticates userID through a password-flow identity
// they hold, and answers a grant for session. The credential goes to the
// directory exactly as a login's does, so it is limited and charged as one; a
// rejection is ErrInvalidCredentials. An accepted identity userID does not hold
// is ErrReauthRequired.
func (s *Service) ReauthWithPassword(ctx context.Context, userID, session, providerID, username, password, clientIP string) (ReauthGrant, error) {
	answer, err := s.checkWithProvider(ctx, providerID, username, password, clientIP)
	if err != nil {
		return ReauthGrant{}, err
	}
	return s.ReauthExternal(userID, session, providerID, answer)
}

// ReauthExternal answers a grant for userID and session once a Sign-in provider
// has vouched for (providerID, answer.Subject) — which must be an identity
// userID already holds, or it is ErrReauthRequired.
func (s *Service) ReauthExternal(userID, session, providerID string, answer ExternalAnswer) (ReauthGrant, error) {
	holder, err := s.store.ExternalIdentityUser(providerID, answer.Subject)
	if errors.Is(err, store.ErrNotFound) || (err == nil && holder.ID != userID) {
		return ReauthGrant{}, ErrReauthRequired
	}
	if err != nil {
		return ReauthGrant{}, err
	}
	if userID == "" || session == "" {
		return ReauthGrant{}, ErrReauthRequired
	}
	raw, err := newToken()
	if err != nil {
		return ReauthGrant{}, err
	}
	now := s.now()
	s.reauth.mu.Lock()
	defer s.reauth.mu.Unlock()
	if s.reauth.grants == nil {
		s.reauth.grants = map[string]reauthGrant{}
	}
	for k, g := range s.reauth.grants {
		if !now.Before(g.expires) {
			delete(s.reauth.grants, k)
		}
	}
	s.reauth.grants[hashToken(raw)] = reauthGrant{
		userID:  userID,
		session: hashToken(session),
		expires: now.Add(ReauthGrantTTL),
	}
	return ReauthGrant{Grant: raw, ExpiresIn: ReauthGrantTTL}, nil
}

// takeReauthGrant spends raw — whoever presents it, so a grant is tried once —
// and reports whether it was live and minted for userID on session. restore puts
// the grant back as it was, if it is still live and has not been presented since.
func (s *Service) takeReauthGrant(raw, userID, session string) (restore func(), ok bool) {
	if raw == "" || userID == "" || session == "" {
		return nil, false
	}
	key := hashToken(raw)
	s.reauth.mu.Lock()
	g, found := s.reauth.grants[key]
	delete(s.reauth.grants, key)
	s.reauth.mu.Unlock()
	if !found || g.userID != userID || g.session != hashToken(session) || !s.now().Before(g.expires) {
		return nil, false
	}
	return func() {
		if !s.now().Before(g.expires) {
			return
		}
		s.reauth.mu.Lock()
		defer s.reauth.mu.Unlock()
		if s.reauth.grants == nil {
			s.reauth.grants = map[string]reauthGrant{}
		}
		s.reauth.grants[key] = g
	}, true
}
