package plugins

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Event sink Extension point, filled by a guest.
//
// There is deliberately nothing clever here. An Installed sink is a
// pluginapi.EventSink like the Webhook Built-in is, so the event-sink Manager
// builds it from the same settings row, the Dispatcher gives it the same bounded
// queue and drop-oldest policy, the worker calls it under the same host deadline,
// and the counters count it the same way. Everything that makes a guest different
// — the sandbox, the instance lifecycle, the allowlist — is BELOW this line, which
// is exactly where ADR-0057's "nothing downstream distinguishes the two" is either
// true or a fiction.

// newEventSink is the pluginapi.EventSinkFactory this Plugin registers with. It
// refuses for a Plugin that was refused at load, naming the reason, so an Admin
// who enables a broken Plugin is told why by the settings save rather than by
// silence.
func (p *Plugin) newEventSink(s pluginapi.Settings) (pluginapi.EventSink, error) {
	p.mu.Lock()
	disabled, lastErr := p.disabled, p.lastError
	p.mu.Unlock()
	if disabled {
		if lastErr == "" {
			lastErr = "it is disabled"
		}
		return nil, fmt.Errorf("plugin %s: %s", p.id, lastErr)
	}
	if p.compiled == nil {
		return nil, fmt.Errorf("plugin %s: no module is loaded", p.id)
	}
	return &guestSink{p: p, settings: s}, nil
}

// guestSink is one configured Installed sink: the Plugin, and the Settings the
// Admin saved. The settings are held HERE and handed to the guest with each call
// rather than installed into it, which is what keeps a secret out of an instance
// that outlives the delivery (ADR-0058 decision 5).
type guestSink struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.EventSink = (*guestSink)(nil)

// Deliver hands one event to the guest and reports what it said.
//
// Three outcomes. The guest answered delivered — nil, and the host counts a
// delivery. The guest answered not-delivered — an error carrying the author's own
// words, which the host counts as a failure and puts on the settings screen. The
// guest trapped, spun past its deadline or answered nothing at all — an error, the
// instance is discarded and rebuilt, and enough of those in a row disable the
// Plugin.
//
// It NEVER blocks past the context: the host's deadline and the Plugin's own call
// timeout both apply, and a guest that ignores them is unwound by the runtime
// rather than asked to stop.
func (g *guestSink) Deliver(ctx context.Context, ev pluginapi.SinkEvent) error {
	// The declared settings are stamped on at CALL time (issue 13): the fixed half
	// was resolved when this sink was built, the manifest-declared half is read now,
	// so a settings save reaches the next delivery without a rebuild.
	req := pluginapi.SinkDeliverRequest{Event: ev, Settings: g.p.withSettingValues(g.settings)}
	var resp pluginapi.SinkDeliverResponse

	err := g.p.callGuest(ctx, exportDeliver, hostOf(g.settings.URL), req, &resp)
	if err != nil {
		if errors.Is(err, ErrDisabled) {
			// A disabled Plugin is not called at all. The event is counted as a
			// failed delivery, which is the honest number — it did not arrive — and
			// the reason is on the settings screen rather than buried in a log.
			return err
		}
		return fmt.Errorf("plugin %s: delivering %s (%s): %w", g.p.id, ev.Type, ev.ID, err)
	}
	if !resp.Delivered {
		detail := resp.Error
		if detail == "" {
			detail = "the plugin reported no detail"
		}
		return fmt.Errorf("plugin %s: delivering %s (%s): %s", g.p.id, ev.Type, ev.ID, detail)
	}
	return nil
}

// hostOf is the host of the URL the Admin typed, normalized the same way the
// allowlist is. An unparseable URL yields "", which matches nothing — the settings
// endpoint refuses to save one, so reaching this means something built a sink the
// API would not have.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return normalizeHost(u.Hostname())
}
