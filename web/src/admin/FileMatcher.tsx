import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ApiError } from "../api/client";
import { AlsoPlaceIcon, UnlinkIcon } from "../browse/ActionIcons";
import type {
  MatcherApplyInput,
  MatcherDocument,
  MatcherGroup,
  MatcherSlot,
  MatcherSlotRecord,
  SlotCollisionDetails,
  SlotPosition,
} from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import {
  arrangeParts,
  arrangementFromFiles,
  changedPaths,
  displaceAt,
  filesAtSlot,
  homeGroupOf,
  ignoreFile,
  isShared,
  movePartBefore,
  ordinalAt,
  placeFile,
  reorderPart,
  restoreFile,
  toApplyFiles,
  unassignFile,
  UNSORTED_GROUP,
  type Arrangement,
  type ArrangedFile,
  type PlaceMode,
} from "./matcherArrangement";
import {
  basename,
  compareTitles,
  comparePosition,
  markFilename,
  splitContainerPrefix,
} from "./matcherCompare";
import {
  NO_PINS,
  changedSlots,
  clearRecord,
  fillRun,
  pairRun,
  recordAt,
  toApplySlots,
  type PinDrafts,
} from "./matcherPins";
import { useDragPlacement, type DropTarget } from "./matcherDrag";
import type { MatcherLabels } from "./matcherLabels";

// The file matcher: one container's Files laid against its Slots, rearranged by
// hand, committed in one go (ADR-0044).
//
// KIND-NEUTRAL BY CONSTRUCTION. Everything this component names is something the
// API also names — groups, slots, files, ordinals, container id. The words an
// Admin reads come from `labels`, and the header chrome comes from `header`, so
// the Album matcher is `albumMatcherLabels` plus a route, not a second screen
// (PRD non-goals: "only the kind-neutral storage, API shape and component that
// make it an adapter later").
//
// Three things it is careful about, all of them learned from the issue:
//
//   1. NOTHING COMMITS UNTIL APPLY. The arrangement is local state; Revert
//      restores the snapshot taken at open; Cancel writes nothing; leaving with
//      unsaved work warns. An Admin has to be able to experiment.
//
//   2. THE COMPARISON IS THE POINT, AND NOISE DESTROYS IT. Numbers are compared as
//      numbers and highlighted on BOTH sides; titles only after normalization, and
//      then character by character (matcherCompare.ts). A correctly-matched dotted
//      filename lights up nothing.
//
//   3. A DROP ONTO AN OCCUPIED SLOT IS A QUESTION, NOT A GUESS. It resolves to a
//      displacement or a merge, chosen by the Admin, and the displaced File's new
//      state is carried in the Apply payload — the arrangement must never leave two
//      distinct Titles resolving onto one Slot.
//
// A Slot's RECORD is edited here too, and it is a SEPARATE decision from where a
// File sits (ADR-0044). Repointing borrows another series' record — the five files
// this whole screen was built for are season 1 of a re-numbered continuation — and
// the borrowed numbering must never reach the code a Slot prints, or those five
// would land on top of the Show's real Season 1. So a borrowed record supplies
// only WORDS, shown against the Slot's own local code, with its own position
// stated as provenance. Like everything else here it is local until Apply, where
// it rides in the same payload and the same transaction.

type ApplyState =
  | { kind: "idle" }
  | { kind: "applying" }
  | { kind: "error"; message: string }
  | { kind: "scan-running"; message: string }
  | { kind: "collision"; message: string; details: SlotCollisionDetails }
  | {
      kind: "applied";
      rearranged: number;
      displaced: string[];
      deferred: string[];
    };

interface Selection {
  path: string;
  mode: PlaceMode;
}

interface PendingDrop {
  path: string;
  target: SlotPosition;
  mode: PlaceMode;
  occupants: string[];
}

/** A request to repoint one or more Slots' records, handed to the kind adapter.
 *
 * The adapter owns the picker because picking a record is the one genuinely
 * kind-specific act on this screen: TV searches series on TMDB, Music would search
 * releases on MusicBrainz. This component stays ignorant of both and only knows
 * what to do with the answer. */
export interface RepointRequest {
  /** The Slots to fill, in their own ascending order. One for a single Slot; a
   * whole group's FILLED Slots for the bulk gesture. */
  targets: SlotPosition[];
  /** The group they live in. */
  group: number;
  /** The record the first target shows now, so the picker can open on it. */
  current: MatcherSlotRecord | null;
  /** Hand back the chosen run. `records` pair against `targets` IN ORDER, and
   * carry their own positions in `externalId`'s numbering — which stays there. */
  onPicked: (externalId: string, records: MatcherSlot[]) => void;
  onCancel: () => void;
}

