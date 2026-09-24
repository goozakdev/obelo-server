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
// the Plugin answers accepted (with an identity) or not.
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
