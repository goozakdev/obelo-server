import type {
  DeviceProfile,
  PlaybackConstraints,
  VideoCodecSupport,
} from "../api/types";
import { preferredSubtitleLang } from "./subtitles";
import { preferredAudioLang } from "./audio";
import { canPlayHlsNatively } from "./nativeHls";

// Derive the Capability profile from the ACTUAL browser, not a hand-written
// list. The server merges this profile + constraints to pick a tier (ADR-0003);
// in this slice the only success tier is directPlay, so the profile must report
// exactly what the browser can play natively:
//
//   - a container/codec the browser CAN play → it appears in the profile →
//     directPlay (e.g. mp4 + h264 + aac, the Dune fixture);
//   - a container/codec the browser CANNOT play → it is absent → the server
//     returns TRANSCODE_REQUIRED (e.g. matroska + mpeg4, the Blade Runner
//     fixture), which the player renders as an honest unsupported state.
//
// We probe with HTMLVideoElement.canPlayType (the broadly-supported signal) and
// fall back to MediaSource.isTypeSupported where present. Probing — rather than
// asserting "browsers do mp4" — keeps the profile honest on whatever engine the
// user actually runs.

// Each container we might claim, with the MIME(s) and codec strings used to
// probe it. The server folds ffprobe aliases ("matroska" ↔ "mkv",
// "mov,mp4,…" ↔ "mp4"), so we report the short container names it expects.
interface ContainerProbe {
  /** Container name the server matches against the File (NormalizeContainer). */
  container: string;
  /** MIME type passed to canPlayType / isTypeSupported. */
  mime: string;
}

// Video codecs we probe, each with the RFC 6381 codec string for the canPlayType
// query and the short codec name the server's device profile expects.
interface VideoCodecProbe {
  /** Codec name in the device profile (matched against File.videoCodec). */
  codec: string;
  /** A representative codecs= parameter for the canPlayType probe. */
  codecParam: string;
  /** MIME container to wrap the probe in (codecs are container-scoped). */
  mime: string;
  maxResolution?: string;
}

interface AudioCodecProbe {
  codec: string;
  codecParam: string;
  mime: string;
}

const VIDEO_CONTAINERS: ContainerProbe[] = [
  { container: "mp4", mime: "video/mp4" },
  { container: "webm", mime: "video/webm" },
  { container: "mkv", mime: "video/x-matroska" },
  { container: "matroska", mime: "video/x-matroska" },
  { container: "ogg", mime: "video/ogg" },
];

const VIDEO_CODECS: VideoCodecProbe[] = [
  // h264 (avc1) — the broadly-supported baseline; cap at 1080p, a sane web cap.
  { codec: "h264", codecParam: 'avc1.640028', mime: "video/mp4", maxResolution: "1080p" },
  // hevc/h265 — supported on Safari/some hardware; allow up to 2160p where present.
  { codec: "hevc", codecParam: "hvc1.1.6.L93.B0", mime: "video/mp4", maxResolution: "2160p" },
  // vp9 / av1 — modern web codecs, typically in webm/mp4.
  { codec: "vp9", codecParam: "vp09.00.10.08", mime: "video/webm", maxResolution: "2160p" },
  { codec: "av1", codecParam: "av01.0.05M.08", mime: "video/mp4", maxResolution: "2160p" },
];

// NOTE — we deliberately DO NOT probe/advertise ac3 (Dolby Digital) or eac3
// (Dolby Digital+). Safari's canPlayType returns "maybe" for `ac-3`/`ec-3`
// (macOS carries the OS decoders for AirPlay/passthrough), but Safari cannot
// actually decode AC3/E-AC3 in an in-page <video> — neither in a progressive mp4
// (direct play) nor in the MPEG-TS the remux tier emits — so advertising them
// produces silent/dead playback of any file whose default audio is AC3/E-AC3.
//
// This matters because a false-positive AUDIO codec is NOT safe the way a false-
// positive container/video codec is: an unclaimed container/video codec falls to
// the server's transcode tier (the header's "degrades to TRANSCODE_REQUIRED"
// invariant), but the server HONORS a claimed audio codec by COPYING it verbatim
// (direct play / remux / in-band rendition), never re-encoding — so a wrong audio
// claim ships bytes the browser can't decode. By omitting ac3/eac3 the server
// transcodes them to AAC (exactly as it already does DTS/TrueHD), which plays
// everywhere. Chrome/Firefox already report "" for these, so this only corrects
// Safari. A genuinely AC3-capable NATIVE client sends its own honest profile and
// still gets passthrough — the server contract is unchanged; only the browser's
// self-report is made honest.
const AUDIO_CODECS: AudioCodecProbe[] = [
  { codec: "aac", codecParam: "mp4a.40.2", mime: "video/mp4" },
  { codec: "mp3", codecParam: "mp4a.40.34", mime: "video/mp4" },
  { codec: "opus", codecParam: "opus", mime: "video/webm" },
  { codec: "vorbis", codecParam: "vorbis", mime: "video/webm" },
  { codec: "flac", codecParam: "flac", mime: "video/mp4" },
];

