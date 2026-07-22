import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAsync } from "../browse/useAsync";
import { episodeContextLabel } from "../browse/episodeLabel";
import Poster from "../browse/Poster";
import { albumArtworkUrl } from "../browse/albumArt";
import { attachHls, type HlsAttachment } from "./hls";
import { usePlayerSession, type PlayerPreference } from "./usePlayerSession";
import {
  applySubtitleSelection,
  defaultTrackId,
  deliverableTextTrackIndex,
  orderedImageTracks,
  orderedTextTracks,
  preferredSubtitleLang,
} from "./subtitles";
import {
  audioRenditionIndex,
  initialAudioId,
  orderedAudioStreams,
  preferredAudioLang,
} from "./audio";
import { initialVideoId, orderedVideoStreams } from "./video";
import type {
  AudioStream,
  SubtitleCandidate,
  SubtitleTrack,
  VideoStream,
} from "../api/types";
import { usePlaybackPrefs } from "./usePlaybackPrefs";
import { loadPreferenceForTitle } from "./playbackPreference";
import { resolvePlayback } from "./playbackResolver";
import { usePlaybackTransport } from "./transport";
import QueuePanel from "./QueuePanel";
import { useQueue } from "./queue/useQueue";
import type { QueueEntry } from "./queue/model";
import { formatTimecode } from "../time";

// The Now Playing bar (now-playing-bar/01): the persistent, shell-owned player.
// Mounted ONCE in App outside <Routes>, so it survives navigation — playback keeps
// going as the user browses (ADR-0018). It owns the single media element and the
// single Playback session, driven by the global `useQueue` store: it plays
// `queue.current`, and each current-entry change re-keys the inner player so the
// prior session is ended cleanly BEFORE the next Title is negotiated (the
// end-before-next invariant the retired PlayerScreen relied on).
//
// This slice keeps the player core behavior unchanged (negotiate → HLS/progressive
// → resume-seek → progress report → end), just relocated: the transport is
// play/pause + prev/next, the seekable progress bar and ±10s skips (slice 02); the
// queue button opens a drawer reusing the QueuePanel, and Clear queue is the
// stop-and-dismiss exit.
//
// Slice 03 adds the VIDEO SURFACE STATE MACHINE: a video plays in a large immersive
// `stage` overlay (bar still docked below), demotes to a custom corner `pip` window
// when the user navigates to a browse route (still playing), re-expands to `stage`,
// and goes fullscreen on the STAGE WRAPPER (so the app's overlay controls survive).
// Audio — and a restored-paused video with no surface shown — is `bar-only`. The
// crux is that all three surfaces reuse the SAME <video> + Playback session: the
// element is mounted once in a single stable wrapper and only its `data-surface`
// attribute/CSS class change, so React never re-mounts it (no re-buffer/re-stream).

/** HLS tiers play via hls.js / native HLS; directPlay uses a progressive src. */
function isHlsTier(tier: string): boolean {
  return tier === "directStream" || tier === "transcode";
}

/** Read the logged-in user's id SYNCHRONOUSLY from the same storage the auth layer
 * hydrates from (mirrors usePlaybackPrefs). The player negotiates on its first
 * render — before the async auth hydrate — so the per-user Playback preference must
 * be keyed off a value available NOW, or a configured Title would negotiate Auto
 * once, then re-negotiate once auth lands. Anon (`null`) gets its own bucket. */
function persistedUserId(): string | null {
  try {
    const raw = window.localStorage.getItem("juicebox.user");
    if (!raw) return null;
    const u = JSON.parse(raw) as { id?: unknown };
    return typeof u?.id === "string" ? u.id : null;
  } catch {
    return null;
  }
}

/** A friendly noun for a Title's media kind (used as the label's degraded fallback
 * when the detail fetch fails). */
function kindLabel(kind: string): string {
  switch (kind) {
    case "movie":
      return "Movie";
    case "episode":
      return "Episode";
    case "track":
      return "Track";
    default:
      return kind;
  }
}

/** A Track is audio-only (no video surface); everything else (Movie/Episode) shows
 * the inline video stage. */
function isVideoKind(kind: string): boolean {
  return kind !== "track";
}

/** How far the ±10s skip buttons jump (video only). */
const SKIP_SECONDS = 10;

/** How much the ↑/↓ volume keyboard shortcuts nudge the volume (0–1). */
const VOLUME_STEP = 0.05;

/** Frame-step size for the , / . shortcuts. The decision carries no frame rate, so
 * we assume ~30fps — close enough to nudge toward a specific frame while paused. */
const FRAME_SECONDS = 1 / 30;

/** Minimum gap between vanished-session recoveries. The HLS layer only escalates
 * after persistent fatal errors, but a genuinely unplayable stream could keep
 * failing right after each re-negotiation — this floor keeps that from spinning
 * the negotiate path faster than a viewer-perceptible re-buffer. */
const RECOVER_MIN_INTERVAL_MS = 5_000;

/** A now-playing label (song/show/movie title or artist name). Renders a Link
 * when a navigation target is known (the detail with parent context has loaded),
 * otherwise a plain span — so the label degrades gracefully while loading / on a
 * failed detail fetch. Keeps the label's own class (and its color) either way. */
function NowPlayingLabel({
  className,
  testId,
  to,
  children,
}: {
  className: string;
  testId: string;
  to?: string;
  children: ReactNode;
}) {
  if (to) {
    return (
      <Link
        to={to}
        className={`${className} now-playing-label-link`}
        data-testid={testId}
      >
        {children}
      </Link>
    );
  }
  return (
    <span className={className} data-testid={testId}>
      {children}
    </span>
  );
}

/** The circular play glyph (filled disc + triangle), sized to the font via em and
 * tinted by `currentColor` so it matches the transport's button color. */
function PlayIcon() {
  return (
    <svg
      className="now-playing-play-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 45 0 C 20.147 0 0 20.147 0 45 c 0 24.853 20.147 45 45 45 s 45 -20.147 45 -45 C 90 20.147 69.853 0 45 0 z M 62.251 46.633 L 37.789 60.756 c -1.258 0.726 -2.829 -0.181 -2.829 -1.633 V 30.877 c 0 -1.452 1.572 -2.36 2.829 -1.634 l 24.461 14.123 C 63.508 44.092 63.508 45.907 62.251 46.633 z"
      />
    </svg>
  );
}

/** The circular pause glyph (filled disc with two cut-out bars), matching
 * {@link PlayIcon}'s sizing/tint. */
function PauseIcon() {
  return (
    <svg
      className="now-playing-play-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 45 0 C 20.147 0 0 20.147 0 45 c 0 24.853 20.147 45 45 45 s 45 -20.147 45 -45 C 90 20.147 69.853 0 45 0 z M 40.899 65.5 c 0 1.104 -0.896 2 -2 2 H 25.523 c -1.104 0 -2 -0.896 -2 -2 v -41 c 0 -1.104 0.896 -2 2 -2 h 13.376 c 1.104 0 2 0.896 2 2 V 65.5 z M 66.477 65.5 c 0 1.104 -0.896 2 -2 2 H 51.101 c -1.104 0 -2 -0.896 -2 -2 v -41 c 0 -1.104 0.896 -2 2 -2 h 13.376 c 1.104 0 2 0.896 2 2 V 65.5 z"
      />
    </svg>
  );
}

/** A rounded square — the stop glyph. Uses the smaller `now-playing-icon` sizing
 * (like prev/next), not the prominent circular play/pause disc. */
function StopIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <rect fill="currentColor" x="18" y="18" width="54" height="54" rx="8" ry="8" />
    </svg>
  );
}

/** Skip-to-previous (bar + double-triangle), tinted by `currentColor` and sized
 * to the font. Its {@link NextIcon} twin is the mirror image. */
function PrevIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 36.63 55.015 l 30.724 -30.724 c 0.769 -0.769 0.769 -2.015 0 -2.784 l -8.591 -8.591 c -0.769 -0.769 -2.015 -0.769 -2.784 0 L 25.256 43.641 c -0.769 0.769 -0.769 2.015 0 2.784 l 8.591 8.591 C 34.616 55.784 35.862 55.784 36.63 55.015 z"
      />
      <path
        fill="currentColor"
        d="M 36.63 34.985 l 30.724 30.724 c 0.769 0.769 0.769 2.015 0 2.784 l -8.591 8.591 c -0.769 0.769 -2.015 0.769 -2.784 0 L 25.256 46.359 c -0.769 -0.769 -0.769 -2.015 0 -2.784 l 8.591 -8.591 C 34.616 34.216 35.862 34.216 36.63 34.985 z"
      />
      <path
        fill="currentColor"
        d="M 44.581 53.043 l 43.451 0 c 1.087 0 1.968 -0.881 1.968 -1.968 V 38.925 c 0 -1.087 -0.881 -1.968 -1.968 -1.968 l -43.451 0 c -1.087 0 -1.968 0.881 -1.968 1.968 v 12.149 C 42.613 52.162 43.494 53.043 44.581 53.043 z"
      />
      <path
        fill="currentColor"
        d="M 0 14.651 l 0 60.699 c 0 1.276 1.035 2.311 2.311 2.311 h 11.464 c 1.276 0 2.311 -1.035 2.311 -2.311 l 0 -60.699 c 0 -1.276 -1.035 -2.311 -2.311 -2.311 l -11.464 0 C 1.035 12.34 0 13.374 0 14.651 z"
      />
    </svg>
  );
}

/** Skip-to-next (bar + double-triangle) — the mirror of {@link PrevIcon}. */
function NextIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 53.37 55.015 L 22.645 24.291 c -0.769 -0.769 -0.769 -2.015 0 -2.784 l 8.591 -8.591 c 0.769 -0.769 2.015 -0.769 2.784 0 l 30.724 30.724 c 0.769 0.769 0.769 2.015 0 2.784 l -8.591 8.591 C 55.384 55.784 54.138 55.784 53.37 55.015 z"
      />
      <path
        fill="currentColor"
        d="M 53.37 34.985 L 22.645 65.709 c -0.769 0.769 -0.769 2.015 0 2.784 l 8.591 8.591 c 0.769 0.769 2.015 0.769 2.784 0 l 30.724 -30.724 c 0.769 -0.769 0.769 -2.015 0 -2.784 l -8.591 -8.591 C 55.384 34.216 54.138 34.216 53.37 34.985 z"
      />
      <path
        fill="currentColor"
        d="M 45.419 53.043 l -43.451 0 C 0.881 53.043 0 52.162 0 51.075 l 0 -12.149 c 0 -1.087 0.881 -1.968 1.968 -1.968 l 43.451 0 c 1.087 0 1.968 0.881 1.968 1.968 v 12.149 C 47.387 52.162 46.506 53.043 45.419 53.043 z"
      />
      <path
        fill="currentColor"
        d="M 90 14.651 l 0 60.699 c 0 1.276 -1.035 2.311 -2.311 2.311 H 76.225 c -1.276 0 -2.311 -1.035 -2.311 -2.311 l 0 -60.699 c 0 -1.276 1.035 -2.311 2.311 -2.311 l 11.464 0 C 88.965 12.34 90 13.374 90 14.651 z"
      />
    </svg>
  );
}

/** Shuffle (crossing arrows), tinted by `currentColor` and sized to the font. */
function ShuffleIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 16.787 66.579 H 5 c -2.761 0 -5 -2.238 -5 -5 s 2.239 -5 5 -5 h 11.787 c 6.839 0 13.362 -3.651 18.367 -10.281 l 6.507 -8.62 c 6.838 -9.061 16.441 -14.257 26.348 -14.257 h 14.246 c 2.762 0 5 2.239 5 5 s -2.238 5 -5 5 H 68.008 c -6.84 0 -13.362 3.651 -18.366 10.281 l -6.507 8.621 C 36.296 61.383 26.692 66.579 16.787 66.579 z"
      />
      <path
        fill="currentColor"
        d="M 82.254 66.579 H 70.468 c -9.905 0 -19.509 -5.196 -26.348 -14.256 l -6.507 -8.621 c -5.004 -6.63 -11.527 -10.281 -18.367 -10.281 H 5 c -2.761 0 -5 -2.239 -5 -5 s 2.239 -5 5 -5 h 14.246 c 9.906 0 19.509 5.196 26.348 14.257 l 6.507 8.62 c 5.004 6.63 11.526 10.281 18.366 10.281 h 11.786 c 2.762 0 5 2.238 5 5 S 85.016 66.579 82.254 66.579 z"
      />
      <polygon fill="currentColor" points="77.6,35.82 85,28.42 77.6,21.02" />
      <path
        fill="currentColor"
        d="M 77.597 40.824 c -0.645 0 -1.294 -0.125 -1.912 -0.38 c -1.869 -0.774 -3.087 -2.597 -3.087 -4.62 V 21.019 c 0 -2.022 1.218 -3.846 3.087 -4.62 c 1.866 -0.772 4.019 -0.347 5.448 1.084 l 7.402 7.402 c 1.953 1.953 1.953 5.119 0.001 7.071 l -7.402 7.403 C 80.177 40.316 78.897 40.824 77.597 40.824 z"
      />
      <polygon fill="currentColor" points="77.6,68.98 85,61.58 77.6,54.18" />
      <path
        fill="currentColor"
        d="M 77.597 73.981 c -0.645 0 -1.294 -0.124 -1.912 -0.381 c -1.869 -0.773 -3.087 -2.597 -3.087 -4.619 V 54.177 c 0 -2.022 1.218 -3.846 3.087 -4.619 c 1.866 -0.775 4.019 -0.348 5.448 1.084 l 7.402 7.402 c 1.953 1.952 1.953 5.118 0 7.07 l -7.402 7.402 C 80.177 73.474 78.897 73.981 77.597 73.981 z"
      />
    </svg>
  );
}

