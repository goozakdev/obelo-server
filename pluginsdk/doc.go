// Package pluginsdk is the Go SDK for writing an Obelo plugin: the hand-rolled
// ABI of ADR-0058, written once, so a Go author writes a provider instead of a
// memory protocol (ADR-0059 decision 9).
//
// # The SDK is a convenience. The schema is the contract.
//
// Nothing here is required to write a plugin, and that is a property this
// repository protects rather than a disclaimer. The reference Discord plugin and
// the test guest under internal/plugins/plugintest are written against the raw
// ABI and import nothing of this module; an author in Rust, Zig or AssemblyScript
// has pluginapi/v1/pluginapi.schema.json and six host-function signatures, which
// is everything. If this SDK and the schema ever disagree, the schema is right.
//
// # What it gives a Go author
//
//   - [Host] — the interface provider code calls: one fetch, one log, three kv
//     calls and the Settings the host resolved for this call. Provider logic
//     depends on this and never on net/http, which is what lets the SAME provider
//     run natively against [github.com/goozakdev/obelo-server/pluginsdk/sdktest].Host
//     in a unit test and inside the sandbox in production.
//   - The wasm implementation of that interface: obelo_alloc, obelo_free,
//     last_error, the six //go:wasmimport declarations, the packed-i64 return
//     convention and the JSON marshalling, once instead of once per plugin.
//   - The dispatchers — metadata.Serve, sink.Serve, subtitle.Serve — which own
//     the //go:wasmexport functions, decode the request, call a value that
//     implements the contract's Go interface, and encode the answer.
//   - [Pacer] and [PacedHost], because ADR-0059 decision 5 makes every plugin
//     pace ITSELF, and seven copies of a mutex and a timestamp is exactly the
//     thing an SDK exists to prevent.
//
// # How a plugin uses it
//
// The exported functions live in the dispatcher packages, so a plugin's main.go
// only has to hand its provider over. It does that from init() and NOT from
// main(): a -buildmode=c-shared module is a WASI reactor, whose _initialize runs
// package initialization and never calls main.
//
//	//go:build wasm
//
//	package main
//
//	import (
//		"github.com/goozakdev/obelo-server/pluginsdk"
//		"github.com/goozakdev/obelo-server/pluginsdk/metadata"
//	)
//
//	func main() {}
//
//	func init() { metadata.Serve(newProvider(pluginsdk.Sandbox())) }
//
// and is built exactly as every other plugin is:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared -o plugin.wasm .
//
// # What it may import
//
// The contract, and the standard library. This module's go.mod has one require
// and it is pluginapi; a second one would be a dependency travelling into every
// plugin built against it. The sdktest sub-package is the one place net/http
// appears, because its whole job is to be the network a native test pretends to
// have.
package pluginsdk
