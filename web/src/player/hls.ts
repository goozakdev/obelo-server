// Attaching an HLS media playlist to a <video> element across the two browser
// engines the player supports (ADR-0004, ADR-0012):
//
//   - Chrome / Firefox / Edge: no native HLS, so we drive playback with hls.js
//     over Media Source Extensions (MSE). hls.js fetches the playlist + segments
//     itself; because the URLs are same-origin and authenticated by the media
//     cookie, no Authorization header is needed (and hls.js can't set one on the
//     segment fetches anyway).
//   - Safari (and iOS): native HLS — the <video> plays the .m3u8 directly when
//     its src is the playlist URL, so we never load hls.js there.
//
// This module is the single seam the player uses for the HLS tiers. It is kept
// free of React so the player component stays declarative and so component tests
// can mock THIS module (jsdom has no MSE, so real hls.js can't run there) while
// the Playwright E2E exercises real hls.js in Chromium.

import type Hls from "hls.js";

/** How many FATAL network errors within {@link sessionLostWindowMs} mean the
 * session is gone (not a lone transient segment fetch) — the first triggers a
 * `startLoad()` retry, the second escalates to a re-negotiation. */
const sessionLostThreshold = 2;
const sessionLostWindowMs = 10_000;

/** Options for {@link attachHls}. All optional — an omitted callback just leaves
 * the corresponding behavior at its default. */
export interface AttachHlsOptions {
  /** Called when the SESSION behind the stream appears to be gone — not a single
   * segment ffmpeg is still producing (that's transient, and `startLoad()` covers
   * it), but a persistent failure where the playlist AND every segment 404. That
   * happens when the server reaped the session after a long pause, or restarted:
   * retrying the same URLs can never recover, so the caller re-negotiates a FRESH
   * session from the current position. Omitted → the seam falls back to restarting
   * the load (the pre-recovery behavior), which is correct when there is no caller
   * that can re-negotiate. */
  onSessionLost?: () => void;
}

/** A live HLS attachment. `detach()` tears down hls.js (stops segment fetches,
 * frees the MSE buffers) and is safe to call more than once. For the native-HLS
 * path it simply clears the <video> src. */
export interface HlsAttachment {
  /** "hls.js" when playing via MSE, "native" when the browser plays the .m3u8
   * directly (Safari). Exposed mainly so tests/diagnostics can assert the path. */
  readonly mode: "hls.js" | "native";
  /** Select the in-band SUBTITLES rendition to display, by its index among the
   * decision's deliverable TEXT tracks (server order — the same order the master
   * playlist lists them), or `null` to turn subtitles off (ADR-0020, slice 03).
   * Text-track selection stays client-side: no server round-trip, instant switch.
   * On hls.js it drives `subtitleTrack`; on native HLS it toggles the matching
   * native TextTrack's mode. Safe to call before the master/renditions have
   * loaded — the desired selection is (re)applied as tracks arrive. */
  setTextTrack(index: number | null): void;
  /** Select the in-band AUDIO rendition to play, by its index among the demuxed
   * multi-audio File's audio Streams (server order — the same order the master
   * playlist lists them, audio-streams/03). Audio is never off, so the index is
   * always a real rendition. On hls.js it drives `audioTrack`; on native HLS it
   * enables the matching entry of `video.audioTracks`. Safe to call before the
   * renditions have loaded — the desired selection is (re)applied as tracks arrive
   * (and re-asserted over hls.js's own default re-selection). A no-op on a muxed
   * single-audio session (there is only one, already playing). */
  setAudioTrack(index: number): void;
  detach(): void;
}

/** The minimal shape of the (non-standard-lib) HTMLMediaElement.audioTracks list
 * the native-HLS path drives — an indexed list of tracks with a settable
 * `enabled`. Not in TS's DOM lib, so we describe just what we touch. */
interface NativeAudioTrackList {
  readonly length: number;
  [index: number]: { enabled: boolean };
}

// The native-HLS feature check lives in ./nativeHls (also used by the capability
// profile, which must keep working when tests mock THIS module); re-exported here
// so the attach seam remains its public home.
import { canPlayHlsNatively } from "./nativeHls";

export { canPlayHlsNatively };

/** Attach an HLS media-playlist `url` to `video`.
 *
 * Resolution order:
 *   1. If the browser plays HLS natively (Safari), set `video.src = url` and
 *      return immediately — no hls.js, no MSE.
 *   2. Otherwise lazy-import hls.js. If it is supported (MSE present), create an
 *      Hls instance, attach the media, and load the playlist.
 *   3. If neither works (no native HLS AND hls.js unsupported), throw so the
 *      caller can surface an honest error rather than a silently dead <video>.
 *
 * hls.js is imported dynamically so its (sizeable) bundle is only pulled in when
 * a directStream/transcode tier is actually chosen — direct play never loads it.
 */
