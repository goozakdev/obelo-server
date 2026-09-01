package enrich

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A rate limit belongs to the HOST, not to whoever happens to be calling it
// (ADR-0049). MusicBrainz counts requests per client address; it neither knows nor
// cares that this server split its work across several Libraries.
//
// It used to. `MusicBrainzProvider` carried its own `next` instant, and
// `Manager.resolveLibrary` builds a provider PER LIBRARY — so a server with three
// music Libraries kept three independent 1-req/sec throttles pointed at one host
// and sent 3 req/sec, each Library correctly believing it was well behaved. The
// global snapshot's provider is a fourth. Nothing in the throttle could see the
// others, because the thing being limited was never the provider instance.
//
// hostThrottles keys one limiter per host, so every provider instance addressing
// that host queues in the same line.

// hostThrottles is the process-wide registry of per-host limiters. Entries are
// never evicted: there is one per configured metadata host (a handful, for the
// life of the process), and dropping one would reset its pacing.
var hostThrottles sync.Map // host string -> *hostThrottle

// hostThrottle serializes requests to one host, holding at least minInterval
// between consecutive starts.
type hostThrottle struct {
	mu          sync.Mutex
	minInterval time.Duration
	next        time.Time // earliest instant the next request may start
}

// throttleFor returns the limiter for baseURL's host, creating it if needed, and
// sets its interval to d.
//
// Last writer wins on the interval, which is correct here because the interval is
// a server-wide setting (config.MusicBrainzRateLimit): every instance addressing a
// host is built from the same value, and when an operator changes it the rebuild
// that follows should take effect for all of them at once.
//
// A baseURL that will not parse falls back to the raw string as the key. That
// still groups instances configured identically — the common case — and a
// misparsed URL is not going to reach a host anyway.
func throttleFor(baseURL string, d time.Duration) *hostThrottle {
	key := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		key = strings.ToLower(u.Host)
	}
	v, _ := hostThrottles.LoadOrStore(key, &hostThrottle{})
	t := v.(*hostThrottle)
	t.mu.Lock()
	t.minInterval = d
	t.mu.Unlock()
	return t
}

// wait blocks until this host's minimum inter-request interval has elapsed since
// the previous request, reserving the slot under the lock and sleeping outside it
// so callers queue rather than convoy. Honors context cancellation.
func (t *hostThrottle) wait(ctx context.Context) error {
	t.mu.Lock()
	if t.minInterval <= 0 {
		t.mu.Unlock()
		return nil
	}
	start := time.Now()
	if t.next.After(start) {
		start = t.next
	}
	t.next = start.Add(t.minInterval)
	t.mu.Unlock()
	return sleepCtx(ctx, time.Until(start))
}
