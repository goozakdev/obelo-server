// The OpenSubtitles Bundled plugin (ADR-0059, .scratch/bundled-plugins issue 09).
//
// It is its own module, and the `require` list is the whole argument for that:
// this plugin depends on the Obelo SDK and on nothing else. The server module is
// not reachable from here — not `internal/enrich`, not `internal/store`, not one
// line of the thing that loads it — so the only way a change to the server can
// reach this code is through the contract, which is the property a Bundled plugin
// exists to prove.
//
// The two `replace` lines are what make a build work from a working tree: neither
// local module is published, and a plugin's own build — `GOOS=wasip1 GOARCH=wasm
// go build -buildmode=c-shared` — may run with GOWORK=off, in which case the
// workspace is not there to resolve them. `make plugins` builds with the workspace
// ON (this module IS in go.work), and both spellings must keep working.
module github.com/goozakdev/obelo-server/plugins/opensubtitles

go 1.26

require (
	github.com/goozakdev/obelo-server/pluginapi v0.0.0
	github.com/goozakdev/obelo-server/pluginsdk v0.0.0
)

replace github.com/goozakdev/obelo-server/pluginapi => ../../pluginapi

replace github.com/goozakdev/obelo-server/pluginsdk => ../../pluginsdk