/** Repeat (looping arrow), tinted by `currentColor` and sized to the font. The
 * repeat-ONE vs repeat-ALL distinction is carried by an adjacent "1" badge in the
 * button, not the icon. */
function RepeatIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 61.74 78.027 H 28.26 C 12.677 78.027 0 65.35 0 49.768 c 0 -15.583 12.677 -28.26 28.26 -28.26 h 44.389 c 2.762 0 5 2.239 5 5 s -2.238 5 -5 5 H 28.26 c -10.068 0 -18.26 8.191 -18.26 18.26 c 0 10.068 8.191 18.26 18.26 18.26 h 33.48 c 10.068 0 18.26 -8.191 18.26 -18.26 c 0 -2.419 -0.464 -4.766 -1.378 -6.977 c -1.056 -2.551 0.157 -5.476 2.709 -6.531 c 2.552 -1.058 5.476 0.157 6.531 2.709 C 89.281 42.397 90 46.03 90 49.768 C 90 65.35 77.322 78.027 61.74 78.027 z"
      />
      <path
        fill="currentColor"
        d="M 59.43 41.042 c -1.552 0 -3.082 -0.721 -4.06 -2.076 c -1.615 -2.24 -1.108 -5.365 1.131 -6.98 l 7.599 -5.479 l -7.599 -5.479 c -2.239 -1.615 -2.746 -4.74 -1.131 -6.98 c 1.616 -2.241 4.741 -2.745 6.98 -1.131 l 13.223 9.535 c 1.303 0.94 2.075 2.449 2.075 4.056 s -0.772 3.116 -2.075 4.056 l -13.223 9.535 C 61.466 40.735 60.443 41.042 59.43 41.042 z"
      />
    </svg>
  );
}

/** Queue (a list with a play-cursor), tinted by `currentColor` and sized to the
 * font. Replaces the "☰ Queue" text button's icon+label. */
function QueueIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 86.5 52.054 H 51.816 c -1.933 0 -3.5 1.567 -3.5 3.5 s 1.567 3.5 3.5 3.5 H 86.5 c 1.933 0 3.5 -1.567 3.5 -3.5 S 88.433 52.054 86.5 52.054 z"
      />
      <path
        fill="currentColor"
        d="M 86.5 73.161 H 41.22 c -1.933 0 -3.5 1.567 -3.5 3.5 s 1.567 3.5 3.5 3.5 H 86.5 c 1.933 0 3.5 -1.567 3.5 -3.5 S 88.433 73.161 86.5 73.161 z"
      />
      <path
        fill="currentColor"
        d="M 41.22 16.839 H 86.5 c 1.933 0 3.5 -1.567 3.5 -3.5 s -1.567 -3.5 -3.5 -3.5 H 41.22 c -1.933 0 -3.5 1.567 -3.5 3.5 S 39.287 16.839 41.22 16.839 z"
      />
      <path
        fill="currentColor"
        d="M 86.5 30.946 H 51.816 c -1.933 0 -3.5 1.567 -3.5 3.5 s 1.567 3.5 3.5 3.5 H 86.5 c 1.933 0 3.5 -1.567 3.5 -3.5 S 88.433 30.946 86.5 30.946 z"
      />
      <path
        fill="currentColor"
        d="M 36.934 58.707 c -1.367 -1.367 -3.583 -1.367 -4.95 0 l -9.504 9.504 V 21.789 l 9.504 9.505 c 1.367 1.366 3.583 1.366 4.95 0 c 1.367 -1.367 1.367 -3.583 0 -4.95 L 21.454 10.864 c -0.005 -0.005 -0.011 -0.008 -0.016 -0.013 c -0.632 -0.625 -1.5 -1.012 -2.46 -1.012 c -0.967 0 -1.842 0.392 -2.475 1.026 L 1.025 26.344 c -1.367 1.367 -1.367 3.583 0 4.95 c 0.683 0.683 1.579 1.025 2.475 1.025 s 1.792 -0.342 2.475 -1.025 l 9.504 -9.504 v 46.422 l -9.504 -9.504 c -1.366 -1.366 -3.583 -1.367 -4.95 0 c -1.367 1.366 -1.367 3.583 0 4.949 l 15.479 15.479 c 0.002 0.002 0.004 0.003 0.005 0.004 c 0.16 0.159 0.335 0.303 0.523 0.429 c 0.049 0.033 0.104 0.054 0.155 0.084 c 0.145 0.087 0.29 0.173 0.447 0.239 c 0.069 0.029 0.143 0.042 0.213 0.066 c 0.145 0.05 0.288 0.103 0.44 0.134 c 0.226 0.046 0.458 0.07 0.692 0.07 s 0.466 -0.024 0.692 -0.07 c 0.153 -0.031 0.296 -0.084 0.44 -0.134 c 0.07 -0.024 0.144 -0.038 0.213 -0.066 c 0.157 -0.066 0.302 -0.152 0.447 -0.239 c 0.05 -0.03 0.106 -0.051 0.155 -0.084 c 0.188 -0.126 0.363 -0.27 0.523 -0.429 c 0.002 -0.002 0.004 -0.003 0.005 -0.004 l 15.479 -15.479 C 38.3 62.29 38.3 60.073 36.934 58.707 z"
      />
    </svg>
  );
}

/** Closed-captions glyph (a rounded rect with "cc"), tinted by `currentColor` and
 * sized to the font — the captions-menu button icon. */
function CaptionsIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 78 16 H 12 c -4.418 0 -8 3.582 -8 8 v 42 c 0 4.418 3.582 8 8 8 h 66 c 4.418 0 8 -3.582 8 -8 V 24 c 0 -4.418 -3.582 -8 -8 -8 z M 41 54.5 c 0 1.104 -0.896 2 -2 2 H 24 c -4.418 0 -8 -3.582 -8 -8 V 41.5 c 0 -4.418 3.582 -8 8 -8 h 15 c 1.104 0 2 0.896 2 2 v 3 c 0 1.104 -0.896 2 -2 2 H 25 c -0.552 0 -1 0.448 -1 1 v 5 c 0 0.552 0.448 1 1 1 h 14 c 1.104 0 2 0.896 2 2 v 3 z M 74 54.5 c 0 1.104 -0.896 2 -2 2 H 57 c -4.418 0 -8 -3.582 -8 -8 V 41.5 c 0 -4.418 3.582 -8 8 -8 h 15 c 1.104 0 2 0.896 2 2 v 3 c 0 1.104 -0.896 2 -2 2 H 58 c -0.552 0 -1 0.448 -1 1 v 5 c 0 0.552 0.448 1 1 1 h 14 c 1.104 0 2 0.896 2 2 v 3 z"
      />
    </svg>
  );
}

/** Audio-menu glyph: a speaker with a musical note, tinted by `currentColor` and
 * sized to the font — the Audio-menu button icon (distinct from the volume speaker
 * and the "cc" captions rect). */
function AudioIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 44 12 c -0.5 0 -1 0.18 -1.4 0.53 L 22.9 30 H 8 c -1.1 0 -2 0.9 -2 2 v 26 c 0 1.1 0.9 2 2 2 h 14.9 l 19.7 17.47 c 0.58 0.52 1.42 0.66 2.14 0.35 C 45.5 77.5 46 76.8 46 76 V 14 c 0 -0.8 -0.5 -1.5 -1.2 -1.8 C 44.5 12.05 44.25 12 44 12 z"
      />
      <path
        fill="currentColor"
        d="M 84 20 c 0 -1.6 -1.5 -2.8 -3 -2.4 l -18 4.8 c -1.3 0.35 -2.2 1.5 -2.2 2.9 v 24.4 c -1.4 -0.75 -3.1 -1.1 -4.9 -0.9 c -4.3 0.5 -7.6 4 -7.6 8 c 0 4.4 3.9 7.8 8.5 7.2 c 4.1 -0.5 7.2 -3.9 7.2 -7.9 V 32 l 12 -3.2 v 14.5 c -1.4 -0.75 -3.1 -1.1 -4.9 -0.9 c -4.3 0.5 -7.6 4 -7.6 8 c 0 4.4 3.9 7.8 8.5 7.2 c 4.1 -0.5 7.2 -3.9 7.2 -7.9 V 20 z"
      />
    </svg>
  );
}

/** Video-menu glyph: a film frame (a rounded rect with sprocket-hole slots), tinted
 * by `currentColor` and sized to the font — the Video-menu button icon (distinct
 * from the Audio speaker and the "cc" captions rect). */
function VideoIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 18 18 C 13.6 18 10 21.6 10 26 v 38 c 0 4.4 3.6 8 8 8 h 54 c 4.4 0 8 -3.6 8 -8 V 26 c 0 -4.4 -3.6 -8 -8 -8 z m 2 8 h 6 v 6 h -6 z m 44 0 h 6 v 6 h -6 z M 20 40 h 6 v 6 h -6 z m 44 0 h 6 v 6 h -6 z M 20 54 h 6 v 6 h -6 z m 44 0 h 6 v 6 h -6 z M 36 30 l 20 12 l -20 12 z"
      />
    </svg>
  );
}

/** Speaker with sound waves — the UNMUTED volume icon, tinted by `currentColor`
 * and sized to the font. */
function VolumeOnIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 64.293 90 c -0.493 0 -0.979 -0.183 -1.356 -0.53 L 35.986 64.59 H 6.526 c -1.105 0 -2 -0.896 -2 -2 V 27.41 c 0 -1.105 0.896 -2 2 -2 h 29.46 L 62.937 0.53 c 0.585 -0.539 1.433 -0.679 2.158 -0.362 C 65.823 0.486 66.293 1.205 66.293 2 V 88 c 0 0.794 -0.47 1.514 -1.198 1.832 C 64.837 89.946 64.565 90 64.293 90 z"
      />
      <path
        fill="currentColor"
        d="M 76.42 73.908 c -0.467 0 -0.937 -0.163 -1.315 -0.494 c -0.832 -0.728 -0.916 -1.991 -0.189 -2.822 C 78.9 66.037 81.474 55.991 81.474 45 S 78.9 23.963 74.915 19.408 c -0.727 -0.832 -0.643 -2.095 0.189 -2.822 c 0.83 -0.728 2.095 -0.643 2.822 0.188 c 4.655 5.323 7.547 16.138 7.547 28.225 c 0 12.087 -2.892 22.903 -7.547 28.225 C 77.532 73.677 76.977 73.908 76.42 73.908 z"
      />
      <path
        fill="currentColor"
        d="M 72.02 62.623 c -0.321 0 -0.647 -0.077 -0.949 -0.241 c -0.972 -0.525 -1.333 -1.739 -0.808 -2.71 c 1.851 -3.421 2.955 -8.906 2.955 -14.672 c 0 -5.765 -1.104 -11.25 -2.955 -14.672 c -0.525 -0.972 -0.164 -2.186 0.808 -2.711 c 0.969 -0.526 2.186 -0.165 2.71 0.808 c 2.185 4.039 3.438 10.08 3.438 16.575 c 0 6.495 -1.253 12.536 -3.438 16.574 C 73.419 62.243 72.731 62.623 72.02 62.623 z"
      />
    </svg>
  );
}

/** Speaker with an "x" — the MUTED volume icon, tinted by `currentColor` and
 * sized to the font. */
