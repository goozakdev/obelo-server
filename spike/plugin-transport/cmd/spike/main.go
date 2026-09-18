// Command spike runs every measurement ADR-0058's table quotes and prints it as
// markdown. Run it from the spike directory after ./build-guests.sh:
//
//	go run ./cmd/spike
//
// Everything it prints is measured on the machine it runs on; the ADR records
// which machine that was.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	transport "obelo-spike/plugin-transport"
	"obelo-spike/plugin-transport/wire"
)

// calls is how many times each per-call measurement calls the guest. largeCalls
// is lower because a mebibyte a call grows the guest's linear memory toward a
// ceiling this spike measures rather than assumes (see survives).
const (
	calls       = 1000
	largeCalls  = 200
	survivalCap = 20000
)

// caller is the two contract calls, whichever transport is underneath.
type caller interface {
	SearchSubtitles(context.Context, wire.SubtitleSearchRequest) (wire.SubtitleSearchResponse, error)
	DownloadSubtitle(context.Context, wire.SubtitleDownloadRequest) (wire.SubtitleDownloadResponse, error)
	Close(context.Context) error
}

// loaded is a compiled module that can be instantiated, whichever transport.
type loaded interface {
	instantiate(context.Context) (caller, error)
	Close(context.Context) error
}

type bareLoaded struct{ *transport.BareRuntime }

func (b bareLoaded) instantiate(ctx context.Context) (caller, error) {
	return b.BareRuntime.Instantiate(ctx)
}

type extismLoaded struct{ *transport.ExtismRuntime }

func (e extismLoaded) instantiate(ctx context.Context) (caller, error) {
	return e.ExtismRuntime.Instantiate(ctx)
}

// variant is one row of the results table.
type variant struct {
	label string
	file  string
	// compile builds the runtime from the module bytes; closeOnContextDone is the
	// deadline mechanism, which is measured both ways.
	compile func(ctx context.Context, wasm []byte, closeOnContextDone bool) (loaded, error)
}

func variants() []variant {
	bare := func(ctx context.Context, wasm []byte, cod bool) (loaded, error) {
		rt, err := transport.NewBareRuntime(ctx, wasm, cod)
		return bareLoaded{rt}, err
	}
	ext := func(ctx context.Context, wasm []byte, cod bool) (loaded, error) {
		rt, err := transport.NewExtismRuntime(ctx, wasm, cod)
		return extismLoaded{rt}, err
	}
	return []variant{
		{"bare wazero / stock Go", transport.BareGo, bare},
		{"bare wazero / TinyGo", transport.BareTinyGo, bare},
		{"Extism / stock Go", transport.ExtismGo, ext},
		{"Extism / TinyGo", transport.ExtismTiny, ext},
	}
}

type row struct {
	variant         string
	sizeBytes       int64
	compileMs       float64
	instantiateMs   float64
	searchPooledUs  float64
	searchPerCallUs float64
	realisticUs     float64
	mibPooledUs     float64
	mibSurvived     int
	noDeadlineUs    float64
}

