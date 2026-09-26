package markerdetect

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestLowerThreadPriorityNicesOnlyTheCallersThread: on Linux a nice value is a
// thread's, so the worker's thread runs at the lowest priority and no other
// thread of the Server does — nor does any thread after the worker is gone.
func TestLowerThreadPriorityNicesOnlyTheCallersThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// The kernel reports 20 - nice.
	before, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	go func() {
		lowerThreadPriority()
		p, _ := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
		got <- p
	}()
	if p := <-got; p != 20-19 {
		t.Errorf("worker thread priority = %d (20 - nice), want nice 19", p)
	}
	after, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("this thread's priority went from %d to %d", before, after)
	}
}

// TestTheLoweredThreadEndsWithItsGoroutine: the goroutine that lowered its
// thread stays locked to it and ends locked, so Go destroys the thread rather
// than handing it, still at the lowest priority, to the rest of the Server. The
// main thread is never destroyed, so a goroutine that happened to run there is
// tried again; this goroutine keeps its own thread, so none runs on that.
func TestTheLoweredThreadEndsWithItsGoroutine(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var tid int
	for range 10 {
		got := make(chan int, 1)
		go func() {
			lowerThreadPriority()
			got <- syscall.Gettid()
		}()
		if tid = <-got; tid != os.Getpid() {
			break
		}
	}
	if tid == os.Getpid() {
		t.Skip("every try ran on the main thread")
	}
	task := fmt.Sprintf("/proc/self/task/%d", tid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(task); errors.Is(err, fs.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			p, _ := syscall.Getpriority(syscall.PRIO_PROCESS, tid)
			t.Fatalf("thread %d outlived the goroutine that lowered it (priority %d, 20 - nice)", tid, p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