function VolumeOffIcon() {
  return (
    <svg
      className="now-playing-icon"
      viewBox="0 0 90 90"
      aria-hidden="true"
      focusable="false"
    >
      <path
        fill="currentColor"
        d="M 56.38 87.734 c -0.74 0 -1.471 -0.273 -2.036 -0.796 L 29.772 64.254 H 3 c -1.657 0 -3 -1.343 -3 -3 V 28.747 c 0 -1.657 1.343 -3 3 -3 h 26.772 L 54.344 3.062 c 0.876 -0.809 2.146 -1.022 3.238 -0.544 c 1.092 0.478 1.797 1.557 1.797 2.748 v 79.468 c 0 1.191 -0.705 2.271 -1.797 2.748 C 57.195 87.652 56.786 87.734 56.38 87.734 z"
      />
      <path
        fill="currentColor"
        d="M 81.658 45 l 7.463 -7.464 c 1.172 -1.171 1.172 -3.071 0 -4.243 c -1.172 -1.171 -3.07 -1.171 -4.242 0 l -7.463 7.464 l -7.463 -7.464 c -1.172 -1.172 -3.07 -1.171 -4.242 0 c -1.172 1.172 -1.172 3.071 0 4.243 L 73.173 45 l -7.463 7.464 c -1.172 1.172 -1.172 3.071 0 4.242 c 0.586 0.586 1.354 0.879 2.121 0.879 s 1.535 -0.293 2.121 -0.879 l 7.463 -7.464 l 7.463 7.464 c 0.586 0.586 1.354 0.879 2.121 0.879 s 1.535 -0.293 2.121 -0.879 c 1.172 -1.171 1.172 -3.07 0 -4.242 L 81.658 45 z"
      />
    </svg>
  );
}

/** The video's presentation surface (slice 03), held in the bar (NOT the Queue
 * model). `stage` = the immersive in-app overlay; `pip` = the custom movable corner
 * window; `bar-only` = no surface (audio, or a video that's neither staged nor
 * PiP'd — e.g. a reload-restored paused entry). All three reuse the one element. */
type VideoSurface = "stage" | "pip" | "bar-only";

export default function NowPlayingBar() {
  const queue = useQueue();
  const location = useLocation();
  const [drawerOpen, setDrawerOpen] = useState(false);

  // Whether the current entry should auto-play. On the bar's FIRST render a present
  // current entry is a RESTORED Queue (hydrated from sessionStorage on reload): it
  // loads seeked to its resume position but PAUSED, because browsers block
  // autoplay-with-sound without a user gesture. Any entry the store points at AFTER
  // mount (a Play affordance / prev / next / advance — all user-initiated) auto-plays.
  const initialEntryId = useRef<string | null | undefined>(undefined);
  if (initialEntryId.current === undefined) {
    initialEntryId.current = queue.current?.entryId ?? null;
  }

  // Escape closes the queue drawer (a keyboard/accessibility affordance alongside
  // the × button and the backdrop click). Declared before the early return so the
  // hook order stays stable across renders.
  useEffect(() => {
    if (!drawerOpen) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") setDrawerOpen(false);
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [drawerOpen]);

  // Clearing the queue (Clear queue) empties it → the bar unmounts its content, but
  // this component stays mounted, so `drawerOpen` would linger and pop the drawer
  // back open when the next media starts the bar again. Close it when the queue goes
  // empty so a fresh playback never re-opens a stale drawer.
  const entry = queue.current;
  useEffect(() => {
    if (!entry) setDrawerOpen(false);
  }, [entry]);

  // ── Video surface state machine (stage ↔ pip ↔ bar-only) ───────────────────
  // Slice 03's surface lives HERE, in the persistent bar — NOT in the per-entry
  // player core below (which re-keys on every advance). Holding it here is what
  // makes the presentation SURVIVE a queue advance: when an Episode ends and the
  // next one auto-plays, the video continues in the SAME view it was in
  // (stage→stage, pip→pip, and — since fullscreen targets this bar — also
  // fullscreen→fullscreen), with the black stage backdrop held continuously across
  // the swap (no flash of the page behind). The <video>+session still re-key per
  // entry; only the surface and its history/fullscreen/chrome machinery persist.
  //
  // (It also fixes a latent race: when the core re-keyed, the outgoing instance's
  // unmount ran history.back() to balance its pushed stage entry, and that async
  // popstate landed on the freshly-mounted instance — which read it as a Back and
  // collapsed the new stage to pip. With the history entry owned here, there is no
  // per-advance push/pop churn, so nothing collapses the next episode's stage.)
  const video = entry ? isVideoKind(entry.title.kind) : false;
  const [surface, setSurface] = useState<VideoSurface>("bar-only");
  const surfaceRef = useRef(surface);
  surfaceRef.current = surface;
  // The bar element — the fullscreen target: its subtree holds BOTH the video stage
  // AND the transport, so the docked controls remain the control surface in FS.
  const barRef = useRef<HTMLDivElement | null>(null);
  // Whether we currently hold a pushed (non-content) history entry for the stage.
  const pushedHistoryRef = useRef(false);
  // The entry id the surface was last reconciled against (see the render-phase
  // reconcile below the early return).
  const surfaceEntryRef = useRef<string | null>(null);
  // Custom PiP offset (a pointer drag nudges the corner box), held here too so a
  // dragged-out pip keeps its position across an advance.
  const [pipOffset, setPipOffset] = useState<{ x: number; y: number } | null>(null);
  const dragRef = useRef<{ startX: number; startY: number; baseX: number; baseY: number } | null>(
    null,
  );

  // Opening the immersive stage pushes a single non-content history entry (same
  // URL, so react-router never sees a navigation) — this is what lets the browser
  // Back button / the mobile back-gesture behave as "exit the immersive view."
  useEffect(() => {
    if (surface !== "stage" || pushedHistoryRef.current) return;
    window.history.pushState({ nowPlayingStage: true }, "");
    pushedHistoryRef.current = true;
  }, [surface]);

  // Back / Esc / back-gesture → collapse stage → pip WITHOUT stopping playback. On
  // unmount (the whole bar going away — queue cleared / logout), if we still hold
  // the pushed entry on the stage, we pop it so history entries don't accumulate.
  useEffect(() => {
    function onPopState() {
      if (!pushedHistoryRef.current) return; // not our stage entry — leave it be
      pushedHistoryRef.current = false;
      if (surfaceRef.current === "stage") setSurface("pip");
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== "Escape" || surfaceRef.current !== "stage") return;
      // In fullscreen, let the browser's Esc just exit fullscreen (staying on the
      // stage); only a WINDOWED-stage Esc collapses to the mini player. At keydown
      // time the fullscreen exit hasn't happened yet, so this still reads as set.
      if (document.fullscreenElement) return;
      // Treat Escape like Back so both go through the same pushed-entry path.
      if (pushedHistoryRef.current) window.history.back();
      else setSurface("pip");
    }
    window.addEventListener("popstate", onPopState);
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("popstate", onPopState);
      window.removeEventListener("keydown", onKeyDown);
      if (pushedHistoryRef.current && surfaceRef.current === "stage") {
        pushedHistoryRef.current = false;
        window.history.back();
      }
    };
  }, []);

  // Navigating to a browse route while on the stage demotes the video to the PiP
  // window (it keeps playing). The pushState above uses the SAME URL, so it never
  // trips this — only a genuine route change does.
  const lastPathRef = useRef(location.pathname);
  useEffect(() => {
    if (location.pathname === lastPathRef.current) return;
    lastPathRef.current = location.pathname;
    if (surfaceRef.current === "stage") setSurface("pip");
  }, [location.pathname]);

  // Immersive auto-hide chrome: while a video fills the stage (windowed OR
  // fullscreen), the AppHeader and the Now Playing bar slide out of view after the
  // pointer goes idle, then slide back in on any pointer / key activity — so the
  // view is just video, uninterrupted, until the user reaches for a control. Driven
  // by two body classes the global CSS keys off of. Managed imperatively (no
  // per-mousemove React render); a 3s idle timer hides.
  useEffect(() => {
    if (!(video && surface === "stage")) return;
    const body = document.body;
    body.classList.add("np-immersive");
    let idle = 0;
    const hide = () => body.classList.add("np-controls-hidden");
    const reveal = () => {
      body.classList.remove("np-controls-hidden");
      window.clearTimeout(idle);
      idle = window.setTimeout(hide, 3000);
    };
    reveal(); // start revealed, then arm the idle timer
    window.addEventListener("pointermove", reveal);
    window.addEventListener("pointerdown", reveal);
    window.addEventListener("keydown", reveal);
    return () => {
      window.clearTimeout(idle);
      window.removeEventListener("pointermove", reveal);
      window.removeEventListener("pointerdown", reveal);
      window.removeEventListener("keydown", reveal);
      body.classList.remove("np-immersive", "np-controls-hidden");
    };
  }, [video, surface]);

  // Fullscreen wraps the BAR — the common ancestor of the video stage AND the bar's
  // control row — not the bare <video> or the stage wrapper. That way the Now
  // Playing bar's transport is inside the fullscreen subtree and remains the control
  // surface in fullscreen. Invoked from pip/bar it first promotes to the stage.
  function toggleFullscreen() {
    const bar = barRef.current;
    if (!bar) return;
    if (document.fullscreenElement) {
      void document.exitFullscreen?.();
      return;
    }
    if (surfaceRef.current !== "stage") setSurface("stage");
    void bar.requestFullscreen?.();
  }

  // Collapse the stage the same way Back does (consume the pushed history entry).
  // If we're fullscreen (the bar is the fullscreen element), leave fullscreen first
  // so shrinking to the corner PiP doesn't strand us in a fullscreened bar.
  function collapseStage() {
    if (document.fullscreenElement) void document.exitFullscreen?.();
    if (pushedHistoryRef.current) window.history.back();
    else setSurface("pip");
  }

  // Custom PiP is a MOVABLE corner box: a pointer drag on the window nudges it by an
  // offset applied as a transform. Kept deliberately minimal (no bounds math).
  function onPipPointerDown(e: ReactPointerEvent) {
    if (surface !== "pip") return;
    dragRef.current = {
      startX: e.clientX,
      startY: e.clientY,
      baseX: pipOffset?.x ?? 0,
      baseY: pipOffset?.y ?? 0,
    };
    e.currentTarget.setPointerCapture?.(e.pointerId);
  }
  function onPipPointerMove(e: ReactPointerEvent) {
    const d = dragRef.current;
    if (!d) return;
    setPipOffset({ x: d.baseX + (e.clientX - d.startX), y: d.baseY + (e.clientY - d.startY) });
  }
  function onPipPointerUp(e: ReactPointerEvent) {
    dragRef.current = null;
    e.currentTarget.releasePointerCapture?.(e.pointerId);
  }

  // The bar is absent when nothing is queued and on the unauthenticated routes
  // (login / first-run setup), where playback makes no sense.
  const onAuthRoute =
    location.pathname.startsWith("/login") || location.pathname.startsWith("/setup");
  if (!entry || onAuthRoute) return null;

  const autoPlay = entry.entryId !== initialEntryId.current;

  // Reconcile the surface to the current entry, in the RENDER phase so there's no
  // one-frame flash (the sanctioned "adjust state when a prop changes" pattern). A
  // video KEEPS an existing stage/pip surface across an entry change — this is what
  // continues an auto-advanced Episode in the SAME view the previous one ended in;
  // coming from bar-only it opens the stage for a user-initiated entry (a Play /
  // advance → autoPlay) and stays hidden for a cold reload-restored paused one.
  // Audio (a Track) is always bar-only.
  if (surfaceEntryRef.current !== entry.entryId) {
    surfaceEntryRef.current = entry.entryId;
    setSurface((cur) => {
      if (!video) return "bar-only";
      if (cur === "stage" || cur === "pip") return cur;
      return autoPlay ? "stage" : "bar-only";
    });
  }

  return (
    <>
      {/* A spacer the height of the fixed bar so page content never hides behind
          it — no per-screen shell restructuring needed. */}
      <div className="now-playing-spacer" aria-hidden="true" />
      <div className="now-playing-bar" data-testid="now-playing-bar" ref={barRef}>
        {/* Persistent black stage backdrop, owned by the bar (not the re-keyed core):
            while the video is staged it fills the viewport BEHIND the player core, so
            when a queue advance briefly unmounts the core to negotiate the next entry,
            the screen stays black — the fluid, uninterrupted hand-off — instead of
            flashing the page behind, until the next video paints over it. */}
        {video && surface === "stage" && (
          <div className="now-playing-stage-backdrop" aria-hidden="true" />
        )}
        {/* The player core is KEYED by the current entry id: a change re-mounts it,
            ending the prior session (its unmount effect) before the next negotiates.
            The surface (above) is held OUTSIDE this key, so it persists across the
            re-mount and the video continues in the same view. */}
        <CurrentPlayer
          key={entry.entryId}
          entry={entry}
          autoPlay={autoPlay}
          surface={surface}
          setSurface={setSurface}
          pipOffset={pipOffset}
          onPipPointerDown={onPipPointerDown}
          onPipPointerMove={onPipPointerMove}
          onPipPointerUp={onPipPointerUp}
          toggleFullscreen={toggleFullscreen}
          collapseStage={collapseStage}
          onOpenQueue={() => setDrawerOpen(true)}
        />
      </div>
      {drawerOpen && (
        <div
          className="now-playing-drawer-backdrop"
          data-testid="now-playing-drawer-backdrop"
          onClick={() => setDrawerOpen(false)}
        >
          <aside
            className="now-playing-drawer"
            data-testid="now-playing-drawer"
            role="dialog"
            aria-label="Queue"
            onClick={(e) => e.stopPropagation()}
          >
            {/* Clear queue (inside the panel) empties the Queue → `queue.current`
                becomes null → this whole bar unmounts (ending the session): the
                stop-and-dismiss exit. The panel's own header carries the × close. */}
            <QueuePanel queue={queue} onClose={() => setDrawerOpen(false)} />
          </aside>
        </div>
      )}
    </>
  );
}

