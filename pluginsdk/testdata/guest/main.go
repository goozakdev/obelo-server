//go:build wasm

// An Obelo Metadata provider built with the Go SDK, whole, in one file.
//
// Compare it with internal/plugins/plugintest/testdata/guest/main.go, which is
// the SAME Extension point written against the raw ABI: 1,100 lines of shapes
// copied out of the schema, a pinned-buffer map, eight exports, packed i64s and
// four host-function round trips. This file is what is left when the SDK owns all
// of that — which is the entire argument for the SDK, and the reason BOTH guests
// exist in this repository. The SDK-free one is the proof no SDK is required; this
// one is the proof the SDK works.
//
// It is built by the ordinary command, from source, at test time:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared -o plugin.wasm .
//
// # Why init() and not main()
//
// A -buildmode=c-shared module is a WASI REACTOR. The host runs _initialize,
// which runs package initialization; main.main is never called at all. Serving the
// provider from main() would leave every call answering "this module serves no
// Metadata provider" — which is what the SDK says, in the plugin's last-error,
// rather than failing silently.
package main

import (
	"time"

	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/internal/testprovider"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// sdk-sample:begin serve

// main is never called. It exists because a Go program needs one.
func main() {}

// init hands the provider over. PacedHost is the whole of what ADR-0059 decision
// 5 asks of a plugin: pace yourself, with your own default, and honour the
// operator's RateLimitMillis when they set one. The interval here is short
// because this is a test guest; a real source uses its published policy (one
// second, for MusicBrainz).
func init() {
	host := pluginsdk.PacedHost(pluginsdk.Sandbox(), 5*time.Millisecond)
	metadata.Serve(testprovider.New(host))
}

// sdk-sample:end serve
