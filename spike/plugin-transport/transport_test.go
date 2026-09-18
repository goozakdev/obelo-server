package transport

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// guest loads a built module or skips: the wasm artifacts are gitignored, so a
// clone that has not run ./build-guests.sh has nothing to test.
func guest(t *testing.T, name string) []byte {
	t.Helper()
	b, err := LoadGuest(BuildDir, name)
	if err != nil {
		t.Skipf("%v", err)
	}
	return b
}

// TestBareGuestAnswersBothCalls is acceptance criterion 1 for the bare option:
// the two Subtitle provider calls, end to end, through the hand-rolled ABI.
func TestBareGuestAnswersBothCalls(t *testing.T) {
	for _, name := range []string{BareGo, BareTinyGo} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rt, err := NewBareRuntime(ctx, guest(t, name), true)
			if err != nil {
				t.Fatalf("compiling %s: %v", name, err)
			}
			defer rt.Close(ctx)
			g, err := rt.Instantiate(ctx)
			if err != nil {
				t.Fatalf("instantiating %s: %v", name, err)
			}
			defer g.Close(ctx)

			sr, err := g.SearchSubtitles(ctx, SmallRequest())
			if err != nil {
				t.Fatalf("SearchSubtitles: %v", err)
			}
			if sr.Outcome != "matched" || len(sr.Candidates) != 1 {
				t.Fatalf("SearchSubtitles = %+v, want one matched candidate", sr)
			}
			if got := sr.Candidates[0].MatchedBy; got != "moviehash" {
				t.Fatalf("MatchedBy = %q, want moviehash — the guest read the request", got)
			}

			dr, err := g.DownloadSubtitle(ctx, SmallDownload())
			if err != nil {
				t.Fatalf("DownloadSubtitle: %v", err)
			}
			if dr.Outcome != "matched" || len(dr.Data) == 0 {
				t.Fatalf("DownloadSubtitle = outcome %q, %d bytes; want matched with bytes", dr.Outcome, len(dr.Data))
			}
			if !strings.Contains(string(dr.Data), "spike says hello") {
				t.Fatalf("DownloadSubtitle returned %q, want the guest's cue", string(dr.Data))
			}

			big, err := g.DownloadSubtitle(ctx, LargeDownload())
			if err != nil {
				t.Fatalf("DownloadSubtitle(1 MiB): %v", err)
			}
			if len(big.Data) != 1<<20 {
				t.Fatalf("DownloadSubtitle(1 MiB) returned %d bytes, want 1048576", len(big.Data))
			}
		})
	}
}

// TestExtismGuestAnswersBothCalls is the same acceptance, through Extism.
func TestExtismGuestAnswersBothCalls(t *testing.T) {
	for _, name := range []string{ExtismGo, ExtismTiny} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rt, err := NewExtismRuntime(ctx, guest(t, name), true)
			if err != nil {
				t.Fatalf("compiling %s: %v", name, err)
			}
			defer rt.Close(ctx)
			g, err := rt.Instantiate(ctx)
			if err != nil {
				t.Fatalf("instantiating %s: %v", name, err)
			}
			defer g.Close(ctx)

			sr, err := g.SearchSubtitles(ctx, SmallRequest())
			if err != nil {
				t.Fatalf("SearchSubtitles: %v", err)
			}
			if sr.Outcome != "matched" || len(sr.Candidates) != 1 {
				t.Fatalf("SearchSubtitles = %+v, want one matched candidate", sr)
			}
			if got := sr.Candidates[0].MatchedBy; got != "moviehash" {
				t.Fatalf("MatchedBy = %q, want moviehash", got)
			}

			dr, err := g.DownloadSubtitle(ctx, SmallDownload())
			if err != nil {
				t.Fatalf("DownloadSubtitle: %v", err)
			}
			if dr.Outcome != "matched" || !strings.Contains(string(dr.Data), "spike says hello") {
				t.Fatalf("DownloadSubtitle = %+v, want the guest's cue", dr.Outcome)
			}

			big, err := g.DownloadSubtitle(ctx, LargeDownload())
			if err != nil {
				t.Fatalf("DownloadSubtitle(1 MiB): %v", err)
			}
			if len(big.Data) != 1<<20 {
				t.Fatalf("DownloadSubtitle(1 MiB) returned %d bytes, want 1048576", len(big.Data))
			}
		})
	}
}

