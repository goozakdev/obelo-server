package markerdetect

// SetLowerWorkerPriority replaces what the worker calls to lower its own
// priority, for a test to observe; the returned func puts it back.
func SetLowerWorkerPriority(f func()) (restore func()) {
	old := lowerWorkerPriority
	lowerWorkerPriority = f
	return func() { lowerWorkerPriority = old }
}

// SetCompareEnds wraps what each comparison of two episodes' ends calls, for a
// test to observe; the returned func puts it back.
func SetCompareEnds(observe func()) (restore func()) {
	old := compareEnds
	compareEnds = func(a, b Print, p Params) (shared, bool) {
		observe()
		return old(a, b, p)
	}
	return func() { compareEnds = old }
}

// ObserveComparisons wraps what each comparison of two episodes' ends calls,
// handing observe the two Prints compared; the returned func puts it back.
func ObserveComparisons(observe func(a, b Print)) (restore func()) {
	old := compareEnds
	compareEnds = func(a, b Print, p Params) (shared, bool) {
		observe(a, b)
		return old(a, b, p)
	}
	return func() { compareEnds = old }
}
