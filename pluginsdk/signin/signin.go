// Package signin is the Sign-in provider Extension point's dispatcher, for both
// flows: the PASSWORD flow's one export, in front of the contract's own
// [pluginapi.SignInProvider] interface, and the REDIRECT flow's two (ADR-0063
// decision 2), in front of [pluginapi.SignInRedirectProvider].
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
// or signin.ServePassword for a directory that checks a username and password.
// Either is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # The password flow
//
// A password check answers accepted with an identity, or not. A wrong password
// is a rejection — Accepted false and no error — and the host never tells the
// person at the form which it was; an error is your directory failing, which the
// host treats exactly as a rejection too, and records for the Admin.
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
// provider ServeRedirect installed — and a lookup on the one ServePassword
// installed, when no redirect provider answers it. A refreshed ID token is verified by the host
// exactly as one from an exchange is.
//
// Like a Subtitle provider's, a Sign-in provider's settings ride WITH the call,
// so the dispatcher publishes them and Host.Settings answers them for the call's
// duration.
package signin

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// PasswordProvider is the password flow's one call, as an alias for the
// contract's own interface.
type PasswordProvider = pluginapi.SignInProvider

// servedPassword is the password provider this module answers with.
var servedPassword PasswordProvider

// ServePassword installs the provider this module answers every password check
// with. Call it from init().
func ServePassword(p PasswordProvider) { servedPassword = p }

// ServedPassword is the password provider currently installed, and nil when
// ServePassword has not run.
func ServedPassword() PasswordProvider { return servedPassword }

// lookupProvider is whichever installed provider answers lookup(subject): the
// redirect provider's, else the password provider's.
func lookupProvider() (pluginapi.SignInLookupProvider, bool) {
	if p, ok := served.(pluginapi.SignInLookupProvider); ok {
		return p, true
	}
	p, ok := servedPassword.(pluginapi.SignInLookupProvider)
	return p, ok
}

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
