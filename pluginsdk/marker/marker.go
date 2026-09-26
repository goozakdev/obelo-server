// Package marker is the Marker provider Extension point's dispatcher: one
// export, `marker_provider_markers`, in front of the contract's own
// [pluginapi.MarkerProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/marker"
//
//	func main() {}
//
//	func init() { marker.Serve(&myMarkers{}) }
//
// Serve is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # What this seam does NOT let a plugin decide
//
// Whether a candidate is used. The host drops one timed on a recording more than
// a few seconds longer or shorter than the File being played — so every
// candidate states the length of the recording it was measured on — and uses
// what survives only where the File's own chapters and the host's own detection
// say nothing. Nothing found is an empty answer; an error is a failure the host
// does not remember as a miss.
//
// A Marker provider's settings ride WITH the call, so the dispatcher publishes
// them and Host.Settings answers them for the call's duration. Settings.URL is
// the operator's source, which the call may reach beside the manifest's hosts.
package marker

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Marker provider's one call, as an alias for the contract's own
// interface.
type Provider = pluginapi.MarkerProvider

// served is the provider this module answers with.
var served Provider

// Serve installs the provider this module answers every markers call with. Call
// it from init().
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run.
func Served() Provider { return served }
