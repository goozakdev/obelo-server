package v1

import "context"

// The Sign-in provider Extension point (ADR-0063): a Plugin proves who someone
// is on behalf of a source the operator controls — their own directory, their
// own identity provider — and the SERVER decides what that is worth. The Plugin
// never issues a session, never names a User of this server, and never picks a
// role; it answers with an External identity and the groups it belongs to.
//
// Two flows exist by ADR, and a Plugin declares which it implements as a
// Capability on its provides entry. This contract carries the PASSWORD flow: the
// host hands over a username and password typed into the ordinary login form, and
// the Plugin answers accepted (with an identity) or not. The REDIRECT flow is at
// the end of this file.
//
// The identity is keyed by (Plugin id, Subject), never by username (ADR-0063
// decision 3). A username is chosen by a person at a source this server does not
// control, so it travels only as a label; Subject is the provider's own stable id
// and is the only thing the host resolves by.

// SignInPasswordRequest is one password check: exactly what was typed into the
// login form. The host never stores the password and never hands it to anything
// but the Plugin being asked.
type SignInPasswordRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SignInIdentity is who the source says the person is.
type SignInIdentity struct {
	// Subject is the source's own stable id for the person — an LDAP entryUUID,
	// an OIDC `sub`. It must not change when the person is renamed; it is the
	// key the host resolves by, together with the Plugin's id.
	Subject string `json:"subject"`
	// Username is the person's name at the source right now. It names a User the
	// first time this identity is seen and is otherwise only remembered, never
	// resolved by.
	Username string `json:"username,omitempty"`
	// Groups are the source's groups this person belongs to. The host stores them
	// on the identity; what they are worth is an Admin's Group mapping, never the
	// Plugin's say-so.
	Groups []string `json:"groups,omitempty"`
}

// SignInPasswordResponse is what a password check answers. Accepted with an
// Identity is the only answer the host signs anyone in on; anything else —
// Accepted false, or Accepted with no Subject — is a rejection, and a rejection
// is never explained to the person at the form.
type SignInPasswordResponse struct {
	Accepted bool            `json:"accepted"`
	Identity *SignInIdentity `json:"identity,omitempty"`
}

// SignInProvider is the Go call surface of the Sign-in provider Extension point.
// An error means the Plugin failed, and the host treats it exactly as a
// rejection: a failing provider is never a reason to sign somebody in, and never
// a reason the next provider is not asked.
type SignInProvider interface {
	CheckPassword(ctx context.Context, req SignInPasswordRequest) (SignInPasswordResponse, error)
}

// SignInProviderFactory builds a Sign-in provider from the Settings the host
// resolved. Settings.Enabled is true for anything the host builds, and Values
// carries whatever an Installed plugin's manifest declared for itself.
type SignInProviderFactory func(Settings) (SignInProvider, error)

// SignInProviderRegistration is what a Sign-in provider hands the host: what it
// is, and how to build it.
type SignInProviderRegistration struct {
	Descriptor Descriptor
	New        SignInProviderFactory
}

// SignInPasswordCall is what the host hands an INSTALLED Sign-in provider for one
// password check: the request and the Settings the host resolved, travelling
// together as a Subtitle provider's do. The response is un-enveloped — a plain
// SignInPasswordResponse.
type SignInPasswordCall struct {
	Request  SignInPasswordRequest `json:"request"`
	Settings Settings              `json:"settings"`
}

// The REDIRECT flow (ADR-0063 decision 2). The host owns the whole round trip:
// it mints state, the PKCE verifier and the nonce, serves the callback, and
// verifies what comes back. A Plugin supplies exactly two things — where to send
// the browser, and how to turn the code it returns with into an identity — and a
// Plugin that declares this flow implements SignInRedirectProvider on the value
// its factory builds. It never serves a route of its own.

// SignInAuthorizeRequest is what the host hands a redirect provider to build the
// URL a browser is sent to. Every value is the host's: the Plugin puts them in
// the URL and keeps none of them.
type SignInAuthorizeRequest struct {
	// State is the host's opaque value for this one round trip.
	State string `json:"state"`
	// CodeChallenge is the PKCE challenge for the verifier only the host holds,
	// by CodeChallengeMethod (always "S256").
	CodeChallenge       string `json:"codeChallenge"`
	CodeChallengeMethod string `json:"codeChallengeMethod"`
	// Nonce is the value an ID token must carry back. A provider that issues none
	// may ignore it.
	Nonce string `json:"nonce"`
	// RedirectURI is where the provider must send the browser back to: the host's
	// own callback.
	RedirectURI string `json:"redirectUri"`
}

// SignInAuthorizeResponse is the URL the browser is sent to.
type SignInAuthorizeResponse struct {
	URL string `json:"url"`
}

// SignInExchangeRequest is the code the browser came back with, and the PKCE
// verifier and redirect URI the token request must repeat.
type SignInExchangeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
}

