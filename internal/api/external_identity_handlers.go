package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/signin"
)

// Attaching an External identity from one's own profile (ADR-0063 decision 3):
// the only way an existing User gains one. Every surface is the CALLER's — there
// is no user id in any of them — and each needs a session:
//
//	GET  /auth/external-identities           → the caller's identities, whether
//	                                           they have a Local password, and
//	                                           the providers of each flow they
//	                                           can attach from
//	POST /auth/external-identities/password  { "provider", "username",
//	                                           "password", proof } → { "identity" }
//	POST /auth/redirect/attach/start         { "provider", proof } → { "url" },
//	                                           and the binding cookie, as a
//	                                           sign-in's start
//	POST /auth/redirect/attach/callback      { "state", "code" } → { "identity" }
//	POST /auth/reauth/password               { "provider", "username",
//	                                           "password" } → { "grant",
//	                                           "expiresIn" }
//	POST /auth/redirect/reauth/start         { "provider" } → { "url" }, as above
//	POST /auth/redirect/reauth/callback      { "state", "code" } → { "grant",
//	                                           "expiresIn" }
//
// A session alone never attaches (auth/reauth.go). proof is "currentPassword"
// — the caller's Local password — or, for a caller without one, "reauthGrant":
// what a re-auth through an identity they already hold answered, good once, for
// five minutes, from this session. A redirect attach carries it at the start,
// the request that asks for the round trip.
//
// Every redirect here returns to the same /sign-in/callback a sign-in does, so
// an operator registers one redirect URI; the server knows which round trip a
// state belongs to, and whose.

// reauthRequiredMessage is every failed proof, whichever proof it was.
const reauthRequiredMessage = "Confirm it is you first: enter your password, or sign in again with a sign-in already attached to your account."

// externalIdentityHeldMessage names nobody: the identity is somebody's, and who
// is not the caller's business.
const externalIdentityHeldMessage = "That sign-in is already attached to another account on this server."

type externalIdentityJSON struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	Username     string `json:"username"`
}

type externalIdentitiesResponse struct {
	Identities []externalIdentityJSON `json:"identities"`
	// HasPassword is whether the caller has a Local password, which is then the
	// proof an attach carries; without one it is a re-auth grant.
	HasPassword bool `json:"hasPassword"`
	// Password and Redirect are the providers the caller can attach from now, by
	// flow.
	Password []signin.Provider      `json:"password"`
	Redirect []redirectProviderJSON `json:"redirect"`
}

type attachedIdentityResponse struct {
	Identity externalIdentityJSON `json:"identity"`
}

// providerNames is every Sign-in provider's display name by id.
func providerNames(deps Deps) map[string]string {
	names := map[string]string{}
	if deps.SignInProviders != nil {
		for _, p := range deps.SignInProviders.Providers() {
			names[p.ID] = p.Name
		}
	}
	if deps.SignInRedirect != nil {
		for _, p := range deps.SignInRedirect.Providers() {
			names[p.ID] = p.Name
		}
	}
	return names
}

func identityJSON(names map[string]string, providerID, username string) externalIdentityJSON {
	name := names[providerID]
	if name == "" {
		name = providerID
	}
	return externalIdentityJSON{Provider: providerID, ProviderName: name, Username: username}
}

func handleExternalIdentities(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		held, err := deps.Auth.ExternalIdentities(id.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "could not list your sign-ins", nil)
			return
		}
		names := providerNames(deps)
		out := externalIdentitiesResponse{
			Identities:  []externalIdentityJSON{},
			HasPassword: id.User.PasswordHash != "",
			Password:    []signin.Provider{},
			Redirect:    []redirectProviderJSON{},
		}
		for _, x := range held {
			out.Identities = append(out.Identities, identityJSON(names, x.PluginID, x.Username))
		}
		if deps.SignInProviders != nil {
			out.Password = append(out.Password, deps.SignInProviders.Providers()...)
		}
		if deps.SignInRedirect != nil {
			for _, p := range deps.SignInRedirect.Ready() {
				out.Redirect = append(out.Redirect, redirectProviderJSON{ID: p.ID, Name: p.Name})
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// writeProofError answers a failed proof or re-auth, and reports whether err
// was one.
func writeProofError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, auth.ErrReauthRequired):
		writeError(w, http.StatusForbidden, codeReauthRequired, reauthRequiredMessage, nil)
	case errors.Is(err, auth.ErrTooManyLoginAttempts):
		var throttled *auth.LoginThrottledError
		if errors.As(err, &throttled) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(throttled.RetryAfter)))
		}
		writeError(w, http.StatusTooManyRequests, codeTooManyAttempts,
			"too many failed sign-in attempts; wait and try again", nil)
	default:
		return false
	}
	return true
}

// writeAttachError maps what an attach can fail with, after the provider said
// yes or no.
func writeAttachError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrExternalIdentityHeld):
		writeError(w, http.StatusConflict, codeExternalIdentityHeld, externalIdentityHeldMessage, nil)
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, codeForbidden, "this account cannot attach a sign-in", nil)
	case errors.Is(err, auth.ErrSignInProviderGone):
		// The provider was uninstalled after it answered: refused, as a sign-in is.
		writeError(w, http.StatusUnauthorized, codeSignInRefused, signInRefusedMessage, nil)
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "the sign-in could not be attached", nil)
	}
}

