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
	// so a settings save reaches the next delivery without a rebuild. Only the
	// remaining-time half comes from callCtx — the bounded context callGuestUnder
	// builds AFTER taking callMu — so Settings.CallRemainingMillis tells the guest
	// what is left once any lock wait behind another in-flight call on this
	// Plugin is over, not the seam's nominal CallTimeout.
	var resp pluginapi.SinkDeliverResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SinkDeliverRequest{Event: ev, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}

	err := g.p.callGuest(ctx, exportDeliver, addrsOf(g.settings), buildReq, &resp)
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

// addrOf is the host and port of the URL the Admin typed — the port the URL
// names, or its scheme's — spelled as the fetch policy compares them (dialKey).
// An unparseable URL yields "", which matches nothing — the settings endpoint
// refuses to save one, so reaching this means something built a sink the API
// would not have.
func addrOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return dialKey(u.Hostname(), targetPort(u))
}

// addrsOf is addrOf of each URL the Admin typed in s: the base URL and, for a
// source that has one, the second (an image host, a CDN), which the operator
// chose as surely as the first. Neither is more than its own host and port.
//
// Only a URL the Admin ENTERED counts (Settings.URLEntered, URL2Entered). A URL
// the host filled in from the manifest's default because the field was left
// empty is the plugin author's choice, not the operator's, so it is left out
// here and gets the allowlist and private-address checks at the lookup and the
// dial like any other target.
func addrsOf(s pluginapi.Settings) []string {
	var out []string
	for _, u := range []struct {
		raw     string
		entered bool
	}{{s.URL, s.URLEntered}, {s.URL2, s.URL2Entered}} {
		if !u.entered {
			continue
		}
		if a := addrOf(u.raw); a != "" {
			out = append(out, a)
		}
	}
	return out
}
