//go:build unix

package seekpreview

import "syscall"

// lowerPriority renices a decoder to the lowest CPU priority: generation is
// background work. Best effort, like Marker detection's.
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
