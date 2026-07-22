import type { PlaybackConstraints, StartPlaybackOptions } from "../api/types";
import type { PlaybackPreference, SubtitlePreference } from "./playbackPreference";
import { rungById, rungConstraints, sourceHeightForSelection } from "./qualityLadder";

// The Playback resolver (appletv-web-parity §1) — the web equivalent of the TV's
// platform-free `PlaybackResolver`. It turns a committed {@link PlaybackPreference}
// into the subset of `POST /titles/{id}/playback` request fields it governs,
// resolved against the NEGOTIATION CONTEXT (the played Title's Editions).
//
// PURE and side-effect-free: given a preference + the Title's Editions it returns
// the override fields; the player merges them into the capability-derived request.
// This is the seam the axes (Quality cap → constraints, Subtitle → burn id, and the
// later AAC → deviceProfile, Force Remux → remuxSelectedOnly) bolt onto — each adds a
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
//
// SUBTITLE is stored by LANGUAGE (+ forced), not id (so a Show's choice ports across
// Episodes whose Subtitle tracks carry different ids); resolution matches the stored
// language+forced against THIS Title's `subtitles[]`. Delivery then follows the SOURCE
// (ADR-0020): a matched TEXT track is a selectable WebVTT track (no request field —
// the player renders it as a <track> / in-band HLS rendition), and an IMAGE track is
// burned in via `burnSubtitleId` ONLY when the resolved tier transcodes / remuxes; on
// a direct-play tier the image track renders locally, so no field is emitted. The
// pre-play transcode signal the resolver reads is the Quality-cap constraints issue 03
// already computes — a downscale rung means the tier transcodes.

/** The subset of {@link StartPlaybackOptions} a preference can override. Grows as
 * axes land (remuxSelectedOnly, a narrowed deviceProfile). Omitted fields mean "take
 * the server / capability default" (Auto / Direct Play / subtitles Off or local). */
export type ResolvedPlayback = Pick<StartPlaybackOptions, "editionId" | "burnSubtitleId"> & {
  /** The Quality-cap override (appletv-web-parity §3): `maxResolution` + the paired
   * `maxBitrate` from the ladder. Present only for a genuine downscale rung; absent =
   * Direct Play (the capability-derived viewport resolution + no manual bitrate cap).
   * The player merges it OVER the capability constraints in negotiate. */
  constraints?: Pick<PlaybackConstraints, "maxResolution" | "maxBitrate">;
};

/** The minimal Subtitle track shape the resolver needs to decide burn-in: the id to
 * emit, the `kind` (text vs image — image is the only burned source, ADR-0020), and
 * the `language` + `forced` the stored preference matches by. Satisfied by the API
 * `SubtitleTrack` (which carries `source`/`label`/`url` besides). */
export interface ResolverSubtitle {
  id: string;
  kind: "text" | "image" | (string & {});
  language?: string;
  forced: boolean;
}

/** The minimal Edition shape the resolver needs: id + name, plus each File's height
 * so the Quality axis can read the source resolution. Satisfied by the catalog
 * `Edition` (files carry `height`) and — for the Edition axis alone — by a bare
 * `{ id, name }` (no files → source height 0 → no rungs apply). */
export interface ResolverEdition {
  id: string;
  name: string;
  files?: { height?: number }[];
}

/** Resolve a committed preference against a Title's Editions + Subtitle tracks into
 * the playback request overrides. A `null`/absent preference, or an all-Auto one,
 * yields `{}` (nothing overridden → today's behaviour). `subtitles` defaults to `[]`,
 * so a caller with no Subtitle context (or a pre-subtitle call site) still resolves
 * the Edition / Quality axes unchanged. */
export function resolvePlayback(
  pref: PlaybackPreference | null | undefined,
  editions: ResolverEdition[],
  subtitles: ResolverSubtitle[] = [],
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
  // Delivery follows the source (ADR-0020): burn an IMAGE Subtitle track ONLY on a
  // tier that transcodes / remuxes. The pre-play transcode signal is a Quality-cap
  // downscale (constraints present) — the same computation issue 03 already made.
  const burnSubtitleId = resolveBurnSubtitle(
    pref?.subtitle ?? null,
    subtitles,
    constraints !== undefined,
  );
  if (burnSubtitleId) resolved.burnSubtitleId = burnSubtitleId;
  return resolved;
}

/** Resolve a committed Subtitle preference into a `burnSubtitleId`, or undefined for
 * "no burn" — the ONLY subtitle-delivery decision the resolver owns (ADR-0020). Off
 * (null pref) and a language with no track on THIS Title both yield undefined. A
 * matched TEXT track also yields undefined: it is a selectable WebVTT track the player
 * renders itself (out-of-band <track> on direct play, in-band HLS rendition on the
 * transcode tiers), never a burn. A matched IMAGE track burns in ONLY when the tier
 * `transcoding` — on direct play the image track renders locally, no field emitted. */
export function resolveBurnSubtitle(
  subtitle: SubtitlePreference | null,
  subtitles: ResolverSubtitle[],
  transcoding: boolean,
): string | undefined {
  if (!subtitle) return undefined; // Off
  const match = matchSubtitle(subtitle, subtitles);
  if (!match || match.kind !== "image") return undefined; // no track / text → no burn
  return transcoding ? match.id : undefined; // image burns only on a transcode/remux tier
}

/** Match a stored subtitle preference (language + forced) to THIS Title's Subtitle
 * tracks — the by-language keying that ports a Show's choice across Episodes whose
 * tracks carry different ids. Case-insensitive on language; `forced` must match
 * exactly (a forced and a full track in one language stay distinct). The FIRST track
 * satisfying both wins when several share the key. undefined when none matches — the
 * preference then adds no request field (the Title lacks that language). */
function matchSubtitle(
  subtitle: SubtitlePreference,
  subtitles: ResolverSubtitle[],
): ResolverSubtitle | undefined {
  const lang = subtitle.language.toLowerCase();
  return subtitles.find(
    (s) => (s.language ?? "").toLowerCase() === lang && s.forced === subtitle.forced,
  );
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
