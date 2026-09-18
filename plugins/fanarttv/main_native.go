//go:build !wasm

// The non-WebAssembly half of this command, and the whole of it is a refusal.
//
// It exists because `go build ./...`, `go vet ./...` and `gofmt -l .` walk this
// module on the developer's own machine, and a package whose only file is behind
// //go:build wasm is a package with NO Go files there — which those tools report
// as an error rather than skipping. The SDK's own test guest avoids this by
// living under testdata/, which the `...` pattern does not enter; a plugin that
// ships cannot.
//
// So the native build produces a binary that says what to run instead. It is
// never installed, never shipped and never called by anything.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "this is an Obelo plugin, not a program: build it with")
	fmt.Fprintln(os.Stderr, "  GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared -o plugin.wasm .")
	fmt.Fprintln(os.Stderr, "or, with its manifest, through `make plugins` from the repository root.")
	os.Exit(1)
}
