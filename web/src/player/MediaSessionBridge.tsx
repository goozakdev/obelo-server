import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { albumArtworkUrl } from "../browse/albumArt";
import { useAsync } from "../browse/useAsync";
import { useLibraryLiveRefresh } from "../events/enrichEvents";
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

/** How long the library's change signal must go quiet before the playing track's
 * cover version is re-read (a scan/enrich burst ticks every ~100 ms). */
const COVER_REFRESH_QUIET_MS = 2_000;

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

  // The detail is read once per track, so a cover re-picked while this track plays
  // would only show from the next one. The Library's live-refresh signal (the one
  // the browse grids use) re-reads just the album's artwork version.
  const [fresh, setFresh] = useState<{ titleId: string; version?: string } | null>(null);
  const refreshCtrl = useRef<AbortController | null>(null);
  const refreshTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // Leaving a track drops its refreshed version too, so coming back to it shows the
  // freshly read detail rather than a cover that may have been re-picked since.
  useEffect(
    () => () => {
      clearTimeout(refreshTimer.current);
      refreshCtrl.current?.abort();
      setFresh(null);
    },
    [musicTitleId],
  );
  // The signal fires on every scan/enrich tick, so it only arms a trailing debounce:
  // one re-read after the burst has been quiet for COVER_REFRESH_QUIET_MS. Only a Track
  // with an album has a cover to refresh.
  const hasAlbumArt = detail?.track != null;
  const refreshArtwork = useCallback(() => {
    if (!musicTitleId || !hasAlbumArt) return;
    clearTimeout(refreshTimer.current);
    refreshTimer.current = setTimeout(() => {
      refreshCtrl.current?.abort();
      const ctrl = new AbortController();
      refreshCtrl.current = ctrl;
      apiClient
        .getTitle(musicTitleId, ctrl.signal)
        .then((d) => {
          if (!ctrl.signal.aborted)
            setFresh({ titleId: musicTitleId, version: d?.track?.albumArtworkVersion });
        })
        .catch(() => {}); // keep the version already shown
    }, COVER_REFRESH_QUIET_MS);
  }, [musicTitleId, hasAlbumArt]);
  useLibraryLiveRefresh(detail?.libraryId ?? "", refreshArtwork);
  const artworkVersion =
    fresh && fresh.titleId === musicTitleId ? fresh.version : detail?.track?.albumArtworkVersion;

  const track: MediaSessionTrack | null =
    music && entry
      ? {
          title: detail?.title ?? entry.title.title,
          artist: detail?.track?.artistName ?? "",
          album: detail?.track?.albumTitle ?? "",
          artworkSrc: detail?.track
            ? albumArtworkUrl(detail.track.albumId, artworkVersion)
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
