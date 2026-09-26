package markerdetect

// SetLowerWorkerPriority replaces what the worker calls to lower its own
// priority, for a test to observe; the returned func puts it back.
func SetLowerWorkerPriority(f func()) (restore func()) {
	old := lowerWorkerPriority
	lowerWorkerPriority = f
	return func() { lowerWorkerPriority = old }
}
