// Package events is the server→client realtime spine (ADR-0016): a single
// in-process Broker that fans out typed events to every connected subscriber of
// GET /api/v1/events. It is deliberately tiny:
//
//   - publishing is NON-BLOCKING — a slow subscriber drops events rather than
//     stalling the publisher (an Enrichment pass must never wait on a browser);
//   - every event maps to a pollable resource, so SSE is an optimization, not
//     the only path to state (ADR-0016).
//
// The Broker is transport-agnostic: the api package's SSE handler subscribes and
// serializes events onto the wire; producers (the enrich worker / handler) only
// Publish. The first realtime event is enrichProgress
// (external-metadata-enrichment); scan/session events slot in later behind the
// same Broker.
package events

import "sync"

// Event type names (the SSE `event:` line). Clients branch on these, not on a
// version string.
const (
	// TypeEnrichProgress reports an Enrichment pass advancing over a Library. It
	// carries no per-user data, so it is broadcast to every subscriber.
	TypeEnrichProgress = "enrichProgress"
	// TypeLibraryUpdated tells connected clients a Library's contents changed (a
	// scan or Enrichment pass altered the catalog) so they should refetch it. It
	// is a "go refetch" nudge — /libraries and /libraries/{id}/titles remain the
	// source of truth. Scoped to the affected Library (AudienceLibrary).
	TypeLibraryUpdated = "libraryUpdated"
	// TypeScanProgress reports a Scanner pass advancing over a Library: counts of
	// Titles/Files found so far, with Complete=true on the terminal event carrying
	// the final counts. It is scoped to the scanned Library (AudienceLibrary), so
	// only subscribers who can see it — and any Admin — receive it.
	TypeScanProgress = "scanProgress"
	// TypeSessionStarted / TypeNowPlaying / TypeSessionEnded are the Admin-only
	// (AudienceAdmin) Playback session lifecycle events powering the Admin's live
	// "now playing" view: a session starting, reporting progress (carrying its
	// current position), and ending (clean stop OR idle-reap). A Member's stream
	// never receives any of them; /sessions/{id}/* resources remain the pollable
	// fallback. They carry the session id so a client correlates started →
	// nowPlaying → ended and updates a row in place.
	TypeSessionStarted = "sessionStarted"
	TypeNowPlaying     = "nowPlaying"
	TypeSessionEnded   = "sessionEnded"
)

// AudienceKind is the closed set of "who may receive this Event" forms. It is a
// small enum (not an open predicate function) so an audience survives the
// producer→transport boundary cleanly, serializes in tests, and can't smuggle a
// closure across packages. Future per-User events would add ONE kind here (e.g.
// audienceUser) plus a case in Audience.matches — a localized change, not a
// redesign.
type AudienceKind int

const (
	// AudienceBroadcast reaches every subscriber. It is the zero value, so an
	// Event built without an explicit audience broadcasts (back-compatible with
	// the original fan-to-all behavior). enrichProgress uses this.
	AudienceBroadcast AudienceKind = iota
	// AudienceAdmin reaches only subscribers whose identity is Admin.
	AudienceAdmin
	// AudienceLibrary reaches only subscribers whose accessible-Library set
	// contains LibraryID (and any Admin, who sees all Libraries). Used by
	// library-scoped events like libraryUpdated.
	AudienceLibrary
)

// Audience describes who may receive an Event: a closed Kind plus an optional
// Library id (set only when Kind is AudienceLibrary). Publish gates every event
// against each subscriber's Identity using matches, BEFORE enqueuing, so a
// subscriber's bounded buffer only ever holds events it is allowed to see.
type Audience struct {
	Kind AudienceKind
	// LibraryID is the scoped Library for an AudienceLibrary audience; ignored
	// for the other kinds.
	LibraryID string
}

// matches reports whether a subscriber with the given Identity may receive an
// event carrying this Audience. This pure predicate is the heart of the gating
// mechanism (unit-tested directly in broker_test.go):
//
//   - AudienceBroadcast  → everyone;
//   - AudienceAdmin      → only Admins;
//   - AudienceLibrary    → an Admin (sees all Libraries) OR a subscriber whose
//     accessible-Library set contains LibraryID.
func (a Audience) matches(id Identity) bool {
	switch a.Kind {
	case AudienceBroadcast:
		return true
	case AudienceAdmin:
		return id.IsAdmin
	case AudienceLibrary:
		if id.IsAdmin {
			return true
		}
		_, ok := id.Libraries[a.LibraryID]
		return ok
	default:
		// An unknown kind reaches no one (fail closed): a misconfigured producer
		// must never accidentally leak an event to subscribers.
		return false
	}
}

// Identity is the authenticated subscriber's access facts the Broker gates on,
// captured ONCE at Subscribe time. UserID is carried for future per-User events;
// IsAdmin gates admin-only events; Libraries is the accessible-Library set the
// /events handler resolves from the subscriber's per-User library grants (the
// same grants the catalog enforces) — empty for an Admin, whose IsAdmin
// short-circuits the library-scoped match to "sees all". A reconnect recomputes
// it — sufficient for v1 (no live re-scoping mid-stream).
type Identity struct {
	UserID    string
	IsAdmin   bool
	Libraries map[string]struct{}
}

// Event is one server→client message. Data is JSON-marshaled into the SSE
// `data:` line by the transport. Audience gates delivery (zero value =
// AudienceBroadcast).
type Event struct {
	Type     string
	Data     any
	Audience Audience
}

