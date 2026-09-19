//go:build wasm

// Command opensubtitles is the Obelo OpenSubtitles Bundled plugin's WebAssembly
// module.
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
// serves no Subtitle provider".
//
// # Why a bare Sandbox and not a PacedHost
//
// The Built-in this replaces never paced itself, and nothing about a Subtitle
// provider asks it to: a search is at most three requests, one after the other,
// made because a viewer pressed "search online", and a download is two. No pass
// walks a library through it. OpenSubtitles' binding limit is a per-account
// DOWNLOAD quota, which pacing does nothing for — the plugin answers it as
// unavailable instead (see the package).
package main

import (
	"github.com/goozakdev/obelo-server/plugins/opensubtitles/opensubtitles"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/subtitle"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	subtitle.Serve(opensubtitles.New(pluginsdk.Sandbox()))
}
