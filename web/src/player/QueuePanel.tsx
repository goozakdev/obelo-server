import { useState, type DragEvent as ReactDragEvent } from "react";
import Poster from "../browse/Poster";
import type { QueueStore } from "./queue/useQueue";
import type { QueueEntry } from "./queue/model";

// The Queue panel (queue/03, redesigned): the Queue made inspectable and editable.
// Extracted from the retired PlayerScreen (now-playing-bar/01) so the persistent Now
// Playing bar can open it in a drawer. It drives the store's reorder / removeEntry /
// clear; because the bar's player is KEYED by the current entry's id and the model
// preserves which entry is current BY id, reordering or removing an UPCOMING entry
// never re-keys the player — the currently-playing session is untouched. Removing the
// current entry advances the pointer (re-keys the player); clearing or removing the
// last entry empties the Queue → the bar dismisses.
//
// The panel is split into two regions: a prominent "Now Playing" card for the current
// entry and a scrollable "Up Next" list. Up-next entries are reordered by DRAG AND
// DROP (native HTML5 drag): the dragged id is tracked in local state (not the
// DataTransfer, so it works headlessly and needs no serialization) and a drop builds
// the FULL permutation handed to `queue.reorder` — which preserves the current entry
// by id, so what's playing is never disturbed; only "what plays next" changes.

/** A friendly noun for a Title's media kind, for the queue panel's entry rows. */
function kindLabel(kind: string): string {
  switch (kind) {
    case "movie":
      return "Movie";
    case "episode":
      return "Episode";
    case "track":
      return "Track";
    case "show":
      return "Show";
    default:
      return kind;
  }
}

/** Trash-can glyph (per the redesign spec) for the per-entry remove action, tinted
 * by `currentColor` and sized to the font via CSS. Replaces the old "Remove" link. */
function TrashIcon() {
  return (
    <svg
      className="queue-entry-remove-icon"
      viewBox="0 0 256 256"
      aria-hidden="true"
      focusable="false"
    >
      <g
        fill="currentColor"
        transform="translate(1.4065934065934016 1.4065934065934016) scale(2.81 2.81)"
      >
        <path d="M 67.692 90 H 22.308 c -3.042 0 -5.518 -2.476 -5.518 -5.518 v -61 c 0 -1.104 0.896 -2 2 -2 h 52.42 c 1.104 0 2 0.896 2 2 v 61 C 73.21 87.524 70.734 90 67.692 90 z M 20.79 25.482 v 59 c 0 0.837 0.681 1.518 1.518 1.518 h 45.385 c 0.837 0 1.518 -0.681 1.518 -1.518 v -59 H 20.79 z" />
        <path d="M 73.196 25.482 H 16.804 c -3.042 0 -5.518 -2.475 -5.518 -5.518 v -4.335 c 0 -3.042 2.475 -5.518 5.518 -5.518 h 56.393 c 3.042 0 5.518 2.475 5.518 5.518 v 4.335 C 78.714 23.007 76.238 25.482 73.196 25.482 z M 16.804 14.112 c -0.837 0 -1.518 0.681 -1.518 1.518 v 4.335 c 0 0.837 0.681 1.518 1.518 1.518 h 56.393 c 0.837 0 1.518 -0.681 1.518 -1.518 v -4.335 c 0 -0.837 -0.681 -1.518 -1.518 -1.518 H 16.804 z" />
        <path d="M 57.197 14.112 H 32.803 c -1.104 0 -2 -0.896 -2 -2 V 5.518 C 30.803 2.476 33.278 0 36.321 0 h 17.358 c 3.043 0 5.519 2.476 5.519 5.518 v 6.594 C 59.197 13.216 58.302 14.112 57.197 14.112 z M 34.803 10.112 h 20.395 V 5.518 C 55.197 4.681 54.516 4 53.679 4 H 36.321 c -0.837 0 -1.518 0.681 -1.518 1.518 V 10.112 z" />
        <path d="M 45 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 47 77.729 46.104 78.624 45 78.624 z" />
        <path d="M 58.222 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 60.222 77.729 59.326 78.624 58.222 78.624 z" />
        <path d="M 31.779 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 33.779 77.729 32.883 78.624 31.779 78.624 z" />
      </g>
    </svg>
  );
}

