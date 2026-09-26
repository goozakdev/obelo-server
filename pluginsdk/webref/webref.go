// Package webref is the Web reference provider Extension point's dispatcher:
// one export, `web_reference_links`, in front of the contract's own
// [pluginapi.WebReferenceProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/webref"
//
//	func main() {}
//
//	func init() { webref.Serve(&myReferences{}) }
//
// Serve is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # What this seam does NOT let a plugin do
//
// Reach the network. The call is a pure computation over the ids in the
// request, and the host refuses every fetch made while it runs — and counts it
// against the Plugin. Nor does the plugin decide what is shown: the host keeps a
// reference only when it is https and names an id the host sent.
//
// Like a Subtitle provider's, a Web reference provider's settings ride WITH the
// call, so the dispatcher publishes them and Host.Settings answers them for the
// call's duration.
package webref

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Web reference provider's one call, as an alias for the
// contract's own interface.
type Provider = pluginapi.WebReferenceProvider

// served is the provider this module answers with.
var served Provider

// Serve installs the provider this module answers every Web reference call
// with. Call it from init().
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run.
func Served() Provider { return served }