// reauthProof is the proof fields an attach request carries.
type reauthProof struct {
	CurrentPassword string `json:"currentPassword"`
	ReauthGrant     string `json:"reauthGrant"`
}

func (p reauthProof) proof() auth.Reauth {
	return auth.Reauth{LocalPassword: p.CurrentPassword, Grant: p.ReauthGrant}
}

type attachPasswordRequest struct {
	Provider string `json:"provider"`
	Username string `json:"username"`
	Password string `json:"password"`
	reauthProof
}

func handleAttachPasswordIdentity(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req attachPasswordRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		answer, err := deps.Auth.AttachWithPassword(r.Context(), id.User.ID, id.Token, req.proof(),
			req.Provider, req.Username, req.Password, clientIP(r))
		if err != nil {
			if !writePasswordCheckError(w, err) {
				writeAttachError(w, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, attachedIdentityResponse{
			Identity: identityJSON(providerNames(deps), req.Provider, answer.Username),
		})
	}
}

// writePasswordCheckError answers what putting a password to a password-flow
// provider can fail with, and reports whether err was one of those.
func writePasswordCheckError(w http.ResponseWriter, err error) bool {
	switch {
	case writeProofError(w, err):
	case errors.Is(err, auth.ErrUnknownSignInProvider):
		writeError(w, http.StatusNotFound, codeNotFound, "no such sign-in provider", nil)
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, codeSignInRefused,
			"the sign-in provider did not accept that username and password", nil)
	default:
		return false
	}
	return true
}

type attachRedirectStartRequest struct {
	Provider string `json:"provider"`
	reauthProof
}

func handleAttachRedirectStart(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req attachRedirectStartRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := deps.Auth.CheckReauth(r.Context(), id.User.ID, id.Token, req.proof(), clientIP(r)); err != nil {
			if !writeProofError(w, err) {
				writeAttachError(w, err)
			}
			return
		}
		started, err := deps.SignInRedirect.StartAttach(r.Context(), req.Provider,
			redirectBaseURL(r)+signInCallbackPath, clientIP(r), id.User.ID)
		writeRedirectStart(w, r, started, err)
	}
}

type attachRedirectCallbackRequest struct {
	State string `json:"state"`
	Code  string `json:"code"`
}

func handleAttachRedirectCallback(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req attachRedirectCallbackRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		binding := ""
		if c, err := r.Cookie(signInBindingCookieNameFor(r)); err == nil {
			binding = c.Value
		}
		// Spent whatever happens next, as a sign-in's is.
		setSignInBindingCookie(w, r, "", 0)

		providerID, answer, err := deps.SignInRedirect.CompleteAttach(r.Context(), req.State, req.Code, binding, id.User.ID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, codeSignInRefused, signInRefusedMessage, nil)
			return
		}
		if err := deps.Auth.AttachExternal(id.User.ID, providerID, answer); err != nil {
			writeAttachError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, attachedIdentityResponse{
			Identity: identityJSON(providerNames(deps), providerID, answer.Username),
		})
	}
}

type reauthGrantResponse struct {
	Grant     string `json:"grant"`
	ExpiresIn int    `json:"expiresIn"`
}

func writeReauthGrant(w http.ResponseWriter, g auth.ReauthGrant) {
	writeJSON(w, http.StatusOK, reauthGrantResponse{Grant: g.Grant, ExpiresIn: int(g.ExpiresIn.Seconds())})
}

type reauthPasswordRequest struct {
	Provider string `json:"provider"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleReauthPassword(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req reauthPasswordRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		grant, err := deps.Auth.ReauthWithPassword(r.Context(), id.User.ID, id.Token,
			req.Provider, req.Username, req.Password, clientIP(r))
		if err != nil {
			if !writePasswordCheckError(w, err) {
				writeError(w, http.StatusInternalServerError, codeInternal, "could not confirm it is you", nil)
			}
			return
		}
		writeReauthGrant(w, grant)
	}
}

func handleReauthRedirectStart(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req redirectStartRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		started, err := deps.SignInRedirect.StartReauth(r.Context(), req.Provider,
			redirectBaseURL(r)+signInCallbackPath, clientIP(r), id.User.ID)
		writeRedirectStart(w, r, started, err)
	}
}

func handleReauthRedirectCallback(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req attachRedirectCallbackRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		binding := ""
		if c, err := r.Cookie(signInBindingCookieNameFor(r)); err == nil {
			binding = c.Value
		}
		setSignInBindingCookie(w, r, "", 0)

		providerID, answer, err := deps.SignInRedirect.CompleteReauth(r.Context(), req.State, req.Code, binding, id.User.ID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, codeSignInRefused, signInRefusedMessage, nil)
			return
		}
		grant, err := deps.Auth.ReauthExternal(id.User.ID, id.Token, providerID, answer)
		if err != nil {
			if !writeProofError(w, err) {
				writeError(w, http.StatusInternalServerError, codeInternal, "could not confirm it is you", nil)
			}
			return
		}
		writeReauthGrant(w, grant)
	}
}
