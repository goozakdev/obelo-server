// Package signin is the Sign-in provider Extension point's dispatcher for the
// REDIRECT flow (ADR-0063 decision 2): its two exports, in front of the
// contract's own [pluginapi.SignInRedirectProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/signin"
//
//	func main() {}
//
//	func init() { signin.ServeRedirect(&myProvider{}) }
//
// ServeRedirect is called from init() and not main(): a -buildmode=c-shared
// module is a WASI reactor and main.main never runs.
//
// # What this seam does NOT let a plugin decide
//
// State, the PKCE verifier and the nonce are the host's, handed in on every call
// and kept by nobody else. The callback is the host's route. And an ID token the
// exchange returns is verified by the HOST against the issuer the operator typed,
// which takes the subject and groups from the token rather than from the
// identity the plugin reported beside it.
//
// # The re-check
//
// A provider that also implements [pluginapi.SignInRefreshProvider] (declaring
// sign-in-refresh) or [pluginapi.SignInLookupProvider] (declaring
// sign-in-lookup) is asked again between sign-ins, so the host's Group mapping
// follows the directory. The dispatcher finds either by type assertion on the
// provider ServeRedirect installed. A refreshed ID token is verified by the host
// exactly as one from an exchange is.
//
// Like a Subtitle provider's, a Sign-in provider's settings ride WITH the call,
// so the dispatcher publishes them and Host.Settings answers them for the call's
// duration.
package signin

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// RedirectProvider is the redirect flow's two calls, as an alias for the
// contract's own interface.
type RedirectProvider = pluginapi.SignInRedirectProvider

// served is the redirect provider this module answers with.
var served RedirectProvider

// ServeRedirect installs the provider this module answers every redirect-flow
// call with. Call it from init().
func ServeRedirect(p RedirectProvider) { served = p }

// Served is the provider currently installed, and nil when ServeRedirect has not
// run.
func Served() RedirectProvider { return served }
