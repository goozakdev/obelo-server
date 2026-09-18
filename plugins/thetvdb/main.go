//go:build wasm

// Command thetvdb is the Obelo TheTVDB Bundled plugin's WebAssembly module.
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
// TheTVDB paced itself before this port — 250 ms between requests, the login
// included — and it paces itself now. Pacing is the plugin's (ADR-0059
// decision 5), and [pluginsdk.PacedHost] spaces EVERY fetch this module makes,
// which is what keeps the login step under the same clock as the data calls it
// was under before. The operator's RateLimitMillis is re-read from Settings on
// each fetch; with none set, the interval is the 250 ms the Go provider used.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/thetvdb/thetvdb"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	metadata.Serve(thetvdb.New(pluginsdk.PacedHost(pluginsdk.Sandbox(), thetvdb.DefaultThrottle)))
}
