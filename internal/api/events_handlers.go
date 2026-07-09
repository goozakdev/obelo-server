package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/marioquake/juicebox/internal/events"
)

// handleEvents serves the single Server-Sent Events stream (ADR-0016):
// GET /api/v1/events. It subscribes to the Broker and streams each event as
//
//	event: <type>
//	data:  <json>
//
// until the client disconnects (request context done) or the Broker closes
// (server shutdown). An initial ": connected" comment flushes the response
// headers so a client (and tests) know the stream is live before any event.
//
// Auth is cookie-capable (requireAuthAllowCookie at the route) because a browser
// EventSource cannot set an Authorization header — same rationale as the media
// GETs; native clients may still send the bearer header.
//
// The subscription is identity-aware (ADR-0016): the authenticated identity the
// auth middleware attached is turned into an events.Identity (role + accessible-
// Library set) so the Broker gates each event's audience per subscriber. The
// handler stays a dumb pipe — it serializes whatever its (already-filtered)
// channel yields.
func handleEvents(broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, codeInternal, "streaming unsupported", nil)
			return
		}

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// Defeat reverse-proxy response buffering so events arrive promptly (ADR-0005).
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		ch, cancel := broker.Subscribe(subscriberIdentity(r))
		defer cancel()

		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case e, open := <-ch:
				if !open {
					return // Broker closed (server shutdown)
				}
				data, err := json.Marshal(e.Data)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
				flusher.Flush()
			}
		}
	}
}

// subscriberIdentity turns the request's authenticated identity into the
// events.Identity the Broker gates on. The accessible-Library set is resolved
// ONCE here, at subscribe time, from the caller's access Scope (the same per-User
// grants the catalog enforces) — seeded onto the request by the requireScope
// middleware on the /events route. A reconnect recomputes it, which is sufficient
// for v1 (no live re-scoping mid-connection).
//
// An Admin resolves to AllLibraries, so its set is left nil — the Broker's
// library-scoped match short-circuits on IsAdmin, so an Admin still receives
// every library-scoped event. A Member's set is exactly their granted Libraries
// (empty when they have no grants → no library-scoped events). The defensive
// branches (no identity, no scope) yield a non-Admin identity with no Libraries,
// matching only broadcast events — fail-closed rather than over-delivering.
func subscriberIdentity(r *http.Request) events.Identity {
	ident, ok := identityFrom(r.Context())
	if !ok {
		return events.Identity{}
	}
	sub := events.Identity{
		UserID:  ident.User.ID,
		IsAdmin: ident.User.Role == "admin",
	}
	if scope, ok := scopeFrom(r.Context()); ok && !scope.AllLibraries {
		set := make(map[string]struct{}, len(scope.LibraryIDs))
		for _, id := range scope.LibraryIDs {
			set[id] = struct{}{}
		}
		sub.Libraries = set
	}
	return sub
}
