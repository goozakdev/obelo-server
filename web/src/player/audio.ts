import type { AudioStream, DecisionStream } from "../api/types";
import { langRank, preferredLang } from "./lang";

// Client-side audio-Stream selection helpers for the player's Audio menu
// (audio-streams/04, ADR-0022) — the audio parallel of subtitles.ts. The player
// lists the decision's audio Streams, orders them by the viewer's preferred audio
// language, pre-selects the one the server resolved, and (on the HLS tiers) maps a
// pick to the in-band AUDIO rendition index. Unlike subtitles, audio is never
// "off": every Stream is selectable and exactly one is always playing.

/** The viewer's preferred audio language — the same value as the subtitle one (see
 * lang.ts). Kept as a named alias for the capability profile's `preferredAudioLang`
 * field and the Audio menu's ordering call sites. */
export const preferredAudioLang = preferredLang;

/** The audio Streams ordered for the Audio menu: the preferred language first,
 * then the File's default disposition, then alphabetically by label. Never filters
 * — every audio Stream is selectable (audio is never turned off) — and never
 * mutates the input. Mirrors orderedTextTracks, with the default disposition
 * standing in for a forced track. */
export function orderedAudioStreams(
  streams: AudioStream[],
  preferred: string,
): AudioStream[] {
  return [...streams].sort((a, b) => {
    const byLang = langRank(a.language, preferred) - langRank(b.language, preferred);
    if (byLang !== 0) return byLang;
    if (a.isDefault !== b.isDefault) return a.isDefault ? -1 : 1;
    return a.label.localeCompare(b.label);
  });
}

/** The audio Stream id to pre-select when a session lands: the Stream the server
 * RESOLVED and is delivering (matched to the decision's `audioStream` by its
 * FFmpeg index — memory → preferredAudioLang → default → first, resolved
 * server-side), falling back to the File's default disposition, then the first
 * Stream. null only for a silent File (no audio Streams). */
export function initialAudioId(
  streams: AudioStream[],
  resolved: DecisionStream | undefined,
): string | null {
  if (streams.length === 0) return null;
  if (resolved) {
    const match = streams.find((s) => s.index === resolved.index);
    if (match) return match.id;
  }
  const def = streams.find((s) => s.isDefault);
  return (def ?? streams[0]).id;
}

/** The position of a selected audio Stream id among the File's audio Streams in
 * SERVER order — i.e. which in-band AUDIO rendition to enable on the HLS tiers.
 * The master playlist advertises one rendition per audio Stream in this exact
 * order (audio-streams/03), so the position is the hls.js / native audioTrack
 * index. null when the id isn't one of the Streams (or the selection is null).
 * Mirrors deliverableTextTrackIndex — pass the decision's audioStreams as-is (the
 * unsorted server order), NOT the re-sorted menu list. */
export function audioRenditionIndex(
  streams: AudioStream[],
  id: string | null,
): number | null {
  if (id == null) return null;
  const i = streams.findIndex((s) => s.id === id);
  return i >= 0 ? i : null;
}
