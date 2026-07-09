import type { DecisionStream, VideoStream } from "../api/types";

// Client-side video-Stream selection helpers for the player's Video menu
// (selectable-video/03, ADR-0025) — the video parallel of audio.ts. The player
// lists the decision's non-cover-art video Streams, pre-selects the one the server
// resolved (the capability-then-quality default), and on a pick re-negotiates the
// whole session. Unlike audio, there is NO in-band video rendition in HLS: a switch
// is always a full re-negotiation (the image-subtitle model), so — deliberately —
// there is no `videoRenditionIndex` helper to mirror; the switch id goes straight
// back as `videoStreamId`.

/** The video Streams ordered for the Video menu: the resolved/default Stream first,
 * then alphabetically by label. Never filters — every video Stream is selectable —
 * and never mutates the input. Mirrors orderedAudioStreams minus the audio-language
 * dimension (video has no preferred-language ordering). */
export function orderedVideoStreams(streams: VideoStream[]): VideoStream[] {
  return [...streams].sort((a, b) => {
    if (a.isDefault !== b.isDefault) return a.isDefault ? -1 : 1;
    return a.label.localeCompare(b.label);
  });
}

/** The video Stream id to pre-select when a session lands: the Stream the server
 * RESOLVED and is delivering (matched to the decision's `videoStream` by its FFmpeg
 * index — Title/Show memory → capability-then-quality default → disposition →
 * first, resolved server-side), falling back to the default disposition, then the
 * first Stream. null only for a File with no video Streams. Mirrors initialAudioId. */
export function initialVideoId(
  streams: VideoStream[],
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
