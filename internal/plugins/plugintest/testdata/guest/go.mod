// Its own module, with no dependencies at all, for two reasons.
//
// The root module must never try to build a wasm-only package, and a plugin
// author's project is not inside this repository either — so the guest is built
// exactly the way theirs is: `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`
// against the standard library and nothing else. It deliberately does NOT import
// pluginapi/v1: an author in another language has only the JSON schema, and a
// guest that re-declares the four shapes it needs is the honest demonstration
// that the schema is enough.
module obelo-test-guest

go 1.26
