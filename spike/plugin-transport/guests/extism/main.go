//go:build wasm

// Guest variant B: the Extism Go PDK.
//
// EVERYTHING IN THIS FILE IS WHAT A PLUGIN AUTHOR WRITES BY HAND under the Extism
// option, and answers.go and wire.go are byte-identical to the bare guest's. The
// line count of this file against guests/bare/main.go is the measurement.
//
// There is no ABI here to get wrong: the PDK's own exports carry the input and
// output buffers, an export returns 0 for success and 1 for failure, and
// pdk.SetError is the error channel the bare guest had to invent (last_error).
package main

import (
	"encoding/json"

	pdk "github.com/extism/go-pdk"
)

func main() {}

//go:wasmexport search
func search() int32 {
	var req SubtitleSearchRequest
	if err := json.Unmarshal(pdk.Input(), &req); err != nil {
		pdk.SetError(err)
		return 1
	}
	out, err := json.Marshal(searchAnswer(req))
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(out)
	return 0
}

//go:wasmexport download
func download() int32 {
	var req SubtitleDownloadRequest
	if err := json.Unmarshal(pdk.Input(), &req); err != nil {
		pdk.SetError(err)
		return 1
	}
	out, err := json.Marshal(downloadAnswer(req))
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(out)
	return 0
}

var spun uint64

// spin never returns; the deadline probe's target, same as the bare guest's.
//
//go:wasmexport spin
func spin() int32 {
	for {
		spun++
	}
}
