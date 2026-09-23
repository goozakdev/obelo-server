package pluginsdk

import (
	"context"
	"time"
)

// CallContext builds the ctx an export dispatcher hands to served provider code,
// from the time remaining the host put on the wire
// (pluginapi.Settings.CallRemainingMillis, ADR-0059 decision 6).
//
// Absent (nil, or zero-and-below) means the host did not tell this seam a
// budget, which today is never — every seam stamps one — but a guest built
// before this field existed, or a host that ever stops, must get exactly what it
// got before: context.Background(), forever.
//
// Present means a context.WithTimeout a MARGIN short of the host's own deadline:
// margin is 2 seconds, clamped to a quarter of the budget so a small budget still
// leaves a positive window (min(2s, budget/4)). A plugin that honours ctx —
// [Pacer.Wait], [GetJSON] and friends — returns an error inside that window
// rather than being unwound by the runtime's own, harder deadline, which is what
// turns a would-be deadline kill into a clean "unavailable" answer instead.
//
// It is one function so every dispatcher — metadata's eight exports, deliver,
// the two subtitle calls — reads the field and builds the ctx the same way.
func CallContext(remainingMillis *int) (context.Context, context.CancelFunc) {
	if remainingMillis == nil || *remainingMillis <= 0 {
		return context.Background(), func() {}
	}
	budget := time.Duration(*remainingMillis) * time.Millisecond
	margin := 2 * time.Second
	if quarter := budget / 4; quarter < margin {
		margin = quarter
	}
	return context.WithTimeout(context.Background(), budget-margin)
}
