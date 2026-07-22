// The Playback preference store (appletv-web-parity §1/§2) — the web equivalent of
// the TV's platform-free `PlaybackPreference` model. It records the pre-play
// configuration a viewer commits from the Playback Options sheet, so the detail
// page's Play/Continue button can REPLAY it on the next play.
//
// OWNERSHIP (client ADR-0011): this store holds ONLY the axes the SERVER has no
// memory of. The audio / video Stream picks are the server's per-user Remembered
// audio / Remembered video (server ADR-0023/0025) and must NOT be duplicated here
// (they'd drift the moment the viewer switches in-player). For THIS slice that
// leaves the Edition and the Quality cap (this slice), with Subtitle / AAC / Force
// Remux slated to join the same struct later.
//
// KEYING: per Active user + per Title (Movies) / per Show (TV). A Movie keys on its
// own Title id; a TV Episode keys on its SHOW id, so a single choice ports across
// the Show's Episodes (which carry different Edition ids — hence Edition is stored
// by NAME, resolved to an id per-Episode at negotiate time by the resolver).
//
// The load/save are pure, storage-injected helpers (callers pass `localStorage`;
// tests pass a real/fake Storage), so the round-trip is unit-testable at the store
// seam without React — the same seam philosophy as usePlaybackPrefs / queue/persist.
// Distinct from usePlaybackPrefs (volume/mute, per-user only): this is a separate
// concern, keyed additionally per Title/Show.

import { isQualityCapId, type QualityCapId } from "./qualityLadder";

const STORAGE_PREFIX = "juicebox.playback-pref";

/** A committed Playback preference — the axes with no server memory (ADR-0011).
 * The Edition + the Quality cap for this slice; future axes (subtitle, AAC,
 * force-remux) slot in here alongside them. */
export interface PlaybackPreference {
  /** The chosen Edition's NAME (not id): the name ports across a Show's Episodes,
   * which each carry a different id for the "same" Edition. `null` = **Auto** — omit
   * `editionId` on the request and let the server pick the best direct-play Edition. */
  editionName: string | null;
  /** The chosen Quality-cap rung id (appletv-web-parity §1/§3), or `null` for
   * **Direct Play** — uncapped, the viewport-derived resolution + no manual bitrate
   * cap. A rung sends its paired `maxResolution` + `maxBitrate` (see qualityLadder);
   * it is a manual override of the viewport default, superseding it at negotiate. */
  qualityCap: QualityCapId | null;
}

/** The all-Auto preference: nothing pinned, every axis deferred to the server /
 * capability default. The backward-compatible default — an unconfigured Title plays
 * exactly as today (Auto Edition + Direct Play quality). */
export const AUTO_PREFERENCE: PlaybackPreference = { editionName: null, qualityCap: null };

/** The scope a preference is keyed to: a Movie keys per Title; a TV Episode keys
 * per Show (so the choice ports across the Show's Episodes — CONTEXT.md). */
export type PreferenceScope =
  | { kind: "title"; id: string }
  | { kind: "show"; id: string };

/** Derive the preference scope for a Title: a Movie (or any non-Episode) keys on
 * its own id; an Episode keys on its Show id. `null` when an Episode carries no
 * Show context (a malformed detail) — the caller then skips the store. */
export function preferenceScopeForTitle(title: {
  id: string;
  kind: string;
  episode?: { showId: string };
}): PreferenceScope | null {
  if (title.kind === "episode") {
    return title.episode ? { kind: "show", id: title.episode.showId } : null;
  }
  return { kind: "title", id: title.id };
}

/** The per-user, per-Title/Show storage key (a logged-out/anon session gets its
 * own bucket, mirroring usePlaybackPrefs). */
export function preferenceKey(userId: string | null, scope: PreferenceScope): string {
  return `${STORAGE_PREFIX}.${userId ?? "anon"}.${scope.kind}.${scope.id}`;
}

/** Load a committed preference, defensively. A missing entry is the all-Auto
 * default; any malformed/partial payload degrades to Auto rather than throwing — a
 * corrupt pref must never break playback. */
export function loadPreference(
  storage: Storage,
  userId: string | null,
  scope: PreferenceScope,
): PlaybackPreference {
  try {
    const raw = storage.getItem(preferenceKey(userId, scope));
    if (!raw) return { ...AUTO_PREFERENCE };
    const parsed = JSON.parse(raw) as Partial<PlaybackPreference>;
    const editionName =
      typeof parsed.editionName === "string" ? parsed.editionName : null;
    // Coerce a stored/foreign quality cap to a known rung; anything else → Direct Play.
    const qualityCap = isQualityCapId(parsed.qualityCap) ? parsed.qualityCap : null;
    return { editionName, qualityCap };
  } catch {
    return { ...AUTO_PREFERENCE };
  }
}

/** Persist a committed preference. Storage failures (quota/private mode) are
 * swallowed: the in-memory choice still governs THIS play, it just won't survive a
 * reload. */
export function savePreference(
  storage: Storage,
  userId: string | null,
  scope: PreferenceScope,
  pref: PlaybackPreference,
): void {
  try {
    storage.setItem(preferenceKey(userId, scope), JSON.stringify(pref));
  } catch {
    // ignore — persistence is best-effort.
  }
}

/** Convenience: load the preference for a whole Title (deriving its scope). Returns
 * the all-Auto default when the Title has no keyable scope. */
export function loadPreferenceForTitle(
  storage: Storage,
  userId: string | null,
  title: { id: string; kind: string; episode?: { showId: string } },
): PlaybackPreference {
  const scope = preferenceScopeForTitle(title);
  return scope ? loadPreference(storage, userId, scope) : { ...AUTO_PREFERENCE };
}
