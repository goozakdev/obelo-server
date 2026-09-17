package eventsink

import (
	"context"
	"fmt"
	"log"
	"sync"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/store"
)

// ManagerStore is the persistence the Manager reads to rebuild the live sinks.
// *store.DB satisfies it; the narrow interface keeps the seam explicit and lets a
// test drive Reload without a live database.
type ManagerStore interface {
	EventSinks() ([]store.EventSinkRow, error)
}

// Manager owns the "read settings → build → swap the live sinks" cycle, the same
// cycle enrich.Manager and subfetch.Manager own for their Extension points. Reload
// runs once at boot and again after every settings save, so enabling a sink,
// changing its URL or narrowing its subscriptions takes effect with no restart.
//
// Sinks are built HERE and never at registration time: the Registry is a value the
// composition root fills before the server serves, and a settings save must not be
// able to add a Plugin to it (issue 02's rule). What a save changes is which
// registered Plugins are currently live, which is exactly what a swap expresses.
type Manager struct {
	store ManagerStore
	reg   *pluginapi.Registry
	disp  *Dispatcher

	// mu serializes concurrent Reloads so the last writer's snapshot stays live.
	mu sync.Mutex
}

// NewManager wires a Manager over the settings store, the Plugin registry the
// composition root built, and the Dispatcher the translator publishes into.
func NewManager(s ManagerStore, reg *pluginapi.Registry, disp *Dispatcher) *Manager {
	return &Manager{store: s, reg: reg, disp: disp}
}

// Dispatcher is where events go. Exposed so the composition root can hand the same
// value to the translator without threading it twice.
func (m *Manager) Dispatcher() *Dispatcher {
	if m == nil {
		return nil
	}
	return m.disp
}

// Counters reports each sink's delivery tally (issue 06 surfaces them on the
// settings response).
func (m *Manager) Counters() map[string]Counters {
	if m == nil {
		return nil
	}
	return m.disp.Counters()
}

// Reload reads the current sink settings, builds every sink that is enabled,
// configured and subscribed to something, and swaps the whole set in. It is
// idempotent, and total: a sink that cannot be built from its settings is skipped
// with a log line rather than failing the reload, because one misconfigured
// outbound integration must never be able to stop a boot (ADR-0001).
//
// Returns an error only if the settings read fails.
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows, err := m.store.EventSinks()
	if err != nil {
		return fmt.Errorf("eventsink: manager reload: %w", err)
	}

	var specs []sinkSpec
	for _, row := range rows {
		spec, ok := m.build(row)
		if !ok {
			continue
		}
		specs = append(specs, spec)
	}
	m.disp.swap(specs)
	return nil
}

// build turns one settings row into a live sink, or reports that it should not be
// one. The four ways a row is not a sink are all ordinary states, not errors:
//
//   - no Plugin claims the slug (a row left behind by a build that had one);
//   - the Admin turned it off;
//   - it needs a secret and has none, or has no URL — the settings endpoint
//     refuses to save that combination enabled, so this is belt and braces;
//   - it subscribes to nothing, or to nothing this server can derive, in which
//     case there is no work for it and the translator should not be doing any.
func (m *Manager) build(row store.EventSinkRow) (sinkSpec, bool) {
	registration, ok := m.reg.EventSink(row.Slug)
	if !ok {
		return sinkSpec{}, false
	}
	if !row.Enabled {
		return sinkSpec{}, false
	}
	d := registration.Descriptor
	if row.URL == "" || (d.RequiresKey && row.Secret == "") {
		return sinkSpec{}, false
	}

	events := make(map[string]struct{}, len(row.Events))
	for _, ev := range row.Events {
		if Supported(ev) {
			events[ev] = struct{}{}
		}
	}
	if len(events) == 0 {
		return sinkSpec{}, false
	}

	plugin, err := registration.New(pluginapi.Settings{
		Enabled: true,
		Secret:  row.Secret,
		URL:     row.URL,
		Events:  row.Events,
	})
	if err != nil {
		// A sink that cannot be built from these settings makes no calls at all
		// rather than half-working (ADR-0001).
		log.Printf("obelo: event sink %q is configured but could not be built, so it is off: %v", row.Slug, err)
		return sinkSpec{}, false
	}
	if plugin == nil {
		log.Printf("obelo: event sink %q built nothing, so it is off", row.Slug)
		return sinkSpec{}, false
	}
	return sinkSpec{slug: row.Slug, plugin: plugin, events: events}, true
}
