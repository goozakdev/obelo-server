import { apiClient } from "../api/client";
import { albumArtworkUrl } from "../browse/albumArt";
import { useAsync } from "../browse/useAsync";
import { usePlaybackTransport } from "./transport";
import { useQueue } from "./queue/useQueue";
import { useMediaSession, type MediaSessionTrack } from "./useMediaSession";

// The Media Session bridge (appletv-parity/11): mounted ONCE app-wide (in App,
// alongside NowPlayingBar, inside the Queue + Transport providers) so it can
// observe the single source of truth for "what's playing" and drive the OS media
// hub / lock screen / media keys for MUSIC.
//
// It reads the current Track from `useQueue` and the real play/pause state from the
// shared `usePlaybackTransport` — the same transport the bar's controls publish
// into — then hands them, plus the existing Queue/transport actions, to
// `useMediaSession`. No transport logic is duplicated: system play/pause routes
// through the transport's toggle; prev/next through the Queue. Renders nothing.
//
// Music-only: a video entry (or an empty Queue) yields `track: null`, which clears
// the session, matching the rest of the surface (Shuffle/Repeat are music-only,
// video is exclusive). Feature detection lives in the hook, so this stays simple.

/** A Track is the one audio-only, music-playable Title kind (CONTEXT.md:
 * Artist → Album → Track). Everything else is video and is not mirrored. */
function isTrackKind(kind: string): boolean {
  return kind === "track";
}

export default function MediaSessionBridge() {
  const queue = useQueue();
  const transport = usePlaybackTransport();

  const entry = queue.current;
  const music = entry != null && isTrackKind(entry.title.kind);
  const musicTitleId = music ? entry.title.id : null;

  // The current Track's Artist/Album/cover come from the Title detail (the lean
  // Queue entry omits them), fetched only for a music entry — the same source the
  // bar's now-playing label uses. A failed/absent fetch degrades to the entry's
  // bare title; playback (and the metadata title) never wait on it.
  const detailState = useAsync(
    (signal) =>
      musicTitleId ? apiClient.getTitle(musicTitleId, signal) : Promise.resolve(null),
    [musicTitleId],
  );
  const detail = detailState.status === "ready" ? detailState.data : null;

  const track: MediaSessionTrack | null =
    music && entry
      ? {
          title: detail?.title ?? entry.title.title,
          artist: detail?.track?.artistName ?? "",
          album: detail?.track?.albumTitle ?? "",
          artworkSrc: detail?.track
            ? albumArtworkUrl(detail.track.albumId)
            : undefined,
        }
      : null;

  useMediaSession({
    track,
    playing: transport.playing,
    // System play/pause drive the SAME element the bar's transport toggles. The
    // OS sends discrete play/pause, so gate the toggle on the current state.
    onPlay: () => {
      if (!transport.playing) transport.toggle();
    },
    onPause: () => {
      if (transport.playing) transport.toggle();
    },
    // prev/next walk the Queue (null at the ends so the OS greys the control).
    onPreviousTrack: queue.hasPrev ? () => queue.prev() : null,
    onNextTrack: queue.hasNext ? () => queue.next() : null,
  });

  return null;
}
