package pluginsdk

import (
	"context"
	"sync"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The guest's own throttle (ADR-0059 decision 5).
//
// ADR-0058 decision 5 once said the host applied the ADR-0049 per-host limiter to
// http_fetch. It never did, and nobody noticed because no guest that needed
// pacing existed. That clause is withdrawn: PACING IS THE PLUGIN'S. A source with
// a published rate policy — MusicBrainz's one request per second is the one that
// bites — is paced by the plugin that knows the policy, and the operator's
// override reaches it through Settings.RateLimitMillis like every other setting.
//
// Which leaves seven plugins each needing a mutex, a timestamp and an interval.
// This is that, once.

// Pacer spaces calls at least an interval apart. The zero value is usable and
// paces nothing, which is the honest default for a source with no published
// policy.
//
// It is per-INSTANCE and not per-host, and inside a guest those are the same
// thing: a plugin instance is one module with one linear memory, the host
// serializes every call into it (ADR-0058 decision 7), and its only way out is
// the host's fetch. The three-Library server that defeated the per-instance
// limiter in ADR-0049 cannot happen here, because the three Libraries share the
// one guest.
type Pacer struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	// sleep is time.Sleep, replaced in the SDK's own tests. A plugin never sets it.
	sleep func(time.Duration)
}

// NewPacer returns a Pacer spacing calls at least interval apart. A zero or
// negative interval paces nothing.
func NewPacer(interval time.Duration) *Pacer {
	return &Pacer{interval: interval}
}

// SetInterval changes the spacing. It exists because the operator's
// RateLimitMillis arrives per call through Settings and may change between two of
// them — a settings save is not a plugin rebuild from in here — so a plugin that
// read it once at construction would honour a number the Admin has since changed.
func (p *Pacer) SetInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.mu.Lock()
	p.interval = d
	p.mu.Unlock()
}

// Interval is the spacing in force.
func (p *Pacer) Interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interval
}

// Wait blocks until the interval since the previous Wait has elapsed, then marks
// now as the previous one. A ctx that is already done returns its error WITHOUT
// waiting and without marking, so a cancelled call does not make the next one pay
// for a request that never happened.
//
// It returns the error rather than panicking on it because the host's deadline is
// real: a plugin asked to wait 1 second inside a call with 200 ms left should
// give up and answer, not spin until the runtime unwinds it.
func (p *Pacer) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.interval > 0 && !p.last.IsZero() {
		if wait := p.interval - time.Since(p.last); wait > 0 {
			if err := p.sleepFor(ctx, wait); err != nil {
				return err
			}
		}
	}
	p.last = time.Now()
	return nil
}

// sleepFor waits, and stops early if the call's deadline arrives first.
//
// In the sandbox ctx.Done() is non-nil whenever the host told this call a
// budget (pluginapi.Settings.CallRemainingMillis, ADR-0059 decision 6): the export
// dispatcher builds ctx from it with [pluginsdk.CallContext], and this is the
// cancellable branch a real deadline reaches. It is nil only for a guest built
// before that field existed, or a call the host genuinely told nothing —
// context.Background() — where this is a plain sleep instead. Both are
// correct; only the cancellable one can be asserted from outside the sandbox.
func (p *Pacer) sleepFor(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		p.sleep(d)
		return ctx.Err()
	}
	if ctx.Done() == nil {
		time.Sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IntervalFrom is the operator's pacing for this call: Settings.RateLimitMillis
// when the host resolved one, and the plugin's own default when it did not.
//
// The pointer is the whole reason this function exists rather than a field read.
// Absent means "use your own default pacing", which is what a source the operator
// has no opinion about must get; 0 means the explicit "do not throttle at all" an
// operator sets for a self-hosted mirror with no rate policy (ADR-0049). An int
// with 0 meaning "off" would make a host that simply forgot the field hammer
// MusicBrainz.
func IntervalFrom(s pluginapi.Settings, ownDefault time.Duration) time.Duration {
	if s.RateLimitMillis == nil {
		return ownDefault
	}
	ms := *s.RateLimitMillis
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// PacedHost wraps a Host so every Fetch is paced, re-reading the operator's
// interval from Settings on each call.
//
// This is the whole of what a bundled plugin has to do about ADR-0059 decision 5:
//
//	host := pluginsdk.PacedHost(pluginsdk.Sandbox(), time.Second)
//
// Everything else on the Host passes straight through — pacing a log line or a kv
// read would be pacing something that never leaves the machine.
func PacedHost(h Host, ownDefault time.Duration) Host {
	return &pacedHost{Host: h, pacer: NewPacer(ownDefault), ownDefault: ownDefault}
}

type pacedHost struct {
	Host
	pacer      *Pacer
	ownDefault time.Duration
}

// Pacer exposes the wrapped pacer, so a plugin that paces something of its own
// (a second host, a login step) shares one clock with its fetches.
func (p *pacedHost) Pacer() *Pacer { return p.pacer }

func (p *pacedHost) Fetch(ctx context.Context, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	p.pacer.SetInterval(IntervalFrom(p.Host.Settings(), p.ownDefault))
	if err := p.pacer.Wait(ctx); err != nil {
		return pluginapi.FetchResponse{}, err
	}
	return p.Host.Fetch(ctx, req)
}

// PacerOf returns the Pacer inside a Host built by [PacedHost], and nil for any
// other Host. It is how a test asserts that pacing happened at all.
func PacerOf(h Host) *Pacer {
	if p, ok := h.(interface{ Pacer() *Pacer }); ok {
		return p.Pacer()
	}
	return nil
}
