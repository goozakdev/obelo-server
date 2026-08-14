package tailnet_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/store"
	"github.com/marioquake/obelo-server/internal/tailnet"
)

// The node key's expiry in the BOOT LOG (ADR-0043). The settings panel is issue
// 04's half; this is the half that reaches an operator who has not opened the web
// UI since setup — which is precisely who this failure finds, six months after the
// last time anybody touched the box, with the symptom "remote access stopped
// working" and nothing local broken.
//
// These tests drive the real Manager against the Fake, so they run in BOTH builds:
// the adapter is what LEARNS the expiry from the Tailnet, but nothing about
// reporting it needs `tsnet` to be linked in, and stranding the ADR's promise
// behind the build tag would leave it untested in the build most people run.

// captureLog redirects the standard logger for the duration of a test and returns
// what was written. The Manager logs through log.Printf like the rest of this
// server, so this is the only way to assert on the line an operator actually sees.
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	flags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return buf
}

// safeBuffer is a bytes.Buffer that is safe to read while a watcher goroutine is
// still writing to it — without it, `-race` fails on the log capture rather than
// on anything under test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestKeyExpiryIsLoggedWhenANodeComesUp covers all four shapes the one line can
// take. Each assertion below is a distinct operator experience, and three of the
// four are failures that are silent if the line is wrong.
func TestKeyExpiryIsLoggedWhenANodeComesUp(t *testing.T) {
	in := func(d time.Duration) *time.Time { at := time.Now().Add(d); return &at }

	cases := []struct {
		name    string
		expiry  *time.Time
		state   tailnet.State
		want    []string
		notWant []string
	}{
		{
			// The GOOD case, and the one a naive implementation gets wrong: a node joined
			// with a tagged auth key — or one whose expiry the operator disabled in the
			// console, which is what the runbook tells them to do — never lapses. Rendering
			// that from the zero time would print "0001-01-01" and warn forever about
			// exactly the configuration that cannot fail.
			name:    "no expiry",
			expiry:  nil,
			state:   tailnet.StateRunning,
			want:    []string{"does not expire"},
			notWant: []string{"0001-01-01", "WARNING", "year 1"},
		},
		{
			name:    "expires in six months",
			expiry:  in(180 * 24 * time.Hour),
			state:   tailnet.StateRunning,
			want:    []string{"expires on", "180 day"},
			notWant: []string{"WARNING", "0001-01-01"},
		},
		{
			// Under the threshold: the one line that has to change an operator's day. It
			// names the date AND the fix, which lives in the Tailscale console and not
			// here — a warning that does not say what to do is a warning people learn to
			// scroll past.
			name:    "expires inside the warning window",
			expiry:  in(9 * 24 * time.Hour),
			state:   tailnet.StateRunning,
			want:    []string{"WARNING", "9 day", "Tailscale console", "Disable key expiry"},
			notWant: []string{"0001-01-01"},
		},
		{
			name:    "already lapsed",
			expiry:  in(-time.Hour),
			state:   tailnet.StateKeyExpired,
			want:    []string{"WARNING", "EXPIRED", "re-authorized", "Tailscale console"},
			notWant: []string{"0001-01-01"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			node := &tailnet.Fake{Fresh: tailnet.Status{State: tc.state, KeyExpiry: tc.expiry}}
			m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)

			if err := m.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			// The line is written by the watcher goroutine, which observes the node's
			// transitions rather than being told by the caller — the same asynchrony that
			// lets a login URL appear without anything polling for it.
			waitFor(t, func() bool { return strings.Contains(logs.String(), "node key") })

			got := logs.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the boot log does not mention %q.\nlog:\n%s", want, got)
				}
			}
			for _, never := range tc.notWant {
				if strings.Contains(got, never) {
					t.Errorf("the boot log contains %q, which it must never.\nlog:\n%s", never, got)
				}
			}
		})
	}
}

// TestKeyExpiryIsLoggedOncePerNodeThatComesUp: the line is worth reading only if
// it is not repeated. A node that transitions while up (a peer appearing, an
// address changing) must not re-log its expiry — but a node that goes away and
// comes back must, because a restart is exactly when an operator reads the log.
func TestKeyExpiryIsLoggedOncePerNodeThatComesUp(t *testing.T) {
	logs := captureLog(t)
	at := time.Now().Add(200 * 24 * time.Hour)
	// Fresh AND Returning: the reconnect below finds state on disk and settles into
	// Returning, and a node whose expiry vanished on reconnect would silently make
	// this test about the wrong thing.
	up := tailnet.Status{State: tailnet.StateRunning, KeyExpiry: &at}
	node := &tailnet.Fake{Fresh: up, Returning: up}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Two further transitions while the node stays up, of the kind a live node emits
	// constantly.
	node.Transition(tailnet.Status{State: tailnet.StateRunning, KeyExpiry: &at, FQDN: "obelo.tail1a2b.ts.net"})
	node.Transition(tailnet.Status{State: tailnet.StateRunning, KeyExpiry: &at, FQDN: "obelo.tail1a2b.ts.net"})
	waitFor(t, func() bool { return strings.Count(logs.String(), "expires on") >= 1 })

	if n := strings.Count(logs.String(), "expires on"); n != 1 {
		t.Errorf("logged the expiry %d times while the node stayed up, want 1.\nlog:\n%s", n, logs.String())
	}

	// Down and up again: the operator gets told again.
	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if err := m.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	waitFor(t, func() bool { return strings.Count(logs.String(), "expires on") >= 2 })
	if n := strings.Count(logs.String(), "expires on"); n != 2 {
		t.Errorf("logged the expiry %d times across a disconnect/reconnect, want 2.\nlog:\n%s", n, logs.String())
	}
}

// TestKeyExpiryWindowMatchesTheADR pins the constant itself. It has a twin —
// EXPIRY_WARNING_DAYS in web/src/admin/AdminRemoteAccessScreen.tsx — and the two
// are one ADR number expressed twice (ADR-0043: "a warning fires under ~14 days").
// A disagreement between them means the panel is calm while the log is shouting,
// which is the worst of both channels.
func TestKeyExpiryWindowMatchesTheADR(t *testing.T) {
	if got, want := tailnet.KeyExpiryWarningWindow, 14*24*time.Hour; got != want {
		t.Errorf("KeyExpiryWarningWindow = %v, want %v — and if this changes, "+
			"EXPIRY_WARNING_DAYS in the admin screen changes with it", got, want)
	}
}

// waitFor polls a condition briefly. The expiry line is written by the Manager's
// watcher goroutine, which observes the node's transitions asynchronously — the
// same asynchrony that lets a login URL appear without anything polling for it.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
