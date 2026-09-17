package transport

import (
	"context"
	"encoding/json"
	"fmt"

	extism "github.com/extism/go-sdk"

	"obelo-spike/plugin-transport/wire"
)

// ExtismRuntime is the same thing BareRuntime is — a compiled artifact reused
// across calls — expressed in the Extism SDK's vocabulary. Extism runs ON wazero,
// so this is not a different sandbox; it is the same sandbox with an ABI, a
// manifest and a host-function convention already written.
type ExtismRuntime struct {
	compiled *extism.CompiledPlugin
}

// NewExtismRuntime compiles wasm. closeOnContextDone is passed through to the
// wazero runtime config underneath, so the deadline model is identical.
func NewExtismRuntime(ctx context.Context, wasm []byte, closeOnContextDone bool) (*ExtismRuntime, error) {
	manifest := extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmData{Data: wasm, Name: "main"}},
		// AllowedHosts stays EMPTY. The SDK's own http_request host function is
		// governed by it, and an empty list means the guest may reach nothing —
		// which is what the sandbox probe checks.
	}
	cfg := extism.PluginConfig{
		EnableWasi:    true,
		RuntimeConfig: runtimeConfig().WithCloseOnContextDone(closeOnContextDone),
	}
	compiled, err := extism.NewCompiledPlugin(ctx, manifest, cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("compiling guest: %w", err)
	}
	return &ExtismRuntime{compiled: compiled}, nil
}

// Close releases the compiled artifact.
func (r *ExtismRuntime) Close(ctx context.Context) error { return r.compiled.Close(ctx) }

// ExtismGuest is one instantiated plugin.
type ExtismGuest struct {
	plugin *extism.Plugin
}

// Instantiate creates a fresh instance from the compiled artifact.
func (r *ExtismRuntime) Instantiate(ctx context.Context) (*ExtismGuest, error) {
	p, err := r.compiled.Instance(ctx, extism.PluginInstanceConfig{
		ModuleConfig: SandboxConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("instantiating guest: %w", err)
	}
	return &ExtismGuest{plugin: p}, nil
}

// Close destroys the instance.
func (g *ExtismGuest) Close(ctx context.Context) error { return g.plugin.Close(ctx) }

// call is the whole ABI on the host side under Extism: marshal, name the export,
// unmarshal. There is no pointer arithmetic to get wrong because there are no
// pointers — the SDK and the PDK own both buffers.
func (g *ExtismGuest) call(ctx context.Context, name string, req, out any) error {
	in, err := json.Marshal(req)
	if err != nil {
		return err
	}
	exit, res, err := g.plugin.CallWithContext(ctx, name, in)
	if err != nil {
		return fmt.Errorf("guest %s (exit %d): %w", name, exit, err)
	}
	return json.Unmarshal(res, out)
}

// SearchSubtitles is the contract call, through Extism.
func (g *ExtismGuest) SearchSubtitles(ctx context.Context, req wire.SubtitleSearchRequest) (wire.SubtitleSearchResponse, error) {
	var resp wire.SubtitleSearchResponse
	return resp, g.call(ctx, "search", req, &resp)
}

// DownloadSubtitle is the contract call, through Extism.
func (g *ExtismGuest) DownloadSubtitle(ctx context.Context, req wire.SubtitleDownloadRequest) (wire.SubtitleDownloadResponse, error) {
	var resp wire.SubtitleDownloadResponse
	return resp, g.call(ctx, "download", req, &resp)
}

// Spin calls the guest export that never returns.
func (g *ExtismGuest) Spin(ctx context.Context) error {
	_, _, err := g.plugin.CallWithContext(ctx, "spin", nil)
	return err
}
