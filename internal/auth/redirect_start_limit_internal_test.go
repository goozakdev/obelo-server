package auth

import (
	"fmt"
	"testing"
	"time"
)

// TestAFullRedirectStartLimitScansAtMostOncePerExpiry: once a scan of the full
// table has found nothing to free, nothing can be freed before the oldest window
// it holds runs out, so no request before then scans again. A window made to run
// out early behind the limiter's back stays unseen until that moment: a scan per
// request would find it.
func TestAFullRedirectStartLimitScansAtMostOncePerExpiry(t *testing.T) {
	l := newRedirectStartLimiter()
	start := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < redirectStartSources; i++ {
		l.charge(fmt.Sprintf("source-%d", i), start)
	}
	if ok, _ := l.allow("new-1", start); ok {
		t.Fatal("a new source with the table full was allowed")
	}

	l.counts["source-0"].windowStart = start.Add(-redirectStartWindow)
	if ok, _ := l.allow("new-2", start.Add(time.Minute)); ok {
		t.Fatal("a new source before the oldest window's end was allowed: the full table was scanned again")
	}
	if len(l.counts) != redirectStartSources {
		t.Fatalf("the table holds %d sources before the oldest window's end, want %d: it was scanned again",
			len(l.counts), redirectStartSources)
	}

	if ok, _ := l.allow("new-2", start.Add(redirectStartWindow)); !ok {
		t.Fatal("a new source when the oldest window ran out was refused")
	}
}
