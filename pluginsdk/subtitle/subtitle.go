// Package subtitle is the Subtitle provider Extension point's dispatcher: the
// two exports of ADR-0021 behind ADR-0057, in front of the contract's own
// [pluginapi.SubtitleProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/subtitle"
//
//	func main() {}
//
//	func init() { subtitle.Serve(&myProvider{}) }
//
// Serve is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # Two things this seam does NOT let a plugin decide
//
// It never reads a file. The release-exact content hash arrives IN the request,
// computed by the host, because a guest has no filesystem and needs none.
//
// It never decides how big a download may be. The host states MaxBytes on the way
// in and checks the answer on the way out; answering with more is a violation the
// host counts, so a provider that cannot fit under the cap should answer
// OutcomeUnavailable and say why.
//
// Like a sink, a Subtitle provider's settings ride WITH the call, so the
// dispatcher publishes them and Host.Settings answers them for the call's
// duration.
package subtitle

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Subtitle provider's two mandatory calls, as an alias for the
// contract's own interface.
type Provider = pluginapi.SubtitleProvider

// served is the provider this module answers with.
var served Provider

// Serve installs the provider this module answers every subtitle call with. Call
// it from init().
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run.
func Served() Provider { return served }
