//go:build wasm

// Guest variant A: bare wazero, hand-rolled JSON-over-guest-memory ABI.
//
// EVERYTHING IN THIS FILE IS WHAT A PLUGIN AUTHOR WRITES BY HAND under the bare
// option. answers.go and wire.go are byte-identical in the extism guest, so the
// line count of THIS file against the extism guest's main.go is the "how much
// plumbing does the author owe us" measurement in ADR-0058.
//
// The ABI:
//   - obelo_alloc(size) -> ptr  host asks the guest for a buffer it may write into
//   - obelo_free(ptr)           host gives it back
//   - search(ptr, len) -> u64   (ptr<<32 | len) of a JSON response, 0 on failure
//   - download(ptr, len) -> u64 same
//   - last_error() -> u64       (ptr<<32 | len) of why the last call returned 0
//   - spin()                    never returns; the deadline probe's target
//
// The guest owns every buffer on both sides of every call. The host never
// fabricates a guest pointer, so the only unsafe conversion here is taking the
// address of a slice we just allocated.
package main

import (
	"encoding/json"
	"errors"
	"unsafe"
)

func main() {}

// errBadPointer is the guest refusing a pointer it did not hand out. Under Extism
// this failure mode does not exist, because the host never holds a pointer.
var errBadPointer = errors.New("bare-abi: pointer was not allocated by this guest")

// pinned keeps every buffer the host holds a pointer to reachable, and is how the
// guest turns a host-supplied pointer back into a slice without unsafe pointer
// arithmetic: if the pointer did not come out of alloc, there is nothing to read.
var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport obelo_free
func free(ptr uint32) {
	delete(pinned, ptr)
}

var lastError []byte

//go:wasmexport last_error
func lastError_() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export is allowed to return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

func fail(err error) uint64 {
	lastError = []byte(err.Error())
	return 0
}

func reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err)
	}
	lastError = nil
	return emit(out)
}

//go:wasmexport search
func search(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail(errBadPointer)
	}
	var req SubtitleSearchRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail(err)
	}
	return reply(searchAnswer(req))
}

//go:wasmexport download
func download(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail(errBadPointer)
	}
	var req SubtitleDownloadRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail(err)
	}
	return reply(downloadAnswer(req))
}

var spun uint64

// spin never returns. It is the target of the deadline probe: a guest that
// ignores its budget has to be stopped by the host, not asked to stop.
//
//go:wasmexport spin
func spin() uint64 {
	for {
		spun++
	}
}
