//go:build wasm

// Command anidb is the Obelo AniDB Bundled plugin's WebAssembly module.
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
// AniDB bans bursty clients, so the Go provider spaced its requests two seconds
// apart and this one does the same. Pacing is the plugin's (ADR-0059
// decision 5); [pluginsdk.PacedHost] spaces every fetch and re-reads the
// operator's RateLimitMillis from Settings on each one. With no override set the
// interval is the 2 s the Go provider used.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/anidb/anidb"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	metadata.Serve(anidb.New(pluginsdk.PacedHost(pluginsdk.Sandbox(), anidb.DefaultThrottle)))
}
