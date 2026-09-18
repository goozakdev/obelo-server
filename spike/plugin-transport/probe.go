package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ProbeResult is one hostile thing a guest tried and what it was told.
type ProbeResult struct {
	Export  string
	Says    string
	Failed  bool // the call returned a wasm trap rather than a message
	TrapErr string
}

// SandboxProbes runs guests/probe under the SAME configuration a real plugin would
// get — WASI instantiated, SandboxConfig, nothing granted — and collects what the
// guest observed when it tried to open a socket, read a file and spawn a process.
//
// The guest is not modified, restricted or told to behave. It is compiled by the
// ordinary command and simply finds that the syscalls are not there.
func SandboxProbes(ctx context.Context, wasm []byte) ([]ProbeResult, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig())
	defer rt.Close(ctx)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, err
	}
	mod, err := rt.InstantiateWithConfig(ctx, wasm, bareModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiating the probe guest: %w", err)
	}
	alloc := mod.ExportedFunction("obelo_alloc")
	free := mod.ExportedFunction("obelo_free")
	if alloc == nil || free == nil {
		return nil, errors.New("probe guest does not export the obelo allocator")
	}

	var out []ProbeResult
	for _, name := range []string{"probe_net", "probe_listen", "probe_selfdial", "probe_file", "probe_dir", "probe_exec"} {
		res := ProbeResult{Export: name}
		fn := mod.ExportedFunction(name)
		if fn == nil {
			res.Failed, res.TrapErr = true, "not exported"
			out = append(out, res)
			continue
		}
		packed, err := fn.Call(ctx)
		if err != nil {
			res.Failed, res.TrapErr = true, err.Error()
			out = append(out, res)
			continue
		}
		if packed[0] != 0 {
			ptr, n := uint32(packed[0]>>32), uint32(packed[0])
			if buf, ok := mod.Memory().Read(ptr, n); ok {
				res.Says = string(buf)
			}
			_, _ = free.Call(ctx, uint64(ptr))
		}
		out = append(out, res)
	}
	return out, nil
}

// SandboxWithoutWASI records what happens when the host grants NOTHING at all, not
// even the WASI module: a stock-Go guest imports wasi_snapshot_preview1 for its
// clock and its random source, so it does not instantiate. This is the reason the
// loader instantiates WASI and withholds every capability inside it, rather than
// withholding WASI itself.
func SandboxWithoutWASI(ctx context.Context, wasm []byte) error {
	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig())
	defer rt.Close(ctx)
	_, err := rt.InstantiateWithConfig(ctx, wasm, bareModuleConfig())
	return err
}

// HostImports reports every module a guest imports from, which is the fact a
// loader would check before instantiating: a module that imports anything but
// wasi_snapshot_preview1 and the host functions this server declares is refused.
func HostImports(ctx context.Context, wasm []byte) ([]string, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig())
	defer rt.Close(ctx)
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var mods []string
	for _, def := range compiled.ImportedFunctions() {
		module, _, _ := def.Import()
		if !seen[module] {
			seen[module] = true
			mods = append(mods, module)
		}
	}
	return mods, nil
}
