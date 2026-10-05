//go:build !windows

package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestKeyframeBoundariesHonoursCtxOnAHungRead: the container-index read is plain file
// I/O, so a hung mount used to block the caller past its ctx. A FIFO with no writer
// blocks open(2) the way a dead NFS/SMB mount blocks a read.
func TestKeyframeBoundariesHonoursCtxOnAHungRead(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "hung.mkv")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	// Release the abandoned reader goroutine when the test ends.
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := KeyframeBoundaries(ctx, "", fifo, 4, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("KeyframeBoundaries blocked past its ctx on a hung read")
	}
}
