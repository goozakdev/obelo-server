package markerdetect

import (
	"runtime"
	"syscall"
)

// lowerThreadPriority runs the calling goroutine at the lowest CPU priority for
// the rest of its life (ADR-0065 §4), for the work detection does in Go: the
// fingerprinting and the comparisons. On Linux a nice value belongs to a thread,
// so the goroutine is locked to its thread and that thread reniced; it is never
// unlocked, so the thread ends with the goroutine instead of going back to run
// the rest of the Server at the lowest priority. Best effort, like lowerPriority.
func lowerThreadPriority() {
	runtime.LockOSThread()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 19)
}
