import type { EpisodeContext, TrackContext } from "../api/types";

// Shared formatting for an Episode's parent-context label, so the Title detail
// page (issue 01) and the Home rows (issue 02) read an Episode identically:
// "The Bear · S01E03". Kept here so there is exactly one place the SxxExx /
// degraded-offline label is derived.

/** The canonical SxxExx code for an Episode, or its degraded-offline label (a
 * date / "Episode N") when there is no canonical numbering (date-based /
 * absolute episodes carry an `episodeLabel` instead). */
export function episodeContextCode(ctx: EpisodeContext): string {
  if (ctx.episodeLabel) return ctx.episodeLabel;
  const s = String(ctx.seasonNumber).padStart(2, "0");
  const e = String(ctx.episodeNumber ?? 0).padStart(2, "0");
  return `S${s}E${e}`;
}

/** The full "Show · S01E03" parent-context label for an Episode in a Home row. */
export function episodeContextLabel(ctx: EpisodeContext): string {
  return `${ctx.showTitle} · ${episodeContextCode(ctx)}`;
}

/** The "Artist · Album" parent-context label for a Track in a Home row
 * (tv-music issue 03), so a bare track title is recognizable in its context. */
export function trackContextLabel(ctx: TrackContext): string {
  return `${ctx.artistName} · ${ctx.albumTitle}`;
}
