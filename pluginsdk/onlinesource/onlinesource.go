// Package onlinesource is the Online source provider Extension point's
// dispatcher: four exports, `online_source_rows`, `online_source_row`,
// `online_source_search` and `online_source_resolve`, in front of the contract's
// own [pluginapi.OnlineSourceProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/onlinesource"
//
//	func main() {}
//
//	func init() { onlinesource.Serve(&mySource{}) }
//
// Serve is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # What this seam does NOT let a plugin decide
//
// What is shown and how it plays. The Plugin RESOLVES and the host PLAYS: the
// host caps and drops what rows(), row() and search() answer, judges every URL a
// variant names (https, the manifest's hosts, a public address), chooses between
// relaying a muxed variant and handing a variant to ffmpeg, and keeps every
// upstream URL from the client. An error is a source that is not responding; an
// empty resolve() is a source that no longer has the item.
//
// An Online source provider's settings ride WITH the call, so the dispatcher
// publishes them and Host.Settings answers them for the call's duration. They are
// server-wide: there is no per-User variant. Settings.URL is the operator's
// source, which the call may reach beside the manifest's hosts.
package onlinesource

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Online source provider's four calls, as an alias for the
// contract's own interface.
type Provider = pluginapi.OnlineSourceProvider

// served is the provider this module answers with.
var served Provider

// Serve installs the provider this module answers every call with. Call it from
// init().
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run.
func Served() Provider { return served }
