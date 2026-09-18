// The Go SDK for Obelo plugin authors (ADR-0059 decision 9).
//
// It requires the contract and NOTHING ELSE, which is the whole point: a plugin
// built against this module carries the standard library, the contract's wire
// types and this glue into its WebAssembly module, and not one line of the
// server.
//
// The `replace` is what makes that true from a working tree. Neither module is
// published, so a build with GOWORK=off — which is what a plugin's own
// `GOOS=wasip1 GOARCH=wasm` build runs under when it is invoked from somewhere
// that is not the workspace — resolves pluginapi from the directory beside this
// one rather than from a proxy that has never heard of it.
module github.com/goozakdev/obelo-server/pluginsdk

go 1.26

require github.com/goozakdev/obelo-server/pluginapi v0.0.0

replace github.com/goozakdev/obelo-server/pluginapi => ../pluginapi
