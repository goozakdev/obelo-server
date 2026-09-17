//go:build wasm

// An Obelo Event sink Plugin, whole, in one file.
//
// This is the guest half of the plugin system: a WebAssembly module that is told
// when something finished on a server and posts a signed document about it. The
// suite builds it from this source at test time
// (`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`), so the loader is
// exercised by a real module rather than a mock — and so an author can read the
// smallest complete example there is.
//
// # The ABI, in full
//
// Exports this file provides:
//
//	obelo_alloc(size u32) -> ptr u32   the host asks for a buffer to write into
//	obelo_free(ptr u32)                the host gives one back
//	deliver(ptr u32, len u32) -> i64   one event in, one answer out, as
//	                                   (ptr<<32 | len) of the response JSON — or 0
//	last_error() -> i64                why the last call answered 0
//
// Imports the host provides, in module "obelo":
//
//	http_fetch(ptr u32, len u32) -> i64   the ONLY way out of the sandbox
//	log(level u32, ptr u32, len u32)      a line in the server log
//
// The guest owns every buffer on both sides. The host never invents a pointer: it
// calls obelo_alloc, writes, and frees — which is why `pinned` can refuse a
// pointer it did not hand out, and why the fetch response comes back as an entry
// this guest already has.
//
// # What it is NOT
//
// There is no filesystem, no environment, no socket and no stdout in here, and
// none is missing: a Plugin's whole job is to answer the call it was given. The
// four misbehaving modes at the bottom exist ONLY so one module can play every
// part the test suite needs; delete them and what is left is the example.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unsafe"
)

func main() {}

// --- the shapes, copied from the contract's JSON schema ----------------------
//
// An author in another language reads pluginapi/v1/pluginapi.schema.json and
// declares these in their own. Only the fields this Plugin uses are here: the
// contract leaves additionalProperties open on every type precisely so a guest
// may ignore what it does not need.

type settings struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type deliverRequest struct {
	// The event is kept as RAW JSON and signed as received, so the bytes this
	// Plugin signs are exactly the bytes it posts. Re-encoding it would sign one
	// document and send another.
	Event    json.RawMessage `json:"event"`
	Settings settings        `json:"settings"`
}

