package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The REDIRECT flow of a Sign-in provider (ADR-0063 decisions 2 and 8), host
// side. Everything a round trip rests on is minted and checked here: state, the
// PKCE verifier, the nonce, and a binding to the browser that started it. A
// Plugin is asked for two things only — an authorize URL and an exchange — and
// what the exchange answers is judged here, never by the Plugin:
//
//   - A provider whose manifest declares an idToken must answer with an ID
//     token, and the token must verify against the issuer and client id the
//     operator typed: signature against the issuer's JWKS, iss, aud, nonce, exp.
//     The subject and groups are then the TOKEN's, whatever the Plugin reported
//     beside it.
//   - A provider that declares none is a plain OAuth2 provider. There is nothing
//     to verify, state and PKCE are all that anchor it, and the Plugin's identity
//     is accepted as given — which the Admin screen says in so many words.
//
// Every refusal is ErrRedirectRefused, whichever check failed: the person at the
// browser learns that the sign-in did not work and nothing about why.

// ErrRedirectRefused is every refused redirect sign-in: an unknown or expired
// state, a browser that did not start it, a failed exchange, or an answer the
// host will not sign anybody in on.
var ErrRedirectRefused = errors.New("signin: the redirect sign-in was refused")

// ErrUnknownRedirectProvider is a start naming no registered redirect-flow
// Sign-in provider.
var ErrUnknownRedirectProvider = errors.New("signin: no such redirect sign-in provider")

// ErrRedirectNotConfigured is a start for a provider whose ID tokens cannot be
// verified yet, because the operator has not typed its issuer and client id.
var ErrRedirectNotConfigured = errors.New("signin: the redirect sign-in provider is not configured")

// redirectTTL is how long a started sign-in may take to come back.
const redirectTTL = 10 * time.Minute

// maxPendingRedirects bounds the sign-ins in flight. Starting one is
// unauthenticated, so the table must not grow with whoever asks. A start at the
// cap gives up the oldest sign-in in flight rather than being refused: refusing
// would let whoever fills the table lock everybody out of signing in.
const maxPendingRedirects = 1024

// maxPendingRedirectsPerClient bounds the sign-ins in flight from one client
// address, so one client cannot fill the table for everybody else. A start at
// this cap gives up that client's oldest: behind a reverse proxy the server was
// not told to trust, every browser shares one address, and a refusal there
// would be a refusal for all of them.
const maxPendingRedirectsPerClient = 16

// IDTokenAudience is implemented by a redirect provider the host can verify:
// the issuer and client id the operator typed, and whether the provider declared
// an ID token at all. internal/plugins' adapter implements it; a provider that
// does not is a plain OAuth2 provider.
type IDTokenAudience interface {
	IDTokenAudience() (issuer, clientID string, declared bool)
}

// RedirectProvider is one redirect-flow Sign-in provider as the login screen and
// the Admin screen list it.
type RedirectProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Verified is whether the host verifies this provider's ID tokens. False is a
	// plain OAuth2 provider, whose identity is not independently verified.
	Verified bool `json:"verified"`
	// Configured is whether it can be used now: a verified provider needs the
	// issuer and client id the operator types.
	Configured bool `json:"configured"`
}

// Redirects is the host half of the redirect flow.
type Redirects struct {
	reg      *pluginapi.Registry
	verifier *idTokenVerifier
	now      func() time.Time

	mu      sync.Mutex
	pending map[string]pendingRedirect
	started uint64
}

// pendingRedirect is one started sign-in, keyed by its state.
type pendingRedirect struct {
	providerID  string
	verifier    string
	nonce       string
	redirectURI string
	client      string
	binding     [sha256.Size]byte
	expires     time.Time
	// seq orders the starts, oldest lowest, for eviction.
	seq uint64
	// purpose is what this round trip was started for. Each is good only at
	// its own callback.
	purpose roundTrip
}

// roundTrip is what a redirect was started for: a sign-in (the zero value), or,
// for the signed-in User user, an attach or a re-authentication.
type roundTrip struct {
	user   string
	reauth bool
}

