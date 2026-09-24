//go:build wasm

// Command oidc is the Obelo OpenID Connect Bundled plugin's WebAssembly module.
//
// It is built by `make plugins` with the reference plugin's own command:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared
//
// A -buildmode=c-shared wasip1 module is a WASI REACTOR: the host runs
// _initialize and main.main is never called, which is why the provider is served
// from init().
package main

import (
	"github.com/goozakdev/obelo-server/plugins/oidc/oidc"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/signin"
)

// main is never called. It exists because a Go program needs one.
func main() {}

func init() {
	signin.ServeRedirect(oidc.New(pluginsdk.Sandbox()))
}
