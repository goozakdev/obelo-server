// Package sink is the Event sink Extension point's dispatcher: one export,
// `deliver`, in front of the contract's own [pluginapi.EventSink] interface.
//
//	//go:build wasm
//
//	package main
//
//	import "github.com/goozakdev/obelo-server/pluginsdk/sink"
//
//	func main() {}
//
//	func init() { sink.Serve(&mySink{}) }
//
// Serve is called from init() and not main() for the reason the metadata package
// gives: a -buildmode=c-shared module is a WASI reactor and main.main never runs.
//
// # Where a sink's settings come from
//
// They ride WITH the call — that is the whole reason a sink needs no settings_get
// (ADR-0058 decision 5: secrets at call time only) — so the dispatcher publishes
// them for the duration of the delivery and a sink built on
// [github.com/goozakdev/obelo-server/pluginsdk.Sandbox] reads them through
// Host.Settings like any provider does. One spelling of "settings", whichever
// seam you fill.
//
// The reference Discord plugin is an Event sink written WITHOUT this package, and
// it stays that way: it is the proof that no SDK is required.
package sink

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Sink is the Event sink's one call. It is an alias for the contract's own
// interface, so a plugin and the Webhook Built-in implement the same Go type.
type Sink = pluginapi.EventSink

// served is the sink this module answers with. No lock: a guest instance is
// single-threaded and the host serializes every call into it.
var served Sink

// Serve installs the sink this module delivers every event to. Call it from
// init().
func Serve(s Sink) { served = s }

// Served is the sink currently installed, and nil when Serve has not run.
func Served() Sink { return served }
