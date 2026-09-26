//go:build !linux

package markerdetect

// lowerThreadPriority is a no-op where a nice value is the whole process's, not
// one thread's: lowering it would lower the Server's. Only the decoder is
// reniced there.
func lowerThreadPriority() {}