// EnrichProgress is the payload of a TypeEnrichProgress event: a snapshot of an
// in-flight (or just-finished) pass over one Library. Complete is true on the
// terminal event so a client can hide its "enriching" indicator and do a final
// refetch.
type EnrichProgress struct {
	LibraryID string `json:"libraryId"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Disabled  int    `json:"disabled"`
	Complete  bool   `json:"complete"`
}

// LibraryUpdated is the payload of a TypeLibraryUpdated event: just the affected
// LibraryID. It is a refetch nudge, not a diff — the client re-reads /libraries
// and /libraries/{id}/titles to pick up whatever changed.
type LibraryUpdated struct {
	LibraryID string `json:"libraryId"`
}

// ScanProgress is the payload of a TypeScanProgress event: a snapshot of an
// in-flight (or just-finished) scan of one Library. It reuses the enrichProgress
// shape conventions so a client handles both indicators with one code path. The
// counts are "found so far" — the Scanner does not pre-count, so there is no
// total mid-walk. Complete is true on the terminal event, which carries the
// authoritative final TitlesFound / FilesFound, so a client hides its "scanning…"
// indicator and does a final refetch.
type ScanProgress struct {
	LibraryID   string `json:"libraryId"`
	TitlesFound int    `json:"titlesFound"`
	FilesFound  int    `json:"filesFound"`
	Complete    bool   `json:"complete"`
}

// SessionEvent is the payload of the Admin-only session lifecycle events
// (sessionStarted / nowPlaying / sessionEnded). It carries enough identity to
// correlate started → nowPlaying → ended (SessionID) and render a live row
// (UserID, TitleID) without an extra fetch. PositionMs is meaningful only for
// nowPlaying — it is omitempty so the started/ended events don't carry a stale
// zero position on the wire.
type SessionEvent struct {
	SessionID  string `json:"sessionId"`
	UserID     string `json:"userId"`
	TitleID    string `json:"titleId"`
	PositionMs int64  `json:"positionMs,omitempty"`
}

// Broker fans out events to subscribers. Create one with NewBroker; the zero
// value is not usable.
type Broker struct {
	mu sync.Mutex
	// subs maps each subscriber's channel to the Identity captured at Subscribe
	// time. Publish gates an event against that Identity before enqueuing.
	subs   map[chan Event]Identity
	closed bool
}

// NewBroker returns an empty, ready Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]Identity)}
}

// Subscribe registers a new subscriber under its authenticated Identity and
// returns its event channel plus an idempotent unsubscribe func. The Identity is
// captured here and used by Publish to gate every event's audience (so the
// channel never even holds an event this subscriber may not see). The channel is
// buffered so a brief consumer stall doesn't immediately drop events; cancel
// removes the subscriber and closes the channel. Subscribing after Close yields
// an already-closed channel.
func (b *Broker) Subscribe(id Identity) (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = id
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish delivers e to every subscriber whose Identity its audience matches,
// non-blocking: a subscriber whose buffer is full drops this event. Gating
// happens HERE, before the enqueue, so a subscriber's bounded buffer only ever
// holds events it is allowed to see (an admin-only flood can't fill a Member's
// buffer and force drops of events the Member is entitled to). It never blocks
// the caller. The send happens under the lock so it can't race a concurrent
// Subscribe/cancel closing the same channel.
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, id := range b.subs {
		if !e.Audience.matches(id) {
			continue
		}
		select {
		case ch <- e:
		default:
		}
	}
}

// PublishEnrichProgress is the typed convenience over Publish for the enrich
// producers. It carries no per-user data, so it stays AudienceBroadcast.
func (b *Broker) PublishEnrichProgress(p EnrichProgress) {
	b.Publish(Event{Type: TypeEnrichProgress, Data: p})
}

// PublishLibraryUpdated announces that a Library's contents changed, scoped to
// that Library (AudienceLibrary) so only subscribers who can see it — and any
// Admin — receive the refetch nudge. Mirrors PublishEnrichProgress: producers
// publish through this typed helper so they can't set the wrong audience.
func (b *Broker) PublishLibraryUpdated(libraryID string) {
	b.Publish(Event{
		Type:     TypeLibraryUpdated,
		Data:     LibraryUpdated{LibraryID: libraryID},
		Audience: Audience{Kind: AudienceLibrary, LibraryID: libraryID},
	})
}

// PublishScanProgress reports a scan advancing over a Library, scoped to that
// Library (AudienceLibrary) so only subscribers who can see it — and any Admin —
// receive the progress. Mirrors PublishLibraryUpdated/PublishEnrichProgress:
// producers publish through this typed helper so they can't set the wrong
// audience. The terminal snapshot (p.Complete) carries the final counts.
func (b *Broker) PublishScanProgress(p ScanProgress) {
	b.Publish(Event{
		Type:     TypeScanProgress,
		Data:     p,
		Audience: Audience{Kind: AudienceLibrary, LibraryID: p.LibraryID},
	})
}

// PublishSessionEvent fans out a Playback session lifecycle event as AudienceAdmin
// — admin-only is non-negotiable, so a Member's stream never receives it. eventType
// must be one of TypeSessionStarted / TypeNowPlaying / TypeSessionEnded (the single
// helper takes the type so the three lifecycle transitions share one audience
// decision; producers can't set the wrong audience). PositionMs on p is meaningful
// only for nowPlaying. Mirrors PublishScanProgress/PublishLibraryUpdated.
func (b *Broker) PublishSessionEvent(eventType string, p SessionEvent) {
	b.Publish(Event{
		Type:     eventType,
		Data:     p,
		Audience: Audience{Kind: AudienceAdmin},
	})
}

// Close drops all subscribers (closing their channels, so their SSE handlers
// return) and marks the Broker closed. Idempotent; called on app shutdown.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}
