package v1

import "context"

// The Event sink Extension point (ADR-0057 decision 6): a Plugin that is TOLD
// when something terminal happened on this server and may only emit outbound HTTP
// in response. One call, request-response, plain data in.
//
// What a sink sees is deliberately narrow. The Broker's SSE snapshots are a
// private shape — they are how a browser draws a progress bar, and they change
// whenever the screen does — so a host-side translator derives a CURATED set of
// events from them and nothing else is ever a contract. A sink event carries ids,
// names and kinds; it never carries a catalog row, because a row is the browse
// API's shape and a sink that wants one can ask for it.

// The curated event types. The set is closed and grows by decision, exactly as the
// Extension points do: a translator that cannot derive an event honestly is a
// reason not to have it, not a reason to widen this list quietly.
const (
	// EventScanCompleted is a Scanner pass over one Library reaching its terminal
	// snapshot: the Library, and that pass's final counts.
	EventScanCompleted = "scan.completed"
	// EventEnrichCompleted is an Enrichment pass over one Library finishing.
	EventEnrichCompleted = "enrich.completed"
	// EventPlaybackStarted / EventPlaybackStopped are a Playback session beginning
	// and ending (a clean stop OR an idle reap — a sink cannot tell them apart,
	// because to the operator's automation they are the same fact).
	EventPlaybackStarted = "playback.started"
	EventPlaybackStopped = "playback.stopped"
	// EventLibraryChanged is a Library's contents changing, the sink-side of the
	// refetch nudge an SSE client gets.
	EventLibraryChanged = "library.changed"
)

// AllEventTypes is every event type the contract defines, in the order an Admin
// reads them. It exists for the same reason AllOutcomes does — so a host-side
// table can be exhaustive by construction — and NOT as the list a settings screen
// offers: what this server can actually derive today is a host fact, narrower than
// this, and it is the host that answers that question.
func AllEventTypes() []string {
	return []string{
		EventScanCompleted,
		EventEnrichCompleted,
		EventPlaybackStarted,
		EventPlaybackStopped,
		EventLibraryChanged,
	}
}

// EventEntity is a reference to one thing on this server: its id, the name an
// operator would recognize it by, and its kind. It is the ONLY way an entity
// crosses this contract — never a catalog row, never a path, never anything the
// Export would refuse to hand another household.
//
// A zero EventEntity means "not relevant to this event" and is omitted from the
// document entirely.
type EventEntity struct {
	ID string `json:"id"`
	// Name is the display name (a Library's name, a Title's name). Present when
	// the host knows it; a sink must not depend on it to identify anything.
	Name string `json:"name,omitempty"`
	// Kind is the entity's kind in this server's vocabulary — a Library's
	// movie/show/music, a Title's movie/episode/track.
	Kind string `json:"kind,omitempty"`
}

// EventActor is WHO a playback event belongs to, and it is the one field in this
// file with a rule rather than a shape. A session relayed over a Link names the
// LINK and never the person watching on the other side (ADR-0054 §3): the sharing
// household's legitimate interests are load and content, and a viewer on another
// household's Server is neither. Exactly one of UserID and LinkID is set.
type EventActor struct {
	// UserID is one of this server's own Users.
	UserID string `json:"userId,omitempty"`
	// LinkID is the Link a relayed session arrived over. Set INSTEAD of UserID.
	LinkID string `json:"linkId,omitempty"`
	// Name is the operator-facing label — the User's username, or the Link's name
	// ("Brandon's server"). Never a name from the other household.
	Name string `json:"name,omitempty"`
}

// EventScan is the terminal counts of a Scanner pass, carried by
// EventScanCompleted. It is a POINTER on the event rather than a value, because
// a pass that found nothing is a real, reportable outcome and "all zero" must not
// read as "no scan block here" — which is exactly what an omit-when-empty value
// would do.
type EventScan struct {
	TitlesFound int `json:"titlesFound"`
	FilesFound  int `json:"filesFound"`
	// Added and Removed are a Targeted scan's delta (ADR-0030); both zero for a
	// full pass, which reports only what it found.
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
	// Scope is the entity label of a Targeted scan, "" for a full one.
	Scope string `json:"scope,omitempty"`
}

// SinkEvent is one curated event, whole. Every field but ID, Type and At is
// optional and omitted when the event has nothing to say there, so a receiving
// script reads one document shape per type rather than a union.
type SinkEvent struct {
	// ID is a stable, unique id for THIS event, generated once by the host and
	// carried unchanged through every delivery attempt. It is what makes the sink
	// call idempotent by construction (ADR-0057 decision 6): a sink that has seen
	// an id may discard it, and an at-least-once outbox added later behind the same
	// call changes nothing on the sink's side.
	ID string `json:"id"`
	// Type is one of the curated event types above.
	Type string `json:"type"`
	// At is when the event happened, RFC 3339 in UTC. A string rather than a
	// time.Time so the wire form is the ONLY form and cannot drift with a Go
	// marshaling detail.
	At string `json:"at"`
	// Library is the Library the event is about, for every type but a playback one.
	Library EventEntity `json:"library,omitzero"`
	// Title is the Title being played, for the playback events.
	Title EventEntity `json:"title,omitzero"`
	// Actor is who the playback session belongs to — a User of this server, or a
	// Link. See EventActor.
	Actor EventActor `json:"actor,omitzero"`
	// Device is the Device a playback session is bound to (ADR-0015).
	Device EventEntity `json:"device,omitzero"`
	// Scan is the terminal counts of a scan.completed event, absent otherwise.
	Scan *EventScan `json:"scan,omitempty"`
}

// EventSink is the Go call surface of the Event sink Extension point. One call,
// taking a context whose deadline the HOST sets and the whole event.
//
// It returns ONLY an error, and deliberately no Outcome. Delivery is best-effort
// and idempotent on the event id; there is no domain judgment for a sink to
// report back, so everything that can go wrong here is transport — which the host
// counts, retries within the deadline if it likes, and then forgets. Nothing about
// a failed delivery is ever shown to a viewer, and nothing about it slows the
// server down (ADR-0057 decision 6: off the publish path, always).
//
// Deliver MUST NOT block past the context. The host's queue is bounded and drops
// its oldest event when it fills, so a sink that ignores its deadline costs the
// operator events, not latency.
type EventSink interface {
	Deliver(ctx context.Context, ev SinkEvent) error
}

// EventSinkFactory builds an Event sink Plugin from the Settings an Admin saved:
// Settings.URL is the target, Settings.Secret the signing key, and
// Settings.Events the subscribed types (which the HOST filters on — a sink is
// never handed an event it did not ask for, so it need not check).
type EventSinkFactory func(Settings) (EventSink, error)

// EventSinkRegistration is what an Event sink Plugin hands the host: what it is,
// and how to build it.
type EventSinkRegistration struct {
	Descriptor Descriptor
	New        EventSinkFactory
}
