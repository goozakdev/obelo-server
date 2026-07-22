import type { PlaybackConstraints } from "../api/types";

// The Quality ladder (appletv-web-parity §1/§3) — the web equivalent of the TV's
// platform-free `QualityLadder`. It is the ONE tunable constant table that encodes
// the Quality-cap decision: each downscale rung pairs a `maxResolution` token with a
// `maxBitrate` ceiling, so picking a rung is a genuine BANDWIDTH mode (a real cap on
// both axes), not merely a resolution guarantee.
//
// Contrast web's prior fixed ceiling (a hardcoded 100 Mbps `maxBitrate` +
// viewport-derived `maxResolution`, neither user-controllable — capabilities.ts).
// `Direct Play` keeps that viewport-derived default (uncapped bitrate); a rung is a
// MANUAL OVERRIDE that supersedes it (merged in usePlayerSession's negotiate).
//
// Rungs are offered STRICTLY BELOW the chosen Edition's source height — never a rung
// at or above the source, because the transcode scale filter never upscales. So the
// available set is re-derived whenever the selected Edition (hence source height)
// changes. The `maxResolution` tokens are the server's (`4k`/`1080p`/`720p`/`sd`,
// all understood by the server's resolutionHeight — see internal/playback/profile.go).

/** The persisted Quality-cap value: a rung id, or `null` for Direct Play (uncapped,
 * viewport-derived resolution). The id doubles as the server `maxResolution` token. */
export type QualityCapId = "4k" | "1080p" | "720p" | "sd";

/** One rung of the ladder: the persisted id + menu label, the paired constraints it
 * sends (`maxResolution` token + `maxBitrate` ceiling in bits/sec), and the pixel
 * `height` the rung represents — a rung is offered only when the source is strictly
 * taller than this (a rung ≥ source would be an upscale the scale filter refuses). */
export interface QualityRung {
  id: QualityCapId;
  label: string;
  maxResolution: string;
  maxBitrate: number;
  height: number;
}

/** THE tunable table (appletv-web-parity §1). Ordered high → low. Change these four
 * rows to re-tune the bandwidth modes; nothing else moves. */
export const QUALITY_LADDER: readonly QualityRung[] = [
  { id: "4k", label: "4K", maxResolution: "4k", maxBitrate: 16_000_000, height: 2160 },
  { id: "1080p", label: "1080p", maxResolution: "1080p", maxBitrate: 8_000_000, height: 1080 },
  { id: "720p", label: "720p", maxResolution: "720p", maxBitrate: 4_000_000, height: 720 },
  { id: "sd", label: "SD", maxResolution: "sd", maxBitrate: 1_500_000, height: 480 },
];

/** True when `v` is a known ladder rung id — the guard `loadPreference` uses to
 * defensively coerce a stored/foreign value to a valid rung (else Direct Play). */
export function isQualityCapId(v: unknown): v is QualityCapId {
  return typeof v === "string" && QUALITY_LADDER.some((r) => r.id === v);
}

/** The rung for a stored id, or undefined for Direct Play (null/unknown id). */
export function rungById(id: string | null | undefined): QualityRung | undefined {
  return id ? QUALITY_LADDER.find((r) => r.id === id) : undefined;
}

/** The paired constraints a rung sends: BOTH a resolution ceiling and a bitrate
 * ceiling, the two together making the rung a genuine bandwidth mode. Merged over
 * the capability-derived constraints in negotiate (a rung supersedes the viewport
 * default + the 100 Mbps Direct-Play ceiling). */
export function rungConstraints(
  rung: QualityRung,
): Pick<PlaybackConstraints, "maxResolution" | "maxBitrate"> {
  return { maxResolution: rung.maxResolution, maxBitrate: rung.maxBitrate };
}

/** The rungs STRICTLY BELOW `sourceHeight` — the genuine downscales for a source of
 * that height (never a rung ≥ source; the scale filter never upscales). An unknown
 * source height (0 — dims absent) yields NO rungs, so the sheet offers Direct Play
 * only rather than an unverifiable downscale. */
export function availableRungs(sourceHeight: number): QualityRung[] {
  if (!(sourceHeight > 0)) return [];
  return QUALITY_LADDER.filter((r) => r.height < sourceHeight);
}

/** The source (pixel) height of an Edition — the tallest of its Files' heights (a
 * multi-part Edition shares one resolution; 0 when no File carries dims). This is
 * what the available rungs are derived against. */
export function editionSourceHeight(
  edition: { files?: { height?: number }[] } | null | undefined,
): number {
  if (!edition?.files) return 0;
  return edition.files.reduce((max, f) => Math.max(max, f.height ?? 0), 0);
}

/** The source height governing the Quality rungs for a selection: the named
 * Edition's height when one is chosen; otherwise (Auto, or a name absent from this
 * Title) the tallest Edition — the best the server might direct-play, so Auto still
 * offers every genuine downscale. */
export function sourceHeightForSelection(
  editions: { name: string; files?: { height?: number }[] }[],
  editionName: string | null,
): number {
  const chosen = editionName ? editions.find((e) => e.name === editionName) : undefined;
  if (chosen) return editionSourceHeight(chosen);
  return editions.reduce((max, e) => Math.max(max, editionSourceHeight(e)), 0);
}