/** The end-of-episode "Up Next" card: a small bottom-right overlay shown in the last
 * 30s of a staged video that has a next Queue entry. It previews that entry formatted
 * like the bar's now-playing label (thumbnail + title + series), with an "Up Next:"
 * lead and a live countdown to the left. Clicking anywhere plays the next entry
 * immediately (the caller wires onPlay to queue.next); Esc dismissal is owned by the
 * caller. The series line needs the parent Show context the lean Queue entry omits,
 * so — like the bar — it fetches the entry's Title detail (a Movie next entry simply
 * shows no series line). */
function UpNextCard({
  entry,
  secondsLeft,
  onPlay,
}: {
  entry: QueueEntry;
  secondsLeft: number;
  onPlay: () => void;
}) {
  const detailState = useAsync(
    (signal) => apiClient.getTitle(entry.title.id, signal),
    [entry.title.id],
  );
  const detail = detailState.status === "ready" ? detailState.data : null;
  const episodeTitle = detail?.title ?? entry.title.title;
  const seriesName = detail?.kind === "episode" && detail.episode ? detail.episode.showTitle : "";

  return (
    <div
      className="now-playing-upnext"
      data-testid="now-playing-upnext"
      role="button"
      tabIndex={0}
      aria-label={`Up next: ${episodeTitle}. Play now.`}
      onClick={onPlay}
      onKeyDown={(e) => {
        // Enter / Space activate the card like a button (Esc is the caller's).
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onPlay();
        }
      }}
    >
      <div className="now-playing-upnext-lead">
        <span className="now-playing-upnext-heading">Up Next:</span>
        <span
          className="now-playing-upnext-countdown"
          data-testid="now-playing-upnext-countdown"
        >
          {secondsLeft}s
        </span>
      </div>
      <div className="now-playing-thumb now-playing-upnext-thumb">
        <Poster titleId={entry.title.id} title={episodeTitle} version={entry.title.artworkVersion} />
      </div>
      <div className="now-playing-labels">
        <span className="now-playing-title" data-testid="now-playing-upnext-title">
          {episodeTitle}
        </span>
        {seriesName && <span className="now-playing-context">{seriesName}</span>}
      </div>
    </div>
  );
}

/** Owns the one media element + Playback session for the current entry, plus the
 * bar's now-playing label and transport. Relocated from the retired PlayerScreen's
 * PlayerCore; behavior (negotiate/HLS/resume/report/end/skip) is unchanged. */
