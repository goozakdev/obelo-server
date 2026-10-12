package seekpreview

import "testing"

// FrameArgs exposes the ffmpeg command line for a test.
var FrameArgs = frameArgs

// NoRenice stops the decoder being reniced to 19 until the test ends: a real
// ffmpeg at nice 19 is starved past any polling deadline on a loaded CI runner.
// It sets a package variable, so its callers must not run in parallel.
func NoRenice(t *testing.T) {
	t.Helper()
	old := lowerPriority
	lowerPriority = func(int) {}
	t.Cleanup(func() { lowerPriority = old })
}
