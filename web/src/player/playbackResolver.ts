import type { PlaybackConstraints, StartPlaybackOptions } from "../api/types";
import type { PlaybackPreference } from "./playbackPreference";
import { rungById, rungConstraints, sourceHeightForSelection } from "./qualityLadder";

// The Playback resolver (appletv-web-parity §1) — the web equivalent of the TV's
// platform-free `PlaybackResolver`. It turns a committed {@link PlaybackPreference}
// into the subset of `POST /titles/{id}/playback` request fields it governs,
// resolved against the NEGOTIATION CONTEXT (the played Title's Editions).
//
// PURE and side-effect-free: given a preference + the Title's Editions it returns
// the override fields; the player merges them into the capability-derived request.
// This is the seam the later axes (Quality cap → constraints, Subtitle → burn id,
// AAC → deviceProfile, Force Remux → remuxSelectedOnly) bolt onto — each adds a
// branch here and a field to {@link ResolvedPlayback}, nothing else moves.
//
// EDITION is stored by NAME (so a Show's choice ports across Episodes with different
// Edition ids); resolution matches the name to THIS Title's Editions and emits that
// Edition's id. A name with no match on this Title degrades to Auto (omit
// `editionId`) — the same graceful fallback the server applies for an absent pick, so
// a Show preference never strands an Episode that lacks that Edition.
//
// QUALITY CAP resolves the stored rung id into the paired `constraints`
// (`maxResolution` + `maxBitrate`, from the ladder). It is validated against the
// SELECTED Edition's source height: a rung at/above the source is a downscale that
// can't apply (the scale filter never upscales), so — like an absent Edition name —
// it degrades to Direct Play (omit the override, take the viewport-derived default).
// Because the source height comes from the SAME `editionName` the pref carries, a
// Show cap replays consistently across Episodes and a shrunk Edition drops a now-
// impossible rung rather than stranding it.

/** The subset of {@link StartPlaybackOptions} a preference can override. Grows as
 * axes land (burnSubtitleId, remuxSelectedOnly, a narrowed deviceProfile). Omitted
 * fields mean "take the server / capability default" (Auto / Direct Play). */
export type ResolvedPlayback = Pick<StartPlaybackOptions, "editionId"> & {
  /** The Quality-cap override (appletv-web-parity §3): `maxResolution` + the paired
   * `maxBitrate` from the ladder. Present only for a genuine downscale rung; absent =
   * Direct Play (the capability-derived viewport resolution + no manual bitrate cap).
   * The player merges it OVER the capability constraints in negotiate. */
  constraints?: Pick<PlaybackConstraints, "maxResolution" | "maxBitrate">;
};

/** The minimal Edition shape the resolver needs: id + name, plus each File's height
 * so the Quality axis can read the source resolution. Satisfied by the catalog
 * `Edition` (files carry `height`) and — for the Edition axis alone — by a bare
 * `{ id, name }` (no files → source height 0 → no rungs apply). */
export interface ResolverEdition {
  id: string;
  name: string;
  files?: { height?: number }[];
}

/** Resolve a committed preference against a Title's Editions into the playback
 * request overrides. A `null`/absent preference, or an all-Auto one, yields `{}`
 * (nothing overridden → today's behaviour). */
export function resolvePlayback(
  pref: PlaybackPreference | null | undefined,
  editions: ResolverEdition[],
): ResolvedPlayback {
  const resolved: ResolvedPlayback = {};
  const editionId = resolveEditionId(pref?.editionName ?? null, editions);
  if (editionId) resolved.editionId = editionId;
  const constraints = resolveQualityConstraints(
    pref?.qualityCap ?? null,
    editions,
    pref?.editionName ?? null,
  );
  if (constraints) resolved.constraints = constraints;
  return resolved;
}

/** Resolve a stored Quality-cap rung id into its paired `{ maxResolution, maxBitrate }`
 * override, or undefined for Direct Play. Direct Play (null id) omits the override.
 * A rung is honoured ONLY when it is a genuine downscale — strictly below the selected
 * Edition's source height; a rung at/above source degrades to Direct Play (the scale
 * filter never upscales), mirroring the Edition axis's degrade-to-Auto. */
export function resolveQualityConstraints(
  qualityCap: string | null,
  editions: ResolverEdition[],
  editionName: string | null,
): Pick<PlaybackConstraints, "maxResolution" | "maxBitrate"> | undefined {
  const rung = rungById(qualityCap);
  if (!rung) return undefined;
  const sourceHeight = sourceHeightForSelection(editions, editionName);
  if (!(rung.height < sourceHeight)) return undefined;
  return rungConstraints(rung);
}

/** Resolve a stored Edition NAME to THIS Title's matching Edition id, or undefined
 * for Auto (no name, or the name isn't among this Title's Editions). Matching by
 * name is what ports a Show's choice across its Episodes (each Episode carries the
 * "same" Edition under a different id). An empty name is treated as Auto — a lone
 * unnamed ("Default") Edition is already the server's default pick. */
export function resolveEditionId(
  editionName: string | null,
  editions: ResolverEdition[],
): string | undefined {
  if (!editionName) return undefined;
  const match = editions.find((e) => e.name === editionName);
  return match?.id;
}
