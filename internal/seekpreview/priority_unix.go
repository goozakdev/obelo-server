//go:build unix

package seekpreview

import "syscall"

// lowerPriority renices a decoder to the lowest CPU priority: generation is
// background work. Best effort, like Marker detection's.
// A variable so tests running real ffmpeg can opt out of being starved.
var lowerPriority = func(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
