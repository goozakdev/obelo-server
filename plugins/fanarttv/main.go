//go:build wasm

// Command fanarttv is the Obelo fanart.tv Bundled plugin's WebAssembly module.
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
// # Why a PacedHost
//
// fanart.tv DID pace, and paced itself, before this port: the Go provider spaced
// successive requests 250 ms apart so that backfilling a large library stayed a
// polite trickle rather than a burst that gets the server throttled or banned.
// Pacing is the plugin's now (ADR-0059 decision 5), so the interval moves here —
// one [pluginsdk.PacedHost] wrapping the sandbox, which re-reads the operator's
// override from Settings.RateLimitMillis on every fetch exactly as the host-side
// throttle used to.
//
// ONE PACER, BOTH CHAINS. fanart.tv serves `kinds: [video, music]`, so the host's
// factory is called once per chain and each call returns a view over this ONE
// module instance — one linear memory, one cache pair, and this one pacer. That
// is the same single process-wide throttle the Go provider had, arrived at by a
// shorter road: it used to be one shared *FanartTVProvider throttle behind two Go
// instances, and it is now simply the only instance there is.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/fanarttv/fanarttv"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	metadata.Serve(fanarttv.New(pluginsdk.PacedHost(pluginsdk.Sandbox(), fanarttv.DefaultInterval)))
}
