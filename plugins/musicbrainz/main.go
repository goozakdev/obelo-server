//go:build wasm

// Command musicbrainz is the Obelo MusicBrainz Bundled plugin's WebAssembly
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
// serves no Metadata provider".
//
// # Why a PacedHost and not a bare Sandbox
//
// MusicBrainz publishes a rate policy — roughly one request a second per client,
// and it answers 503 once you exceed it — and ADR-0049 is the incident report
// about what happens when a server ignores it. ADR-0059 decision 5 moved that
// pacing OUT of the host and into the plugin that knows the policy, so this is
// where the server's one-request-a-second promise to MusicBrainz now lives.
//
// [pluginsdk.PacedHost] wraps Fetch and nothing else, re-reading
// Settings.RateLimitMillis on every call: absent keeps the second below, 0 is the
// operator saying their mirror has no policy, and n paces at n milliseconds
// (ADR-0049 — the two are different instructions and the pointer is what keeps
// them apart).
//
// ONE PACER IS ENOUGH, which is the part that used to be hard. The Go provider
// needed a process-wide limiter keyed by HOST (internal/enrich/hostthrottle.go),
// because Manager.resolveLibrary builds a provider per Library and three
// Libraries kept three independent 1-req/sec throttles pointed at one host. A
// guest has no such problem: a plugin instance is one module with one linear
// memory, the host serializes every call into it (ADR-0058 decision 7), and its
// only way out is the host's fetch — so every Library's enrichment queues behind
// this one Pacer by construction.
//
// The pacer covers the COVER ART ARCHIVE host too, which is deliberate: it is one
// interval for everything this plugin fetches, exactly as the process-wide
// limiter was one interval per host and the album cover was fetched through the
// same code path. Splitting it per host would be a change, not a port.
package main

import (
	"github.com/goozakdev/obelo-server/plugins/musicbrainz/musicbrainz"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/metadata"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	host := pluginsdk.PacedHost(pluginsdk.Sandbox(), musicbrainz.DefaultInterval)
	metadata.Serve(musicbrainz.New(host))
}