func main() {
	ctx := context.Background()
	fmt.Printf("# Spike results\n\n%s, Go %s, wazero compiler backend.\nEach per-call figure is the mean of %d calls.\n\n",
		runtime.GOOS+"/"+runtime.GOARCH, runtime.Version(), calls)

	var rows []row
	for _, v := range variants() {
		wasm, err := transport.LoadGuest(transport.BuildDir, v.file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", v.label, err)
			continue
		}
		r, err := measure(ctx, v, wasm)
		if err != nil {
			fmt.Fprintf(os.Stderr, "measuring %s: %v\n", v.label, err)
			continue
		}
		rows = append(rows, r)
	}

	fmt.Println("| Variant | Guest size | Compile | Instantiate | search, pooled | search, instance-per-call | 64 KiB download | 1 MiB download | 1 MiB calls one instance survives |")
	fmt.Println("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, r := range rows {
		fmt.Printf("| %s | %s | %.0f ms | %.2f ms | %.0f µs | %.0f µs | %.0f µs | %.1f ms | %d |\n",
			r.variant, humanSize(r.sizeBytes), r.compileMs, r.instantiateMs,
			r.searchPooledUs, r.searchPerCallUs, r.realisticUs, r.mibPooledUs/1000, r.mibSurvived)
	}

	fmt.Print("\n## Deadline overhead (WithCloseOnContextDone)\n\n")
	fmt.Println("| Variant | search, pooled, deadline ON | search, pooled, deadline OFF | overhead |")
	fmt.Println("| --- | ---: | ---: | ---: |")
	for _, r := range rows {
		fmt.Printf("| %s | %.1f µs | %.1f µs | %+.1f%% |\n", r.variant,
			r.searchPooledUs, r.noDeadlineUs, 100*(r.searchPooledUs-r.noDeadlineUs)/r.noDeadlineUs)
	}

	probes(ctx)
}

func measure(ctx context.Context, v variant, wasm []byte) (row, error) {
	r := row{variant: v.label}
	if fi, err := os.Stat(filepath.Join(transport.BuildDir, v.file)); err == nil {
		r.sizeBytes = fi.Size()
	}

	// Compile: the once-per-installed-plugin cost. Median of 5.
	var compileTimes []time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		rt, err := v.compile(ctx, wasm, true)
		if err != nil {
			return r, err
		}
		compileTimes = append(compileTimes, time.Since(start))
		_ = rt.Close(ctx)
	}
	r.compileMs = float64(median(compileTimes)) / float64(time.Millisecond)

	rt, err := v.compile(ctx, wasm, true)
	if err != nil {
		return r, err
	}
	defer rt.Close(ctx)

	// Instantiate: the per-call cost if the loader instantiates per call. Median
	// of 50, because it is small enough for one sample to be noise.
	var instTimes []time.Duration
	for i := 0; i < 50; i++ {
		start := time.Now()
		g, err := rt.instantiate(ctx)
		if err != nil {
			return r, err
		}
		instTimes = append(instTimes, time.Since(start))
		_ = g.Close(ctx)
	}
	r.instantiateMs = float64(median(instTimes)) / float64(time.Millisecond)

	// search, pooled: one instance reused for every call.
	g, err := rt.instantiate(ctx)
	if err != nil {
		return r, err
	}
	small := transport.SmallRequest()
	runtime.GC()
	start := time.Now()
	for i := 0; i < calls; i++ {
		if _, err := g.SearchSubtitles(ctx, small); err != nil {
			return r, err
		}
	}
	r.searchPooledUs = perCallUs(time.Since(start))

	// 64 KiB download, pooled: a real subtitle for a feature film.
	realistic := transport.RealisticDownload()
	runtime.GC()
	start = time.Now()
	for i := 0; i < calls; i++ {
		if _, err := g.DownloadSubtitle(ctx, realistic); err != nil {
			return r, err
		}
	}
	r.realisticUs = perCallUs(time.Since(start))

	// 1 MiB download, pooled. A guest's linear memory only ever GROWS — wasm has
	// no way to give a page back — so a pooled instance handed a mebibyte at a
	// time eventually traps. largeCalls is under that ceiling for every variant;
	// the ceiling itself is measured separately below.
	big := transport.LargeDownload()
	runtime.GC()
	start = time.Now()
	for i := 0; i < largeCalls; i++ {
		if _, err := g.DownloadSubtitle(ctx, big); err != nil {
			return r, fmt.Errorf("1 MiB download %d of %d: %w", i, largeCalls, err)
		}
	}
	r.mibPooledUs = float64(time.Since(start)) / float64(largeCalls) / float64(time.Microsecond)
	_ = g.Close(ctx)

	// How many 1 MiB calls ONE pooled instance survives. This is the number the
	// pool-vs-per-call decision turns on: a pool is only safe with a recycle
	// policy, and the policy needs a number.
	r.mibSurvived = survives(ctx, rt, big)

	// search, instance-per-call: a fresh guest for every call, which is what
	// "no shared state between calls" would cost.
	runtime.GC()
	start = time.Now()
	for i := 0; i < calls; i++ {
		g, err := rt.instantiate(ctx)
		if err != nil {
			return r, err
		}
		if _, err := g.SearchSubtitles(ctx, small); err != nil {
			return r, err
		}
		_ = g.Close(ctx)
	}
	r.searchPerCallUs = perCallUs(time.Since(start))

	// The same pooled search with the deadline mechanism off, to price it.
	rtOff, err := v.compile(ctx, wasm, false)
	if err != nil {
		return r, err
	}
	defer rtOff.Close(ctx)
	gOff, err := rtOff.instantiate(ctx)
	if err != nil {
		return r, err
	}
	defer gOff.Close(ctx)
	runtime.GC()
	start = time.Now()
	for i := 0; i < calls; i++ {
		if _, err := gOff.SearchSubtitles(ctx, small); err != nil {
			return r, err
		}
	}
	r.noDeadlineUs = perCallUs(time.Since(start))

	return r, nil
}

