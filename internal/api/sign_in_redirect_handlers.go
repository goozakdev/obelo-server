package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/signin"
)

// Sign-in providers (ADR-0063), the REDIRECT flow. Web-only (decision 8): a TV
// or an iPad reaches an outside identity through the device authorization grant,
// approved from a browser signed in this way. Three public surfaces:
//
//	GET  /auth/sign-in-providers  → { "providers": [ { "id", "name" } ] }
//	                                the redirect providers a person can use now
//	POST /auth/redirect/start     { "provider" } → { "url" }, and the binding
//	                                cookie the callback must carry back
//	POST /auth/redirect/callback  { "state", "code", "device" } → a login's body
//
// The browser comes back to the SPA's /sign-in/callback, which posts what it
// was handed to the callback here. Everything the round trip rests on — state,
// PKCE, the nonce, the binding, and the verdict on the ID token — is the host's;
// see internal/signin.

// signInCallbackPath is the SPA route a provider sends the browser back to
// (web/src/App.tsx).
const signInCallbackPath = "/sign-in/callback"

// The binding cookie ties a callback to the browser that started the sign-in,
// so a code minted for somebody else's session cannot be replayed into this
// one. One name per scheme, for the reason the media cookie has two.
const (
	signInBindingCookieName       = "obelo_sign_in"
	secureSignInBindingCookieName = "__Secure-obelo_sign_in"
	signInBindingCookiePath       = APIPrefix + "/auth/redirect"
	signInBindingCookieMaxAge     = 10 * time.Minute
)

func signInBindingCookieNameFor(r *http.Request) string {
	if requestIsHTTPS(r) {
		return secureSignInBindingCookieName
	}
	return signInBindingCookieName
}

func setSignInBindingCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	c := &http.Cookie{
		Name:     signInBindingCookieNameFor(r),
		Value:    value,
		Path:     signInBindingCookiePath,
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge <= 0 {
		c.MaxAge = -1
		c.Expires = time.Unix(0, 0)
	}
	http.SetCookie(w, c)
}

// signInRefusedMessage is what every refused redirect sign-in says.
const signInRefusedMessage = "the sign-in could not be completed; try again"

type redirectProvidersResponse struct {
	Providers []redirectProviderJSON `json:"providers"`
}

type redirectProviderJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func handleRedirectSignInProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := []redirectProviderJSON{}
		if deps.SignInRedirect != nil {
			for _, p := range deps.SignInRedirect.Ready() {
				out = append(out, redirectProviderJSON{ID: p.ID, Name: p.Name})
			}
		}
		writeJSON(w, http.StatusOK, redirectProvidersResponse{Providers: out})
	}
}

type redirectStartRequest struct {
	Provider string `json:"provider"`
}

type redirectStartResponse struct {
	URL string `json:"url"`
}

func handleRedirectSignInStart(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req redirectStartRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		started, err := deps.SignInRedirect.Start(r.Context(), req.Provider, externalBaseURL(r)+signInCallbackPath, clientIP(r))
		switch {
		case errors.Is(err, signin.ErrUnknownRedirectProvider):
			writeError(w, http.StatusNotFound, codeNotFound, "no such sign-in provider", nil)
			return
		case errors.Is(err, signin.ErrRedirectNotConfigured):
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"this sign-in provider is not configured yet", nil)
			return
		case err != nil:
			writeError(w, http.StatusBadGateway, codeServiceUnavailable,
				"the sign-in provider could not be reached", nil)
			return
		}
		setSignInBindingCookie(w, r, started.Binding, signInBindingCookieMaxAge)
		writeJSON(w, http.StatusOK, redirectStartResponse{URL: started.URL})
	}
}

type redirectCallbackRequest struct {
	State  string `json:"state"`
	Code   string `json:"code"`
	Device struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		ClientID string `json:"clientId"`
	} `json:"device"`
}

func handleRedirectSignInCallback(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SignInRedirect == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"redirect sign-in is not available on this server", nil)
			return
		}
		var req redirectCallbackRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Device.ClientID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "device.clientId is required", nil)
			return
		}
		binding := ""
		if c, err := r.Cookie(signInBindingCookieNameFor(r)); err == nil {
			binding = c.Value
		}
		// The binding is spent whatever happens next: a state is good for one try.
		setSignInBindingCookie(w, r, "", 0)

		providerID, answer, err := deps.SignInRedirect.Complete(r.Context(), req.State, req.Code, binding)
		if err != nil {
			writeError(w, http.StatusUnauthorized, codeSignInRefused, signInRefusedMessage, nil)
			return
		}
		res, err := deps.Auth.SignInExternal(providerID, answer, auth.DeviceInput{
			Name:     req.Device.Name,
			Platform: req.Device.Platform,
			ClientID: req.Device.ClientID,
		})
		switch {
		case errors.Is(err, auth.ErrUsernameCollision):
			writeError(w, http.StatusConflict, codeSignInUsernameTaken, signInUsernameTakenMessage, nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "sign-in failed", nil)
			return
		}
		setMediaCookie(w, r, res.Token)
		writeJSON(w, http.StatusOK, loginResponse{
			Token:  res.Token,
			User:   toUserJSON(res.User),
			Device: toDeviceJSON(res.Device),
		})
	}
}
