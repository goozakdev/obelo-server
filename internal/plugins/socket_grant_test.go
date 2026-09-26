package plugins

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// readCountingMemory is a guest memory that counts the host's reads of it and
// holds nothing.
type readCountingMemory struct {
	api.Memory
	reads int
}

func (m *readCountingMemory) Read(uint32, uint32) ([]byte, bool) {
	m.reads++
	return nil, false
}

// memoryOnlyModule is a guest module with a memory and no exports.
type memoryOnlyModule struct {
	api.Module
	mem *readCountingMemory
}

func (m memoryOnlyModule) Memory() api.Memory                   { return m.mem }
func (m memoryOnlyModule) ExportedFunction(string) api.Function { return nil }

// TestTheSocketHostFunctionDoesNoWorkWithoutAGrant: a call with no socket grant
// that reaches for the socket host function is refused before its request is so
// much as read out of guest memory — audited and counted, like any other reach
// for a socket it was not given.
func TestTheSocketHostFunctionDoesNoWorkWithoutAGrant(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	p := newPlugin("sink", "", Options{
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, format)
		},
	}.withDefaults())
	p.resolveLimits()
	defer p.close(context.Background())
	h := &hostFuncs{p: p}
	mod := memoryOnlyModule{mem: &readCountingMemory{}}

	if err := p.beginCall("", false); err != nil {
		t.Fatal(err)
	}
	h.socket(context.Background(), mod, 0, 64)
	p.endCall()

	if mod.mem.reads != 0 {
		t.Fatalf("the host read the request out of guest memory %d times, want 0: a call without the grant does no work",
			mod.mem.reads)
	}
	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(logged, "refused a socket") {
		t.Errorf("no audit line for the refused socket:\n%s", logged)
	}
	if st := p.Status(); st.LastError == "" {
		t.Error("the refused socket left no last error; reaching for a socket without the grant is a violation")
	}
}
