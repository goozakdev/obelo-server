//go:build wasm

// Command omdb is the Obelo OMDb Bundled plugin's WebAssembly module.
//
// It is built by `make plugins` with the reference plugin's own command:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared
//
// # Why init() and not main()
//
// A -buildmode=c-shared wasip1 module is a WASI REACTOR. The host runs
// _initialize, which runs package initialization, and main.main is NEVER called.
// Serving the provider from main() would leave every call answering "this module
// serves no Metadata provider".
//
// # Why a PacedHost and not a bare Sandbox
//
// OMDb paced itself before this port — 250 ms between requests, so that
// backfilling a large library stays a polite trickle rather than a burst that
// gets the client throttled or banned — and it paces itself now. Pacing is the
// plugin's (ADR-0059 decision 5), and [pluginsdk.PacedHost] is where a bundled
// plugin puts it: it spaces every fetch and re-reads the operator's
// RateLimitMillis override from Settings on each one. That override reaching OMDb
// at all is the one thing decision 5 deliberately changed ("the operator's
// rate-limit setting reaches every provider through the fixed Settings shape");
// with no override set, the interval is the 250 ms the Go provider used.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/omdb/omdb"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	metadata.Serve(omdb.New(pluginsdk.PacedHost(pluginsdk.Sandbox(), omdb.DefaultThrottle)))
}
