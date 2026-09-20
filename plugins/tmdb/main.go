//go:build wasm

// Command tmdb is the Obelo TMDB Bundled plugin's WebAssembly module.
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
// # Why a bare Sandbox and not a PacedHost
//
// TMDB paces nothing, and did not before this port (ADR-0049 gave the one-request-
// a-second rule to MusicBrainz and to nobody else). Wrapping the host in a
// [pluginsdk.PacedHost] here would be a behaviour change smuggled in with a
// conversion whose whole claim is that there is none. MusicBrainz's plugin is the
// one that wants a pacer.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/tmdb/tmdb"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() { metadata.Serve(tmdb.New(pluginsdk.Sandbox())) }
