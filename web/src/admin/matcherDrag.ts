import { useCallback, useEffect, useRef, useState } from "react";

// Dragging on the file matcher, on POINTER events rather than HTML5
// drag-and-drop.
//
// HTML5 DnD does not exist on touch, and this screen has to work on a tablet
// (PRD user story 16) — an Admin sorting a Show is exactly the person sitting on
// a sofa with the files on screen. Pointer events are the one input model that
// covers mouse, pen and finger with the same code.
//
// Drag is only ever the FASTER path, never the only one. Every drop this module
// can produce is also reachable by clicking a File and then clicking a target
// (FileMatcher's selection path), which is what makes the screen keyboard-usable
// and what makes a cross-season move easier than holding a drag past a collapsed
// section. Both paths converge on the same `DropTarget` and the same handler, so
// they cannot drift apart.

/** Where a drag (or a click-then-click) can land. Kind-neutral: a Slot, one part
 * of a multi-part Slot (drop in FRONT of it — the reorder gesture), the
 * unassigned column of a group, or the Ignored section. */
export type DropTarget =
  | { kind: "slot"; group: number; slot: number }
  | { kind: "part"; group: number; slot: number; path: string }
  | { kind: "unassigned" }
  | { kind: "ignored" };

/** Read the drop target out of the DOM, from whatever element the pointer was
 * released over. The DOM carries the target as data attributes so that the drag
 * layer needs no registry, no measurement and no per-target subscription — and so
 * that a test can assert exactly what a given element means. */
export function readDropTarget(el: Element | null): DropTarget | null {
  const host = el?.closest?.("[data-drop]") as HTMLElement | null | undefined;
  if (!host) return null;
  const group = Number(host.dataset.group ?? "");
  const slot = Number(host.dataset.slot ?? "");
  switch (host.dataset.drop) {
    case "slot":
      return Number.isFinite(group) && Number.isFinite(slot)
        ? { kind: "slot", group, slot }
        : null;
    case "part":
      return host.dataset.path
        ? { kind: "part", group, slot, path: host.dataset.path }
        : null;
    case "unassigned":
      return { kind: "unassigned" };
    case "ignored":
      return { kind: "ignored" };
    default:
      return null;
  }
}

/** How far to scroll this frame when a drag is near the top or bottom of the
 * viewport, so a file can be dragged into a season that is off screen. Returns 0
 * in the middle band, and a signed pixel step that grows as the pointer presses
 * further into the edge. */
export function autoScrollStep(clientY: number, viewportHeight: number): number {
  const zone = 80;
  const max = 24;
  if (clientY < zone) return -Math.ceil(((zone - clientY) / zone) * max);
  if (clientY > viewportHeight - zone) {
    return Math.ceil(((clientY - (viewportHeight - zone)) / zone) * max);
  }
  return 0;
}

/** How far the pointer must travel before a press becomes a drag rather than a
 * click. Below it the gesture falls through to the click path, so a slightly
 * shaky tap still selects instead of dropping the file somewhere random. */
const DRAG_THRESHOLD_PX = 6;

export interface DragState {
  path: string;
  x: number;
  y: number;
}

/** Pointer dragging for the matcher. `onDrop` receives the same `DropTarget` the
 * click path builds, so the two paths share every rule below this line. */
export function useDragPlacement(onDrop: (path: string, target: DropTarget) => void) {
  const [drag, setDrag] = useState<DragState | null>(null);
  const stepRef = useRef(0);
  const frameRef = useRef<number | null>(null);

  // Auto-scroll runs on its own frame loop rather than on pointermove, so a
  // finger held still at the edge of the screen keeps scrolling instead of
  // stopping — which is the whole reason the affordance exists.
  useEffect(() => {
    if (!drag) return;
    let cancelled = false;
    const tick = () => {
      if (cancelled) return;
      if (stepRef.current !== 0 && typeof window.scrollBy === "function") {
        try {
          window.scrollBy(0, stepRef.current);
        } catch {
          // jsdom (and any host without layout) has no scrolling; the drag itself
          // is unaffected.
        }
      }
      frameRef.current = window.requestAnimationFrame(tick);
    };
    frameRef.current = window.requestAnimationFrame(tick);
    return () => {
      cancelled = true;
      if (frameRef.current !== null) window.cancelAnimationFrame(frameRef.current);
      frameRef.current = null;
    };
  }, [drag]);

  const startDrag = useCallback(
    (path: string, event: { clientX: number; clientY: number; button?: number }) => {
      if (event.button !== undefined && event.button !== 0) return;
      const startX = event.clientX;
      const startY = event.clientY;
      let moved = false;

      const onMove = (ev: PointerEvent) => {
        if (!moved) {
          const dx = ev.clientX - startX;
          const dy = ev.clientY - startY;
          if (Math.sqrt(dx * dx + dy * dy) < DRAG_THRESHOLD_PX) return;
          moved = true;
        }
        // Once it IS a drag, stop the browser turning it into a scroll or a text
        // selection. (The cards also carry `touch-action: none`.)
        if (ev.cancelable) ev.preventDefault();
        stepRef.current = autoScrollStep(ev.clientY, window.innerHeight || 0);
        setDrag({ path, x: ev.clientX, y: ev.clientY });
      };

      const finish = (ev: PointerEvent) => {
        cleanup();
        if (!moved) return; // a tap: the click handler owns it
        // A touch pointer is implicitly captured by the element it went down on,
        // so enter/leave never fire on the target underneath. elementFromPoint is
        // what makes the same code work for finger and mouse alike.
        const el = document.elementFromPoint?.(ev.clientX, ev.clientY) ?? null;
        const target = readDropTarget(el);
        if (target) onDrop(path, target);
      };

      const onCancel = () => cleanup();

      function cleanup() {
        stepRef.current = 0;
        setDrag(null);
        window.removeEventListener("pointermove", onMove);
        window.removeEventListener("pointerup", finish);
        window.removeEventListener("pointercancel", onCancel);
      }

      window.addEventListener("pointermove", onMove, { passive: false });
      window.addEventListener("pointerup", finish);
      window.addEventListener("pointercancel", onCancel);
    },
    [onDrop],
  );

  return { drag, startDrag };
}
