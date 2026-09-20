//go:build wasm

package pluginsdk

import (
	"encoding/json"
	"unsafe"
)

// The hand-rolled ABI of ADR-0058 decision 3, guest side, written ONCE.
//
//	EXPORTS this file provides
//	  obelo_alloc(size u32) -> ptr u32   the host asks the guest for a buffer
//	  obelo_free(ptr u32)                the host gives one back
//	  last_error() -> i64                (ptr<<32 | len) of why the last call
//	                                     answered 0
//
// The contract calls themselves are exported by the dispatcher packages
// (pluginsdk/metadata, pluginsdk/sink, pluginsdk/subtitle), because a
// //go:wasmexport is a compile-time fact and a module that exported eight
// metadata calls it cannot answer would be a module lying about what it is.
//
// THE GUEST OWNS EVERY BUFFER ON BOTH SIDES. The host never fabricates a guest
// pointer: it calls obelo_alloc, writes, and frees. [pinned] is what makes that
// checkable from in here — a pointer that did not come out of alloc simply is not
// in the map — and it is also what keeps the buffer reachable while the host holds
// nothing but an integer pointing into linear memory.

// pinned keeps every buffer the host holds a pointer to alive, and turns a
// host-supplied pointer back into a slice without pointer arithmetic.
var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func wasmAlloc(size uint32) uint32 { return alloc(size) }

//go:wasmexport obelo_free
func wasmFree(ptr uint32) { free(ptr) }

//go:wasmexport last_error
func wasmLastError() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

func alloc(size uint32) uint32 {
	if size == 0 {
		// A zero-length buffer still needs a distinct address: the host writes
		// nothing into it but will hand the pointer back to free.
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

func free(ptr uint32) { delete(pinned, ptr) }

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export may return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

// lastError is why the last call answered 0. The host reads it through
// last_error() and puts it in the plugin's last-error and the server log.
var lastError []byte

// Fail records why this call cannot answer and returns the 0 the ABI reads as
// "no response". It is for the ABI going wrong — a request that is not the shape
// the call takes — and NOT for a source having nothing to say: that is an
// Outcome, and a plugin that failed the call instead of answering no-match would
// have the host counting a transport failure against it.
func Fail(msg string) uint64 {
	lastError = []byte(msg)
	return 0
}

// Reply encodes a response into guest memory and answers with the packed pointer
// and length the host reads.
func Reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return Fail("encoding the response: " + err.Error())
	}
	lastError = nil
	return emit(out)
}

// TakeRequest decodes one call's request out of the buffer the host wrote it
// into. A pointer this guest did not allocate is refused, which is what makes
// "the guest owns every buffer on both sides" a property rather than a promise.
func TakeRequest(ptr, n uint32, out any) bool {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return false
	}
	return json.Unmarshal(buf[:n], out) == nil
}

// hostRoundTrip is the shape of every request-response host function: marshal
// into a buffer this guest allocated, call, read the answer out of a buffer the
// host obtained from the SAME allocator, free both.
func hostRoundTrip(call func(ptr, n uint32) uint64, req any, out any) bool {
	in, err := json.Marshal(req)
	if err != nil {
		return false
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := call(ptr, uint32(len(in)))
	free(ptr)
	return readAnswer(packed, out)
}

// readAnswer decodes a packed (ptr<<32 | len) answer and frees the buffer.
func readAnswer(packed uint64, out any) bool {
	if packed == 0 {
		return false
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return false
	}
	err := json.Unmarshal(buf[:rlen], out)
	free(rptr)
	return err == nil
}