function CurrentPlayer({
  entry,
  autoPlay,
  surface,
  setSurface,
  pipOffset,
  onPipPointerDown,
  onPipPointerMove,
  onPipPointerUp,
  toggleFullscreen,
  collapseStage,
  onOpenQueue,
}: {
  entry: QueueEntry;
  autoPlay: boolean;
  // Presentation surface — OWNED by the persistent NowPlayingBar and threaded in,
  // so it survives this core's per-entry re-key (see the parent's surface machine).
  surface: VideoSurface;
  setSurface: (s: VideoSurface) => void;
  pipOffset: { x: number; y: number } | null;
  onPipPointerDown: (e: ReactPointerEvent) => void;
  onPipPointerMove: (e: ReactPointerEvent) => void;
  onPipPointerUp: (e: ReactPointerEvent) => void;
  toggleFullscreen: () => void;
  collapseStage: () => void;
  onOpenQueue: () => void;
}) {
  const queue = useQueue();
  const transport = usePlaybackTransport();
  const titleId = entry.title.id;
  const video = isVideoKind(entry.title.kind);

  // Resume offset (ms) the server stored on the entry's TitleSummary; we seek the
  // <video> here and report it to the session so tracking continues from the right
  // spot. A watched Title starts from 0.
  const resumeMs = entry.title.watched ? 0 : entry.title.resumePositionMs;

  // The rich now-playing label + artwork come from the Title detail (Artist/Album,
  // Show/Season context the lean Queue entry omits). Fetched independently of
  // playback: a failure degrades the label but NEVER blocks playback. It also
  // carries the Editions the Playback preference resolves against (below).
  const detailState = useAsync((signal) => apiClient.getTitle(titleId, signal), [titleId]);
  const detail = detailState.status === "ready" ? detailState.data : null;

  // Replay the committed Playback preference (appletv-web-parity §1/§3): resolve the
  // saved Edition NAME to THIS Title's Edition id AND the saved Quality-cap rung to its
  // paired constraints, then negotiate with them. The preference key is derived
  // synchronously — a Movie from its own id, an Episode from the Show id threaded onto
  // the entry — so a Title with NO stored config negotiates immediately (unchanged
  // behaviour). Only a Title WITH a stored Edition or Quality pick waits (`pending`) for
  // the detail (both axes resolve against the detail's Editions — the name→id map and
  // the source height) so the first request already carries the overrides (no
  // start-then-re-negotiate). An Episode without a threaded showId, or a detail that
  // failed to load, degrades to Auto / Direct Play rather than blocking.
  const userId = persistedUserId();
  const prefTitle =
    entry.title.kind === "episode"
      ? entry.showId
        ? { id: titleId, kind: "episode", episode: { showId: entry.showId } }
        : null
      : { id: titleId, kind: entry.title.kind };
  const storedPref = prefTitle
    ? loadPreferenceForTitle(window.localStorage, userId, prefTitle)
    : null;
  const hasStoredConfig =
    !!storedPref && (storedPref.editionName !== null || storedPref.qualityCap !== null);
  let playerPreference: PlayerPreference | undefined;
  if (hasStoredConfig) {
    if (detail) {
      const resolved = resolvePlayback(storedPref, detail.editions ?? []);
      playerPreference = { editionId: resolved.editionId, constraints: resolved.constraints };
    } else if (detailState.status === "error") {
      playerPreference = {}; // can't resolve → Auto / Direct Play, never block
    } else {
      playerPreference = { pending: true }; // wait for the detail, then negotiate once
    }
  }
  // Seed the pre-play Audio / Video Stream picks (appletv-web-parity §1, issue 04)
  // straight off the TRANSIENT Queue entry — the Playback Options sheet's Play attached
  // them to the head entry, NOT to the persisted preference (client ADR-0011). So they
  // seed usePlayerSession REGARDLESS of `hasStoredConfig` and need NO detail to resolve
  // (they are ids, not by-name/height): they never force `pending`. The session seeds
  // its audio/video refs from these ONCE, so an in-player switch later supersedes them.
  if (entry.audioStreamId || entry.videoStreamId) {
    playerPreference = {
      ...(playerPreference ?? {}),
      audioStreamId: entry.audioStreamId,
      videoStreamId: entry.videoStreamId,
    };
  }

  const session = usePlayerSession(apiClient, titleId, resumeMs, playerPreference);
  const videoRef = useRef<HTMLVideoElement | null>(null);
  // Guard so we only auto-seek to the resume point once (the first loadedmetadata).
  const seekedRef = useRef(false);
  // When a burn-in re-negotiation restarts the stream (subtitles/04), the new
  // <video> should resume where the viewer was — this holds that position (ms) so
  // the next loadedmetadata seeks to it instead of the entry's original resume.
  const pendingSeekMsRef = useRef<number | null>(null);
  // Whether the player was actually PLAYING when an escalating re-negotiation
  // (burn-in / audio switch) restarted the stream — set beside pendingSeekMsRef so
  // the restarted stream resumes playback EXPLICITLY. Relying on the element's
  // autoplay attribute across an MSE re-attach is not portable: real Chrome can
  // leave the new stream paused with NO event or console output — the player looks
  // frozen (hls.js quietly pre-buffers and then idles) with nothing to diagnose.
  const resumeAfterSeekRef = useRef(false);
  // Guards for the vanished-session recovery (a reap after a long pause, or a
  // server restart): one re-negotiation in flight at a time (recoveringRef), and
  // never more than one per window (lastRecoverAtRef) so a storm of fatal errors —
  // or a genuinely unplayable file — can't spin the negotiate path.
  const recoveringRef = useRef(false);
  const lastRecoverAtRef = useRef(0);
  // One explicit play() attempt for the INITIAL load of an autoPlay entry. The
  // autoplay ATTRIBUTE alone is not reliable for an HLS/MSE attach in every
  // browser (real Chrome can leave it paused with no event) — and when a
  // remembered audio pick escalates the very first negotiation to an HLS tier,
  // that is exactly the shape the entry starts with.
  const autoStartedRef = useRef(false);
  // Reflects the real media element state so the play/pause button is honest.
  const [playing, setPlaying] = useState(false);
  // Track the element's real position + duration (from timeupdate / metadata /
  // durationchange) so the progress bar and time labels reflect actual playback,
  // same philosophy as `playing` — honest media state, not a guess.
  const [currentTime, setCurrentTime] = useState(0);
  const [duration, setDuration] = useState(0);

  // Per-user volume + mute (localStorage), applied to the element below.
  const prefs = usePlaybackPrefs();

  // The video surface (stage ↔ pip ↔ bar-only) and its history/fullscreen/chrome
  // machinery now live in the persistent NowPlayingBar (above), threaded in as
  // props — so the presentation survives this core's per-entry re-key and the video
  // continues in the same view when the Queue advances. This core just renders into
  // the surface the bar hands it and reports user surface changes back up.

  // ── End-of-episode "Up Next" card ─────────────────────────────────────────────
  // In the last 30s of a STAGED video that has a next Queue entry, preview it in a
  // small bottom-right card (thumbnail + title + series, an "Up Next:" lead and a
  // live countdown). A click plays it immediately; Esc dismisses the card for THIS
  // entry (the dismissal resets naturally when the core re-keys onto the next entry).
  // Gated to the stage — the immersive viewing surface — so it never intrudes while
  // the user is browsing with the pip, and it rides inside .now-playing-bar so it
  // survives fullscreen.
  const nextEntry = queue.upNext[0];
  const secondsLeft = duration > 0 ? Math.ceil(duration - currentTime) : 0;
  const [upNextDismissed, setUpNextDismissed] = useState(false);
  const showUpNext =
    video &&
    surface === "stage" &&
    !!nextEntry &&
    !upNextDismissed &&
    duration > 0 &&
    secondsLeft > 0 &&
    secondsLeft <= 30;

  // Esc dismisses the card. Registered in the CAPTURE phase so it runs BEFORE the
  // bar's stage-collapse keydown (a bubble-phase window listener) — stopPropagation
  // then keeps that Esc from also collapsing the stage to pip, so a single Esc just
  // hides the card. Only armed while the card is up; once hidden, Esc collapses the
  // stage as usual.
  useEffect(() => {
    if (!showUpNext) return;
    function onEscCapture(e: KeyboardEvent) {
      if (e.key !== "Escape") return;
      setUpNextDismissed(true);
      e.stopPropagation();
      e.preventDefault();
    }
    window.addEventListener("keydown", onEscCapture, true);
    return () => window.removeEventListener("keydown", onEscCapture, true);
  }, [showUpNext]);

  // ── Keyboard playback shortcuts (immersive stage / fullscreen only) ───────────
  // Space/k play/pause, ←/→ + j/l skip ∓10s, ↑/↓ volume, 0–9 seek to N0%, , / .
  // frame-step while paused, c toggle captions, n/p next/prev, f fullscreen, m mute.
  // (Esc is handled with Back above: exit fullscreen if fullscreen, else collapse to
  // pip.) Scoped to the stage so they never hijack Space-to-scroll / arrows while the
  // user is browsing with the pip. A latest-ref keeps the window listener registered
  // once yet always calling into current state; we bail when a form control is
  // focused so the seek / volume sliders keep their own arrow + space behavior.
  const shortcutRef = useRef<(e: KeyboardEvent) => void>(() => {});
  shortcutRef.current = (e: KeyboardEvent) => {
    if (!video || surface !== "stage") return;
    const el = e.target as HTMLElement | null;
    const tag = el?.tagName;
    if (el?.isContentEditable || tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") {
      return;
    }
    // A modifier chord (⌘K, Ctrl+F, …) belongs to the browser/OS, not playback.
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    // 0–9 → seek to 0%, 10%, … 90% of the duration.
    if (e.key >= "0" && e.key <= "9") {
      e.preventDefault();
      seekToFraction(Number(e.key) / 10);
      return;
    }
    switch (e.key) {
      case " ":
        // A focused button/link already activates on Space — don't double-toggle.
        if (tag === "BUTTON" || tag === "A") return;
        e.preventDefault(); // no page scroll
        togglePlay();
        break;
      case "k":
      case "K":
        e.preventDefault();
        togglePlay();
        break;
      case "ArrowLeft":
      case "j":
      case "J":
        e.preventDefault();
        skip(-SKIP_SECONDS);
        break;
      case "ArrowRight":
      case "l":
      case "L":
        e.preventDefault();
        skip(SKIP_SECONDS);
        break;
      case "ArrowUp":
        e.preventDefault();
        nudgeVolume(VOLUME_STEP);
        break;
      case "ArrowDown":
        e.preventDefault();
        nudgeVolume(-VOLUME_STEP);
        break;
      case ",":
        e.preventDefault();
        frameStep(-1);
        break;
      case ".":
        e.preventDefault();
        frameStep(1);
        break;
      case "c":
      case "C":
        e.preventDefault();
        // Toggle captions: off if any track is showing, else the first text track.
        if (currentSubId != null) selectSubtitle(null);
        else if (textTracks[0]) selectSubtitle(textTracks[0].id);
        break;
      case "n":
      case "N":
        e.preventDefault();
        if (queue.hasNext) queue.next();
        break;
      case "p":
      case "P":
        e.preventDefault();
        if (queue.hasPrev) queue.prev();
        break;
      case "f":
      case "F":
        e.preventDefault();
        toggleFullscreen();
        break;
      case "m":
      case "M":
        e.preventDefault();
        prefs.toggleMuted();
        break;
    }
  };
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => shortcutRef.current(e);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The now-playing label reads the Title detail fetched above (near usePlayerSession,
  // where the Playback preference also resolves against its Editions). A failed fetch
  // degrades the label to the bare title + kind and NEVER blocks playback.
  const primaryLabel = detail?.title ?? entry.title.title;
  let contextLabel = "";
  if (detail?.kind === "track" && detail.track) {
    contextLabel = detail.track.artistName; // "Artist" (· Title primary)
  } else if (detail?.kind === "episode" && detail.episode) {
    contextLabel = episodeContextLabel(detail.episode); // "Show · S01E03"
  } else if (detailState.status === "error") {
    contextLabel = kindLabel(entry.title.kind); // degraded fallback
  }

  // Where each label navigates once the detail (with parent context) has loaded,
  // so the user can jump back to what they're listening to / watching:
  //  - music: the SONG title opens its ALBUM, the ARTIST name opens the artist;
  //  - TV: both labels open the SHOW; movie: the title opens the movie's detail.
  // Left undefined while the detail is loading or on error → a plain span.
  let titleLinkTo: string | undefined;
  let contextLinkTo: string | undefined;
  if (detail?.kind === "track" && detail.track) {
    titleLinkTo = `/music/albums/${encodeURIComponent(detail.track.albumId)}`;
    contextLinkTo = `/music/artists/${encodeURIComponent(detail.track.artistId)}`;
  } else if (detail?.kind === "episode" && detail.episode) {
    titleLinkTo = `/shows/${encodeURIComponent(detail.episode.showId)}`;
    contextLinkTo = titleLinkTo;
  } else if (detail?.kind === "movie") {
    titleLinkTo = `/titles/${encodeURIComponent(titleId)}`;
  }

  // The left thumbnail: for a Track, its ALBUM cover (GET /albums/{id}/artwork),
  // not the track-keyed poster — so the bar shows the album art of the song
  // playing. Falls back to the placeholder if the album has no cover (404).
  // Video (Movie/Episode) keeps its own title poster.
  const albumArtSrc =
    detail?.kind === "track" && detail.track
      ? albumArtworkUrl(detail.track.albumId)
      : undefined;

  const positionMs = () => {
    const v = videoRef.current;
    return v ? Math.floor(v.currentTime * 1000) : 0;
  };

  // End the session (final report + DELETE) when this core unmounts (a queue
  // advance re-keying it, or the Queue emptying so the bar unmounts). Runs once.
  useEffect(() => {
    return () => session.end(positionMs());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const status = session.status;

  // Skip an entry the server can't play (404 Missing / unplayable): once for this
  // mount (the core is re-keyed per entry, so this fires at most once per entry).
  // At the end of the Queue there's nowhere to go, so the honest state shows.
  const skippedRef = useRef(false);
  useEffect(() => {
    if (skippedRef.current) return;
    if (status.kind === "error" || status.kind === "unsupported") {
      skippedRef.current = true;
      if (queue.hasNext) queue.next();
    }
  }, [status.kind, queue]);

  // Surfaced HLS-attach failure (hls.js unsupported AND no native HLS).
  const [hlsError, setHlsError] = useState<string | null>(null);

  // For the HLS tiers the <video> gets NO `src` — hls.js attaches the media
  // playlist over MSE (or, on Safari, attachHls sets the native-HLS src). directPlay
  // keeps its progressive `src` and never enters here.
  const streamUrl = status.kind === "ready" ? status.decision.streamUrl : null;
  const tier = status.kind === "ready" ? status.decision.tier : null;
  // The live HLS attachment (hls.js / native), kept in state so the captions
  // effect can drive its in-band subtitle selection once it's ready (slice 03).
  const [hlsAttachment, setHlsAttachment] = useState<HlsAttachment | null>(null);
  // Recover a vanished session: the HLS layer calls this when the playlist +
  // segments persistently 404 (the session was reaped after a long pause, or the
  // server restarted). Re-negotiate a fresh session from the live position and
  // resume there — reusing the same pending-seek/resume machinery a burn/audio
  // escalation uses. Held in a ref so the attach effect's onSessionLost closure
  // stays stable (the effect must not re-run — and re-attach — on every render).
  const recoverRef = useRef<() => void>(() => {});
  recoverRef.current = () => {
    const now = Date.now();
    if (recoveringRef.current) return; // a re-negotiation is already in flight
    if (now - lastRecoverAtRef.current < RECOVER_MIN_INTERVAL_MS) return;
    recoveringRef.current = true;
    lastRecoverAtRef.current = now;
    // Resume the fresh stream where the viewer was, and keep playing if they were —
    // onLoadedMetadata consumes these exactly as it does after a burn/audio switch.
    pendingSeekMsRef.current = positionMs();
    resumeAfterSeekRef.current = !videoRef.current?.paused;
    seekedRef.current = false;
    session.recover(positionMs());
  };
  useEffect(() => {
    if (!streamUrl || !tier || !isHlsTier(tier)) return;
    const v = videoRef.current;
    if (!v) return;
    setHlsError(null);
    let attachment: HlsAttachment | null = null;
    let cancelled = false;
    void attachHls(v, streamUrl, { onSessionLost: () => recoverRef.current() })
      .then((a) => {
        if (cancelled) {
          a.detach();
          return;
        }
        attachment = a;
        setHlsAttachment(a);
        v.setAttribute("data-hls-mode", a.mode);
        // A new stream is attached — recovery (if any) is complete; re-arm the guard
        // so a LATER vanish can recover again.
        recoveringRef.current = false;
      })
      .catch((err) => {
        if (cancelled) return;
        setHlsError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
      attachment?.detach();
      setHlsAttachment(null);
    };
  }, [streamUrl, tier]);

  // Apply the persisted volume + mute to the element whenever they change and
  // once it's mounted (status ready). Honours the user's saved preference on load.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    v.volume = prefs.volume;
    v.muted = prefs.muted;
  }, [prefs.volume, prefs.muted, status.kind]);

  // ── Captions (text subtitles, subtitles/02) ───────────────────────────────
  // The deliverable text tracks the decision offers, ordered by the viewer's
  // preferred language (client-sent + sorted client-side). Image tracks and
  // unconvertible text are excluded by orderedTextTracks. Stable per session
  // (the decision doesn't change once ready), so the <track> list never churns.
  const preferredLang = useMemo(() => preferredSubtitleLang(), []);
  const negotiatedSubs: SubtitleTrack[] =
    status.kind === "ready" ? status.decision.subtitles : [];
  // Subtitles fetched mid-session via "search online" (subtitles/05). They aren't in
  // the decision (which was negotiated before the fetch), so the player carries them
  // alongside and merges them into the menu + <track> list; a fetched track is always
  // delivered out-of-band (it's not in the HLS master playlist), so it plays on every
  // tier like any other text track.
  const [fetchedTracks, setFetchedTracks] = useState<SubtitleTrack[]>([]);
  const decisionSubs = useMemo(
    () => (fetchedTracks.length ? [...negotiatedSubs, ...fetchedTracks] : negotiatedSubs),
    [negotiatedSubs, fetchedTracks],
  );
  const textTracks = useMemo(
    () => orderedTextTracks(decisionSubs, preferredLang),
    [decisionSubs, preferredLang],
  );
  // The IMAGE tracks (PGS/VOBSUB/DVD, embedded or sidecar). Selecting one BURNS it
  // into the video via a fresh transcode negotiation (subtitles/04); they are never
  // auto-displayed. Kept beside the text tracks so the one captions menu lists both.
  const imageTracks = useMemo(
    () => orderedImageTracks(decisionSubs, preferredLang),
    [decisionSubs, preferredLang],
  );
  // The selected text-track id, or null for "off". Defaults OFF unless a track is
  // forced (auto-display). Initialized once per session when the decision lands.
  // The burned-in image sub (if any) is owned by the session (session.burnSubtitleId).
  const [selectedSubId, setSelectedSubId] = useState<string | null>(null);
  const [captionsOpen, setCaptionsOpen] = useState(false);
  const subsInitRef = useRef(false);
  // "Search online" flow (subtitles/05): trigger a provider fetch for the viewer's
  // preferred language, list the returned candidates, and on a pick add the fetched
  // track and turn it on. Available to any User (Members included).
  const [searchState, setSearchState] = useState<
    "idle" | "searching" | "results" | "empty" | "error"
  >("idle");
  const [candidates, setCandidates] = useState<SubtitleCandidate[]>([]);
  const [fetchingId, setFetchingId] = useState<string | null>(null);
  useEffect(() => {
    if (subsInitRef.current || status.kind !== "ready") return;
    subsInitRef.current = true;
    setSelectedSubId(defaultTrackId(textTracks));
  }, [status.kind, textTracks]);

  // Apply the selection whenever it changes (or the element (re)mounts / the HLS
  // attachment becomes ready). Instant switch — no reload, no server round-trip;
  // the choice therefore survives resume/seek within the session. Delivery differs
  // by tier (ADR-0020): direct play toggles the out-of-band <track> element;
  // the HLS tiers select the in-band SUBTITLES rendition via hls.js/native (mapped
  // to the rendition index by the deliverable-text-track order, slice 03).
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    if (tier && isHlsTier(tier)) {
      // In-band selection maps against the NEGOTIATED tracks only (a fetched track
      // isn't in the master playlist); -1 (off) when the selection is a fetched
      // track. The out-of-band <track> for a fetched selection is toggled separately
      // below so it still renders on an HLS stream.
      hlsAttachment?.setTextTrack(deliverableTextTrackIndex(negotiatedSubs, selectedSubId));
      applySubtitleSelection(v, selectedSubId);
    } else {
      applySubtitleSelection(v, selectedSubId);
    }
  }, [selectedSubId, textTracks, status.kind, tier, hlsAttachment, negotiatedSubs]);

  // The currently-selected caption id across both delivery paths: the burned-in
  // image sub when one is active, else the client-side text selection (null = Off).
  const currentSubId = session.burnSubtitleId ?? selectedSubId;

  // Select a caption from the menu. Text tracks + Off switch client-side instantly
  // (ADR-0020); an image track is BURNED IN via a fresh transcode negotiation that
  // restarts the session and resumes where the viewer was (subtitles/04). Choosing
  // a text track or Off while a burn is active first clears the burn (re-negotiate
  // back to the cheap tier), then applies the text selection.
  function selectSubtitle(id: string | null) {
    setCaptionsOpen(false);
    const image = id != null ? imageTracks.find((t) => t.id === id) : undefined;
    if (image) {
      // Burn-in: fresh negotiation. The new stream should resume from here.
      setSelectedSubId(null);
      pendingSeekMsRef.current = positionMs();
      resumeAfterSeekRef.current = !videoRef.current?.paused;
      seekedRef.current = false;
      session.selectBurnSubtitle(image.id, positionMs());
      return;
    }
    // Text track or Off (client-side selection).
    if (session.burnSubtitleId != null) {
      pendingSeekMsRef.current = positionMs();
      resumeAfterSeekRef.current = !videoRef.current?.paused;
      seekedRef.current = false;
      session.selectBurnSubtitle(null, positionMs());
    }
    setSelectedSubId(id);
  }

  // Trigger an online search for the preferred language (defaulting to English when
  // the client sent none). Zero candidates is a normal "nothing found" state, not an
  // error; a disabled/offline provider degrades to the same empty result.
  async function searchOnline() {
    setSearchState("searching");
    setCandidates([]);
    try {
      const found = await apiClient.searchSubtitles(titleId, preferredLang || "en");
      setCandidates(found);
      setSearchState(found.length ? "results" : "empty");
    } catch {
      setSearchState("error");
    }
  }

  // Fetch a chosen candidate: the server downloads + converts + caches it and returns
  // the new track, which we add to the session's fetched tracks and turn on. It plays
  // out-of-band like any other text track.
  async function pickCandidate(c: SubtitleCandidate) {
    setFetchingId(c.id);
    try {
      const track = await apiClient.fetchSubtitle(titleId, c.language || preferredLang || "en", c);
      setFetchedTracks((prev) =>
        prev.some((t) => t.id === track.id) ? prev : [...prev, track],
      );
      setSearchState("idle");
      setCandidates([]);
      setCaptionsOpen(false);
      if (track.kind === "text") setSelectedSubId(track.id);
    } catch {
      setSearchState("error");
    } finally {
      setFetchingId(null);
    }
  }

  // ── Audio menu (audio Streams, audio-streams/04, ADR-0022) ─────────────────
  // The played File's selectable audio Streams, ordered by the viewer's preferred
  // audio language (the same value the capability profile sent, so the menu order
  // matches what the server resolved). The RESOLVED Stream the delivery carries is
  // pre-selected. Inverted asymmetry vs subtitles: on the HLS tiers a multi-audio
  // File is demuxed, so switching is IN-BAND and instant (no restart); on direct
  // play a non-default pick escalates ONCE via a fresh negotiation (the session's
  // one audio restart), after which switching is in-band too.
  const preferredAudio = useMemo(() => preferredAudioLang(), []);
  // Server order (NOT the re-sorted menu) — the master playlist advertises the
  // in-band AUDIO renditions in this order, so it maps a pick to a rendition index.
  const negotiatedAudio: AudioStream[] =
    status.kind === "ready" ? status.decision.audioStreams : [];
  // The menu list (preferred-language first, then default, then by label).
  const audioStreams = useMemo(
    () => orderedAudioStreams(negotiatedAudio, preferredAudio),
    [negotiatedAudio, preferredAudio],
  );
  // The active audio Stream id — the one playing. Initialized (per session) to the
  // server-resolved Stream; an in-band pick updates it instantly, a direct-play
  // escalation re-initializes it from the re-negotiated decision when it lands.
  const [selectedAudioId, setSelectedAudioId] = useState<string | null>(null);
  const [audioOpen, setAudioOpen] = useState(false);
  // Re-initialize the active audio on EACH new session (initial negotiation AND the
  // escalation re-negotiation), keyed on the session id so a plain re-render doesn't
  // reset a live in-band pick.
  const audioInitSessionRef = useRef<string | null>(null);
  useEffect(() => {
    if (status.kind !== "ready") return;
    const sid = status.decision.sessionId;
    if (audioInitSessionRef.current === sid) return;
    audioInitSessionRef.current = sid;
    setSelectedAudioId(initialAudioId(status.decision.audioStreams, status.decision.audioStream));
  }, [status]);

  // Apply the in-band selection on the HLS tiers whenever it changes (or the HLS
  // attachment becomes ready). Instant, client-side — no server round-trip — so the
  // choice survives seek and pause/resume within the session. A no-op on direct play
  // (no in-band renditions) and when the id isn't one of the demuxed renditions.
  useEffect(() => {
    if (!tier || !isHlsTier(tier)) return;
    const idx = audioRenditionIndex(negotiatedAudio, selectedAudioId);
    if (idx != null) hlsAttachment?.setAudioTrack(idx);
  }, [selectedAudioId, tier, hlsAttachment, negotiatedAudio]);

  // Pick an audio Stream from the menu. On the HLS tiers (demuxed multi-audio) the
  // switch is in-band and instant; on direct play a non-default pick escalates via a
  // fresh negotiation (end-and-re-negotiate with audioStreamId, resuming at the live
  // position — the same loop image subtitles use), which may go busy. A no-op when
  // the Stream is already active.
  function selectAudio(id: string) {
    setAudioOpen(false);
    if (id === selectedAudioId) return;
    if (tier && isHlsTier(tier) && negotiatedAudio.length >= 2) {
      // In-band, no restart. The apply effect drives hls.js/native; record the pick
      // so a later re-negotiation (a burn switch) carries it forward.
      setSelectedAudioId(id);
      session.noteAudioStream(id);
      return;
    }
    // Direct play: the one escalating switch. Capture the live position so the
    // remuxed stream resumes where the viewer was.
    pendingSeekMsRef.current = positionMs();
    resumeAfterSeekRef.current = !videoRef.current?.paused;
    seekedRef.current = false;
    session.selectAudioStream(id, positionMs());
  }

  // ── Video menu (video Streams, selectable-video/03, ADR-0025) ──────────────
  // The played File's selectable video Streams, with the RESOLVED (capability-then-
  // quality) Stream pre-selected. Unlike audio there is NO in-band video rendition in
  // HLS, so a switch is ALWAYS a fresh negotiation (the image-subtitle model): it
  // re-buffers briefly and can go busy, never the instant in-band audio flip. The
  // menu appears only when the File offers ≥2 selectable video Streams.
  const negotiatedVideo: VideoStream[] =
    status.kind === "ready" ? status.decision.videoStreams : [];
  const videoStreams = useMemo(() => orderedVideoStreams(negotiatedVideo), [negotiatedVideo]);
  // The active video Stream id — the one playing. Initialized (per session) to the
  // server-resolved Stream; a switch re-initializes it from the re-negotiated
  // decision when it lands.
  const [selectedVideoId, setSelectedVideoId] = useState<string | null>(null);
  const [videoOpen, setVideoOpen] = useState(false);
  // Re-initialize the active video on EACH new session (initial negotiation AND a
  // switch re-negotiation), keyed on the session id so a plain re-render doesn't
  // reset the selection.
  const videoInitSessionRef = useRef<string | null>(null);
  useEffect(() => {
    if (status.kind !== "ready") return;
    const sid = status.decision.sessionId;
    if (videoInitSessionRef.current === sid) return;
    videoInitSessionRef.current = sid;
    setSelectedVideoId(initialVideoId(status.decision.videoStreams, status.decision.videoStream));
  }, [status]);

  // Pick a video Stream from the menu — always an escalating switch (no in-band
  // video rendition): a fresh negotiation carrying videoStreamId that ends the
  // current session and resumes at the live position, preserving the audio Stream and
  // subtitle selection (they carry through negotiate() unchanged). May go busy. A
  // no-op when the Stream is already active. Mirrors the direct-play audio escalation.
  function selectVideo(id: string) {
    setVideoOpen(false);
    if (id === selectedVideoId) return;
    // Capture the live position so the re-negotiated stream resumes where the viewer
    // was (the same seek-restore the burn/audio switches use).
    pendingSeekMsRef.current = positionMs();
    resumeAfterSeekRef.current = !videoRef.current?.paused;
    seekedRef.current = false;
    session.selectVideoStream(id, positionMs());
  }

  function onLoadedMetadata() {
    const v = videoRef.current;
    if (!v) return;
    // Re-assert the caption selection once the element (and its <track> cues) have
    // loaded metadata — the native TextTrack may not have existed on the first
    // apply, so this guarantees a forced/selected track actually shows.
    applySubtitleSelection(v, selectedSubId);
    if (Number.isFinite(v.duration)) setDuration(v.duration);
    // Seek once to the resume point — the entry's original resume on first load, or
    // the live position captured when a burn-in re-negotiation restarted the stream.
    if (!seekedRef.current) {
      const pending = pendingSeekMsRef.current;
      const targetMs = pending != null ? pending : resumeMs;
      pendingSeekMsRef.current = null;
      if (targetMs > 0) {
        seekedRef.current = true;
        v.currentTime = targetMs / 1000;
        setCurrentTime(v.currentTime);
      }
      // A mid-play escalation (burn-in / audio switch) restarted the stream: resume
      // playback EXPLICITLY if the viewer was playing. The element's autoplay
      // attribute is not reliable across an MSE re-attach (real Chrome can leave the
      // new stream paused with no event at all — a silent freeze), so ask, and if
      // the browser refuses, say so on the console instead of freezing mutely; the
      // bar then honestly shows Paused and a click (a fresh gesture) resumes.
      if (pending != null && resumeAfterSeekRef.current) {
        resumeAfterSeekRef.current = false;
        playOrDefer(v, "auto-resume after re-negotiation");
      } else if (pending == null && autoPlay && v.paused && !autoStartedRef.current) {
        // The INITIAL load of a freshly-played entry: start playback explicitly too.
        // A remembered audio pick can put even the FIRST negotiation on an HLS tier,
        // where the autoplay attribute alone may silently not fire (real Chrome).
        autoStartedRef.current = true;
        playOrDefer(v, "autoplay");
      }
    }
  }

  // playOrDefer starts playback, and when the browser REFUSES (Chrome requires
  // transient user activation for unmuted play, and the gesture that started this
  // flow expired during negotiation/buffering) it retries ONCE on the user's next
  // gesture anywhere on the page — that interaction carries fresh activation, so
  // the retry succeeds and "video started paused after a slow negotiation" heals
  // on the first click instead of demanding the user find the play button. The
  // refusal is also logged so the state is diagnosable.
  function playOrDefer(v: HTMLVideoElement, what: string) {
    v.play()?.catch((err) => {
      // eslint-disable-next-line no-console
      console.error(`[player] ${what} was blocked (will retry on the next interaction):`, err);
      const retry = () => {
        void v.play()?.catch(() => {});
      };
      document.addEventListener("pointerdown", retry, { once: true, capture: true });
      document.addEventListener("keydown", retry, { once: true, capture: true });
    });
  }
  function onDurationChange() {
    const v = videoRef.current;
    if (v && Number.isFinite(v.duration)) setDuration(v.duration);
  }
  function onTimeUpdate() {
    const v = videoRef.current;
    if (v) setCurrentTime(v.currentTime);
  }
  function onPlay() {
    setPlaying(true);
    session.startReporting(positionMs);
    session.report(positionMs(), "playing");
  }
  function onPause() {
    setPlaying(false);
    session.stopReporting();
    session.report(positionMs(), "paused");
  }
  function onSeeked() {
    session.report(positionMs(), videoRef.current?.paused ? "paused" : "playing");
  }
  function onEnded() {
    // Report the final position (≈ duration) so the server can cross the Watched
    // threshold; the server owns that decision.
    const v = videoRef.current;
    const finalMs = v ? Math.floor((v.duration || v.currentTime) * 1000) : positionMs();
    // Natural-end advance branches on Repeat mode (slice 04). Under repeat-one the
    // SAME entry replays: the store DOESN'T move (advance() would be a no-op), so we
    // re-seek and re-play THIS element directly — the store never re-keys this core.
    if (queue.repeat === "one" && v) {
      session.report(finalMs, "paused"); // count the completed pass (Watched threshold)
      v.currentTime = 0;
      setCurrentTime(0);
      void v.play().catch(() => {});
      return; // playing again — onPlay resumes reporting; stay on this entry
    }
    setPlaying(false);
    session.stopReporting();
    session.report(finalMs, "paused");
    // Otherwise advance the store's pointer: repeat-all wraps the last entry to the
    // first, off stops cleanly at the end (a no-op). A pointer change re-keys this
    // core → ends this session, negotiates the next.
    queue.advance();
  }

  function togglePlay() {
    const v = videoRef.current;
    if (!v) return;
    if (v.paused) void v.play().catch(() => {});
    else v.pause();
  }

  // Publish this element's play/pause state to the shared transport so out-of-bar
  // affordances (an album track row's toggle) reflect the current song. The
  // publish/register callbacks are stable, so this only re-fires when `playing`
  // flips.
  useEffect(() => {
    transport.publishPlaying(playing);
  }, [playing, transport.publishPlaying]);

  // Register this element's toggle while this player is mounted; on unmount (entry
  // change / queue emptied) clear it and reset the shared playing flag so a stale
  // pause icon can't linger on a row after playback ends.
  useEffect(() => {
    transport.registerToggle(togglePlay);
    return () => {
      transport.registerToggle(null);
      transport.publishPlaying(false);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [transport.registerToggle, transport.publishPlaying]);

  // Seek to an absolute position (seconds), clamped to the media bounds, and fire
  // a progress report so resume position + Watch state stay accurate — reusing the
  // same session.report() path onSeeked uses (jsdom doesn't emit a `seeked` event
  // for a programmatic currentTime set, so we report directly here). Drives both
  // the progress-bar click/drag and the ±10s buttons.
  function seekTo(seconds: number) {
    const v = videoRef.current;
    if (!v) return;
    const max = Number.isFinite(v.duration) && v.duration > 0 ? v.duration : duration;
    const upper = max > 0 ? max : seconds;
    const clamped = Math.min(Math.max(seconds, 0), upper);
    v.currentTime = clamped;
    setCurrentTime(clamped);
    session.report(Math.floor(clamped * 1000), v.paused ? "paused" : "playing");
  }
  function skip(deltaSeconds: number) {
    const v = videoRef.current;
    if (!v) return;
    seekTo(v.currentTime + deltaSeconds);
  }
  // Nudge the persisted volume by `delta`, clamped to 0–1, mirroring the volume
  // slider's unmute/mute-at-zero behavior (used by the ↑/↓ keyboard shortcuts).
  function nudgeVolume(delta: number) {
    const next = Math.min(Math.max(prefs.volume + delta, 0), 1);
    prefs.setVolume(next);
    if (next > 0 && prefs.muted) prefs.setMuted(false);
    else if (next === 0 && !prefs.muted) prefs.setMuted(true);
  }
  // Seek to a fraction (0–1) of the media duration — the 0–9 number-key shortcuts
  // (0 = start … 9 = 90%). Uses the live element duration, falling back to state.
  function seekToFraction(fraction: number) {
    const v = videoRef.current;
    const d = v && Number.isFinite(v.duration) && v.duration > 0 ? v.duration : duration;
    if (d > 0) seekTo(d * fraction);
  }
  // Step one frame in `direction` (±1), but only while paused (the , / . shortcuts).
  function frameStep(direction: number) {
    const v = videoRef.current;
    if (!v || !v.paused) return;
    seekTo(v.currentTime + direction * FRAME_SECONDS);
  }

  // Time labels: elapsed on the left, remaining (as −m:ss) on the right.
  const elapsedLabel = formatTimecode(currentTime * 1000);
  const remainingSeconds = duration > 0 ? Math.max(duration - currentTime, 0) : 0;
  const remainingLabel = `-${formatTimecode(remainingSeconds * 1000)}`;

  const hls = tier != null && isHlsTier(tier);

  return (
    <>
      {/* The ONE video surface (video kinds only, once negotiated). The wrapper is a
          SINGLE stable node — only its `data-surface` (+ CSS) changes across stage /
          pip / bar-only, so the <video> inside is never re-parented/re-mounted and
          the Playback session is never re-negotiated. Audio keeps its <video> in the
          DOM (it plays the audio) but forced bar-only (visually hidden). The `surface`
          value and the fullscreen target (the bar) are owned by NowPlayingBar, so on a
          Queue advance this wrapper re-mounts UNDER a surface that stayed put — the
          bar's persistent black backdrop covers the gap so the hand-off is seamless. */}
      {status.kind === "ready" && (
        <div
          className={video ? "now-playing-surface" : "now-playing-surface now-playing-surface-audio"}
          data-surface={video ? surface : "bar-only"}
          data-testid={video ? "now-playing-stage" : "now-playing-audio"}
          style={
            video && surface === "pip" && pipOffset
              ? { transform: `translate(${pipOffset.x}px, ${pipOffset.y}px)` }
              : undefined
          }
        >
          {/* The media container is the STABLE first child; the surface controls are
              conditional SIBLINGS after it, so a surface change never reconciles the
              <video> out of the tree. In pip it doubles as the drag handle. */}
          <div
            className="now-playing-surface-media"
            onPointerDown={video && surface === "pip" ? onPipPointerDown : undefined}
            onPointerMove={video && surface === "pip" ? onPipPointerMove : undefined}
            onPointerUp={video && surface === "pip" ? onPipPointerUp : undefined}
          >
            <video
              ref={videoRef}
              className="player-video now-playing-video"
              data-testid="player-video"
              data-tier={status.decision.tier}
              data-stream-url={status.decision.streamUrl}
              data-autoplay={autoPlay ? "true" : "false"}
              {...(hls ? {} : { src: status.decision.streamUrl })}
              autoPlay={autoPlay}
              playsInline
              onLoadedMetadata={onLoadedMetadata}
              onDurationChange={onDurationChange}
              onTimeUpdate={onTimeUpdate}
              onPlay={onPlay}
              onPause={onPause}
              onSeeked={onSeeked}
              onEnded={onEnded}
            >
              {/* Out-of-band WebVTT tracks (ADR-0020) — DIRECT PLAY ONLY: one
                  <track> per deliverable text subtitle. The src is same-origin, so
                  the media cookie authenticates the fetch — no JS header needed.
                  Modes are managed imperatively (applySubtitleSelection); `default`
                  seeds the forced track so it shows even before the effect runs.
                  On the HLS tiers subtitles ride IN-BAND via the master playlist's
                  SUBTITLES rendition (slice 03), so no out-of-band <track> here —
                  it would be redundant and is unreliable on native HLS. */}
              {textTracks
                .filter((t) => !hls || t.source === "fetched")
                .map((t) => (
                  <track
                    key={t.id}
                    kind="subtitles"
                    src={t.url}
                    srcLang={t.language || undefined}
                    label={t.label}
                    default={t.id === selectedSubId}
                    data-sub-id={t.id}
                  />
                ))}
            </video>
          </div>

          {/* No on-screen stage overlay controls: on the stage — windowed OR
              fullscreen — the Now Playing bar itself is the control surface. It
              slides in on pointer/key activity and back out when idle. Fullscreen
              targets the BAR (the stage + the bar's control row share it as their
              parent), so the same docked transport survives fullscreen. */}

          {/* Custom PiP window: minimal controls — play/pause docked at the bottom,
              expand pinned to the top-left corner, and close (which stops playback)
              pinned to the top-right corner. */}
          {video && surface === "pip" && (
            <>
              <button
                className="nav-link now-playing-pip-corner now-playing-pip-corner-expand"
                type="button"
                data-testid="now-playing-pip-expand"
                aria-label="Expand to stage"
                onClick={() => setSurface("stage")}
              >
                ⤢
              </button>
              {/* Close stops playback entirely: emptying the queue makes
                  `queue.current` null → the whole bar unmounts (the stop-and-dismiss
                  exit), tearing down the <video>. */}
              <button
                className="nav-link now-playing-pip-corner now-playing-pip-corner-close"
                type="button"
                data-testid="now-playing-pip-close"
                aria-label="Stop"
                onClick={queue.clear}
              >
                ×
              </button>
              <div className="now-playing-pip-controls" data-testid="now-playing-pip-controls">
                <button
                  className="nav-link"
                  type="button"
                  data-testid="now-playing-pip-play"
                  aria-label={playing ? "Pause" : "Play"}
                  aria-pressed={playing}
                  onClick={togglePlay}
                >
                  {playing ? <PauseIcon /> : <PlayIcon />}
                </button>
              </div>
            </>
          )}

          {hlsError && (
            <p className="status status-error" data-testid="player-hls-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {hlsError}
            </p>
          )}
        </div>
      )}

      {/* End-of-episode Up Next card — fixed bottom-right, inside the bar so it
          survives fullscreen. Clicking it plays the next entry now (a manual skip →
          queue.next). */}
      {showUpNext && nextEntry && (
        <UpNextCard entry={nextEntry} secondsLeft={secondsLeft} onPlay={() => queue.next()} />
      )}

      <div className="now-playing-inner">
        {/* Left: thumbnail + now-playing label. */}
        <div className="now-playing-left">
          <div className="now-playing-thumb">
            <Poster
              titleId={titleId}
              title={primaryLabel}
              version={entry.title.artworkVersion}
              src={albumArtSrc}
            />
          </div>
          <div className="now-playing-labels">
            <NowPlayingLabel
              className="now-playing-title"
              testId="now-playing-title"
              to={titleLinkTo}
            >
              {primaryLabel}
            </NowPlayingLabel>
            {contextLabel && (
              <NowPlayingLabel
                className="now-playing-context"
                testId="now-playing-context"
                to={contextLinkTo}
              >
                {contextLabel}
              </NowPlayingLabel>
            )}
          </div>
        </div>

        {/* Center: prev / play-pause / next (plus the negotiation status when the
            element isn't ready yet), with the seekable progress bar stacked
            beneath — so the progress spans only the controls, not the whole bar. */}
        <div className="now-playing-center">
          <div className="now-playing-controls">
          {/* Shuffle toggle — MUSIC ONLY (a Track), absent for video. Non-destructive:
              un-shuffle restores the authored order. Reflects on/off via aria-pressed
              + the is-active style. */}
          {!video && (
            <button
              className={`nav-link now-playing-shuffle${queue.shuffle ? " is-active" : ""}`}
              type="button"
              data-testid="now-playing-shuffle"
              aria-label="Shuffle"
              aria-pressed={queue.shuffle}
              onClick={() => queue.setShuffle(!queue.shuffle)}
            >
              <ShuffleIcon />
            </button>
          )}
          <button
            className="nav-link"
            type="button"
            data-testid="player-prev"
            aria-label="Previous"
            onClick={queue.prev}
            disabled={!queue.hasPrev}
          >
            <PrevIcon />
          </button>
          {/* ±10s skip — video only (Movie/Episode), once the element is ready;
              absent for music (a Track has no seek-by-10s affordance). */}
          {status.kind === "ready" && video && (
            <button
              className="nav-link"
              type="button"
              data-testid="now-playing-skip-back"
              aria-label="Back 10 seconds"
              onClick={() => skip(-SKIP_SECONDS)}
            >
              −10
            </button>
          )}
          {status.kind === "ready" ? (
            <button
              className="nav-link now-playing-play"
              type="button"
              data-testid="now-playing-play-pause"
              aria-label={playing ? "Pause" : "Play"}
              aria-pressed={playing}
              onClick={togglePlay}
            >
              {playing ? <PauseIcon /> : <PlayIcon />}
            </button>
          ) : (
            <PlayerStatus session={session} title={primaryLabel} />
          )}
          {/* Stop — sits immediately right of play/pause. Empties the Queue, which
              makes `queue.current` null → the whole bar unmounts (the stop-and-dismiss
              exit shared with the queue drawer's Clear button), ending the session. */}
          <button
            className="nav-link"
            type="button"
            data-testid="now-playing-stop"
            aria-label="Stop"
            onClick={queue.clear}
          >
            <StopIcon />
          </button>
          {status.kind === "ready" && video && (
            <button
              className="nav-link"
              type="button"
              data-testid="now-playing-skip-forward"
              aria-label="Forward 10 seconds"
              onClick={() => skip(SKIP_SECONDS)}
            >
              +10
            </button>
          )}
          <button
            className="nav-link"
            type="button"
            data-testid="player-next"
            aria-label="Next"
            onClick={queue.next}
            disabled={!queue.hasNext}
          >
            <NextIcon />
          </button>
          {/* Repeat — MUSIC ONLY, absent for video. Three visually-distinct states:
              off (dim 🔁), repeat-all (active 🔁), repeat-one (🔂). Distinct
              aria-label + data-repeat per state; a click cycles off → all → one. */}
          {!video && (
            <button
              className={`nav-link now-playing-repeat${queue.repeat !== "off" ? " is-active" : ""}`}
              type="button"
              data-testid="now-playing-repeat"
              data-repeat={queue.repeat}
              aria-label={
                queue.repeat === "one"
                  ? "Repeat one"
                  : queue.repeat === "all"
                    ? "Repeat all"
                    : "Repeat off"
              }
              aria-pressed={queue.repeat !== "off"}
              onClick={queue.cycleRepeat}
            >
              <RepeatIcon />
              {queue.repeat === "one" && (
                <span className="now-playing-repeat-one" aria-hidden="true">
                  1
                </span>
              )}
            </button>
          )}
          </div>

          {/* The seekable progress bar under the transport: elapsed on the left,
              remaining on the right, a range input to click/drag-seek in between.
              Rendered once the element is ready for both music and video. Sits
              inside the centre so it spans only the controls' width. */}
          {status.kind === "ready" && (
            <div className="now-playing-progress">
              <span className="now-playing-time" data-testid="now-playing-elapsed">
                {elapsedLabel}
              </span>
              <input
                className="now-playing-seek"
                type="range"
                min={0}
                max={duration > 0 ? duration : 0}
                step={0.1}
                data-testid="now-playing-progress"
                aria-label="Seek"
                value={Math.min(currentTime, duration > 0 ? duration : currentTime)}
                onChange={(e) => seekTo(Number(e.target.value))}
              />
              <span className="now-playing-time" data-testid="now-playing-remaining">
                {remainingLabel}
              </span>
            </div>
          )}
        </div>

        {/* Right: volume (icon toggles mute, slider sets level), the video surface
            controls (expand-to-stage / fullscreen — video only), and the queue
            button (opens the drawer). */}
        <div className="now-playing-right">
          <div className="now-playing-volume">
            <button
              className="nav-link now-playing-mute"
              type="button"
              data-testid="now-playing-mute"
              aria-label={prefs.muted ? "Unmute" : "Mute"}
              aria-pressed={prefs.muted}
              onClick={prefs.toggleMuted}
            >
              {prefs.muted || prefs.volume === 0 ? <VolumeOffIcon /> : <VolumeOnIcon />}
            </button>
            <input
              className="now-playing-volume-slider"
              type="range"
              min={0}
              max={1}
              step={0.01}
              data-testid="now-playing-volume-slider"
              aria-label="Volume"
              value={prefs.muted ? 0 : prefs.volume}
              onChange={(e) => {
                const next = Number(e.target.value);
                prefs.setVolume(next);
                // Dragging the slider off zero unmutes; dragging to zero mutes.
                if (next > 0 && prefs.muted) prefs.setMuted(false);
                else if (next === 0 && !prefs.muted) prefs.setMuted(true);
              }}
            />
          </div>
          {/* Surface toggle + fullscreen (video only). Off the stage the button
              re-expands the video to it; on the stage it shrinks the video into the
              corner PiP window (this is the one on-screen control the immersive
              overlay used to own that the bar lacked). Fullscreen promotes to the
              stage first. */}
          {status.kind === "ready" && video && (
            <>
              {surface === "stage" ? (
                <button
                  className="nav-link now-playing-lg-icon"
                  type="button"
                  data-testid="now-playing-collapse"
                  aria-label="Collapse to picture in picture"
                  onClick={collapseStage}
                >
                  ⤡
                </button>
              ) : (
                <button
                  className="nav-link now-playing-lg-icon"
                  type="button"
                  data-testid="now-playing-expand"
                  aria-label="Expand to stage"
                  onClick={() => setSurface("stage")}
                >
                  ⤢
                </button>
              )}
              <button
                className="nav-link now-playing-lg-icon"
                type="button"
                data-testid="now-playing-fullscreen"
                aria-label="Fullscreen"
                onClick={toggleFullscreen}
              >
                ⛶
              </button>
            </>
          )}
          {/* Captions menu — video only, once ready, when the Title offers at least
              one text OR image track. Lists Off + every text track (client-side,
              instant) + every image track (burned in via a fresh transcode
              negotiation, subtitles/04), ordered by the preferred language. */}
          {status.kind === "ready" && video && (
            <div className="now-playing-captions">
              <button
                className={`nav-link now-playing-lg-icon now-playing-cc${currentSubId ? " is-active" : ""}`}
                type="button"
                data-testid="now-playing-captions"
                aria-label="Subtitles"
                aria-haspopup="menu"
                aria-expanded={captionsOpen}
                aria-pressed={currentSubId != null}
                onClick={() =>
                  setCaptionsOpen((o) => {
                    if (o) setSearchState("idle");
                    return !o;
                  })
                }
              >
                <CaptionsIcon />
              </button>
              {captionsOpen && (
                <ul
                  className="now-playing-captions-menu"
                  data-testid="now-playing-captions-menu"
                  role="menu"
                >
                  <li role="none">
                    <button
                      className="now-playing-captions-item"
                      type="button"
                      role="menuitemradio"
                      aria-checked={currentSubId == null}
                      data-testid="captions-off"
                      onClick={() => selectSubtitle(null)}
                    >
                      {currentSubId == null ? "✓ " : ""}Off
                    </button>
                  </li>
                  {textTracks.map((t) => (
                    <li role="none" key={t.id}>
                      <button
                        className="now-playing-captions-item"
                        type="button"
                        role="menuitemradio"
                        aria-checked={currentSubId === t.id}
                        data-sub-id={t.id}
                        data-sub-lang={t.language || ""}
                        onClick={() => selectSubtitle(t.id)}
                      >
                        {currentSubId === t.id ? "✓ " : ""}
                        {t.label}
                      </button>
                    </li>
                  ))}
                  {imageTracks.map((t) => (
                    <li role="none" key={t.id}>
                      <button
                        className="now-playing-captions-item now-playing-captions-image"
                        type="button"
                        role="menuitemradio"
                        aria-checked={currentSubId === t.id}
                        data-sub-id={t.id}
                        data-sub-lang={t.language || ""}
                        data-sub-kind="image"
                        onClick={() => selectSubtitle(t.id)}
                      >
                        {currentSubId === t.id ? "✓ " : ""}
                        {t.label} (burn in)
                      </button>
                    </li>
                  ))}
                  {/* Search online (subtitles/05): fetch a subtitle from the external
                      provider for a language this Title lacks. Available to any User. */}
                  <li role="none" className="now-playing-captions-sep">
                    {searchState === "results" || searchState === "empty" ? (
                      <ul
                        className="now-playing-captions-candidates"
                        data-testid="captions-candidates"
                        role="menu"
                      >
                        {searchState === "empty" && (
                          <li className="now-playing-captions-note" data-testid="captions-none-found">
                            No subtitles found online
                          </li>
                        )}
                        {candidates.map((c) => (
                          <li role="none" key={c.id}>
                            <button
                              className="now-playing-captions-item"
                              type="button"
                              role="menuitem"
                              data-testid="captions-candidate"
                              data-candidate-id={c.id}
                              disabled={fetchingId != null}
                              onClick={() => pickCandidate(c)}
                            >
                              {fetchingId === c.id ? "… " : ""}
                              {c.label}
                            </button>
                          </li>
                        ))}
                      </ul>
                    ) : (
                      <button
                        className="now-playing-captions-item now-playing-captions-search"
                        type="button"
                        role="menuitem"
                        data-testid="captions-search-online"
                        disabled={searchState === "searching"}
                        onClick={searchOnline}
                      >
                        {searchState === "searching"
                          ? "Searching…"
                          : searchState === "error"
                            ? "Search failed — retry"
                            : "Search online…"}
                      </button>
                    )}
                  </li>
                </ul>
              )}
            </div>
          )}
          {/* Video menu — video only, once ready, and ONLY when the File offers more
              than one selectable video Stream (a single-video File has nothing to
              pick, so the menu is hidden). Lists every video Stream (title tag, else
              a resolution token), marks the active one. A pick is a fresh
              re-negotiation (brief re-buffer, possible 503) — the image-subtitle
              model, not the instant audio flip. Copy is "Video" — never "Video track"
              (CONTEXT.md). */}
          {status.kind === "ready" && video && videoStreams.length >= 2 && (
            <div className="now-playing-video-select">
              <button
                className="nav-link now-playing-lg-icon now-playing-video-btn"
                type="button"
                data-testid="now-playing-video"
                aria-label="Video"
                aria-haspopup="menu"
                aria-expanded={videoOpen}
                onClick={() => setVideoOpen((o) => !o)}
              >
                <VideoIcon />
              </button>
              {videoOpen && (
                <ul
                  className="now-playing-video-menu"
                  data-testid="now-playing-video-menu"
                  role="menu"
                >
                  {videoStreams.map((s) => (
                    <li role="none" key={s.id}>
                      <button
                        className="now-playing-captions-item now-playing-video-item"
                        type="button"
                        role="menuitemradio"
                        aria-checked={selectedVideoId === s.id}
                        data-video-id={s.id}
                        onClick={() => selectVideo(s.id)}
                      >
                        {selectedVideoId === s.id ? "✓ " : ""}
                        {s.label}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
          {/* Audio menu — video only, once ready, and ONLY when the File offers more
              than one audio Stream (a single-audio File has nothing to pick, so the
              menu is inert/hidden). Lists every audio Stream (language/layout/label),
              marks the active one, ordered by the preferred audio language. Switching
              is in-band on the HLS tiers; a direct-play non-default pick escalates
              once. Copy is "Audio" — never "Audio track" (CONTEXT.md). */}
          {status.kind === "ready" && video && audioStreams.length >= 2 && (
            <div className="now-playing-audio">
              <button
                className="nav-link now-playing-lg-icon now-playing-audio-btn"
                type="button"
                data-testid="now-playing-audio"
                aria-label="Audio"
                aria-haspopup="menu"
                aria-expanded={audioOpen}
                onClick={() => setAudioOpen((o) => !o)}
              >
                <AudioIcon />
              </button>
              {audioOpen && (
                <ul
                  className="now-playing-audio-menu"
                  data-testid="now-playing-audio-menu"
                  role="menu"
                >
                  {audioStreams.map((s) => (
                    <li role="none" key={s.id}>
                      <button
                        className="now-playing-captions-item now-playing-audio-item"
                        type="button"
                        role="menuitemradio"
                        aria-checked={selectedAudioId === s.id}
                        data-audio-id={s.id}
                        data-audio-lang={s.language || ""}
                        onClick={() => selectAudio(s.id)}
                      >
                        {selectedAudioId === s.id ? "✓ " : ""}
                        {s.label}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
          {/* Queue button — hidden in fullscreen (the drawer lives outside the
              fullscreened bar, so it couldn't show there anyway; see CSS). */}
          <button
            className="nav-link now-playing-lg-icon now-playing-queue"
            type="button"
            data-testid="now-playing-queue-button"
            aria-label="Open queue"
            onClick={onOpenQueue}
          >
            <QueueIcon />
          </button>
        </div>
      </div>
    </>
  );
}

/** The negotiation status shown in the transport's centre before the media element
 * is ready: preparing, a busy state with the auto-/manual-retry UX, the honest
 * unsupported dead-end, or a readable error. Relocated verbatim from PlayerScreen. */
function PlayerStatus({
  session,
  title,
}: {
  session: ReturnType<typeof usePlayerSession>;
  title: string;
}) {
  const status = session.status;
  if (status.kind === "ready") return null; // the play/pause button is shown instead
  if (status.kind === "negotiating") {
    return (
      <span className="status status-loading" data-testid="player-negotiating">
        Preparing playback&hellip;
      </span>
    );
  }
  if (status.kind === "busy") {
    return (
      <span className="notice" data-testid="player-busy">
        <span data-testid="player-busy-message" role="status">
          {status.message}
        </span>
        {!status.retrying && (
          <button
            className="nav-link"
            type="button"
            data-testid="player-busy-retry"
            onClick={() => session.retry()}
          >
            Try again
          </button>
        )}
      </span>
    );
  }
  if (status.kind === "unsupported") {
    return (
      <span className="notice" data-testid="player-unsupported">
        <span data-testid="player-unsupported-message">{status.message}</span>
        <span className="status status-loading" data-testid="player-unsupported-reason">
          Reason: {status.reason}
        </span>
      </span>
    );
  }
  // status.kind === "error"
  return (
    <span className="status status-error" data-testid="player-negotiate-error" role="alert">
      <span className="dot dot-error" aria-hidden="true" />
      {status.message} ({title})
    </span>
  );
}
