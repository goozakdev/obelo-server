package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/goozakdev/obelo-server/internal/eventsink"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Admin-scope Event sink settings (ADR-0057 decision 6), the exact shape of the
// metadata- and subtitle-provider settings surfaces: an Admin views the registered
// sink Plugins joined with the current DB settings and saves a partial update,
// which rebuilds + hot-swaps the live sinks with no restart. The secret is NEVER
// returned — only a hasSecret boolean — because it is a signing key and is handled
// with exactly the care an API key is.
//
// The one thing this surface has that the provider surfaces do not is the
// subscribed-event list, the first user of pluginapi.Settings.Events. The server
// answers with the event types it can actually derive today (availableEvents), so
// the screen offers only those and an Admin can never subscribe to something
// nothing produces.
//
// Every route is Admin-only (wired behind requireAuth + requireAdmin via the
// /settings/ subtree in api.go).

// EventSinkSettingsStore is the persistence the event-sink settings handlers read
// and write. *store.DB satisfies it; the narrow interface keeps the HTTP layer
// testable.
type EventSinkSettingsStore interface {
	EventSinks() ([]store.EventSinkRow, error)
	UpsertEventSink(u store.EventSinkUpsert) error
}

// --- Wire shapes ------------------------------------------------------------

// eventSinkJSON is one sink in the GET/PUT response: the Plugin's static facts
// joined with the current settings. hasSecret reports whether a signing key is on
// file WITHOUT exposing it.
type eventSinkJSON struct {
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	RequiresSecret bool     `json:"requiresSecret"`
	Enabled        bool     `json:"enabled"`
	HasSecret      bool     `json:"hasSecret"`
	URL            string   `json:"url"`
	Events         []string `json:"events"`
	Description    string   `json:"description"`
	DocsURL        string   `json:"docsURL"`
}

// eventSinksResponse is the GET/PUT body: the joined sink list plus the event
// types this server can derive. availableEvents is what the settings screen
// offers; it grows as translations land, and a client must not hard-code it.
type eventSinksResponse struct {
	Sinks           []eventSinkJSON `json:"sinks"`
	AvailableEvents []string        `json:"availableEvents"`
}

// eventSinkUpdateJSON is one sink's partial update in the PUT body. Every field is
// a pointer so omitted (nil) is distinguishable from an explicit value — the
// secret semantics depend on it: secret omitted = unchanged, "" = clear, non-empty
// = set. events omitted = unchanged, [] = subscribe to nothing.
type eventSinkUpdateJSON struct {
	Slug    string    `json:"slug"`
	Enabled *bool     `json:"enabled,omitempty"`
	Secret  *string   `json:"secret,omitempty"`
	URL     *string   `json:"url,omitempty"`
	Events  *[]string `json:"events,omitempty"`
}

// updateEventSinksRequest is the PUT body: per-sink partial updates.
type updateEventSinksRequest struct {
	Sinks []eventSinkUpdateJSON `json:"sinks,omitempty"`
}

// --- Routing ----------------------------------------------------------------

// handleEventSinkSettingsSubtree dispatches the event-sink routes off the
// /settings/ subtree (already behind requireAuth + requireAdmin):
//
//	GET  /settings/event-sinks → registered sinks + settings view
//	PUT  /settings/event-sinks → partial update, rebuild + swap
//
// There is deliberately no /test leaf. A provider probe asks a third party whether
// a credential works; a sink's secret is this server's OWN signing key, so there is
// nothing to ask anyone. The way to test a sink is to subscribe it and run a scan.
func handleEventSinkSettingsSubtree(deps Deps, rest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rest != "event-sinks" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleGetEventSinks(deps)(w, r)
		case http.MethodPut:
			handleUpdateEventSinks(deps)(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
		}
	}
}

// --- GET --------------------------------------------------------------------