// TestADeadlineStopsASpinningGuest is the deadline model: a guest that never
// returns is unwound by the host when its context expires, and the host is free
// again. Without WithCloseOnContextDone(true) this test hangs forever, which is
// exactly why the loader must set it.
func TestADeadlineStopsASpinningGuest(t *testing.T) {
	ctx := context.Background()
	rt, err := NewBareRuntime(ctx, guest(t, BareGo), true)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	defer rt.Close(ctx)
	g, err := rt.Instantiate(ctx)
	if err != nil {
		t.Fatalf("instantiating: %v", err)
	}
	defer g.Close(ctx)

	callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = g.Spin(callCtx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Spin returned nil — the guest that never returns, returned")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Spin took %v to be stopped by a 200ms deadline", elapsed)
	}
	t.Logf("a spinning guest was stopped after %v with: %v", elapsed.Round(time.Millisecond), err)
}

// TestAGuestCannotReachTheNetworkOrFilesystem is the sandbox claim of ADR-0058.
// The probe guest is compiled by the ordinary command and asks for a socket, a
// file and a process; it is granted no host function and gets none of them.
func TestAGuestCannotReachTheNetworkOrFilesystem(t *testing.T) {
	ctx := context.Background()
	results, err := SandboxProbes(ctx, guest(t, ProbeGo))
	if err != nil {
		t.Fatalf("running the probes: %v", err)
	}
	says := map[string]string{}
	for _, r := range results {
		t.Logf("%s -> %s%s", r.Export, r.Says, r.TrapErr)
		says[r.Export] = r.Says + r.TrapErr
		if says[r.Export] == "" {
			t.Fatalf("%s reported nothing at all", r.Export)
		}
	}

	// The three that must simply fail.
	for _, name := range []string{"probe_net", "probe_file", "probe_dir", "probe_exec"} {
		if strings.Contains(says[name], "NO ERROR") {
			t.Fatalf("%s succeeded inside the sandbox: %s", name, says[name])
		}
	}

	// net.Listen and a self-dial DO succeed, and that is not a hole: Go's wasip1
	// port ships an in-process fake network stack, so the guest is talking to
	// itself inside its own linear memory. The proof is that the address it claims
	// to have bound is not bound on THIS machine.
	if !strings.Contains(says["probe_selfdial"], "NO ERROR") {
		t.Logf("note: the guest could not even dial itself: %s", says["probe_selfdial"])
	}
	if strings.Contains(says["probe_listen"], "NO ERROR") {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:34517", 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatal("the guest's listener is reachable on the host — the sandbox did not hold")
		}
		t.Logf("the guest's claimed listener is not on the host: %v", err)
	}
}

// TestAGuestImportsOnlyWASI pins the fact a loader checks before it instantiates:
// the module asks the host for nothing but wasi_snapshot_preview1 (and, for an
// Extism guest, extism:host/env). Anything else is a module refusing to be
// sandboxed, and is refused back.
func TestAGuestImportsOnlyWASI(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		want map[string]bool
	}{
		{BareGo, map[string]bool{"wasi_snapshot_preview1": true}},
		{ExtismGo, map[string]bool{"wasi_snapshot_preview1": true, "extism:host/env": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods, err := HostImports(ctx, guest(t, tc.name))
			if err != nil {
				t.Fatalf("compiling: %v", err)
			}
			for _, m := range mods {
				if !tc.want[m] {
					t.Fatalf("guest imports %q, which the host never offered", m)
				}
			}
			t.Logf("%s imports from %v", tc.name, mods)
		})
	}
}
