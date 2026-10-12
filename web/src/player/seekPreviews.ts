import type { SeekPreviews } from "../api/types";

/** Where one Seek preview frame lives: the sheet index and the frame's pixel offset
 * inside it. */
export interface SeekPreviewFrame {
  sheet: number;
  /** The clamped frame index. */
  index: number;
  x: number;
  y: number;
}

/** The client lookup rule of docs/api-contract.md "Seek previews — frozen contract"
 * (e): the frame index is floor(t / intervalMs) clamped to [0, frameCount-1], placed
 * row-major in its sheet's grid. */
export function seekPreviewFrame(m: SeekPreviews, positionMs: number): SeekPreviewFrame {
  const perSheet = m.columns * m.rows;
  const raw = Math.floor(positionMs / m.intervalMs);
  const index = Math.min(Math.max(raw, 0), Math.max(m.frameCount - 1, 0));
  const cell = index % perSheet;
  return {
    sheet: Math.floor(index / perSheet),
    index,
    x: (cell % m.columns) * m.frameWidth,
    y: Math.floor(cell / m.columns) * m.frameHeight,
  };
}
