package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// The hand-rolled ABI of ADR-0058 decision 3, host side. It is small enough to
// state in full:
//
//	EXPORTS the guest provides
//	  obelo_alloc(size u32) -> ptr u32   the host asks the guest for a buffer
//	  obelo_free(ptr u32)                the host gives one back
//	  <call>(ptr u32, len u32) -> i64    one per contract call: (ptr<<32 | len)
//	                                     of the JSON response, or 0
//	  last_error() -> i64                (ptr<<32 | len) of why the last call
//	                                     answered 0
//
//	IMPORTS the host provides, in module "obelo"
//	  http_fetch(ptr u32, len u32) -> i64  a FetchRequest in, a FetchResponse out,
//	                                       in a buffer the GUEST allocated
//	  log(level u32, ptr u32, len u32)     a line, prefixed with the Plugin id
//
// THE GUEST OWNS EVERY BUFFER ON BOTH SIDES. The host never fabricates a guest
// pointer: it calls obelo_alloc, writes, and frees. That is what makes a guest
// able to refuse a pointer it did not hand out, and it is why http_fetch calls
// back into obelo_alloc from inside the host function rather than inventing an
// address.
//
// A response is read out of guest memory BEFORE it is freed. Memory().Read
// returns a VIEW of the guest's linear memory, not a copy, so unmarshaling after
// the free would read a buffer the guest has been told it may reuse.

// The three fixed exports, plus the module name the host functions live under.
const (
	exportAlloc     = "obelo_alloc"
	exportFree      = "obelo_free"
	exportLastError = "last_error"

	// exportDeliver is the Event sink Extension point's one contract call
	// (pluginapi.EventSink.Deliver). A SinkDeliverRequest in, a
	// SinkDeliverResponse out.
	exportDeliver = "deliver"

	// hostModule is the namespace the host functions are imported from. A module
	// importing anything but this and wasi_snapshot_preview1 is refused at load,
	// before it is instantiated.
	hostModule = "obelo"

	// wasiModule is instantiated because a stock-Go guest imports it for its clock
	// and its random source and will not load without it — and then every
	// capability inside it is simply never configured (ADR-0058 decision 4).
	wasiModule = "wasi_snapshot_preview1"
)

// Log levels for the log host function. A guest passes the number; anything else
// is read as info, because a bad level is not worth losing the line over.
const (
	LogDebug uint32 = 0
	LogInfo  uint32 = 1
	LogWarn  uint32 = 2
	LogError uint32 = 3
)

// errNoExport is what a call into a function the module does not export answers.
var errNoExport = errors.New("the module does not export that call")

// errGuestRefused is a guest that RAN TO COMPLETION and answered the ABI's `0`
// with a sentence in last_error(): the module was entered, it did its work, it
// decided the call could not be answered, and it came back to say so. That is a
// different fact from a trap, a deadline kill or a response that is not the
// contract's shape, all of which say the instance is unusable, and
// callGuestUnder's policy is where the difference is spent.
//
// It is a SENTINEL rather than a string match, and the message it wraps is
// byte-for-byte what it always was — "the guest refused the call: <detail>" —
// because that sentence is on the Plugins screen and in operators' logs.
var errGuestRefused = errors.New("the guest refused the call")

// compiled is one Plugin's runtime and the module compiled into it. Compiling is
// the expensive half — 43 ms for a TinyGo guest, 606 ms for a stock-Go one — so it
// happens once, at load, and every instance after a trap or a deadline kill is
// rebuilt from this artifact in well under two milliseconds (ADR-0058 decision 7).
type compiled struct {
	rt     wazero.Runtime
	module wazero.CompiledModule
}

// compilationCache is ONE in-memory compilation cache for this whole process,
// shared by every Plugin's runtime (wazero supports exactly that, and it is what
// the type is for).
//
// Compiling a stock-Go guest is the expensive half — measured at ~670 ms for the
// bundled TMDB module, against ~20 ms when the cache answers — and this server
// compiles the SAME module far more often than once. Every install, uninstall,
// enable, disable and settings save goes through the Manager's rebuild-and-swap,
// which re-reads the whole plugins directory and recompiles EVERY module in it
// (that is deliberate: "a rebuild produces what a reboot would"). With the seven
// Bundled plugins of ADR-0059 that is about five seconds of recompilation each
// time an Admin flicks a switch, for bytes that did not change.
//
// It is keyed by the module's own bytes, so two Plugins that happen to ship the
// same module share the compiled code and a Plugin whose module was REPLACED
// compiles afresh — which is what the boot-time re-assert needs.
//
// It holds compiled code for the lifetime of the process, which is bounded by the
// number of DISTINCT modules a server has: a handful. It is created lazily so a
// binary that never loads a Plugin never allocates one.
var compilationCache = sync.OnceValue(wazero.NewCompilationCache)

// newRuntime builds the sandbox ADR-0058 decision 4 describes and compiles wasm
// into it: WASI instantiated and nothing inside it granted, the host module with
// exactly the functions this slice defines, and a per-call deadline the runtime
// itself enforces.
//
// WithCloseOnContextDone(true) is the only way to stop a guest that spins. It
// costs roughly 78 µs per call and is bought without argument, because the call it
// guards exists to make an HTTP request that takes 100–500 ms.
func newRuntime(ctx context.Context, wasm []byte, host *hostFuncs) (*compiled, error) {
	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithCompilationCache(compilationCache())
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiating wasi: %w", err)
	}
	if err := host.instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	module, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compiling the module: %w", err)
	}
	if err := checkImports(module); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	if module.ExportedFunctions()[exportAlloc] == nil || module.ExportedFunctions()[exportFree] == nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("the module exports no %s/%s, so the host has no way to hand it a request",
			exportAlloc, exportFree)
	}
	return &compiled{rt: rt, module: module}, nil
}