// probes prints the sandbox and deadline evidence the ADR quotes verbatim.
func probes(ctx context.Context) {
	fmt.Print("\n## Sandbox probe (bare wazero, WASI instantiated, nothing granted)\n\n")
	wasm, err := transport.LoadGuest(transport.BuildDir, transport.ProbeGo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping probes: %v\n", err)
		return
	}
	results, err := transport.SandboxProbes(ctx, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probes: %v\n", err)
		return
	}
	fmt.Println("| The guest tried | It was told |")
	fmt.Println("| --- | --- |")
	for _, r := range results {
		fmt.Printf("| `%s` | `%s%s` |\n", r.Export, r.Says, r.TrapErr)
	}

	if err := transport.SandboxWithoutWASI(ctx, wasm); err != nil {
		fmt.Printf("\nWith no WASI module instantiated at all, the guest does not even load: `%v`\n", err)
	} else {
		fmt.Println("\nWith no WASI module instantiated at all, the guest still loaded.")
	}

	for _, name := range []string{transport.BareGo, transport.ExtismGo} {
		mods, err := transport.HostImports(ctx, name2bytes(name))
		if err != nil {
			continue
		}
		fmt.Printf("\n`%s` imports from: %v\n", name, mods)
	}

	// The deadline, measured rather than asserted.
	rt, err := transport.NewBareRuntime(ctx, name2bytes(transport.BareGo), true)
	if err != nil {
		return
	}
	defer rt.Close(ctx)
	g, err := rt.Instantiate(ctx)
	if err != nil {
		return
	}
	defer g.Close(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = g.Spin(callCtx)
	fmt.Printf("\nA guest that never returns, given a 200 ms deadline, was stopped after %v with: `%v`\n",
		time.Since(start).Round(time.Millisecond), err)
}

func name2bytes(name string) []byte {
	b, _ := transport.LoadGuest(transport.BuildDir, name)
	return b
}

// survives counts how many 1 MiB downloads ONE fresh instance answers before its
// linear memory is exhausted. survivalCap bounds the experiment, not the guest.
func survives(ctx context.Context, rt loaded, req wire.SubtitleDownloadRequest) int {
	if os.Getenv("OBELO_SPIKE_INTERPRETER") == "1" {
		return -1 // meaningless under the interpreter, and far too slow to find out
	}
	g, err := rt.instantiate(ctx)
	if err != nil {
		return 0
	}
	defer g.Close(ctx)
	for i := 0; i < survivalCap; i++ {
		if _, err := g.DownloadSubtitle(ctx, req); err != nil {
			return i
		}
	}
	return survivalCap
}

func perCallUs(total time.Duration) float64 {
	return float64(total) / float64(calls) / float64(time.Microsecond)
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