type deliverResponse struct {
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type fetchRequest struct {
	Method  string   `json:"method,omitempty"`
	URL     string   `json:"url"`
	Headers []header `json:"headers,omitempty"`
	Body    []byte   `json:"body,omitempty"`
}

type fetchResponse struct {
	Status  int      `json:"status,omitempty"`
	Headers []header `json:"headers,omitempty"`
	Body    []byte   `json:"body,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// --- what this Plugin actually does ------------------------------------------

//go:wasmexport deliver
func deliver(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var req deliverRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail("the request is not a SinkDeliverRequest: " + err.Error())
	}
	if answer, misbehaving := misbehave(req); misbehaving {
		return answer
	}
	return reply(post(req))
}

// post signs the event and sends it to the URL the Admin configured. The
// signature is HMAC-SHA256 over the exact bytes of the body, hex, under the
// secret the host handed over with this call — so a receiving script can reject
// anything this Plugin did not send.
func post(req deliverRequest) deliverResponse {
	body := []byte(req.Event)
	mac := hmac.New(sha256.New, []byte(req.Settings.Secret))
	mac.Write(body)

	resp := fetch(fetchRequest{
		Method: "POST",
		URL:    req.Settings.URL,
		Headers: []header{
			{Name: "Content-Type", Value: "application/json"},
			{Name: "X-Obelo-Signature", Value: "sha256=" + hex.EncodeToString(mac.Sum(nil))},
		},
		Body: body,
	})

	switch {
	case resp.Refused != "":
		// The host would not make this request and will refuse it again, so there
		// is nothing to retry and nothing to be clever about.
		logLine(levelError, "the host refused the request: "+resp.Refused)
		return deliverResponse{Error: "refused by the host: " + resp.Refused}
	case resp.Error != "":
		return deliverResponse{Error: "the request failed: " + resp.Error}
	case resp.Status < 200 || resp.Status > 299:
		return deliverResponse{Error: "the target answered " + itoa(resp.Status)}
	}
	logLine(levelInfo, "delivered one document")
	return deliverResponse{Delivered: true}
}

// --- the ABI glue an author writes once --------------------------------------

// pinned keeps every buffer the host holds a pointer to reachable, and is how a
// host-supplied pointer becomes a slice again without pointer arithmetic: a
// pointer that did not come out of alloc simply is not in here.
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
func lastErrorFn() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export may return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

func fail(msg string) uint64 {
	lastError = []byte(msg)
	return 0
}

func reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err.Error())
	}
	lastError = nil
	return emit(out)
}

//go:wasmimport obelo http_fetch
func hostFetch(ptr, n uint32) uint64

//go:wasmimport obelo log
func hostLog(level, ptr, n uint32)

const (
	levelInfo  uint32 = 1
	levelError uint32 = 3
)

// fetch asks the HOST to perform a request. The answer comes back in a buffer the
// host obtained from alloc, so it is already one of ours.
func fetch(req fetchRequest) fetchResponse {
	in, err := json.Marshal(req)
	if err != nil {
		return fetchResponse{Error: err.Error()}
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := hostFetch(ptr, uint32(len(in)))
	free(ptr)

	if packed == 0 {
		return fetchResponse{Error: "the host answered nothing"}
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return fetchResponse{Error: "the host answered with a buffer this guest does not hold"}
	}
	var resp fetchResponse
	uerr := json.Unmarshal(buf[:rlen], &resp)
	free(rptr)
	if uerr != nil {
		return fetchResponse{Error: uerr.Error()}
	}
	return resp
}

// logLine writes one line to the server log, prefixed by the host with this
// Plugin's id. It is why a guest needs no stdout.
func logLine(level uint32, msg string) {
	b := []byte(msg)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	hostLog(level, ptr, uint32(len(b)))
	free(ptr)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [8]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

// --- the four parts this one module plays for the suite ----------------------
//
// EVERYTHING BELOW THIS LINE IS TEST SCAFFOLDING, not an example. A real Plugin
// has none of it: the suite needs a guest that panics, one that never returns and
// two that reach for hosts they were not given, and building four modules to get
// them would be four compiles and four things to keep in step. The mode is read
// out of the target URL the test configures, which is the one thing a test can
// set that reaches this far in.

var spun uint64

func misbehave(req deliverRequest) (uint64, bool) {
	switch mode(req.Settings.URL) {
	case "panic":
		panic("this plugin fails on every call, on purpose")
	case "hang":
		// Never returns. The host's deadline has to stop it, because nothing in
		// here is going to.
		for {
			spun++
		}
	case "forbidden":
		return reply(reportRefusal(fetch(fetchRequest{URL: "https://not-allowed.example.test/steal"}))), true
	case "metadata":
		// A host the manifest DOES allowlist, whose address is one no plugin may
		// reach. The allowlist is a claim; the address rule is the answer.
		return reply(reportRefusal(fetch(fetchRequest{URL: "http://169.254.169.254/latest/meta-data/"}))), true
	}
	return 0, false
}

// reportRefusal hands whatever the host said back through the delivery answer, so
// a test can see the refusal a guest actually received.
func reportRefusal(resp fetchResponse) deliverResponse {
	if resp.Refused != "" {
		return deliverResponse{Error: "refused: " + resp.Refused}
	}
	if resp.Error != "" {
		return deliverResponse{Error: "error: " + resp.Error}
	}
	return deliverResponse{Error: "the host allowed a fetch it should have refused, answering " + itoa(resp.Status)}
}

func mode(target string) string {
	const marker = "obelo-mode="
	i := strings.Index(target, marker)
	if i < 0 {
		return ""
	}
	m := target[i+len(marker):]
	if j := strings.IndexAny(m, "&#"); j >= 0 {
		m = m[:j]
	}
	return m
}