/** A six-dot grip that marks an up-next row as draggable, tinted by `currentColor`. */
function DragHandleIcon() {
  return (
    <svg
      className="queue-entry-grip-icon"
      viewBox="0 0 16 16"
      aria-hidden="true"
      focusable="false"
    >
      <g fill="currentColor">
        <circle cx="5.5" cy="3" r="1.4" />
        <circle cx="10.5" cy="3" r="1.4" />
        <circle cx="5.5" cy="8" r="1.4" />
        <circle cx="10.5" cy="8" r="1.4" />
        <circle cx="5.5" cy="13" r="1.4" />
        <circle cx="10.5" cy="13" r="1.4" />
      </g>
    </svg>
  );
}

/** An "×" close glyph for the panel header. */
function CloseIcon() {
  return (
    <svg className="queue-panel-close-icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
      <path
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        d="M6 6l12 12M18 6L6 18"
      />
    </svg>
  );
}

/** One rendered Queue entry row (shared by the Now Playing card and the Up Next
 * list). Up-next rows get a drag handle and become drag sources/targets; the current
 * row is fixed at the top and only offers remove. */
function EntryRow({
  entry,
  isCurrent,
  onRemove,
  drag,
}: {
  entry: QueueEntry;
  isCurrent: boolean;
  onRemove: () => void;
  drag?: {
    isDragging: boolean;
    isDragOver: boolean;
    onDragStart: (e: ReactDragEvent) => void;
    onDragEnd: () => void;
    onDragOver: (e: ReactDragEvent) => void;
    onDragLeave: () => void;
    onDrop: (e: ReactDragEvent) => void;
  };
}) {
  const className =
    `queue-entry${isCurrent ? " queue-entry-current" : ""}` +
    (drag?.isDragging ? " queue-entry-dragging" : "") +
    (drag?.isDragOver ? " queue-entry-drag-over" : "");

  return (
    <li
      className={className}
      data-testid="queue-entry"
      data-entry-id={entry.entryId}
      data-title-id={entry.title.id}
      aria-current={isCurrent ? "true" : undefined}
      draggable={drag ? true : undefined}
      onDragStart={drag?.onDragStart}
      onDragEnd={drag?.onDragEnd}
      onDragOver={drag?.onDragOver}
      onDragLeave={drag?.onDragLeave}
      onDrop={drag?.onDrop}
    >
      {drag && (
        <span className="queue-entry-grip" aria-hidden="true">
          <DragHandleIcon />
        </span>
      )}
      <div className="queue-entry-poster">
        <Poster titleId={entry.title.id} title={entry.title.title} />
      </div>
      <div className="queue-entry-info">
        <span className="queue-entry-title" data-testid="queue-entry-title">
          {entry.title.title}
        </span>
        <span className="queue-entry-kind">{kindLabel(entry.title.kind)}</span>
      </div>
      <button
        className="nav-link queue-entry-remove"
        type="button"
        data-testid="queue-remove"
        aria-label={`Remove ${entry.title.title}`}
        onClick={onRemove}
      >
        <TrashIcon />
      </button>
    </li>
  );
}

/** The in-player queue panel (PRD stories 18, 34–35), redesigned. The current entry
 * sits in a prominent "Now Playing" card; the rest render in a scrollable "Up Next"
 * list, reordered by drag and drop. Reorder is constrained to the up-next region (an
 * up-next entry never moves above the now-playing one), and the store's `reorder`
 * preserves the current entry by id — so what's playing is never disturbed. */
