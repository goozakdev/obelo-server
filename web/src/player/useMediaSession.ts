import { useEffect, useMemo, useRef } from "react";

// The browser Media Session integration (appletv-parity/11): the web analogue of
// the TV's MPNowPlayingInfoCenter / MPRemoteCommandCenter. It reflects the current
// music Track into the OS media hub / lock screen / media keys, and lets those
// system transport controls drive the SAME Now Playing player the bar uses.
//
// This module is deliberately a THIN, dependency-free hook: it neither reads the
// Queue nor owns any transport logic. The caller (MediaSessionBridge) resolves the
// current Track's metadata + play state from the existing stores and passes in the
// action callbacks; this hook only mirrors that into `navigator.mediaSession` and
// clears it on stop. Feature-detected — where the API is absent (older browsers,
// jsdom) every call is a graceful no-op.
//
// Music-only by construction: the caller passes `track: null` for a video entry or
// an empty Queue, which clears the session — video playback is never reflected.

/** The current music Track as the Media Session should present it. Empty strings
 * are fine (the OS just omits them); `artworkSrc` is a same-origin cover URL. */
export interface MediaSessionTrack {
  title: string;
  artist: string;
  album: string;
  artworkSrc?: string;
}

/** What the hook needs to mirror + drive playback. `track` null = nothing to show
 * (stopped / video / empty Queue) → the session is cleared. `onPreviousTrack` /
 * `onNextTrack` are null when there is no prev/next, so the OS greys out that
 * control instead of firing a no-op. */
export interface MediaSessionInput {
  track: MediaSessionTrack | null;
  playing: boolean;
  onPlay: () => void;
  onPause: () => void;
  onPreviousTrack: (() => void) | null;
  onNextTrack: (() => void) | null;
}

/** The subset of `navigator.mediaSession` we touch. */
type MediaSessionLike = Pick<
  MediaSession,
  "metadata" | "playbackState" | "setActionHandler"
>;

/** The live browser Media Session, or null when the API (or the `MediaMetadata`
 * constructor it needs) is unavailable — the single feature-detection gate every
 * effect below bails on, so an absent API is a clean no-op. */
function resolveMediaSession(): MediaSessionLike | null {
  if (typeof navigator === "undefined") return null;
  const ms = navigator.mediaSession as MediaSession | undefined;
  if (!ms || typeof ms.setActionHandler !== "function") return null;
  if (typeof MediaMetadata !== "function") return null;
  return ms;
}

/** Mirror the current music Track + play state into `navigator.mediaSession` and
 * wire the system transport controls to the passed-in actions. A no-op when the
 * API is absent. */
export function useMediaSession(input: MediaSessionInput): void {
  // Resolve once — `navigator.mediaSession` is a stable singleton.
  const session = useMemo(resolveMediaSession, []);

  // Latest input, so the action handlers (registered once) always call the current
  // callbacks/state without re-registering on every render.
  const inputRef = useRef(input);
  inputRef.current = input;

  const { track, playing } = input;
  // Depend on the primitive fields, not the (per-render) `track` object identity,
  // so metadata is rebuilt only when it actually changes — not on every render.
  const title = track?.title;
  const artist = track?.artist;
  const album = track?.album;
  const artworkSrc = track?.artworkSrc;
  const hasTrack = track != null;

  // Metadata + playbackState: reflect the current Track, or clear on stop (no
  // Track = empty Queue / video / stopped).
  useEffect(() => {
    if (!session) return;
    if (!hasTrack) {
      session.metadata = null;
      session.playbackState = "none";
      return;
    }
    session.metadata = new MediaMetadata({
      title: title ?? "",
      artist: artist ?? "",
      album: album ?? "",
      artwork: artworkSrc ? [{ src: artworkSrc }] : [],
    });
    session.playbackState = playing ? "playing" : "paused";
  }, [session, hasTrack, title, artist, album, artworkSrc, playing]);

  // play / pause handlers: registered once, delegating to the latest callbacks.
  useEffect(() => {
    if (!session) return;
    session.setActionHandler("play", () => inputRef.current.onPlay());
    session.setActionHandler("pause", () => inputRef.current.onPause());
    return () => {
      // Full teardown when the hook unmounts — leave the OS session clean.
      session.setActionHandler("play", null);
      session.setActionHandler("pause", null);
      session.setActionHandler("previoustrack", null);
      session.setActionHandler("nexttrack", null);
      session.metadata = null;
      session.playbackState = "none";
    };
  }, [session]);

  // previous / next: registered only when a prev/next exists, so the OS control is
  // greyed out (handler null) at the ends of the Queue.
  const canPrev = input.onPreviousTrack != null;
  const canNext = input.onNextTrack != null;
  useEffect(() => {
    if (!session) return;
    session.setActionHandler(
      "previoustrack",
      canPrev ? () => inputRef.current.onPreviousTrack?.() : null,
    );
  }, [session, canPrev]);
  useEffect(() => {
    if (!session) return;
    session.setActionHandler(
      "nexttrack",
      canNext ? () => inputRef.current.onNextTrack?.() : null,
    );
  }, [session, canNext]);
}
