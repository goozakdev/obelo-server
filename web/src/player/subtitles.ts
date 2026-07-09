import type { SubtitleTrack } from "../api/types";

// Client-side text-subtitle selection helpers for the captions menu (ADR-0020,
// subtitles/02). Text-track selection never touches the server: the player lists
// the decision's deliverable text tracks, orders them by the viewer's preferred
// language, auto-displays a forced track, and toggles the native <track> mode.

/** The viewer's preferred subtitle language as an ISO-639-1 primary subtag,
 * derived from the browser (navigator.language, e.g. "en-US" → "en"). This is the
 * `preferredSubtitleLang` the capability profile sends AND the key the menu sorts
 * by, so the menu order matches what the server was told. "" when unknown. */
export function preferredSubtitleLang(): string {
  if (typeof navigator === "undefined") return "";
  const lang = navigator.language || (navigator.languages && navigator.languages[0]) || "";
  return lang.split(/[-_]/)[0]?.toLowerCase() ?? "";
}

/** The DELIVERABLE text tracks of a decision (kind "text" with an out-of-band
 * url), ordered for the captions menu: the preferred language first, then forced
 * tracks, then alphabetically by label. Image tracks and any text track without a
 * delivery url are excluded — they can't be shown as a <track>. */
export function orderedTextTracks(
  tracks: SubtitleTrack[],
  preferred: string,
): SubtitleTrack[] {
  const pref = preferred.toLowerCase();
  const text = tracks.filter((t) => t.kind === "text" && !!t.url);
  return [...text].sort((a, b) => {
    const ap = (a.language ?? "").toLowerCase() === pref && pref !== "" ? 0 : 1;
    const bp = (b.language ?? "").toLowerCase() === pref && pref !== "" ? 0 : 1;
    if (ap !== bp) return ap - bp;
    if (a.forced !== b.forced) return a.forced ? -1 : 1;
    return a.label.localeCompare(b.label);
  });
}

/** The IMAGE tracks of a decision (kind "image"), ordered for the captions menu:
 * the preferred language first, then alphabetically by label. Image tracks are
 * burned in on selection (a fresh transcode negotiation) — they are NEVER
 * auto-displayed, even when forced (PRD: forced image subs are not auto-burned),
 * so forcedness does not affect their order. */
export function orderedImageTracks(
  tracks: SubtitleTrack[],
  preferred: string,
): SubtitleTrack[] {
  const pref = preferred.toLowerCase();
  const image = tracks.filter((t) => t.kind === "image");
  return [...image].sort((a, b) => {
    const ap = (a.language ?? "").toLowerCase() === pref && pref !== "" ? 0 : 1;
    const bp = (b.language ?? "").toLowerCase() === pref && pref !== "" ? 0 : 1;
    if (ap !== bp) return ap - bp;
    return a.label.localeCompare(b.label);
  });
}

/** The track id to auto-display on load, or null for "off": a forced text track
 * (subtitles default OFF unless forced, PRD). Takes the already-ordered list so a
 * forced preferred-language track wins when several are forced. */
export function defaultTrackId(ordered: SubtitleTrack[]): string | null {
  const forced = ordered.find((t) => t.forced);
  return forced ? forced.id : null;
}

/** The deliverable TEXT tracks of a decision in SERVER order (kind "text" with a
 * url), unsorted — the order the server lists them AND the order the HLS master
 * playlist lists its in-band SUBTITLES renditions. Used to map a selected track
 * id to an hls.js/native subtitle-track index (subtitles/03). Distinct from
 * orderedTextTracks, which re-sorts for the MENU. */
export function deliverableTextTracks(tracks: SubtitleTrack[]): SubtitleTrack[] {
  return tracks.filter((t) => t.kind === "text" && !!t.url);
}

/** The index of a selected track id among the deliverable text tracks (server
 * order), i.e. which in-band SUBTITLES rendition to enable on the HLS tiers, or
 * null when the id isn't a deliverable text track. */
export function deliverableTextTrackIndex(
  tracks: SubtitleTrack[],
  id: string | null,
): number | null {
  if (id == null) return null;
  const i = deliverableTextTracks(tracks).findIndex((t) => t.id === id);
  return i >= 0 ? i : null;
}

/** Apply the selected text-track id to a <video>'s native TextTracks: the chosen
 * track is set "showing", every other track WE own is "disabled" (so switching is
 * instant — no reload). A null selection turns all our tracks off.
 *
 * Each of our <track> elements carries a UNIQUE data-sub-id and exposes the native
 * TextTrack it produced via its `.track` property, so we key off that id directly.
 * Matching by (label, language) would be ambiguous — two same-language tracks (an
 * embedded English sub + an English .srt sidecar, or plain + SDH) share both, so
 * the wrong track would show or a track could never be turned off. Keying on our
 * own id sidesteps that entirely (and ignores any in-band track the HLS player
 * adds, which carries no data-sub-id). */
export function applySubtitleSelection(
  video: HTMLVideoElement,
  selectedId: string | null,
): void {
  const els = video.querySelectorAll<HTMLTrackElement>("track[data-sub-id]");
  els.forEach((el) => {
    const tt = el.track;
    if (!tt) return; // not yet registered (jsdom, or pre-metadata) — re-runs later
    tt.mode = selectedId != null && el.getAttribute("data-sub-id") === selectedId
      ? "showing"
      : "disabled";
  });
}