func handleGetEventSinks(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := buildEventSinksResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read event sink settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- PUT --------------------------------------------------------------------

func handleUpdateEventSinks(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.EventSinks == nil {
			// A build wired without the settings store answers honestly rather than
			// accepting a save it cannot persist.
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"event sink settings are not available on this server", nil)
			return
		}
		var req updateEventSinksRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// Resolve each partial update against the current row, validating before any
		// write (all-or-nothing on validation, mirroring both provider settings PUTs).
		current, err := currentEventSinkRows(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read event sink settings", nil)
			return
		}
		var upserts []store.EventSinkUpsert
		for _, u := range req.Sinks {
			registration, ok := deps.Plugins.EventSink(u.Slug)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown, "unknown event sink: "+u.Slug, nil)
				return
			}
			row := current[u.Slug] // zero value = never configured
			desired := store.EventSinkUpsert{
				Slug:    u.Slug,
				Enabled: row.Enabled,
				Secret:  row.Secret,
				URL:     row.URL,
				Events:  row.Events,
			}
			if u.Enabled != nil {
				desired.Enabled = *u.Enabled
			}
			if u.Secret != nil {
				desired.Secret = strings.TrimSpace(*u.Secret)
			}
			if u.URL != nil {
				desired.URL = strings.TrimSpace(*u.URL)
			}
			if u.Events != nil {
				events, bad := normalizeSinkEvents(*u.Events)
				if bad != "" {
					writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidSetting,
						"this server does not emit the event "+bad, nil)
					return
				}
				desired.Events = events
			}
			if desired.URL != "" && !validSinkURL(desired.URL) {
				writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidBaseURL,
					"the target URL must be an absolute http:// or https:// URL", nil)
				return
			}
			// A sink with nowhere to post, or no key to sign with, cannot be turned
			// on: the server must never post unsigned, and never post nowhere.
			if desired.Enabled && desired.URL == "" {
				writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidSetting,
					"a target URL is required to enable "+registration.Descriptor.Name, nil)
				return
			}
			if desired.Enabled && registration.Descriptor.RequiresKey && desired.Secret == "" {
				writeError(w, http.StatusUnprocessableEntity, codeProviderKeyRequired,
					"a signing secret is required to enable "+registration.Descriptor.Name, nil)
				return
			}
			upserts = append(upserts, desired)
		}

		for _, u := range upserts {
			if err := deps.EventSinks.UpsertEventSink(u); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save event sink settings", nil)
				return
			}
		}

		// Hot-swap the live sinks so an enable / URL / subscription change takes
		// effect with no restart (mirrors the two provider PUTs calling Reload).
		if deps.EventSinkManager != nil {
			if err := deps.EventSinkManager.Reload(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply event sink settings", nil)
				return
			}
		}

		resp, err := buildEventSinksResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read event sink settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- helpers ----------------------------------------------------------------

// normalizeSinkEvents trims and de-duplicates a subscription list, preserving the
// Admin's order, and reports the first event type this server cannot derive.
// Refusing an unknown type is the whole reason availableEvents is on the response:
// an Admin who subscribes to something nothing emits would see silence and have no
// way to tell it from a broken receiver.
func normalizeSinkEvents(in []string) (events []string, invalid string) {
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		ev := strings.TrimSpace(raw)
		if ev == "" {
			continue
		}
		if !eventsink.Supported(ev) {
			return nil, ev
		}
		if seen[ev] {
			continue
		}
		seen[ev] = true
		events = append(events, ev)
	}
	return events, ""
}

// validSinkURL reports whether a target is an absolute http(s) URL with a host.
// Where it POINTS is not checked here: an operator aiming a webhook at a box on
// their own LAN is the point of this product (ADR-0001), and what the safe fetcher
// refuses is where that box then REDIRECTS us.
func validSinkURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// currentEventSinkRows reads the persisted sink rows into a slug-keyed map (a
// missing sink = the zero row = never configured).
func currentEventSinkRows(deps Deps) (map[string]store.EventSinkRow, error) {
	if deps.EventSinks == nil {
		return map[string]store.EventSinkRow{}, nil
	}
	rows, err := deps.EventSinks.EventSinks()
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.EventSinkRow, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out, nil
}

// buildEventSinksResponse joins the registered Event sink Plugins with the DB
// rows, masking the secret to a hasSecret boolean. The list is in registration
// order, which is what keeps the screen deterministic now that the catalog is a
// value the composition root builds (ADR-0057 decision 5).
func buildEventSinksResponse(deps Deps) (eventSinksResponse, error) {
	rows, err := currentEventSinkRows(deps)
	if err != nil {
		return eventSinksResponse{}, err
	}
	resp := eventSinksResponse{AvailableEvents: eventsink.SupportedEventTypes()}
	for _, registration := range deps.Plugins.EventSinks() {
		d := registration.Descriptor
		row := rows[d.Slug]
		events := row.Events
		if events == nil {
			events = []string{}
		}
		resp.Sinks = append(resp.Sinks, eventSinkJSON{
			Slug:           d.Slug,
			Name:           d.Name,
			RequiresSecret: d.RequiresKey,
			Enabled:        row.Enabled,
			HasSecret:      row.Secret != "",
			URL:            row.URL,
			Events:         events,
			Description:    d.Description,
			DocsURL:        d.DocsURL,
		})
	}
	if resp.Sinks == nil {
		resp.Sinks = []eventSinkJSON{}
	}
	return resp, nil
}