// checkImports is the load-time half of the sandbox rule: a module asking for a
// namespace this host did not offer is refused BEFORE it is instantiated, so the
// question "what can this code call" is answered by reading the module rather than
// by watching it run.
func checkImports(module wazero.CompiledModule) error {
	for _, def := range module.ImportedFunctions() {
		mod, name, _ := def.Import()
		if mod != wasiModule && mod != hostModule {
			return fmt.Errorf("the module imports %s.%s; a Plugin may import only %s and %s",
				mod, name, wasiModule, hostModule)
		}
	}
	return nil
}

func (c *compiled) close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.rt.Close(ctx)
}

// instance is one live guest: one linear memory, one set of exports, NO
// concurrency. Two calls into it at once would share that memory, so the Plugin
// holds one instance and serializes every call through it (ADR-0058 decision 7).
type instance struct {
	mod       api.Module
	alloc     api.Function
	free      api.Function
	lastError api.Function
	calls     map[string]api.Function
}

// instantiate creates a fresh guest from the compiled artifact.
//
// WithStartFunctions("_initialize") is NOT optional. A -buildmode=c-shared guest
// exports _initialize rather than _start, wazero runs _start by default, and
// without this the FIRST call traps inside the guest's uninitialized runtime.
// WithName("") keeps instances anonymous so a rebuilt one does not collide with
// the module it replaces.
//
// Nothing else is configured, and that absence is the sandbox: no preopened
// directory, no WithFS/WithFSConfig, no environment, no stdio, and wazero has no
// host sockets to withhold.
func (c *compiled) instantiate(ctx context.Context) (*instance, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize")
	mod, err := c.rt.InstantiateModule(ctx, c.module, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiating the module: %w", err)
	}
	inst := &instance{
		mod:       mod,
		alloc:     mod.ExportedFunction(exportAlloc),
		free:      mod.ExportedFunction(exportFree),
		lastError: mod.ExportedFunction(exportLastError),
		calls:     map[string]api.Function{},
	}
	if inst.alloc == nil || inst.free == nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("the module exports no %s/%s", exportAlloc, exportFree)
	}
	return inst, nil
}

func (i *instance) close(ctx context.Context) {
	if i == nil {
		return
	}
	// A module already closed by a deadline says so; there is nothing to do about
	// it and nothing to report, because the close is what we wanted.
	_ = i.mod.Close(ctx)
}

func (i *instance) call(name string) api.Function {
	if fn, ok := i.calls[name]; ok {
		return fn
	}
	fn := i.mod.ExportedFunction(name)
	i.calls[name] = fn
	return fn
}

// invoke is the whole hand-rolled ABI in one function: ask the guest for a
// buffer, write the request JSON into it, call the export, read the packed
// pointer/length it answers with, copy the bytes out, give both buffers back.
//
// It returns the number of response bytes alongside the error so the caller can
// hold the recycle budget ADR-0058 decision 7 requires: a guest's linear memory
// only ever grows, so an instance answering large payloads has to be retired on a
// byte budget rather than kept forever.
func (i *instance) invoke(ctx context.Context, name string, req, out any) (int, error) {
	fn := i.call(name)
	if fn == nil {
		return 0, fmt.Errorf("%w: %s", errNoExport, name)
	}
	in, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("encoding the request: %w", err)
	}
	res, err := i.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return 0, fmt.Errorf("guest allocator: %w", err)
	}
	ptr := uint32(res[0])
	if !i.mod.Memory().Write(ptr, in) {
		return 0, errors.New("writing the request into guest memory")
	}

	packed, callErr := fn.Call(ctx, uint64(ptr), uint64(len(in)))
	// Free the request buffer whatever happened, but never let a failure to free
	// mask why the call itself failed.
	if _, ferr := i.free.Call(ctx, uint64(ptr)); ferr != nil && callErr == nil {
		callErr = fmt.Errorf("guest free: %w", ferr)
	}
	if callErr != nil {
		return 0, callErr
	}
	if len(packed) == 0 || packed[0] == 0 {
		return 0, fmt.Errorf("%w: %s", errGuestRefused, i.guestError(ctx))
	}

	rptr, rlen := uint32(packed[0]>>32), uint32(packed[0])
	buf, ok := i.mod.Memory().Read(rptr, rlen)
	if !ok {
		return 0, errors.New("reading the response out of guest memory")
	}
	// Read handed back a VIEW of guest memory. Unmarshal BEFORE freeing it.
	err = json.Unmarshal(buf, out)
	if _, ferr := i.free.Call(ctx, uint64(rptr)); ferr != nil && err == nil {
		err = fmt.Errorf("guest free: %w", ferr)
	}
	if err != nil {
		return int(rlen), fmt.Errorf("the guest's response is not the contract's shape: %w", err)
	}
	return int(rlen), nil
}

// guestError reads the detail behind a 0 answer. A guest that exports no error
// channel, or whose channel is empty, gets a sentence rather than an empty
// message: "the call failed" with nothing after it is the least useful line a log
// can carry.
func (i *instance) guestError(ctx context.Context) string {
	if i.lastError == nil {
		return "(the guest exports no " + exportLastError + ", so there is no detail)"
	}
	packed, err := i.lastError.Call(ctx)
	if err != nil || len(packed) == 0 || packed[0] == 0 {
		return "(no detail)"
	}
	buf, ok := i.mod.Memory().Read(uint32(packed[0]>>32), uint32(packed[0]))
	if !ok {
		return "(unreadable detail)"
	}
	return string(buf)
}
