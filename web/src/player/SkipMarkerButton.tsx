import { apiClient } from "../api/client";
import type { Marker } from "../api/types";
import { useAsync } from "../browse/useAsync";

// The player's Skip button (ADR-0065): while the playback position is inside a
// stored Marker of a kind this player recognizes, offer to skip to its end. The
// Markers come from the server for the session's own File; a failed or empty
// fetch simply offers nothing — a missing Skip button never interrupts playback.

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
}

export default function SkipMarkerButton({ sessionId, positionMs, onSkip }: SkipMarkerButtonProps) {
  const state = useAsync((signal) => apiClient.getSessionMarkers(sessionId, signal), [sessionId]);
  const marker = state.status === "ready" ? activeMarker(state.data, positionMs) : null;
  if (!marker) return null;
  return (
    <button
      type="button"
      className="skip-marker"
      data-testid="skip-marker"
      data-kind={marker.kind}
      onClick={() => onSkip(marker.endMs)}
    >
      {MARKER_LABELS[marker.kind]}
    </button>
  );
}
