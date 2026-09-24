//go:build unix

package markerdetect

import "syscall"

// lowerPriority renices a decoder to the lowest CPU priority (ADR-0065 §4:
// detection is the lowest-priority background work there is). Best effort: a
// process that has already exited, or a host that refuses, just runs as it is.
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
