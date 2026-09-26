// Package lyric is the Lyric provider Extension point's dispatcher: one export,
// `lyric_provider_lyrics`, in front of the contract's own
// [pluginapi.LyricProvider] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/lyric"
//
//	func main() {}
//
//	func init() { lyric.Serve(&myLyrics{}) }
//
// Serve is called from init() and not main(): a -buildmode=c-shared module is a
// WASI reactor and main.main never runs.
//
// # What this seam does NOT let a plugin decide
//
// Whether its answer is kept. The host judges every answer: a Synced answer
// timed for a recording more than a few seconds off the track's own length is
// kept only as Plain, and one naming another recording than the request's is
// dropped whole — so say the length your lines were timed for, and the
// recording, when your source knows them. Nothing found is an empty answer; an
// error is a failure the host does not remember as a miss.
//
// A Lyric provider's settings ride WITH the call, so the dispatcher publishes
// them and Host.Settings answers them for the call's duration. Settings.URL is
// the operator's source, which the call may reach beside the manifest's hosts.
package lyric

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Lyric provider's one call, as an alias for the contract's own
// interface.
type Provider = pluginapi.LyricProvider

// served is the provider this module answers with.
var served Provider

// Serve installs the provider this module answers every lyrics call with. Call
// it from init().
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run.
func Served() Provider { return served }