/** True when the browser reports it can (probably/maybe) play `mime`. We accept
 * both "probably" and "maybe": "maybe" is the honest answer for many real
 * codecs, and a false positive degrades to the server's TRANSCODE_REQUIRED
 * rather than a broken stream. An empty string is a definite no. */
function canPlay(video: HTMLVideoElement, mime: string): boolean {
  let verdict = "";
  try {
    verdict = video.canPlayType(mime);
  } catch {
    verdict = "";
  }
  if (verdict === "probably" || verdict === "maybe") return true;
  // Fall back to MediaSource where canPlayType was unsure/empty.
  const MS = (globalThis as { MediaSource?: { isTypeSupported?: (t: string) => boolean } })
    .MediaSource;
  if (MS && typeof MS.isTypeSupported === "function") {
    try {
      return MS.isTypeSupported(mime);
    } catch {
      return false;
    }
  }
  return false;
}

/** Probe the browser and build the device profile + per-request constraints to
 * send on a playback request. `maxBitrate` is generous (we are not bandwidth-
 * constrained in a self-hosted LAN); `maxResolution` tracks the viewport so we
 * never ask for more pixels than the screen can show. */
export function deriveCapabilityProfile(): {
  deviceProfile: DeviceProfile;
  constraints: PlaybackConstraints;
} {
  const video = document.createElement("video");

  const containers = VIDEO_CONTAINERS.filter((c) => canPlay(video, c.mime))
    .map((c) => c.container)
    // De-dup (mkv + matroska probe the same mime); keep first occurrence.
    .filter((c, i, arr) => arr.indexOf(c) === i);

  const videoCodecs: VideoCodecSupport[] = VIDEO_CODECS.filter((c) =>
    canPlay(video, `${c.mime}; codecs="${c.codecParam}"`),
  ).map((c) => ({ codec: c.codec, maxResolution: c.maxResolution }));

  const audioCodecs = AUDIO_CODECS.filter((c) =>
    canPlay(video, `${c.mime}; codecs="${c.codecParam}"`),
  ).map((c) => c.codec);

  return {
    deviceProfile: {
      containers,
      videoCodecs,
      audioCodecs,
      // Stereo is universally safe in a browser; multichannel passthrough is not
      // guaranteed, so we cap conservatively. The server only blocks when a File
      // exceeds this, which is the honest outcome.
      maxAudioChannels: 2,
      // WebVTT is the only text subtitle format a <track> renders natively.
      textSubtitleFormats: ["webvtt"],
      // On the hls.js (MSE) path a copied HEVC video rides MPEG-TS — hls.js
      // (≥1.6) demuxes HEVC-in-TS itself, and the TS pipeline's dictated cuts
      // give the exact playlists strict MSE playback needs. The native path
      // (Safari) must NOT set this: Apple's player requires HEVC in fMP4.
      hevcInMpegts: !canPlayHlsNatively(video),
    },
    constraints: {
      maxBitrate: maxBitrate(),
      maxResolution: viewportMaxResolution(),
      // The viewer's preferred audio language (browser locale). The server uses it
      // in the default-audio resolution order (memory → preferredAudioLang →
      // default disposition → first, ADR-0023) so the delivered audio matches the
      // viewer's usual language absent a remembered pick; the client sorts the Audio
      // menu by the same value (audio-streams/04).
      preferredAudioLang: preferredAudioLang() || undefined,
      // The viewer's preferred subtitle language (browser locale). The server uses
      // it only to sort/label the track menu (no persistent per-User preference
      // this slice); the client sorts by the same value (ADR-0020).
      preferredSubtitleLang: preferredSubtitleLang() || undefined,
    },
  };
}

// A generous bitrate cap — direct play on a LAN is not bandwidth-limited, and a
// too-low cap would needlessly flip a playable file into TRANSCODE_REQUIRED.
function maxBitrate(): number {
  return 100_000_000; // 100 Mbps
}

// Cap requested resolution at the viewport's height in device pixels, bucketed
// to the standard rungs the server understands. There is no point asking for 4K
// on a 720p panel.
function viewportMaxResolution(): string {
  const dpr = typeof window !== "undefined" ? window.devicePixelRatio || 1 : 1;
  const h =
    typeof window !== "undefined" && window.screen?.height
      ? window.screen.height * dpr
      : 1080;
  if (h >= 2160) return "2160p";
  if (h >= 1080) return "1080p";
  if (h >= 720) return "720p";
  return "480p";
}
