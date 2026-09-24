package api

import (
	"errors"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/signin"
)

// Sign-in providers (ADR-0063), the password flow. Two surfaces:
//
//	POST /auth/login                        the unchanged login form; a username
//	                                        collision after a provider accepted
//	                                        answers 409 SIGN_IN_USERNAME_TAKEN
//	GET  /settings/sign-in-providers        → { "providers": [ { "id", "name" } ] }
//	PUT  /settings/sign-in-providers        { "order": [ id, ... ] } → the same
//
// The list is the order a login asks the password-flow Sign-in providers in
// after the Local password; the first to accept wins.

// signInUsernameTakenMessage is what the person at the login form reads. It
// points at the two ways forward and names nobody.
const signInUsernameTakenMessage = "Your sign-in was accepted, but an account with that username already " +
	"exists on this server. If it is yours, sign in to it and attach this sign-in from your profile; " +
	"otherwise ask an Admin."

type signInProvidersResponse struct {
	Providers []signin.Provider `json:"providers"`
}

type signInOrderRequest struct {
	Order []string `json:"order"`
}

func handleSignInProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		src := deps.SignInProviders
		if src == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"sign-in providers are not available on this server", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req signInOrderRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			if err := src.SetOrder(req.Order); err != nil {
				if errors.Is(err, signin.ErrInvalidOrder) {
					writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
					return
				}
				writeError(w, http.StatusInternalServerError, codeInternal,
					"the sign-in provider order could not be saved", nil)
				return
			}
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			return
		}
		providers := src.Providers()
		if providers == nil {
			providers = []signin.Provider{}
		}
		writeJSON(w, http.StatusOK, signInProvidersResponse{Providers: providers})
	}
}
