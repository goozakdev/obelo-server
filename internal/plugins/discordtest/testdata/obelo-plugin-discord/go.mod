// The Obelo Discord Event sink: the reference Installed plugin.
//
// Its own module, with NO dependencies at all — not even on the server it plugs
// into. A plugin author has the JSON schema (pluginapi/v1/pluginapi.schema.json)
// and six host functions; importing the server module would hide that, and would
// make an author in another language look at a Go example that cheats.
//
// Built the way every plugin is built:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
module github.com/goozakdev/obelo-plugin-discord

go 1.26