// SignInExchangeResponse is what an exchange answers. Accepted with an Identity
// is the only answer anyone is signed in on. IDToken is the raw ID token when the
// provider issued one; the host verifies it and takes the subject and groups from
// IT, never from Identity, so a Plugin cannot vouch for anybody the token does
// not name.
type SignInExchangeResponse struct {
	Accepted bool            `json:"accepted"`
	Identity *SignInIdentity `json:"identity,omitempty"`
	IDToken  string          `json:"idToken,omitempty"`
	// RefreshToken is the refresh token the provider issued, when it issued one.
	// The host stores it on the identity and hands it back to a provider that
	// declares CapabilitySignInRefresh, to re-check the identity between
	// sign-ins; it is never shown to anyone.
	RefreshToken string `json:"refreshToken,omitempty"`
}

// SignInRedirectProvider is the Go call surface of the redirect flow. An error
// is the Plugin failing, and the host refuses the sign-in.
type SignInRedirectProvider interface {
	AuthorizeURL(ctx context.Context, req SignInAuthorizeRequest) (SignInAuthorizeResponse, error)
	Exchange(ctx context.Context, req SignInExchangeRequest) (SignInExchangeResponse, error)
}

// SignInAuthorizeCall is what the host hands an INSTALLED redirect provider to
// build an authorize URL: the request and the resolved Settings. The response is
// un-enveloped — a plain SignInAuthorizeResponse.
type SignInAuthorizeCall struct {
	Request  SignInAuthorizeRequest `json:"request"`
	Settings Settings               `json:"settings"`
}

// SignInExchangeCall is what the host hands an INSTALLED redirect provider for
// one exchange. The response is un-enveloped — a plain SignInExchangeResponse.
type SignInExchangeCall struct {
	Request  SignInExchangeRequest `json:"request"`
	Settings Settings              `json:"settings"`
}

// The RE-CHECK (ADR-0063 decision 4). Between sign-ins the host asks a provider
// again about an identity it vouched for, so an Admin's Group mapping follows the
// directory, and a person removed there loses their sessions here. A provider
// answers through lookup(subject) (CapabilitySignInLookup) or by redeeming a
// refresh token (CapabilitySignInRefresh); the host never stores a password to
// do it.
//
// What the answer is worth is the host's: "gone" or "disabled" revokes every
// session the User holds; an error, or a status the host does not know, is the
// provider being unreachable, and changes nothing but when it is asked again.

// SignInStatus is what a re-check says about an identity.
type SignInStatus string

const (
	// SignInActive: the identity exists and may sign in. The answer's identity
	// carries its groups now.
	SignInActive SignInStatus = "active"
	// SignInGone: the source no longer knows the identity.
	SignInGone SignInStatus = "gone"
	// SignInDisabled: the source knows the identity and refuses it.
	SignInDisabled SignInStatus = "disabled"
)

// AllSignInStatuses is every SignInStatus, in declaration order.
func AllSignInStatuses() []SignInStatus {
	return []SignInStatus{SignInActive, SignInGone, SignInDisabled}
}

// SignInLookupRequest asks about one identity by the subject the provider
// vouched for.
type SignInLookupRequest struct {
	Subject string `json:"subject"`
}

// SignInLookupResponse is what a lookup answers. Identity, with the identity's
// groups now, is read only when Status is active, and its subject must be the
// one asked about.
type SignInLookupResponse struct {
	Status   SignInStatus    `json:"status"`
	Identity *SignInIdentity `json:"identity,omitempty"`
}

// SignInLookupProvider is the Go call surface of lookup(subject). An error is
// the Plugin or its source failing, which the host treats as unreachable.
type SignInLookupProvider interface {
	Lookup(ctx context.Context, req SignInLookupRequest) (SignInLookupResponse, error)
}

// SignInLookupCall is what the host hands an INSTALLED Sign-in provider for one
// lookup. The response is un-enveloped — a plain SignInLookupResponse.
type SignInLookupCall struct {
	Request  SignInLookupRequest `json:"request"`
	Settings Settings            `json:"settings"`
}

// SignInRefreshRequest is the refresh token the provider handed back last.
type SignInRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// SignInRefreshResponse is what a refresh answers. When Status is active, a
// provider that declares ID tokens must answer a fresh IDToken, which the host
// verifies — signature, iss, aud, exp — and takes the subject and groups from;
// a plain OAuth2 provider answers Identity instead. RefreshToken is the rotated
// token, when the source rotated it.
type SignInRefreshResponse struct {
	Status       SignInStatus    `json:"status"`
	Identity     *SignInIdentity `json:"identity,omitempty"`
	IDToken      string          `json:"idToken,omitempty"`
	RefreshToken string          `json:"refreshToken,omitempty"`
}

// SignInRefreshProvider is the Go call surface of a refresh. An error is the
// Plugin or its source failing, which the host treats as unreachable.
type SignInRefreshProvider interface {
	Refresh(ctx context.Context, req SignInRefreshRequest) (SignInRefreshResponse, error)
}

// SignInRefreshCall is what the host hands an INSTALLED redirect provider for one
// refresh. The response is un-enveloped — a plain SignInRefreshResponse.
type SignInRefreshCall struct {
	Request  SignInRefreshRequest `json:"request"`
	Settings Settings             `json:"settings"`
}
