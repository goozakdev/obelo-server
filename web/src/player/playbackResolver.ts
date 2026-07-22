import type { StartPlaybackOptions } from "../api/types";
import type { PlaybackPreference } from "./playbackPreference";

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
// EDITION is the only axis in this slice. It is stored by NAME (so a Show's choice
// ports across Episodes with different Edition ids); resolution matches the name to
// THIS Title's Editions and emits that Edition's id. A name with no match on this
// Title degrades to Auto (omit `editionId`) — the same graceful fallback the server
// applies for an absent pick, so a Show preference never strands an Episode that
// lacks that Edition.

/** The subset of {@link StartPlaybackOptions} a preference can override. Grows as
 * axes land (constraints, burnSubtitleId, remuxSelectedOnly, a narrowed
 * deviceProfile); for now just `editionId`. Omitted fields mean "take the server /
 * capability default" (Auto). */
export type ResolvedPlayback = Pick<StartPlaybackOptions, "editionId">;

/** The minimal Edition shape the resolver needs (id + name) — satisfied by the
 * catalog `Edition` and the decision's `edition`, so any of them can drive it. */
export interface ResolverEdition {
  id: string;
  name: string;
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
  return resolved;
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
