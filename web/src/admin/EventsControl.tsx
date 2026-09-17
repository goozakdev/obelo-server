// The one control that picks which events a Plugin is told about (ADR-0057
// decision 6, plugin-system/05). It belongs to the Event sink Extension point and
// nothing else: a Metadata or Subtitle provider is ASKED questions and has nothing
// to subscribe to, so no provider surface renders this.
//
// The offered list comes from the server (`availableEvents`), never from here. The
// server knows which of the curated events its translator can actually derive
// today, and offering an Admin one that nothing produces would leave them unable
// to tell an unimplemented event from a broken receiver. As translations land the
// control grows with no change to this file.
//
// It is a checkbox list rather than a multi-select `<select>` because the set is
// tiny, every option needs a plain-language label, and a native multi-select is
// the worst-understood control on the web.

/** Plain-language labels for the curated event types. A type with no entry here
 * falls back to its id, so a server newer than this bundle still renders a usable
 * (if terse) control instead of an empty one. */
const EVENT_LABELS: Record<string, string> = {
  "scan.completed": "Scan finished",
  "enrich.completed": "Metadata pass finished",
  "playback.started": "Playback started",
  "playback.stopped": "Playback stopped",
  "library.changed": "Library changed",
};

/** What each event means, for the Admin who has not read the ADR. */
const EVENT_HINTS: Record<string, string> = {
  "scan.completed": "A library finished scanning, with the final counts.",
  "enrich.completed": "A metadata pass over a library finished.",
  "playback.started": "Someone started playing a title.",
  "playback.stopped": "A playback session ended.",
  "library.changed": "A library's contents changed.",
};

export function eventLabel(eventType: string): string {
  return EVENT_LABELS[eventType] ?? eventType;
}

export default function EventsControl({
  slug,
  available,
  selected,
  onChange,
  disabled = false,
}: {
  /** The Plugin this control configures — namespaces the test ids and input ids. */
  slug: string;
  /** The event types the SERVER offers, in its own order. */
  available: string[];
  /** The event types currently subscribed to. */
  selected: string[];
  /** Called with the next subscription list whenever a box is toggled. */
  onChange: (events: string[]) => void;
  disabled?: boolean;
}) {
  function toggle(eventType: string, on: boolean) {
    // Emit in the SERVER's order rather than click order, so the list a save
    // sends is stable no matter how the Admin got there.
    onChange(
      available.filter((ev) =>
        ev === eventType ? on : selected.includes(ev),
      ),
    );
  }

  return (
    <fieldset className="field sink-events" data-testid={`sink-events-${slug}`}>
      <legend className="field-label">Events</legend>
      {available.length === 0 ? (
        <p className="field-hint" data-testid={`sink-events-empty-${slug}`}>
          This server does not emit any events yet.
        </p>
      ) : (
        available.map((ev) => (
          <label className="sink-event-option" key={ev}>
            <input
              type="checkbox"
              id={`sink-event-${slug}-${ev}`}
              data-testid={`sink-event-${slug}-${ev}`}
              checked={selected.includes(ev)}
              disabled={disabled}
              onChange={(e) => toggle(ev, e.target.checked)}
            />
            <span className="sink-event-label">{eventLabel(ev)}</span>
            {EVENT_HINTS[ev] && (
              <span className="field-hint sink-event-hint">{EVENT_HINTS[ev]}</span>
            )}
          </label>
        ))
      )}
      <p className="field-hint">
        Nothing selected means nothing is sent — the server does not even build the
        event.
      </p>
    </fieldset>
  );
}