// NewRedirects returns the redirect flow over reg's Sign-in providers.
func NewRedirects(reg *pluginapi.Registry) *Redirects {
	return &Redirects{
		reg:      reg,
		verifier: newIDTokenVerifier(),
		now:      time.Now,
		pending:  map[string]pendingRedirect{},
	}
}

// Providers lists every registered redirect-flow Sign-in provider, in
// registration order.
func (r *Redirects) Providers() []RedirectProvider {
	var out []RedirectProvider
	for _, reg := range r.reg.SignInProviders() {
		if !reg.Descriptor.HasCapability(pluginapi.CapabilityRedirectSignIn) {
			continue
		}
		p := RedirectProvider{ID: reg.Descriptor.Slug, Name: reg.Descriptor.Name, Configured: true}
		if built, err := reg.New(pluginapi.Settings{Enabled: true}); err == nil {
			if a, ok := built.(IDTokenAudience); ok {
				issuer, clientID, declared := a.IDTokenAudience()
				p.Verified = declared
				p.Configured = !declared || (issuer != "" && clientID != "")
			}
		} else {
			p.Configured = false
		}
		out = append(out, p)
	}
	return out
}

// Ready lists the redirect providers a person can sign in with now.
func (r *Redirects) Ready() []RedirectProvider {
	var out []RedirectProvider
	for _, p := range r.Providers() {
		if p.Configured {
			out = append(out, p)
		}
	}
	return out
}

// Started is a sign-in on its way to the provider: the URL to send the browser
// to, and the binding the browser must present when it comes back.
type Started struct {
	URL     string
	Binding string
}

// provider builds the redirect provider registered as id.
func (r *Redirects) provider(id string) (pluginapi.SignInRedirectProvider, error) {
	reg, ok := r.reg.SignInProvider(id)
	if !ok || !reg.Descriptor.HasCapability(pluginapi.CapabilityRedirectSignIn) {
		return nil, ErrUnknownRedirectProvider
	}
	built, err := reg.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		log.Printf("obelo: sign-in provider %s: %v", id, err)
		return nil, ErrRedirectNotConfigured
	}
	p, ok := built.(pluginapi.SignInRedirectProvider)
	if !ok {
		return nil, ErrUnknownRedirectProvider
	}
	return p, nil
}

// audienceOf is what the host verifies p's ID tokens against.
func audienceOf(p pluginapi.SignInRedirectProvider) (issuer, clientID string, declared bool) {
	if a, ok := p.(IDTokenAudience); ok {
		return a.IDTokenAudience()
	}
	return "", "", false
}

// Start mints a sign-in with providerID that will come back to redirectURI, for
// the client at the address client.
func (r *Redirects) Start(ctx context.Context, providerID, redirectURI, client string) (Started, error) {
	return r.start(ctx, providerID, redirectURI, client, roundTrip{})
}

// StartAttach mints a round trip with providerID that attaches what it comes
// back with to the signed-in User userID (ADR-0063 decision 3). It is checked
// exactly as a sign-in is, and is good only at CompleteAttach, for that User.
func (r *Redirects) StartAttach(ctx context.Context, providerID, redirectURI, client, userID string) (Started, error) {
	if userID == "" {
		return Started{}, ErrRedirectRefused
	}
	return r.start(ctx, providerID, redirectURI, client, roundTrip{user: userID})
}

// StartReauth mints a round trip with providerID that re-authenticates the
// signed-in User userID before an attach. It is checked exactly as a sign-in is,
// and is good only at CompleteReauth, for that User.
func (r *Redirects) StartReauth(ctx context.Context, providerID, redirectURI, client, userID string) (Started, error) {
	if userID == "" {
		return Started{}, ErrRedirectRefused
	}
	return r.start(ctx, providerID, redirectURI, client, roundTrip{user: userID, reauth: true})
}

