//go:build !unix

package markerdetect

// lowerPriority is a no-op where there is no nice value to set.
func lowerPriority(int) {}
