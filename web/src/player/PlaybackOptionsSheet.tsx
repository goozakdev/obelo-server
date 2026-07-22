import { useEffect, useRef, useState } from "react";
import type { TitleDetail } from "../api/types";
import {
  AUTO_PREFERENCE,
  loadPreferenceForTitle,
  preferenceScopeForTitle,
  savePreference,
  type PlaybackPreference,
} from "./playbackPreference";

// The Playback Options sheet (appletv-web-parity §1/§2) — the pre-play
// configuration surface, opened from Movie / Episode detail. It is built PURELY
// from the `GET /titles/{id}` payload ALREADY IN HAND (`title`): no negotiation, no
// network call, no provider-quota spend on open (the TV's "Model B").
//
// COMMIT MODEL: edits are a local DRAFT. **Play** commits the draft as the saved
// Playback preference (savePreference) AND starts playback (onPlay → the detail's
// existing play(), which Continues at the resume position when in progress, else
// from the start). Backing out (Cancel / Esc / backdrop) DISCARDS the draft — the
// saved preference is untouched.
//
// This slice carries ONE axis — the Edition. Rows: **Auto** (omit `editionId`, let
// the server pick the best direct-play Edition) + one row per `title.editions`. The
// choice persists BY EDITION NAME (playbackPreference) so it ports across a Show's
// Episodes; the resolver maps the name back to an id per-Episode at negotiate time.
// Quality / Audio / Subtitle / AAC / Force-Remux are later sections bolted onto this
// same skeleton.

/** A native <dialog> sheet, opened with showModal() (Esc-to-close, top layer, focus
 * containment, ::backdrop for free — mirroring EditItemDialog). Controlled by the
 * detail screen: `open` drives showModal/close, `onClose` fires on any dismissal. */
export default function PlaybackOptionsSheet({
  title,
  userId,
  open,
  onClose,
  onPlay,
}: {
  /** The already-loaded Title detail — the sole source the sheet is built from. */
  title: TitleDetail;
  /** The Active user (localStorage keying); null for an anon bucket. */
  userId: string | null;
  open: boolean;
  onClose: () => void;
  /** Start playback of the committed configuration — the detail's existing play()
   * (Continue at the resume position when in progress, else from the start). */
  onPlay: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  // The DRAFT Edition name (null = Auto), seeded from the saved preference each time
  // the sheet opens so it always reflects the committed choice, and discarded on a
  // back-out (we only persist on Play).
  const [draft, setDraft] = useState<PlaybackPreference>(AUTO_PREFERENCE);

  // Drive the native dialog imperatively; keep React's `open` in sync via its own
  // close event (Esc / backdrop). Re-seed the draft from the store on each open.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      setDraft(loadPreferenceForTitle(window.localStorage, userId, title));
      dialog.showModal();
    }
    if (!open && dialog.open) dialog.close();
  }, [open, userId, title]);

  const scope = preferenceScopeForTitle(title);
  const resuming = !title.watched && title.resumePositionMs > 0;

  // Commit the draft as the saved preference, then start playback. Persisting only
  // happens here (never on a row click), so backing out discards the draft.
  function commitAndPlay() {
    if (scope) savePreference(window.localStorage, userId, scope, draft);
    onClose();
    onPlay();
  }

  return (
    <dialog
      ref={dialogRef}
      className="edit-item-dialog playback-options-sheet"
      data-testid="playback-options-sheet"
      // Native close (Esc / dialog.close()) → keep the parent's `open` in sync.
      onClose={onClose}
      // Clicking the ::backdrop (the dialog element itself, outside the panel)
      // dismisses without committing.
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="edit-item-panel playback-options-panel">
        <div className="edit-item-header">
          <h2 className="edit-item-title">Playback options</h2>
          <button
            className="nav-link edit-item-close"
            type="button"
            data-testid="playback-options-close"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </div>

        <div className="playback-options-body">
          <EditionSection
            editions={title.editions}
            selected={draft.editionName}
            onSelect={(editionName) => setDraft((d) => ({ ...d, editionName }))}
          />
        </div>

        <div className="playback-options-footer">
          <button
            className="auth-submit play-button"
            type="button"
            data-testid="playback-options-play"
            onClick={commitAndPlay}
          >
            {resuming ? "Continue" : "Play"}
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="playback-options-cancel"
            onClick={onClose}
          >
            Cancel
          </button>
        </div>
      </div>
    </dialog>
  );
}

// The Edition section: an **Auto** row (omit editionId) + one row per Edition. The
// active row is marked (radio semantics — aria-checked + data-active). Selecting a
// row only updates the DRAFT; nothing persists until Play. Persisted by NAME, so the
// row's stored value is `edition.name` (Auto → null).
function EditionSection({
  editions,
  selected,
  onSelect,
}: {
  editions: TitleDetail["editions"];
  /** The draft Edition name, or null for Auto. */
  selected: string | null;
  onSelect: (editionName: string | null) => void;
}) {
  return (
    <section className="playback-options-section" data-testid="edition-section">
      <h3 className="section-title playback-options-section-title">Edition</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Edition">
        <OptionRow
          label="Auto"
          hint="Best available — the server picks the direct-play edition."
          active={selected === null}
          testId="edition-option-auto"
          onSelect={() => onSelect(null)}
        />
        {editions.map((ed) => (
          <OptionRow
            key={ed.id}
            label={ed.name || "Default"}
            active={selected === ed.name}
            testId="edition-option"
            dataName={ed.name}
            onSelect={() => onSelect(ed.name)}
          />
        ))}
      </ul>
    </section>
  );
}

// One selectable option row (a radio in a list). The active row shows a mark and
// carries aria-checked + data-active for tests / styling.
function OptionRow({
  label,
  hint,
  active,
  testId,
  dataName,
  onSelect,
}: {
  label: string;
  hint?: string;
  active: boolean;
  testId: string;
  dataName?: string;
  onSelect: () => void;
}) {
  return (
    <li className="playback-options-item">
      <button
        type="button"
        role="radio"
        aria-checked={active}
        className={`playback-options-row${active ? " is-active" : ""}`}
        data-testid={testId}
        data-edition-name={dataName}
        data-active={active ? "1" : undefined}
        onClick={onSelect}
      >
        <span className="playback-options-mark" aria-hidden="true">
          {active ? "●" : "○"}
        </span>
        <span className="playback-options-row-text">
          <span className="playback-options-row-label">{label}</span>
          {hint && <span className="playback-options-row-hint">{hint}</span>}
        </span>
      </button>
    </li>
  );
}