export async function attachHls(
  video: HTMLVideoElement,
  url: string,
  opts: AttachHlsOptions = {},
): Promise<HlsAttachment> {
  // Native HLS first: it's cheaper and the canonical path on Apple devices.
  if (canPlayHlsNatively(video)) {
    video.src = url;
    let detached = false;
    // Native (Safari) has no error-recovery ladder to run, but the same reaped-
    // session failure applies: after a long pause the playlist + segments 404 and
    // the element stalls, then fires `error` (MEDIA_ERR_NETWORK). Treat that as a
    // lost session and let the caller re-negotiate; a genuinely fatal media error
    // re-negotiates once too, which is a safe fallback (the caller guards against a
    // spin). Best-effort — removed on detach.
    const onNativeError = () => opts.onSessionLost?.();
    if (opts.onSessionLost) video.addEventListener("error", onNativeError);
    // The desired subtitle selection, (re)applied on demand: native in-band
    // subtitle tracks materialize on <video>.textTracks asynchronously as the
    // master + rendition load, so we both apply immediately and re-apply here.
    let desired: number | null = null;
    const applyNative = () => selectNativeTextTrack(video, desired);
    // The desired audio rendition, (re)applied on demand for the same reason the
    // subtitle selection is: native in-band audio tracks materialize on
    // <video>.audioTracks asynchronously as the master + renditions load.
    let desiredAudio: number | null = null;
    const applyNativeAudio = () => selectNativeAudioTrack(video, desiredAudio);
    return {
      mode: "native",
      setTextTrack(index) {
        desired = index;
        applyNative();
      },
      setAudioTrack(index) {
        desiredAudio = index;
        applyNativeAudio();
      },
      detach() {
        if (detached) return;
        detached = true;
        video.removeEventListener("error", onNativeError);
        video.removeAttribute("src");
        try {
          video.load();
        } catch {
          // best-effort teardown
        }
      },
    };
  }

  const { default: HlsCtor } = await import("hls.js");
  if (!HlsCtor.isSupported()) {
    throw new Error("This browser cannot play HLS (no native HLS and no MSE).");
  }

  const hls: Hls = new HlsCtor({
    // Single-rendition, server-driven; we don't manage an ABR ladder (out of
    // scope). hls.js handles segment fetching, buffering, and seek/byte-range
    // against the same-origin, cookie-authenticated URLs.
    //
    // Buffer caps sized for COPIED-video sessions (ADR-0024), which deliver the
    // source bitrate — a 4K remux can run 60-80 Mbps. hls.js's defaults hold 30s
    // ahead (maxBufferSize is a floor, not a cap: max(maxBufferLength,
    // maxBufferSize/bitrate)) and an unlimited back-buffer; at remux bitrates
    // that blows straight past Chrome's ~150MB SourceBuffer quota, producing a
    // burst of mediaError/bufferFullError (QuotaExceededError) on every start.
    // ~15s ahead stays under quota at 80 Mbps while remaining plenty for a
    // same-network on-demand server, and a finite back-buffer lets MSE evict
    // played content instead of accumulating until the quota kills appends.
    maxBufferLength: 15,
    maxMaxBufferLength: 30,
    backBufferLength: 30,
  });
  hls.loadSource(url);
  hls.attachMedia(video);

  // The desired in-band subtitle selection. hls.js parses the master's SUBTITLES
  // renditions asynchronously, so we remember the wanted index and (re)apply it
  // both immediately and whenever the subtitle-track list updates — otherwise a
  // selection made before the renditions loaded would be lost. Default off (we
  // drive selection from the captions menu; a forced track is enabled explicitly).
  let desired: number | null = null;
  const applyHlsSubtitle = () => {
    if (desired == null) {
      hls.subtitleDisplay = false;
      hls.subtitleTrack = -1;
    } else {
      hls.subtitleDisplay = true;
      hls.subtitleTrack = desired;
    }
  };
  // Re-apply once the renditions are known (and on any later update).
  hls.on(HlsCtor.Events.SUBTITLE_TRACKS_UPDATED, applyHlsSubtitle);

  // The desired in-band AUDIO rendition (audio-streams/04). hls.js auto-selects the
  // master's DEFAULT audio rendition and re-selects it on (re)parse, so we remember
  // the wanted index and re-apply it on AUDIO_TRACKS_UPDATED — otherwise hls.js's
  // own default selection would stomp a non-default pick. null = take hls.js's
  // default (audio is never off, so we never force -1).
  let desiredAudio: number | null = null;
  const applyHlsAudio = () => {
    if (desiredAudio != null && desiredAudio >= 0) hls.audioTrack = desiredAudio;
  };
  hls.on(HlsCtor.Events.AUDIO_TRACKS_UPDATED, applyHlsAudio);

  // Error handling + recovery — the canonical hls.js integration we previously
  // never wired. hls.js does NOT log its errors to the console: they are events,
  // and an unhandled FATAL error silently stops ALL loading — the player freezes
  // with no console output, no network activity, and no trace anywhere (the
  // reported Chrome symptom). So: log EVERY error (details + fatality) under a
  // stable prefix diagnosable from the user's console, and apply the standard
  // recovery ladder for fatal ones — a network error restarts loading (transient
  // segment fetch failures: the on-demand server may briefly 404/timeout a segment
  // ffmpeg is still producing), a media error asks MSE to recover (once, then a
  // second time after swapping audio codecs, per the hls.js playbook). Anything
  // else, or a repeat within a few seconds, is genuinely dead — leave it stopped,
  // with the reason on the console instead of a silent freeze.
  let mediaRecoveries = 0;
  let lastRecovery = 0;
  // Consecutive FATAL network errors in a tight window. One is usually transient;
  // a run of them means the URLs are dead (the session was reaped/restarted), so
  // restarting the load can't help and we escalate to a re-negotiation instead.
  let networkFatals = 0;
  let lastNetworkFatal = 0;
  hls.on(HlsCtor.Events.ERROR, (_evt, data) => {
    const desc = `${data.type}/${data.details}${data.fatal ? " (FATAL)" : ""}`;
    // eslint-disable-next-line no-console
    console[data.fatal ? "error" : "warn"](`[player:hls] ${desc}`, data.reason ?? "");
    if (!data.fatal) return;
    const now = Date.now();
    switch (data.type) {
      case "networkError": {
        // A FATAL network error means hls.js already exhausted its own per-request
        // retries. The FIRST one is usually transient — a segment ffmpeg is still
        // producing, or a brief keepalive hiccup — so restart loading from the
        // current position. But when they keep coming in a tight window, the SESSION
        // itself is gone (reaped after a long pause, or a server restart): the
        // playlist AND every segment now 404, so startLoad() just loops on dead URLs.
        // Escalate to a full re-negotiation, which mints a fresh session + URL. With
        // no onSessionLost wired (no caller that can re-negotiate) keep the old
        // best-effort restart so behavior is unchanged.
        networkFatals = now - lastNetworkFatal < sessionLostWindowMs ? networkFatals + 1 : 1;
        lastNetworkFatal = now;
        if (networkFatals >= sessionLostThreshold && opts.onSessionLost) {
          networkFatals = 0;
          opts.onSessionLost();
          break;
        }
        hls.startLoad();
        break;
      }
      case "mediaError":
        if (mediaRecoveries === 0 || now - lastRecovery > 5000) {
          mediaRecoveries++;
          lastRecovery = now;
          hls.recoverMediaError();
        } else if (mediaRecoveries === 1) {
          mediaRecoveries++;
          lastRecovery = now;
          // Second strike inside the window: the hls.js-documented escalation.
          hls.swapAudioCodec();
          hls.recoverMediaError();
        }
        break;
      default:
        // Unrecoverable — leave it stopped; the console line above says why.
        break;
    }
  });

  let detached = false;
  return {
    mode: "hls.js",
    setTextTrack(index) {
      desired = index;
      applyHlsSubtitle();
    },
    setAudioTrack(index) {
      desiredAudio = index;
      applyHlsAudio();
    },
    detach() {
      if (detached) return;
      detached = true;
      try {
        hls.destroy();
      } catch {
        // best-effort teardown
      }
    },
  };
}