export default function FileMatcher({
  matcher,
  labels,
  header,
  loadGroup,
  apply,
  repointRecord,
  onClose,
  onApplied,
}: {
  /** The container's whole working set, as the server last read it. */
  matcher: MatcherDocument;
  /** Every word the Admin reads. The kind-neutral seam. */
  labels: MatcherLabels;
  /** Kind-specific chrome above the tray — poster, title, the "wrong series"
   * escape hatch. Rendered as given; this component never looks inside it. */
  header?: ReactNode;
  /** Fetch ONE group's provider records (`?group=N`). Called on expand, because
   * the first load is deliberately cheap: opening a ten-season Show costs one
   * round-trip, not ten, against a rate-limited API. */
  loadGroup?: (group: number) => Promise<MatcherGroup | null>;
  /** Commit the arrangement AND the records. Answers with the re-read document. */
  apply: (input: MatcherApplyInput) => Promise<MatcherDocument>;
  /** Render the kind-specific record picker. Omitted (the degraded case, and the
   * Music adapter until it has one) simply means no Slot offers to be repointed. */
  repointRecord?: (request: RepointRequest) => ReactNode;
  /** Leave the screen. */
  onClose: () => void;
  /** Hand the re-read document back to the owner after a successful Apply. */
  onApplied?: (next: MatcherDocument) => void;
}) {
  const [base, setBase] = useState<Arrangement>(() => arrangementFromFiles(matcher.files));
  const [arr, setArr] = useState<Arrangement>(base);
  const [slotsByGroup, setSlotsByGroup] = useState<Map<number, MatcherSlot[]>>(() =>
    seedSlots(matcher.groups),
  );
  const [groupNotes, setGroupNotes] = useState<Map<number, GroupNote>>(() =>
    seedNotes(matcher.groups),
  );
  const [expanded, setExpanded] = useState<Set<number>>(() => new Set<number>());
  const [extraSlots, setExtraSlots] = useState<Map<number, number[]>>(() => new Map());
  const [extraGroups, setExtraGroups] = useState<number[]>([]);
  const [selection, setSelection] = useState<Selection | null>(null);
  const [pending, setPending] = useState<PendingDrop | null>(null);
  const [ignoredOpen, setIgnoredOpen] = useState(false);
  const [applyState, setApplyState] = useState<ApplyState>({ kind: "idle" });
  const [leaving, setLeaving] = useState(false);
  // The records the Admin has repointed this session, and the picker they have
  // open. Both are local until Apply, exactly like the arrangement.
  const [pins, setPins] = useState<PinDrafts>(NO_PINS);
  const [repointing, setRepointing] = useState<{ targets: SlotPosition[]; group: number } | null>(
    null,
  );
  const [repointNote, setRepointNote] = useState<string | null>(null);

  // A new document (the first load, or the re-read one Apply hands back) replaces
  // the arrangement AND the snapshot Revert restores — the Admin's changes are in
  // it now, so the screen's "unchanged" baseline has moved.
  useEffect(() => {
    const next = arrangementFromFiles(matcher.files);
    setBase(next);
    setArr(next);
    setSlotsByGroup(seedSlots(matcher.groups));
    setGroupNotes(seedNotes(matcher.groups));
    setSelection(null);
    setPending(null);
    setPins(NO_PINS);
    setRepointing(null);
    setRepointNote(null);
  }, [matcher]);

  // The record a Slot has according to the SERVER — what a draft is compared
  // against, and what a cleared pin falls back to.
  const storedRecordAt = useCallback(
    (position: SlotPosition): MatcherSlotRecord | undefined =>
      (slotsByGroup.get(position.group) ?? []).find((s) => s.slot === position.slot)?.record,
    [slotsByGroup],
  );

  const changes = useMemo(() => changedPaths(base, arr), [base, arr]);
  const repointed = useMemo(() => changedSlots(pins), [pins]);
  // A repointed record is a change like any other: it counts towards Apply, it is
  // undone by Revert, and it makes leaving warn.
  const changeCount = changes.length + repointed.length;
  const dirty = changeCount > 0;

  // Leaving with unsaved work warns. The in-app paths (Cancel, the header's own
  // links) go through `requestClose`; this covers the browser's own navigation,
  // which React cannot intercept.
  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  // --- Placing -------------------------------------------------------------

  const place = useCallback(
    (path: string, target: SlotPosition, mode: PlaceMode) => {
      const occupants = filesAtSlot(arr, target).filter((f) => f.path !== path);
      if (occupants.length > 0) {
        // The drop that must not be offerable: two distinct Files on one Slot is
        // either a displacement or a merge, and only the Admin knows which. Ask.
        setPending({ path, target, mode, occupants: occupants.map((f) => f.path) });
        return;
      }
      setArr(placeFile(arr, path, target, mode));
    },
    [arr],
  );

  /** The ONE handler both gestures land in — a pointer drop and a click-then-click
   * are the same event by the time they get here, which is what keeps the click
   * path (and therefore the keyboard path) from quietly falling behind. */
  const handleDrop = useCallback(
    (path: string, target: DropTarget, mode: PlaceMode = "move") => {
      setSelection(null);
      switch (target.kind) {
        case "slot":
          place(path, { group: target.group, slot: target.slot }, mode);
          break;
        case "part": {
          if (target.path === path) break;
          const position = { group: target.group, slot: target.slot };
          // Dropping a part in FRONT of another part of the same Slot reorders
          // them; dropping a foreign File there is just a drop on that Slot.
          if (filesAtSlot(arr, position).some((f) => f.path === path)) {
            setArr(movePartBefore(arr, position, path, target.path));
          } else {
            place(path, position, mode);
          }
          break;
        }
        case "unassigned":
          setArr(unassignFile(arr, path));
          break;
        case "ignored":
          setArr(ignoreFile(arr, path));
          break;
      }
    },
    [arr, place],
  );

  const { drag, startDrag } = useDragPlacement(handleDrop);

  /** The click half of the equivalence: a selected File plus a click on any drop
   * target performs exactly the drop that dragging it there would. */
  const clickTarget = useCallback(
    (target: DropTarget) => {
      if (!selection) return;
      handleDrop(selection.path, target, selection.mode);
    },
    [selection, handleDrop],
  );

  const resolvePending = useCallback(
    (how: "displace" | "merge") => {
      if (!pending) return;
      const { path, target, mode } = pending;
      setPending(null);
      const placed = placeFile(arr, path, target, mode);
      setArr(how === "displace" ? displaceAt(placed, target, path) : placed);
    },
    [arr, pending],
  );

  // --- Groups --------------------------------------------------------------

  const toggleGroup = useCallback(
    (group: number) => {
      setExpanded((current) => {
        const next = new Set(current);
        if (next.has(group)) next.delete(group);
        else next.add(group);
        return next;
      });
      // Provider records load ON EXPAND and are then kept. The first response was
      // complete for everything local, so the counts, the unassigned Files and the
      // Slots local Files claim were already right before this call — and stay
      // right if it fails.
      const note = groupNotes.get(group);
      if (!loadGroup || note?.loaded || note?.loading) return;
      setGroupNotes((notes) => new Map(notes).set(group, { ...(note ?? emptyNote()), loading: true }));
      void loadGroup(group)
        .then((loaded) => {
          if (loaded) {
            setSlotsByGroup((slots) => new Map(slots).set(group, loaded.slots));
          }
          setGroupNotes((notes) =>
            new Map(notes).set(group, {
              loading: false,
              loaded: true,
              unavailable: loaded?.slotsUnavailable,
              error: null,
            }),
          );
        })
        .catch((err) => {
          setGroupNotes((notes) =>
            new Map(notes).set(group, {
              loading: false,
              loaded: false,
              unavailable: undefined,
              error: errorMessage(err),
            }),
          );
        });
    },
    [groupNotes, loadGroup],
  );

  // --- Records -------------------------------------------------------------

  /** Open the picker on one Slot, or on a whole group's filled Slots. */
  const startRepoint = useCallback((group: number, targets: SlotPosition[]) => {
    if (targets.length === 0) return;
    setRepointNote(null);
    setRepointing({ group, targets });
  }, []);

  /** Take the picked run. The records pair against the targets IN ORDER and keep
   * their own numbering; nothing here writes a borrowed number onto a Slot. */
  const takeRepoint = useCallback(
    (externalId: string, records: MatcherSlot[]) => {
      if (!repointing) return;
      const { targets } = repointing;
      const paired = pairRun(targets, records);
      setPins((current) => fillRun(current, targets, records, externalId, storedRecordAt));
      setRepointing(null);
      // Runs of different lengths are normal — a borrowed season rarely has exactly
      // as many records as the group has files — so say what happened rather than
      // guessing at an alignment the Admin did not state.
      if (targets.length === 1) {
        setRepointNote(null);
        return;
      }
      const parts = [`Filled ${paired.length} of ${targets.length} ${labels.slotNounPlural}`];
      if (records.length > paired.length) {
        parts.push(`${records.length - paired.length} record(s) went unused`);
      }
      if (targets.length > paired.length) {
        parts.push(
          `the last ${targets.length - paired.length} kept the record they had — pick again from there`,
        );
      }
      setRepointNote(`${parts.join("; ")}.`);
    },
    [repointing, storedRecordAt, labels.slotNounPlural],
  );

  const dropRecord = useCallback(
    (position: SlotPosition) => {
      setRepointNote(null);
      setPins((current) => clearRecord(current, position, storedRecordAt));
    },
    [storedRecordAt],
  );

  const addSlot = useCallback((group: number, next: number) => {
    setExtraSlots((current) => {
      const merged = new Map(current);
      merged.set(group, [...(merged.get(group) ?? []), next]);
      return merged;
    });
  }, []);

  /* The other half of "+ add". A Slot added here is the Admin's own invention —
     the provider never listed it — so it is the ONE Slot on this screen that can
     be taken away again, and until now nothing could: Revert leaves extraSlots
     alone, so a mis-click was permanent for the session. Provider Slots are not
     removable and must not be: this screen cannot delete a record out of TMDB,
     and pretending otherwise would be a button that lies. */
  const removeSlot = useCallback((position: SlotPosition) => {
    setExtraSlots((current) => {
      const merged = new Map(current);
      const left = (merged.get(position.group) ?? []).filter((n) => n !== position.slot);
      if (left.length === 0) merged.delete(position.group);
      else merged.set(position.group, left);
      return merged;
    });
  }, []);

  // --- Applying ------------------------------------------------------------

  async function runApply() {
    setApplyState({ kind: "applying" });
    try {
      const slots = toApplySlots(pins);
      const next = await apply(
        slots.length > 0 ? { files: toApplyFiles(arr), slots } : { files: toApplyFiles(arr) },
      );
      const applied = next.applied;
      onApplied?.(next);
      // Apply normally closes at once, with enrichment filling in over SSE. It
      // does NOT when the server had something to say: a Placement onto a File
      // the catalog never probed is stored but cannot become an Episode until the
      // next scan, and a screen that closed on that would look like it had
      // silently dropped the Admin's most deliberate correction.
      if (applied && (applied.deferred.length > 0 || applied.displaced.length > 0)) {
        setApplyState({
          kind: "applied",
          rearranged: applied.rearranged,
          displaced: applied.displaced,
          deferred: applied.deferred,
        });
        return;
      }
      setApplyState({ kind: "idle" });
      onClose();
    } catch (err) {
      setApplyState(classifyApplyError(err));
    }
  }

  /** Merge every File the server named onto the contested Slot, in the order it
   * named them — one of the three fixes a SLOT_COLLISION offers. */
  function mergeCollision(details: SlotCollisionDetails) {
    setApplyState({ kind: "idle" });
    setArr(arrangeParts(arr, details.slot, details.paths));
  }

  function requestClose() {
    if (dirty) {
      setLeaving(true);
      return;
    }
    onClose();
  }

  // --- Derived view --------------------------------------------------------

  const files = useMemo(() => [...arr.values()], [arr]);
  const ignored = files.filter((f) => f.state === "ignored");
  const unsorted = files.filter(
    (f) => f.state !== "ignored" && f.state !== "placed" && homeGroupOf(f) === UNSORTED_GROUP,
  );

  const groupNumbers = useMemo(() => {
    const numbers = new Set<number>();
    for (const g of matcher.groups) if (g.number !== UNSORTED_GROUP) numbers.add(g.number);
    for (const g of extraGroups) numbers.add(g);
    for (const f of files) {
      for (const p of f.placements) numbers.add(p.group);
      const home = homeGroupOf(f);
      if (home !== UNSORTED_GROUP) numbers.add(home);
    }
    return [...numbers].sort((a, b) => a - b);
  }, [matcher.groups, extraGroups, files]);

  const providerCounts = useMemo(() => {
    const out = new Map<number, number>();
    for (const g of matcher.groups) out.set(g.number, g.slotCount);
    return out;
  }, [matcher.groups]);

  const selected = selection ? arr.get(selection.path) : undefined;

  return (
    <section className="file-matcher" data-testid="file-matcher">
      {header}

      {matcher.slotsUnavailable && (
        <p
          className="status status-warn matcher-degraded"
          data-testid="matcher-slots-unavailable"
          data-reason={matcher.slotsUnavailable}
          role="status"
        >
          {labels.unavailableSentence(matcher.slotsUnavailable)}
        </p>
      )}

      <div className="matcher-toolbar" data-testid="matcher-toolbar">
        <button
          className="auth-submit"
          type="button"
          data-testid="matcher-apply"
          disabled={!dirty || applyState.kind === "applying"}
          onClick={() => void runApply()}
        >
          {applyState.kind === "applying"
            ? "Applying…"
            : dirty
              ? `Apply ${changeCount} ${changeCount === 1 ? "change" : "changes"}`
              : "Apply"}
        </button>
        <button
          className="nav-link"
          type="button"
          data-testid="matcher-revert"
          disabled={!dirty || applyState.kind === "applying"}
          onClick={() => {
            setArr(base);
            setSelection(null);
            setPending(null);
            // Records revert with everything else. A pin the Admin set this session
            // must not be the one change Revert cannot undo.
            setPins(NO_PINS);
            setRepointing(null);
            setRepointNote(null);
          }}
        >
          Revert
        </button>
        <button
          className="nav-link"
          type="button"
          data-testid="matcher-cancel"
          disabled={applyState.kind === "applying"}
          onClick={requestClose}
        >
          Cancel
        </button>
        <span className="matcher-toolbar-hint" data-testid="matcher-dirty-hint">
          {dirty ? "Nothing is written until you press Apply." : "No changes yet."}
        </span>
      </div>

      {selected && (
        <div className="matcher-selection" data-testid="matcher-selection" role="status">
          <span>
            <strong>{basename(selected.path)}</strong>{" "}
            {selection?.mode === "share"
              ? `— choose another ${labels.slotNoun} to ALSO place it on`
              : `— choose a ${labels.slotNoun} to place it on, or a Files column to leave it unassigned`}
          </span>
          <button
            className="nav-link"
            type="button"
            data-testid="matcher-selection-clear"
            onClick={() => setSelection(null)}
          >
            Cancel
          </button>
        </div>
      )}

      {repointNote && (
        <p className="status matcher-repoint-note" data-testid="matcher-repoint-note" role="status">
          {repointNote}
        </p>
      )}

      {repointing && repointRecord && (
        <RepointDialog
          title={
            repointing.targets.length === 1
              ? `Choose the record that should decorate ${labels.slotCode(
                  repointing.targets[0].group,
                  repointing.targets[0].slot,
                )}.`
              : `Choose where ${labels.groupName(repointing.group)}'s records come from. The ${
                  repointing.targets.length
                } filled ${labels.slotNounPlural} take them in order, starting at the one you pick.`
          }
          hint={
            <>
              This changes only what {repointing.targets.length === 1 ? "it is" : "they are"}{" "}
              decorated with. The {labels.slotNounPlural} keep their own numbering, their
              place in the library and their watch history — the borrowed{" "}
              {labels.slotNounPlural}&rsquo; numbers stay with the {labels.seriesNoun} they
              came from.
            </>
          }
          onCancel={() => setRepointing(null)}
        >
          {repointRecord({
            targets: repointing.targets,
            group: repointing.group,
            current: recordAt(pins, repointing.targets[0], storedRecordAt),
            onPicked: takeRepoint,
            onCancel: () => setRepointing(null),
          })}
        </RepointDialog>
      )}

      {applyState.kind === "error" && (
        <p className="status status-error" data-testid="matcher-apply-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {applyState.message}
        </p>
      )}

      {applyState.kind === "scan-running" && (
        <div className="status status-error matcher-scan-running" data-testid="matcher-scan-running" role="alert">
          <p>{applyState.message}</p>
          <button
            className="auth-submit"
            type="button"
            data-testid="matcher-retry-apply"
            onClick={() => void runApply()}
          >
            Try again
          </button>
        </div>
      )}

      {applyState.kind === "collision" && (
        <CollisionPanel
          details={applyState.details}
          message={applyState.message}
          labels={labels}
          onMerge={() => mergeCollision(applyState.details)}
          onMove={(path) => {
            setApplyState({ kind: "idle" });
            setSelection({ path, mode: "move" });
          }}
          onUnassign={(path) => {
            setApplyState({ kind: "idle" });
            setArr((current) => unassignFile(current, path));
          }}
        />
      )}

      {applyState.kind === "applied" && (
        <div className="matcher-applied" data-testid="matcher-applied" role="status">
          <p>
            {applyState.rearranged}{" "}
            {applyState.rearranged === 1 ? "file was" : "files were"} rearranged.
          </p>
          {applyState.displaced.length > 0 && (
            <p data-testid="matcher-applied-displaced">
              Taken off {applyState.displaced.length === 1 ? "its" : "their"}{" "}
              {labels.slotNoun} to make room, and now unassigned:{" "}
              {applyState.displaced.map(basename).join(", ")}
            </p>
          )}
          {applyState.deferred.length > 0 && (
            <p data-testid="matcher-applied-deferred">
              Stored, but not yet playable: {applyState.deferred.map(basename).join(", ")}.
              These files have never been probed, so the {labels.slotNoun} appears after the
              next scan.
            </p>
          )}
          <button
            className="auth-submit"
            type="button"
            data-testid="matcher-applied-done"
            onClick={onClose}
          >
            Done
          </button>
        </div>
      )}

      {leaving && (
        <div className="matcher-confirm-leave" data-testid="matcher-confirm-leave" role="alertdialog">
          <p>
            You have {changeCount} unsaved{" "}
            {changeCount === 1 ? "change" : "changes"}. Leaving now writes nothing.
          </p>
          <button
            className="auth-submit"
            type="button"
            data-testid="matcher-confirm-leave-discard"
            onClick={onClose}
          >
            Discard and leave
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="matcher-confirm-leave-stay"
            onClick={() => setLeaving(false)}
          >
            Keep sorting
          </button>
        </div>
      )}

      {/* The Unsorted tray: Files in no group folder and Files nothing could
          number. Pinned above the groups and ALWAYS visible, so it can be dragged
          from into whichever group happens to be open. */}
      <section
        className="matcher-unsorted"
        data-testid="matcher-unsorted"
        data-drop="unassigned"
      >
        <h3 className="matcher-unsorted-title">
          {labels.unsortedName} ({unsorted.length})
        </h3>
        <p className="matcher-hint">{labels.unsortedHint}</p>
        {unsorted.length === 0 && (
          <p className="status status-empty" data-testid="matcher-unsorted-empty">
            Nothing here — every file claims a {labels.groupNoun}.
          </p>
        )}
        <ul className="matcher-file-list">
          {unsorted.map((f) => (
            <FileCard
              key={f.path}
              file={f}
              labels={labels}
              containerTitle={matcher.title}
              selected={selection?.path === f.path}
              onPick={() => setSelection((s) => (s?.path === f.path ? null : { path: f.path, mode: "move" }))}
              onDragStart={(e) => startDrag(f.path, e)}
              onIgnore={() => setArr((current) => ignoreFile(current, f.path))}
            />
          ))}
        </ul>
      </section>

      {groupNumbers.map((group) => (
        <GroupSection
          key={group}
          group={group}
          labels={labels}
          arrangement={arr}
          expanded={expanded.has(group)}
          note={groupNotes.get(group)}
          providerSlotCount={providerCounts.get(group) ?? 0}
          slots={slotsByGroup.get(group) ?? []}
          extraSlots={extraSlots.get(group) ?? []}
          containerTitle={matcher.title}
          ownSeries={matcher.seriesExternalId}
          pins={pins}
          storedRecordAt={storedRecordAt}
          canRepoint={repointRecord !== undefined}
          selection={selection}
          pending={pending}
          onRepoint={startRepoint}
          onClearRecord={dropRecord}
          onToggle={() => toggleGroup(group)}
          onAddSlot={(n) => addSlot(group, n)}
          onRemoveSlot={removeSlot}
          onPick={(path, mode) =>
            setSelection((s) => (s?.path === path && s.mode === mode ? null : { path, mode }))
          }
          onDragStart={startDrag}
          onClickTarget={clickTarget}
          onIgnore={(path) => setArr((current) => ignoreFile(current, path))}
          onUnassign={(path) => setArr((current) => unassignFile(current, path))}
          onReorder={(target, path, delta) =>
            setArr((current) => reorderPart(current, target, path, delta))
          }
          onResolvePending={resolvePending}
          onCancelPending={() => setPending(null)}
        />
      ))}

      <div className="matcher-add-group">
        <button
          className="nav-link"
          type="button"
          data-testid="matcher-add-group"
          onClick={() =>
            setExtraGroups((current) => [
              ...current,
              (groupNumbers.length > 0 ? Math.max(...groupNumbers) : 0) + 1,
            ])
          }
        >
          + add {labels.groupNoun}
        </button>
      </div>

      {/* Ignored: settled, not work. Collapsed at the bottom, restorable at any
          time, and never destructive — the file stays exactly where it is. */}
      <section className="matcher-ignored" data-testid="matcher-ignored" data-drop="ignored">
        <button
          className="nav-link"
          type="button"
          data-testid="matcher-ignored-toggle"
          aria-expanded={ignoredOpen}
          onClick={() => setIgnoredOpen((v) => !v)}
        >
          {ignoredOpen ? "▾" : "▸"} Ignored ({ignored.length})
        </button>
        {ignoredOpen && (
          <ul className="matcher-file-list">
            {ignored.length === 0 && (
              <li className="status status-empty" data-testid="matcher-ignored-empty">
                Nothing is ignored.
              </li>
            )}
            {ignored.map((f) => (
              <li key={f.path} className="matcher-file" data-testid="matcher-ignored-file" data-path={f.path}>
                <span className="matcher-file-name">{basename(f.path)}</span>
                <button
                  className="nav-link"
                  type="button"
                  data-testid="matcher-restore"
                  onClick={() => setArr((current) => restoreFile(current, f.path))}
                >
                  Restore
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      {drag && (
        <div
          className="matcher-drag-ghost"
          data-testid="matcher-drag-ghost"
          style={{ left: drag.x, top: drag.y }}
          aria-hidden="true"
        >
          {basename(drag.path)}
        </div>
      )}
    </section>
  );
}

// --- Repoint dialog ---------------------------------------------------------

/* Borrowing a record is started from a Slot, and a Slot is almost always far down
   a long grid — so a panel spliced in ABOVE the groups is a panel the Admin never
   sees. They click "Take the record from elsewhere", the viewport does not move,
   and the screen looks like the click did nothing. A native <dialog> puts the
   choice over the page wherever they happen to be standing, and comes with focus
   trapping, a dimmed backdrop and ESC already wired. Chrome mirrors
   .edit-item-dialog: a transparent dialog with a painted panel inside, the body
   scrolling on its own because the picker carries a search grid AND a record list.
*/
function RepointDialog({
  title,
  hint,
  onCancel,
  children,
}: {
  title: string;
  hint: ReactNode;
  onCancel: () => void;
  children: ReactNode;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  return (
    <dialog
      ref={dialogRef}
      className="matcher-repoint-dialog"
      data-testid="matcher-repoint"
      aria-labelledby="matcher-repoint-title"
      onCancel={(e) => {
        // ESC fires a native cancel; route it through the caller so the draft
        // state clears with it rather than leaving a closed-but-open dialog.
        e.preventDefault();
        onCancel();
      }}
      onClick={(e) => {
        if (e.target === dialogRef.current) onCancel();
      }}
    >
      <div className="matcher-repoint-panel">
        <header className="matcher-repoint-header">
          <h2
            className="matcher-repoint-target"
            id="matcher-repoint-title"
            data-testid="matcher-repoint-target"
          >
            {title}
          </h2>
          <button
            className="button-secondary matcher-repoint-close"
            type="button"
            data-testid="matcher-repoint-close"
            aria-label="Close"
            onClick={onCancel}
          >
            ×
          </button>
        </header>
        <div className="matcher-repoint-body">
          <p className="matcher-hint">{hint}</p>
          {children}
        </div>
      </div>
    </dialog>
  );
}

// --- Group ------------------------------------------------------------------

interface GroupNote {
  loading: boolean;
  loaded: boolean;
  unavailable?: MatcherGroup["slotsUnavailable"];
  error: string | null;
}

const emptyNote = (): GroupNote => ({ loading: false, loaded: false, error: null });

function seedSlots(groups: readonly MatcherGroup[]): Map<number, MatcherSlot[]> {
  const out = new Map<number, MatcherSlot[]>();
  for (const g of groups) out.set(g.number, g.slots);
  return out;
}

function seedNotes(groups: readonly MatcherGroup[]): Map<number, GroupNote> {
  const out = new Map<number, GroupNote>();
  for (const g of groups) {
    out.set(g.number, {
      loading: false,
      loaded: g.slotsLoaded,
      unavailable: g.slotsUnavailable,
      error: null,
    });
  }
  return out;
}

function GroupSection({
  group,
  labels,
  arrangement,
  expanded,
  note,
  providerSlotCount,
  slots,
  extraSlots,
  containerTitle,
  ownSeries,
  pins,
  storedRecordAt,
  canRepoint,
  selection,
  pending,
  onRepoint,
  onClearRecord,
  onToggle,
  onAddSlot,
  onRemoveSlot,
  onPick,
  onDragStart,
  onClickTarget,
  onIgnore,
  onUnassign,
  onReorder,
  onResolvePending,
  onCancelPending,
}: {
  group: number;
  labels: MatcherLabels;
  arrangement: Arrangement;
  expanded: boolean;
  note?: GroupNote;
  providerSlotCount: number;
  slots: MatcherSlot[];
  extraSlots: number[];
  containerTitle: string;
  ownSeries?: string;
  pins: PinDrafts;
  storedRecordAt: (position: SlotPosition) => MatcherSlotRecord | undefined;
  canRepoint: boolean;
  selection: Selection | null;
  pending: PendingDrop | null;
  onRepoint: (group: number, targets: SlotPosition[]) => void;
  onClearRecord: (position: SlotPosition) => void;
  onToggle: () => void;
  onAddSlot: (slot: number) => void;
  onRemoveSlot: (position: SlotPosition) => void;
  onPick: (path: string, mode: PlaceMode) => void;
  onDragStart: (path: string, e: { clientX: number; clientY: number; button?: number }) => void;
  onClickTarget: (target: DropTarget) => void;
  onIgnore: (path: string) => void;
  onUnassign: (path: string) => void;
  onReorder: (target: SlotPosition, path: string, delta: number) => void;
  onResolvePending: (how: "displace" | "merge") => void;
  onCancelPending: () => void;
}) {
  const members = [...arrangement.values()].filter(
    (f) => f.state !== "ignored" && homeGroupOf(f) === group,
  );
  const placed = members.filter((f) => f.state === "placed");
  const unassigned = members.filter((f) => f.state !== "placed");

  const slotByNumber = new Map(slots.map((s) => [s.slot, s]));
  const slotNumbers = new Set<number>([...slots.map((s) => s.slot), ...extraSlots]);
  for (const f of arrangement.values()) {
    for (const p of f.placements) if (p.group === group) slotNumbers.add(p.slot);
  }
  const ordered = [...slotNumbers].sort((a, b) => a - b);
  const highest = ordered.length > 0 ? ordered[ordered.length - 1] : 0;
  // Only a Slot with a File can be repointed: a record decorates something, and an
  // empty Slot has nothing to decorate and no Title to carry the record.
  const filled = ordered
    .map((slot) => ({ group, slot }))
    .filter((position) => filesAtSlot(arrangement, position).length > 0);

  return (
    <section className="matcher-group" data-testid="matcher-group" data-group={group}>
      <button
        className="matcher-group-toggle"
        type="button"
        data-testid="matcher-group-toggle"
        aria-expanded={expanded}
        onClick={onToggle}
      >
        <span className="matcher-group-name">{labels.groupName(group)}</span>
        <span className="matcher-group-counts" data-testid="matcher-group-counts">
          {placed.length} assigned · {unassigned.length} unassigned
          {providerSlotCount > 0 ? ` · ${providerSlotCount} ${labels.slotNounPlural}` : ""}
        </span>
      </button>

      {expanded && (
        <div className="matcher-columns">
          {note?.loading && (
            <p className="status status-loading" data-testid="matcher-group-loading">
              Loading {labels.slotNounPlural}&hellip;
            </p>
          )}
          {note?.unavailable && (
            <p
              className="status status-warn"
              data-testid="matcher-group-unavailable"
              data-reason={note.unavailable}
              role="status"
            >
              {labels.unavailableSentence(note.unavailable)}
            </p>
          )}
          {note?.error && (
            <p className="status status-error" data-testid="matcher-group-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {note.error}
            </p>
          )}

          <div
            className="matcher-column matcher-files"
            data-testid="matcher-files"
            data-drop="unassigned"
            data-group={group}
          >
            <h4 className="matcher-column-title">Files</h4>
            {unassigned.length === 0 && (
              <p className="status status-empty" data-testid="matcher-files-empty">
                Every file in this {labels.groupNoun} is on a {labels.slotNoun}.
              </p>
            )}
            <ul className="matcher-file-list">
              {unassigned.map((f) => (
                <FileCard
                  key={f.path}
                  file={f}
                  labels={labels}
                  containerTitle={containerTitle}
                  selected={selection?.path === f.path}
                  onPick={() => onPick(f.path, "move")}
                  onDragStart={(e) => onDragStart(f.path, e)}
                  onIgnore={() => onIgnore(f.path)}
                />
              ))}
            </ul>
            {selection && (
              <button
                className="nav-link matcher-drop-here"
                type="button"
                data-testid="matcher-unassign-here"
                onClick={() => onClickTarget({ kind: "unassigned" })}
              >
                Leave {basename(selection.path)} unassigned
              </button>
            )}
          </div>

          <div className="matcher-column matcher-slots" data-testid="matcher-slots">
            <h4 className="matcher-column-title">{capitalize(labels.slotNounPlural)}</h4>
            {ordered.length === 0 && (
              <p className="status status-empty" data-testid="matcher-slots-empty">
                No {labels.slotNounPlural} in this {labels.groupNoun} yet.
              </p>
            )}
            <ul className="matcher-slot-list">
              {ordered.map((slot) => (
                <SlotCard
                  key={slot}
                  position={{ group, slot }}
                  record={slotByNumber.get(slot)}
                  pinned={recordAt(pins, { group, slot }, storedRecordAt)}
                  ownSeries={ownSeries}
                  canRepoint={canRepoint}
                  canRemove={extraSlots.includes(slot)}
                  onRepoint={onRepoint}
                  onRemoveSlot={onRemoveSlot}
                  onClearRecord={onClearRecord}
                  arrangement={arrangement}
                  labels={labels}
                  containerTitle={containerTitle}
                  selection={selection}
                  pending={
                    pending && pending.target.group === group && pending.target.slot === slot
                      ? pending
                      : null
                  }
                  onClickTarget={onClickTarget}
                  onPick={onPick}
                  onDragStart={onDragStart}
                  onUnassign={onUnassign}
                  onReorder={onReorder}
                  onResolvePending={onResolvePending}
                  onCancelPending={onCancelPending}
                />
              ))}
            </ul>
            <button
              className="nav-link matcher-add-slot"
              type="button"
              data-testid="matcher-add-slot"
              onClick={() => onAddSlot(highest + 1)}
            >
              + add {labels.slotNoun}
            </button>
            {/* The bulk gesture. A contiguous run is the real shape of this
                problem — five files the provider counts in another series — so
                borrowing a whole run is ONE act, not one pin per Slot. */}
            {canRepoint && filled.length > 0 && (
              <button
                className="nav-link matcher-fill-records"
                type="button"
                data-testid="matcher-group-fill-records"
                onClick={() => onRepoint(group, filled)}
              >
                Fill these {labels.slotNounPlural}&rsquo; records from another {labels.seriesNoun}
                &hellip;
              </button>
            )}
          </div>
        </div>
      )}
    </section>
  );
}

// --- Slot -------------------------------------------------------------------

function SlotCard({
  position,
  record,
  pinned,
  ownSeries,
  canRepoint,
  canRemove,
  onRepoint,
  onRemoveSlot,
  onClearRecord,
  arrangement,
  labels,
  containerTitle,
  selection,
  pending,
  onClickTarget,
  onPick,
  onDragStart,
  onUnassign,
  onReorder,
  onResolvePending,
  onCancelPending,
}: {
  position: SlotPosition;
  record?: MatcherSlot;
  /** The record decorating this Slot right now — the Admin's draft if they
   * touched it, else the server's. Null means "this container's own record at this
   * position", which for a position the provider does not list means bare. */
  pinned: MatcherSlotRecord | null;
  ownSeries?: string;
  canRepoint: boolean;
  /** This Slot was added on this screen, so it can be taken away again. */
  canRemove: boolean;
  onRepoint: (group: number, targets: SlotPosition[]) => void;
  onRemoveSlot: (position: SlotPosition) => void;
  onClearRecord: (position: SlotPosition) => void;
  arrangement: Arrangement;
  labels: MatcherLabels;
  containerTitle: string;
  selection: Selection | null;
  pending: PendingDrop | null;
  onClickTarget: (target: DropTarget) => void;
  onPick: (path: string, mode: PlaceMode) => void;
  onDragStart: (path: string, e: { clientX: number; clientY: number; button?: number }) => void;
  onUnassign: (path: string) => void;
  onReorder: (target: SlotPosition, path: string, delta: number) => void;
  onResolvePending: (how: "displace" | "merge") => void;
  onCancelPending: () => void;
}) {
  const parts = filesAtSlot(arrangement, position);
  // The numbers disagree on BOTH sides or neither: the Slot's own code is
  // highlighted exactly when one of the Files on it claims a different position.
  const mismatch = parts.some((f) => comparePosition(f.parsed, position).differs);
  // A borrowed record supplies the WORDS. It never supplies the code — that stays
  // the Slot's own, which is the whole discipline of ADR-0044.
  const shownName = pinned ? pinned.name : record?.name;
  // Only a Slot with a File has anything to decorate, or a Title to carry the pin.
  const repointable = canRepoint && parts.length > 0;
  // ...and only an EMPTY one can be deleted. A Slot holding a file is holding the
  // Admin's own work; taking the file off first is one click and says out loud
  // where the file went, which deleting the Slot underneath it would not.
  const removable = canRemove && parts.length === 0;

  return (
    <li
      className={`matcher-slot${mismatch ? " is-mismatch" : ""}`}
      data-testid="matcher-slot"
      data-drop="slot"
      data-group={position.group}
      data-slot={position.slot}
      data-position-mismatch={mismatch ? "true" : "false"}
    >
      {/* Code — title on the left, the record actions hard right, on ONE line. */}
      <div className="matcher-slot-head">
        <button
          className="matcher-slot-target"
          type="button"
          data-testid="matcher-slot-target"
          onClick={() => onClickTarget({ kind: "slot", ...position })}
          aria-label={`${labels.slotCode(position.group, position.slot)}${shownName ? ` ${shownName}` : ""}`}
        >
          {/* Code, separator, words — on ONE line, punctuated the way a filename
              punctuates the same three things, so the Slot's label and the label of
              the File placed under it can be read against each other as two lines of
              the same shape rather than compared item by item. */}
          {/* Never marked. The Slot's words are what the file is being measured
              AGAINST; colouring both sides left an Admin comparing two highlighted
              strings instead of reading one. The card's border still carries the
              disagreement, which is what says "look at this row". */}
          <span className="matcher-slot-code" data-testid="matcher-slot-code">
            {labels.slotCode(position.group, position.slot)}
          </span>
          {/* The spaces are IN the separator, not a flex gap: this line is text an
              Admin copies and a screen reader reads, and " - " is how the filename
              beside it punctuates the same join. */}
          <span className="matcher-slot-sep">{" - "}</span>
          {shownName ? (
            <span className="matcher-slot-name" data-testid="matcher-slot-name" title={shownName}>
              {shownName}
            </span>
          ) : (
            <span className="matcher-slot-name is-bare" data-testid="matcher-slot-bare">
              no title
            </span>
          )}
        </button>

        {(repointable || removable) && (
          <SlotActionsMenu
            slotCode={labels.slotCode(position.group, position.slot)}
            pinned={pinned !== null}
            slotNoun={labels.slotNoun}
            canRepoint={repointable}
            canRemove={removable}
            onRepoint={() => onRepoint(position.group, [position])}
            onClearRecord={() => onClearRecord(position)}
            onRemove={() => onRemoveSlot(position)}
          />
        )}
      </div>

      {/* Provenance, and only provenance. The borrowed position is stated here so
          the Admin can see where the words came from; it is never the code above,
          because the borrowed run is numbered from 1 and the container has a real
          group 1 of its own (ADR-0044). */}
      {pinned && (
        <span
          className="matcher-slot-pin"
          data-testid="matcher-slot-pin"
          data-record-group={pinned.group}
          data-record-slot={pinned.slot}
          data-record-series={pinned.externalId ?? ""}
        >
          Record from {labels.slotCode(pinned.group, pinned.slot)}
          {pinned.externalId && pinned.externalId !== ownSeries
            ? ` of ${labels.seriesNoun} ${pinned.externalId}`
            : ` of this ${labels.seriesNoun}`}
        </span>
      )}

      {/* An empty Slot is a PLACE, not a missing name. Left to itself it collapsed
          to the height of its own title, so a column of them read as a list of
          names with gaps between them and nothing said what the gaps were FOR. It
          carries the outline of the card a File would make instead — and it is the
          click target for the click-to-place half of the gesture, because the
          outline is the part of an empty Slot an Admin actually aims at. */}
      {parts.length === 0 && (
        <button
          className="matcher-slot-empty"
          type="button"
          data-testid="matcher-slot-empty"
          onClick={() => onClickTarget({ kind: "slot", ...position })}
        >
          {selection ? "Place it here" : "Drag a file here"}
        </button>
      )}

      {parts.map((file, index) => (
        <PartRow
          key={file.path}
          file={file}
          position={position}
          index={index}
          total={parts.length}
          labels={labels}
          slotName={shownName}
          containerTitle={containerTitle}
          selected={selection?.path === file.path}
          onPick={onPick}
          onDragStart={onDragStart}
          onUnassign={onUnassign}
          onReorder={onReorder}
        />
      ))}

      {pending && (
        <div className="matcher-occupied-choice" data-testid="matcher-occupied-choice" role="alertdialog">
          <p>
            {labels.slotCode(position.group, position.slot)} already holds{" "}
            {pending.occupants.map(basename).join(", ")}. What should happen to{" "}
            {basename(pending.path)}?
          </p>
          <button
            className="auth-submit"
            type="button"
            data-testid="matcher-choice-merge"
            onClick={() => onResolvePending("merge")}
          >
            Keep both, as ordered parts
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="matcher-choice-displace"
            onClick={() => onResolvePending("displace")}
          >
            Replace — put the other back in Files
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="matcher-choice-cancel"
            onClick={onCancelPending}
          >
            Cancel
          </button>
        </div>
      )}
    </li>
  );
}

/* The two record actions used to sit under the Slot as bare links, on a row of
   their own, on EVERY filled Slot — a permanent second line of text competing with
   the one line that matters (code — title) and doubling the height of the column.
   They are corrections, reached rarely and deliberately, so they belong behind a
   kebab on the title line itself. Same shell as the browse list's
   EpisodeActionsMenu: click-outside and Escape close it, the parent owns the acts. */
function SlotActionsMenu({
  slotCode,
  pinned,
  slotNoun,
  canRepoint,
  canRemove,
  onRepoint,
  onClearRecord,
  onRemove,
}: {
  slotCode: string;
  pinned: boolean;
  slotNoun: string;
  canRepoint: boolean;
  canRemove: boolean;
  onRepoint: () => void;
  onClearRecord: () => void;
  onRemove: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocPointer(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="row-menu matcher-slot-menu" ref={ref}>
      <button
        type="button"
        className="row-menu-toggle"
        data-testid="matcher-slot-menu-toggle"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Record actions for ${slotCode}`}
        onClick={() => setOpen((v) => !v)}
      >
        ⋮
      </button>
      {open && (
        <ul className="row-menu-list" role="menu" data-testid="matcher-slot-menu">
          {canRepoint && (
            <li className="row-menu-item" role="none">
              <button
                type="button"
                className="row-menu-button"
                role="menuitem"
                data-testid="matcher-slot-repoint"
                onClick={() => {
                  setOpen(false);
                  onRepoint();
                }}
              >
                {pinned ? "Use a different record\u2026" : "Take the record from elsewhere\u2026"}
              </button>
            </li>
          )}
          {canRepoint && pinned && (
            <li className="row-menu-item" role="none">
              <button
                type="button"
                className="row-menu-button"
                role="menuitem"
                data-testid="matcher-slot-clear-record"
                onClick={() => {
                  setOpen(false);
                  onClearRecord();
                }}
              >
                Use this {slotNoun}&rsquo;s own record
              </button>
            </li>
          )}
          {canRemove && (
            <li className="row-menu-item" role="none">
              <button
                type="button"
                className="row-menu-button row-menu-button-danger"
                role="menuitem"
                data-testid="matcher-slot-delete"
                onClick={() => {
                  setOpen(false);
                  onRemove();
                }}
              >
                Delete this {slotNoun}
              </button>
            </li>
          )}
        </ul>
      )}
    </div>
  );
}

function PartRow({
  file,
  position,
  index,
  total,
  labels,
  slotName,
  containerTitle,
  selected,
  onPick,
  onDragStart,
  onUnassign,
  onReorder,
}: {
  file: ArrangedFile;
  position: SlotPosition;
  index: number;
  total: number;
  labels: MatcherLabels;
  /** The words the Slot is showing — borrowed or its own. The filename is compared
   * against whatever the Admin can actually see. */
  slotName?: string;
  containerTitle: string;
  selected: boolean;
  onPick: (path: string, mode: PlaceMode) => void;
  onDragStart: (path: string, e: { clientX: number; clientY: number; button?: number }) => void;
  onUnassign: (path: string) => void;
  onReorder: (target: SlotPosition, path: string, delta: number) => void;
}) {
  const numbers = comparePosition(file.parsed, position);
  const titles = compareTitles(slotName, file.path, containerTitle);
  const shared = isShared(file);
  const label = splitContainerPrefix(file.path, containerTitle);
  const marks = markFilename(label.rest, file.path, containerTitle);

  return (
    <div
      className={`matcher-part${selected ? " is-selected" : ""}`}
      data-testid="matcher-part"
      data-drop="part"
      data-group={position.group}
      data-slot={position.slot}
      data-path={file.path}
      data-ordinal={ordinalAt(file, position)}
      data-shared={shared ? "true" : "false"}
      data-title-mismatch={titles.differs ? "true" : "false"}
    >
      {total > 1 && (
        <span className="matcher-part-label" data-testid="matcher-part-label">
          Part {index + 1}
        </span>
      )}
      {/* One line, and the container's name cut off the front of it: what is left
          starts with the position, directly under the Slot's own position, which is
          the comparison this whole screen exists to make. The stand-in is dimmed
          rather than removed — it has to be visible enough to say a name was cut,
          and quiet enough that the eye lands on the position behind it. The full
          name is on the title, so nothing is hidden from an Admin who wants it.

          The per-file acts ride on the END of that same line. They are two icons
          and two arrows; giving them a row of their own put a band of controls
          under every file, between the filename and whatever the file has to say
          about itself. */}
      <div className="matcher-part-head">
        <button
          className="matcher-part-name"
          type="button"
          data-testid="matcher-part-pick"
          aria-pressed={selected}
          title={basename(file.path)}
          onPointerDown={(e) => onDragStart(file.path, e)}
          onClick={() => onPick(file.path, "move")}
        >
          {label.elided && (
            <span className="matcher-part-elision" data-testid="matcher-part-elision">
              {label.elided}
            </span>
          )}
          {/* The disagreement is coloured ON the filename, on the characters it is
              about: the position it claims when that is not where it now sits, the
              title it claims when the record says otherwise. Nothing is restated
              underneath — the file's own name is the only text an Admin has to
              read to decide. */}
          {marks.map((mark, i) =>
            (mark.kind === "position" && numbers.differs) ||
            (mark.kind === "title" && titles.differs) ? (
              <span
                key={i}
                className="matcher-part-mark"
                data-testid={`matcher-part-mark-${mark.kind}`}
              >
                {mark.text}
              </span>
            ) : (
              <span key={i}>{mark.text}</span>
            ),
          )}
        </button>
        <span className="matcher-part-actions">
          {total > 1 && (
            <>
              <button
                className="nav-link"
                type="button"
                data-testid="matcher-part-up"
                disabled={index === 0}
                aria-label={`Move ${basename(file.path)} earlier`}
                onClick={() => onReorder(position, file.path, -1)}
              >
                ↑
              </button>
              <button
                className="nav-link"
                type="button"
                data-testid="matcher-part-down"
                disabled={index === total - 1}
                aria-label={`Move ${basename(file.path)} later`}
                onClick={() => onReorder(position, file.path, 1)}
              >
                ↓
              </button>
            </>
          )}
          {/* One File across several Slots. Deliberately its own control rather than
              a modifier on the drag: dragging a placed File MOVES it, which is what
              an Admin expects, and "this file is also the next episode" is rare
              enough to be worth its own act.

              Both acts are icons: they repeat on every placed File, and as sentences
              they were the widest thing in the column — two lines of prose restating
              what the row already shows. The words survive as the tooltip and as the
              accessible name, which is where a rarely-used control's explanation
              belongs. */}
          <button
            className="matcher-part-action"
            type="button"
            data-testid="matcher-part-share"
            title={`Include this file in an additional ${labels.slotNoun}`}
            aria-label={`Include this file in an additional ${labels.slotNoun}`}
            onClick={() => onPick(file.path, "share")}
          >
            <AlsoPlaceIcon />
          </button>
          <button
            className="matcher-part-action"
            type="button"
            data-testid="matcher-part-unassign"
            title={`Unlink this file from ${labels.slotNoun}`}
            aria-label={`Unlink this file from ${labels.slotNoun}`}
            onClick={() => onUnassign(file.path)}
          >
            <UnlinkIcon />
          </button>
        </span>
      </div>

      {file.unreadable && (
        // The one thing this screen cannot show by placing the file, and the one thing an
        // Admin looking at a tidy row of slots most needs to know: the filename numbered it,
        // so it sits here looking finished, and no Episode was built because ffprobe cannot
        // read the bytes (ADR-0047).
        <span className="matcher-part-unreadable" data-testid="matcher-part-unreadable" role="alert">
          This file could not be read, so nothing plays from this {labels.slotNoun}. Replace it
          on disk and rescan, or ignore it.
        </span>
      )}

      {shared && (
        <span className="matcher-part-shared" data-testid="matcher-part-shared">
          Shared with {labels.slotNounPlural}{" "}
          {file.placements
            .filter((p) => !(p.group === position.group && p.slot === position.slot))
            .map((p) => labels.slotCode(p.group, p.slot))
            .join(", ")}
        </span>
      )}

      {numbers.differs && numbers.claimed && (
        <span
          className="matcher-part-claim is-mismatch"
          data-testid="matcher-position-mismatch"
        >
          the filename says {labels.slotCode(numbers.claimed.group, numbers.claimed.slot)}
        </span>
      )}

      {file.orphaned && (
        <span className="matcher-part-orphaned" data-testid="matcher-part-orphaned">
          This correction points at a file that is no longer on disk.
        </span>
      )}
    </div>
  );
}

// --- File card --------------------------------------------------------------

function FileCard({
  file,
  labels,
  containerTitle,
  selected,
  onPick,
  onDragStart,
  onIgnore,
}: {
  file: ArrangedFile;
  labels: MatcherLabels;
  containerTitle: string;
  selected: boolean;
  onPick: () => void;
  onDragStart: (e: { clientX: number; clientY: number; button?: number }) => void;
  onIgnore: () => void;
}) {
  // Elided exactly like a placed File. Every File in this column belongs to the
  // same container, so the prefix is as repetitive here as it is on a Slot — and
  // this is the column an Admin reads WHILE dragging, against the Slots beside it.
  const label = splitContainerPrefix(file.path, containerTitle);
  return (
    <li
      className={`matcher-file${selected ? " is-selected" : ""}`}
      data-testid="matcher-file"
      data-path={file.path}
      data-state={file.state}
    >
      <button
        className="matcher-file-pick"
        type="button"
        data-testid="matcher-file-pick"
        aria-pressed={selected}
        onPointerDown={onDragStart}
        onClick={onPick}
      >
        <span className="matcher-file-name" title={basename(file.path)}>
          {label.elided && (
            <span className="matcher-part-elision" data-testid="matcher-file-elision">
              {label.elided}
            </span>
          )}
          {label.rest}
        </span>
        {file.parsed.length > 0 && (
          <span className="matcher-file-claim" data-testid="matcher-file-claim">
            {file.parsed.map((p) => labels.slotCode(p.group, p.slot)).join(", ")}
          </span>
        )}
        {file.parsed.length === 0 && (
          <span className="matcher-file-claim is-bare" data-testid="matcher-file-unnumbered">
            no {labels.slotNoun} number in the filename
          </span>
        )}
      </button>
      {file.decided && file.state === "unassigned" && (
        <span className="matcher-file-note" data-testid="matcher-file-decided">
          You took this off its {labels.slotNoun}.
        </span>
      )}
      {file.orphaned && (
        <span className="matcher-file-note" data-testid="matcher-file-orphaned">
          This correction points at a file that is no longer on disk.
        </span>
      )}
      {file.unreadable && (
        <span className="matcher-file-unreadable" data-testid="matcher-file-unreadable" role="alert">
          This file could not be read. Replace it on disk and rescan, or ignore it.
        </span>
      )}
      <button
        className="nav-link"
        type="button"
        data-testid="matcher-file-ignore"
        onClick={onIgnore}
      >
        Ignore
      </button>
    </li>
  );
}

// --- Collision --------------------------------------------------------------

/** `409 SLOT_COLLISION` is actionable BY CONTRACT: the server names the Slot and
 * every path claiming it, precisely so this panel can offer the three fixes that
 * resolve it. It is never a collision the matcher created — it is two filenames
 * that already parse onto one Slot, which the Admin has not seen and which the
 * Scanner mangles today. Naming both paths is the whole point. */
function CollisionPanel({
  details,
  message,
  labels,
  onMerge,
  onMove,
  onUnassign,
}: {
  details: SlotCollisionDetails;
  message: string;
  labels: MatcherLabels;
  onMerge: () => void;
  onMove: (path: string) => void;
  onUnassign: (path: string) => void;
}) {
  return (
    <div className="matcher-collision" data-testid="matcher-collision" role="alert">
      <p data-testid="matcher-collision-message">{message}</p>
      <p>
        {labels.slotCode(details.slot.group, details.slot.slot)} is claimed by{" "}
        {details.paths.length} files:
      </p>
      <ul className="matcher-collision-paths">
        {details.paths.map((path) => (
          <li key={path} data-testid="matcher-collision-path" data-path={path}>
            <code>{path}</code>
            <button
              className="nav-link"
              type="button"
              data-testid="matcher-collision-move"
              onClick={() => onMove(path)}
            >
              Move this one to another {labels.slotNoun}…
            </button>
            <button
              className="nav-link"
              type="button"
              data-testid="matcher-collision-unassign"
              onClick={() => onUnassign(path)}
            >
              Take this one off its {labels.slotNoun}
            </button>
          </li>
        ))}
      </ul>
      <button
        className="auth-submit"
        type="button"
        data-testid="matcher-collision-merge"
        onClick={onMerge}
      >
        Merge them onto {labels.slotCode(details.slot.group, details.slot.slot)} as parts
      </button>
    </div>
  );
}

// --- Errors -----------------------------------------------------------------

function classifyApplyError(err: unknown): ApplyState {
  if (err instanceof ApiError) {
    if (err.code === "SLOT_COLLISION") {
      const details = readCollisionDetails(err.details);
      if (details) return { kind: "collision", message: err.message, details };
    }
    // A scan holds the Library's lock and NOTHING was written, so the only thing
    // the Admin has to do is press the button again in a minute.
    if (err.code === "SCAN_RUNNING") return { kind: "scan-running", message: err.message };
    return { kind: "error", message: err.message };
  }
  return { kind: "error", message: errorMessage(err) };
}

function readCollisionDetails(details: Record<string, unknown> | undefined): SlotCollisionDetails | null {
  if (!details) return null;
  const slot = details.slot as { group?: number; slot?: number } | undefined;
  const paths = details.paths;
  if (!slot || !Array.isArray(paths)) return null;
  return {
    slot: { group: slot.group ?? 0, slot: slot.slot ?? 0 },
    paths: paths.filter((p): p is string => typeof p === "string"),
  };
}

const capitalize = (s: string): string => (s === "" ? s : s[0].toUpperCase() + s.slice(1));
