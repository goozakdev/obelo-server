import { useEffect, useRef } from "react";
import { apiClient } from "../api/client";
import type { Marker } from "../api/types";
import { useAsync } from "../browse/useAsync";

// The player's Skip button (ADR-0065): while the playback position is inside a
// stored Marker of a kind this player recognizes, offer to skip to its end. The
// Markers come from the server for the session's own File; a failed or empty
// fetch simply offers nothing — a missing Skip button never interrupts playback.
//
// A Marker the server marks `autoSkip` (the viewer's own setting for its kind,
// ADR-0065 §6) is skipped without a click, once: seeking back into it plays it and
// offers the button. The Credits Marker the server flags `watchedPoint` (the one
// whose crossing marks the Title watched) reads "Next episode" when the Queue's
// next entry is an Episode, and plays it, whether pressed or auto-skipped; any
// other Credits Marker, or one with no next Episode, stays Skip.

/** The Marker kinds this player offers Skip for, with their button labels. */
export const MARKER_LABELS: Record<string, string> = {
  intro: "Skip Intro",
  recap: "Skip Recap",
  credits: "Skip Credits",
  preview: "Skip Preview",
};

/** The recognized Marker containing positionMs (start inclusive, end exclusive),
 * or null. When Markers overlap, the one that started last wins — it is the
 * narrower thing the viewer is inside. */
export function activeMarker(markers: Marker[], positionMs: number): Marker | null {
  let found: Marker | null = null;
  for (const m of markers) {
    if (!Object.prototype.hasOwnProperty.call(MARKER_LABELS, m.kind)) continue;
    if (positionMs < m.startMs || positionMs >= m.endMs) continue;
    if (!found || m.startMs >= found.startMs) found = m;
  }
  return found;
}

export interface SkipMarkerButtonProps {
  sessionId: string;
  positionMs: number;
  /** Seek to this position (ms) — the Marker's end. */
  onSkip: (positionMs: number) => void;
  /** Play the Queue's next Episode, first reporting `fromMs` — a position inside
   * the Credits — so the Title counts as watched. Absent when there is none, and a
   * Credits Marker then skips within the File like any other. */
  onNextEpisode?: (fromMs: number) => void;
  /** Whether to render the button; auto-skip happens either way. */
  showButton?: boolean;
}

export default function SkipMarkerButton({
  sessionId,
  positionMs,
  onSkip,
  onNextEpisode,
  showButton = true,
}: SkipMarkerButtonProps) {
  const state = useAsync((signal) => apiClient.getSessionMarkers(sessionId, signal), [sessionId]);
  const marker = state.status === "ready" ? activeMarker(state.data, positionMs) : null;
  const nextEpisode = marker?.kind === "credits" && marker.watchedPoint ? onNextEpisode : undefined;
  const skip = () => {
    if (!marker) return;
    if (nextEpisode) nextEpisode(Math.floor(Math.max(positionMs, marker.startMs)));
    else onSkip(marker.endMs);
  };

  // Auto-skip each Marker at most once, keyed by its span.
  const autoSkipped = useRef(new Set<string>());
  const key = marker ? `${marker.kind}:${marker.startMs}:${marker.endMs}` : "";
  const skipRef = useRef(skip);
  skipRef.current = skip;
  useEffect(() => {
    if (!marker?.autoSkip || autoSkipped.current.has(key)) return;
    autoSkipped.current.add(key);
    skipRef.current();
  }, [key, marker?.autoSkip]);

  if (!marker || !showButton) return null;
  return (
    <button
      type="button"
      className="skip-marker"
      data-testid="skip-marker"
      data-kind={marker.kind}
      onClick={skip}
    >
      {nextEpisode ? "Next episode" : MARKER_LABELS[marker.kind]}
    </button>
  );
}