/** Apply a subtitle selection to a native-HLS <video>'s in-band TextTracks: show
 * the index-th subtitle/caption track (in track order, which mirrors the master
 * playlist's rendition order) and disable the rest; `null` disables all. Best-
 * effort — the tracks may not have materialized yet, in which case a later
 * re-apply (setTextTrack) catches them. */
function selectNativeTextTrack(video: HTMLVideoElement, index: number | null): void {
  const subs: TextTrack[] = [];
  for (let i = 0; i < video.textTracks.length; i++) {
    const tt = video.textTracks[i];
    if (tt.kind === "subtitles" || tt.kind === "captions") subs.push(tt);
  }
  subs.forEach((tt, i) => {
    tt.mode = index != null && i === index ? "showing" : "disabled";
  });
}

/** Enable the index-th native in-band audio track and disable the rest, on a
 * native-HLS <video> (audio-streams/04). The renditions appear on
 * <video>.audioTracks in master-playlist order (the same server order the index
 * is computed from). Best-effort: the list may not have materialized yet, in which
 * case a later re-apply (setAudioTrack) catches it. `audioTracks` is not in TS's
 * DOM lib, so we read it through a narrow cast. */
function selectNativeAudioTrack(video: HTMLVideoElement, index: number | null): void {
  const tracks = (video as unknown as { audioTracks?: NativeAudioTrackList }).audioTracks;
  if (!tracks) return;
  for (let i = 0; i < tracks.length; i++) {
    tracks[i].enabled = index != null && i === index;
  }
}
