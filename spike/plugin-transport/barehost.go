// Package transport is the spike's HOST side: the two ways this server could call
// a Subtitle provider that lives in a WebAssembly module, written so the same two
// contract calls go through both and can be timed against each other.
//
// Nothing here ships. See README.md and docs/adr/0058-*.md.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"obelo-spike/plugin-transport/wire"
)

// BareRuntime is one wazero runtime holding one compiled module. Compiling is the
// expensive half (see the ADR's table), so it happens once per installed plugin
// and every call reuses the artifact.
type BareRuntime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
}

// SandboxConfig is the ONLY module configuration this spike ever uses, spelled out
// so the ADR can quote it: WASI is instantiated because a stock-Go guest imports
// it for its clock and its random source, and then nothing at all is granted —
// no preopened directory, no environment, no stdio, no sockets. wazero has no
// socket support to withhold; the filesystem is withheld by never calling
// WithFS/WithFSConfig.
func SandboxConfig() wazero.ModuleConfig {
	return wazero.NewModuleConfig().WithName("")
}

// runtimeConfig is wazero's default, which selects the optimizing COMPILER on
// amd64 and arm64 and falls back to the interpreter elsewhere. There is no
// exported way to ask which one was chosen, so OBELO_SPIKE_INTERPRETER=1 forces
// the interpreter and the difference in the measurements is the answer.
func runtimeConfig() wazero.RuntimeConfig {
	if os.Getenv("OBELO_SPIKE_INTERPRETER") == "1" {
		return wazero.NewRuntimeConfigInterpreter()
	}
	return wazero.NewRuntimeConfig()
}

// bareModuleConfig is SandboxConfig plus the one thing a -buildmode=c-shared
// guest needs and wazero does not do by default: run `_initialize` rather than
// `_start`. Without it the guest's runtime is never set up and the FIRST export
// call traps in runtime.notInitialized (stock Go) or wasmExportCheckRun (TinyGo).
// Extism does this itself, which is one of the things it is for.
func bareModuleConfig() wazero.ModuleConfig {
	return SandboxConfig().WithStartFunctions("_initialize")
}

// NewBareRuntime compiles wasm. closeOnContextDone is the per-call deadline
// mechanism: with it on, wazero inserts a check the runtime uses to unwind a guest
// whose context is done, which is the only way to stop a guest that spins.
func NewBareRuntime(ctx context.Context, wasm []byte, closeOnContextDone bool) (*BareRuntime, error) {
	cfg := runtimeConfig().WithCloseOnContextDone(closeOnContextDone)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiating wasi: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compiling guest: %w", err)
	}
	return &BareRuntime{rt: rt, compiled: compiled}, nil
}

// Close releases the runtime and every instance it still holds.
func (r *BareRuntime) Close(ctx context.Context) error { return r.rt.Close(ctx) }

// Exports reports the names the compiled module exports, which is how the loader
// would check a module is a Subtitle provider before believing its manifest.
func (r *BareRuntime) Exports() []string {
	var names []string
	for name := range r.compiled.ExportedFunctions() {
		names = append(names, name)
	}
	return names
}

// BareGuest is one instantiated module: one linear memory, one set of exports, no
// concurrency. Two calls into the same instance at the same time would share that
// memory, so a pool hands one instance to one call at a time.
type BareGuest struct {
	mod      api.Module
	alloc    api.Function
	free     api.Function
	search   api.Function
	download api.Function
	lastErr  api.Function
	spin     api.Function
}

// Instantiate creates a fresh guest from the compiled module. This is the cost the
// instance-per-call decision is about.
func (r *BareRuntime) Instantiate(ctx context.Context) (*BareGuest, error) {
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, bareModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiating guest: %w", err)
	}
	g := &BareGuest{
		mod:      mod,
		alloc:    mod.ExportedFunction("obelo_alloc"),
		free:     mod.ExportedFunction("obelo_free"),
		search:   mod.ExportedFunction("search"),
		download: mod.ExportedFunction("download"),
		lastErr:  mod.ExportedFunction("last_error"),
		spin:     mod.ExportedFunction("spin"),
	}
	if g.alloc == nil || g.free == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("guest does not export the obelo allocator")
	}
	return g, nil
}

// Close destroys the instance and its memory.
func (g *BareGuest) Close(ctx context.Context) error { return g.mod.Close(ctx) }

// call is the whole hand-rolled ABI on the host side: ask the guest for a buffer,
// write the request JSON into it, call the export, read the packed pointer/length
// it answers with, copy the bytes out, give both buffers back.
func (g *BareGuest) call(ctx context.Context, fn api.Function, req, out any) error {
	if fn == nil {
		return errors.New("guest does not export that call")
	}
	in, err := json.Marshal(req)
	if err != nil {
		return err
	}
	res, err := g.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return fmt.Errorf("guest alloc: %w", err)
	}
	ptr := uint32(res[0])
	if !g.mod.Memory().Write(ptr, in) {
		return errors.New("writing the request into guest memory")
	}
	packed, err := fn.Call(ctx, uint64(ptr), uint64(len(in)))
	if _, ferr := g.free.Call(ctx, uint64(ptr)); ferr != nil && err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	if packed[0] == 0 {
		return fmt.Errorf("guest refused the call: %s", g.guestError(ctx))
	}
	rptr, rlen := uint32(packed[0]>>32), uint32(packed[0])
	buf, ok := g.mod.Memory().Read(rptr, rlen)
	if !ok {
		return errors.New("reading the response out of guest memory")
	}
	// Read returns a view of the guest's memory; unmarshal before freeing it.
	err = json.Unmarshal(buf, out)
	if _, ferr := g.free.Call(ctx, uint64(rptr)); ferr != nil && err == nil {
		err = ferr
	}
	return err
}

func (g *BareGuest) guestError(ctx context.Context) string {
	if g.lastErr == nil {
		return "(the guest exports no error channel)"
	}
	packed, err := g.lastErr.Call(ctx)
	if err != nil || packed[0] == 0 {
		return "(no detail)"
	}
	buf, ok := g.mod.Memory().Read(uint32(packed[0]>>32), uint32(packed[0]))
	if !ok {
		return "(unreadable detail)"
	}
	return string(buf)
}

// SearchSubtitles is the contract call, through the hand-rolled ABI.
func (g *BareGuest) SearchSubtitles(ctx context.Context, req wire.SubtitleSearchRequest) (wire.SubtitleSearchResponse, error) {
	var resp wire.SubtitleSearchResponse
	return resp, g.call(ctx, g.search, req, &resp)
}

// DownloadSubtitle is the contract call, through the hand-rolled ABI.
func (g *BareGuest) DownloadSubtitle(ctx context.Context, req wire.SubtitleDownloadRequest) (wire.SubtitleDownloadResponse, error) {
	var resp wire.SubtitleDownloadResponse
	return resp, g.call(ctx, g.download, req, &resp)
}

// Spin calls the guest export that never returns. It comes back only when the
// context's deadline closes the module out from under it.
func (g *BareGuest) Spin(ctx context.Context) error {
	if g.spin == nil {
		return errors.New("guest does not export spin")
	}
	_, err := g.spin.Call(ctx)
	return err
}