func (r *Redirects) start(ctx context.Context, providerID, redirectURI, client string, purpose roundTrip) (Started, error) {
	p, err := r.provider(providerID)
	if err != nil {
		return Started{}, err
	}
	if issuer, clientID, declared := audienceOf(p); declared && (issuer == "" || clientID == "") {
		return Started{}, ErrRedirectNotConfigured
	}
	state, verifier, nonce, binding := randomToken(), randomToken(), randomToken(), randomToken()
	challenge := sha256.Sum256([]byte(verifier))

	resp, err := p.AuthorizeURL(ctx, pluginapi.SignInAuthorizeRequest{
		State:               state,
		CodeChallenge:       base64.RawURLEncoding.EncodeToString(challenge[:]),
		CodeChallengeMethod: "S256",
		Nonce:               nonce,
		RedirectURI:         redirectURI,
	})
	if err != nil {
		log.Printf("obelo: sign-in provider %s: %v", providerID, err)
		return Started{}, fmt.Errorf("signin: %s could not build an authorize URL: %w", providerID, err)
	}
	u, perr := url.Parse(resp.URL)
	if perr != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Started{}, fmt.Errorf("signin: %s answered an authorize URL that is not an absolute http(s) URL", providerID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	fromClient := 0
	var oldest, oldestFromClient string
	for k, v := range r.pending {
		if !now.Before(v.expires) {
			delete(r.pending, k)
			continue
		}
		if oldest == "" || v.seq < r.pending[oldest].seq {
			oldest = k
		}
		if v.client == client {
			fromClient++
			if oldestFromClient == "" || v.seq < r.pending[oldestFromClient].seq {
				oldestFromClient = k
			}
		}
	}
	if fromClient >= maxPendingRedirectsPerClient {
		delete(r.pending, oldestFromClient)
	} else if len(r.pending) >= maxPendingRedirects {
		delete(r.pending, oldest)
	}
	r.started++
	r.pending[state] = pendingRedirect{
		providerID:  providerID,
		verifier:    verifier,
		nonce:       nonce,
		redirectURI: redirectURI,
		client:      client,
		binding:     sha256.Sum256([]byte(binding)),
		expires:     now.Add(redirectTTL),
		seq:         r.started,
		purpose:     purpose,
	}
	return Started{URL: u.String(), Binding: binding}, nil
}

// take removes and returns the sign-in started under state. A state is good for
// one attempt, whatever that attempt's outcome.
func (r *Redirects) take(state string) (pendingRedirect, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[state]
	if !ok {
		return pendingRedirect{}, false
	}
	delete(r.pending, state)
	if !r.now().Before(p.expires) {
		return pendingRedirect{}, false
	}
	return p, true
}

// Complete finishes the sign-in started under state with the code the browser
// came back with, and answers who it signs in as: the provider's id and the
// judged answer, ready for auth.Service.SignInExternal.
func (r *Redirects) Complete(ctx context.Context, state, code, binding string) (string, auth.ExternalAnswer, error) {
	return r.complete(ctx, state, code, binding, roundTrip{})
}

// CompleteAttach finishes the attach userID started under state, and answers
// the identity to attach to them. A sign-in's state, or an attach another User
// started, is refused like any other failed check.
func (r *Redirects) CompleteAttach(ctx context.Context, state, code, binding, userID string) (string, auth.ExternalAnswer, error) {
	if userID == "" {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	return r.complete(ctx, state, code, binding, roundTrip{user: userID})
}

// CompleteReauth finishes the re-authentication userID started under state, and
// answers the identity it came back as. Any other round trip's state is refused
// like any other failed check.
func (r *Redirects) CompleteReauth(ctx context.Context, state, code, binding, userID string) (string, auth.ExternalAnswer, error) {
	if userID == "" {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	return r.complete(ctx, state, code, binding, roundTrip{user: userID, reauth: true})
}

// complete is Complete, CompleteAttach and CompleteReauth: purpose is what the
// round trip must have been started for.
func (r *Redirects) complete(ctx context.Context, state, code, binding string, purpose roundTrip) (string, auth.ExternalAnswer, error) {
	if state == "" || code == "" {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	pending, ok := r.take(state)
	if !ok {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	if pending.purpose != purpose {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	presented := sha256.Sum256([]byte(binding))
	if binding == "" || subtle.ConstantTimeCompare(presented[:], pending.binding[:]) != 1 {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	p, err := r.provider(pending.providerID)
	if err != nil {
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	resp, err := p.Exchange(ctx, pluginapi.SignInExchangeRequest{
		Code:         code,
		CodeVerifier: pending.verifier,
		RedirectURI:  pending.redirectURI,
	})
	if err != nil {
		log.Printf("obelo: sign-in provider %s: %v", pending.providerID, err)
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	answer, err := r.judgeExchange(ctx, p, resp, pending.nonce)
	if err != nil {
		log.Printf("obelo: sign-in provider %s: the redirect sign-in was refused: %v", pending.providerID, err)
		return "", auth.ExternalAnswer{}, ErrRedirectRefused
	}
	// A refresh token is kept only for a provider that will be asked to redeem
	// it: one nobody will use is a credential stored for nothing.
	if reg, ok := r.reg.SignInProvider(pending.providerID); ok &&
		reg.Descriptor.HasCapability(pluginapi.CapabilitySignInRefresh) {
		answer.RefreshToken = resp.RefreshToken
	}
	return pending.providerID, answer, nil
}

// judgeExchange is the host's whole judgment on one exchange.
func (r *Redirects) judgeExchange(ctx context.Context, p pluginapi.SignInRedirectProvider, resp pluginapi.SignInExchangeResponse, nonce string) (auth.ExternalAnswer, error) {
	if !resp.Accepted || resp.Identity == nil {
		return auth.ExternalAnswer{}, errors.New("the provider did not accept")
	}
	issuer, clientID, declared := audienceOf(p)
	if !declared {
		// A plain OAuth2 provider: state and PKCE are all that anchor it, and its
		// identity is taken as given. A token it answers anyway is one nobody
		// could verify, and an unverifiable token is refused rather than ignored.
		if resp.IDToken != "" {
			return auth.ExternalAnswer{}, errors.New("a provider that declares no ID token answered one")
		}
		subject := strings.TrimSpace(resp.Identity.Subject)
		username := strings.TrimSpace(resp.Identity.Username)
		if subject == "" || username == "" {
			return auth.ExternalAnswer{}, errors.New("the answer names no subject or no username")
		}
		return auth.ExternalAnswer{Subject: subject, Username: username, Groups: tidyGroups(resp.Identity.Groups)}, nil
	}

	// An OpenID Connect provider: the token or nothing. Subject and groups are the
	// verified token's; the Plugin's identity supplies a username only when the
	// token carries none, and a username is a label, never a key.
	if resp.IDToken == "" {
		return auth.ExternalAnswer{}, errors.New("the provider declares ID tokens and answered none")
	}
	if issuer == "" || clientID == "" {
		return auth.ExternalAnswer{}, errors.New("the issuer or the client id is not configured")
	}
	claims, err := r.verifier.verify(ctx, resp.IDToken, issuer, clientID, nonce)
	if err != nil {
		return auth.ExternalAnswer{}, err
	}
	username := claims.PreferredUsername
	if username == "" {
		username = strings.TrimSpace(resp.Identity.Username)
	}
	if username == "" {
		return auth.ExternalAnswer{}, errors.New("neither the ID token nor the provider names a username")
	}
	return auth.ExternalAnswer{Subject: claims.Subject, Username: username, Groups: tidyGroups(claims.Groups)}, nil
}

// tidyGroups trims, de-duplicates and drops blank groups, for the reason Judge
// gives.
func tidyGroups(in []string) []string {
	var groups []string
	seen := map[string]bool{}
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		groups = append(groups, g)
	}
	return groups
}

// randomToken is 32 bytes from crypto/rand, base64url: a state, a PKCE verifier
// (43 characters, inside RFC 7636's 43-128), a nonce or a binding.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("signin: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