export default function QueuePanel({
  queue,
  onClose,
}: {
  queue: QueueStore;
  onClose?: () => void;
}) {
  const { entries, index } = queue;
  const current = index >= 0 ? entries[index] : null;
  const upNext = index >= 0 ? entries.slice(index + 1) : entries;

  // The entryId currently being dragged, and the up-next row it's hovering over (the
  // insertion point). Kept in React state rather than the DataTransfer so the reorder
  // is driven purely by component state (works headlessly; no serialization).
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dragOverId, setDragOverId] = useState<string | null>(null);

  function resetDrag() {
    setDraggingId(null);
    setDragOverId(null);
  }

  // Move the dragged up-next entry to just BEFORE `targetId`, sending the full
  // permutation to the store (which preserves the current entry by id).
  function reorderBefore(targetId: string) {
    if (!draggingId || draggingId === targetId) {
      resetDrag();
      return;
    }
    const ids = entries.map((e) => e.entryId);
    const from = ids.indexOf(draggingId);
    if (from < 0) {
      resetDrag();
      return;
    }
    ids.splice(from, 1);
    const to = ids.indexOf(targetId);
    if (to < 0) {
      resetDrag();
      return;
    }
    ids.splice(to, 0, draggingId);
    queue.reorder(ids);
    resetDrag();
  }

  // Drop into the empty space at the end of the list → move the dragged entry last.
  function reorderToEnd() {
    if (!draggingId) {
      resetDrag();
      return;
    }
    const ids = entries.map((e) => e.entryId);
    const from = ids.indexOf(draggingId);
    if (from < 0) {
      resetDrag();
      return;
    }
    ids.splice(from, 1);
    ids.push(draggingId);
    queue.reorder(ids);
    resetDrag();
  }

  function dragProps(entryId: string) {
    return {
      isDragging: draggingId === entryId,
      isDragOver: dragOverId === entryId && draggingId !== null && draggingId !== entryId,
      onDragStart: (e: ReactDragEvent) => {
        setDraggingId(entryId);
        if (e.dataTransfer) {
          e.dataTransfer.effectAllowed = "move";
          // Some browsers require data to be set for a drag to initiate.
          try {
            e.dataTransfer.setData("text/plain", entryId);
          } catch {
            /* jsdom / restricted DataTransfer — state alone drives the reorder */
          }
        }
      },
      onDragEnd: resetDrag,
      onDragOver: (e: ReactDragEvent) => {
        if (!draggingId) return;
        e.preventDefault();
        if (e.dataTransfer) e.dataTransfer.dropEffect = "move";
        if (dragOverId !== entryId) setDragOverId(entryId);
      },
      onDragLeave: () => {
        setDragOverId((cur) => (cur === entryId ? null : cur));
      },
      onDrop: (e: ReactDragEvent) => {
        e.preventDefault();
        reorderBefore(entryId);
      },
    };
  }

  return (
    <section className="queue-panel" data-testid="queue-panel">
      <header className="queue-panel-header">
        <h2 className="queue-panel-heading">Queue</h2>
        <div className="queue-panel-header-actions">
          <button
            className="nav-link"
            type="button"
            data-testid="queue-clear"
            onClick={queue.clear}
          >
            Clear queue
          </button>
          {onClose && (
            <button
              className="nav-link queue-panel-close"
              type="button"
              data-testid="queue-panel-close"
              aria-label="Close queue"
              onClick={onClose}
            >
              <CloseIcon />
            </button>
          )}
        </div>
      </header>

      <div className="queue-panel-body">
        {current && (
          <section className="queue-section queue-section-now" aria-label="Now Playing">
            <h3 className="queue-section-title">Now Playing</h3>
            <ul className="queue-section-list">
              <EntryRow
                entry={current}
                isCurrent
                onRemove={() => queue.removeEntry(current.entryId)}
              />
            </ul>
          </section>
        )}

        <section className="queue-section queue-section-upnext" aria-label="Up Next">
          <h3 className="queue-section-title">Up Next</h3>
          {upNext.length === 0 ? (
            <p className="queue-empty" data-testid="queue-upnext-empty">
              Nothing up next.
            </p>
          ) : (
            <ol
              className="queue-section-list queue-upnext-list"
              data-testid="queue-panel-list"
              onDragOver={(e) => {
                if (draggingId) e.preventDefault();
              }}
              onDrop={(e) => {
                // A drop that didn't land on a row (the gap below the list) appends.
                if (e.currentTarget === e.target) {
                  e.preventDefault();
                  reorderToEnd();
                }
              }}
            >
              {upNext.map((entry) => (
                <EntryRow
                  key={entry.entryId}
                  entry={entry}
                  isCurrent={false}
                  onRemove={() => queue.removeEntry(entry.entryId)}
                  drag={dragProps(entry.entryId)}
                />
              ))}
            </ol>
          )}
        </section>
      </div>
    </section>
  );
}
