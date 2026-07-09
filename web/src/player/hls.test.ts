import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { attachHls, canPlayHlsNatively } from "./hls";

// Unit tests for the HLS attach seam. hls.js itself needs MSE (absent in jsdom),
// so we MOCK the hls.js module for the MSE branch and exercise the native-HLS
// branch via a stubbed canPlayType. The real hls.js path is covered by the
// Playwright E2E in Chromium.

const { HlsMock, instances } = vi.hoisted(() => {
  const instances: Array<{
    loadSource: ReturnType<typeof vi.fn>;
    attachMedia: ReturnType<typeof vi.fn>;
    destroy: ReturnType<typeof vi.fn>;
    on: ReturnType<typeof vi.fn>;
    handlers: Record<string, () => void>;
    subtitleTrack: number;
    subtitleDisplay: boolean;
    audioTrack: number;
    config: Record<string, unknown>;
  }> = [];
  class HlsMock {
    static supported = true;
    static isSupported() {
      return HlsMock.supported;
    }
    // The subset of Hls.Events the attach seam subscribes to.
    static Events = {
      SUBTITLE_TRACKS_UPDATED: "hlsSubtitleTracksUpdated",
      AUDIO_TRACKS_UPDATED: "hlsAudioTracksUpdated",
      ERROR: "hlsError",
    };
    loadSource = vi.fn();
    attachMedia = vi.fn();
    destroy = vi.fn();
    startLoad = vi.fn();
    recoverMediaError = vi.fn();
    swapAudioCodec = vi.fn();
    // Selection state the seam drives (mirrors hls.js's real props).
    subtitleTrack = -1;
    subtitleDisplay = false;
    audioTrack = 0;
    handlers: Record<string, (...args: unknown[]) => void> = {};
    on = vi.fn((evt: string, cb: (...args: unknown[]) => void) => {
      this.handlers[evt] = cb;
    });
    config: Record<string, unknown>;
    constructor(config: Record<string, unknown> = {}) {
      this.config = config;
      instances.push(this);
    }
  }
  return { HlsMock, instances };
});

vi.mock("hls.js", () => ({ default: HlsMock }));

function makeVideo(nativeHls: boolean): HTMLVideoElement {
  const v = document.createElement("video");
  vi.spyOn(v, "canPlayType").mockImplementation((mime: string) =>
    nativeHls && mime.includes("mpegurl") ? "maybe" : "",
  );
  if (nativeHls) {
    // The native path also requires the audioTracks API (Safari has it; Chrome's
    // is flag-gated, which routes Chrome to hls.js even though it now answers
    // canPlayType for HLS — the in-band-switch-does-nothing fix).
    Object.defineProperty(v, "audioTracks", { value: { length: 0 }, configurable: true });
  }
  return v;
}

beforeEach(() => {
  instances.length = 0;
  HlsMock.supported = true;
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("attachHls", () => {
  it("native HLS (Safari) → sets the <video> src to the playlist, no hls.js", async () => {
    const v = makeVideo(true);
    const att = await attachHls(v, "/api/v1/sessions/s/hls/index.m3u8");
    expect(att.mode).toBe("native");
    expect(v.getAttribute("src")).toBe("/api/v1/sessions/s/hls/index.m3u8");
    expect(instances).toHaveLength(0);
    att.detach();
    expect(v.getAttribute("src")).toBeNull();
  });

  it("native HLS setAudioTrack toggles the matching video.audioTracks entry", async () => {
    const v = makeVideo(true);
    // jsdom has no audioTracks; stub a minimal AudioTrackList the seam drives.
    const tracks = [
      { enabled: true },
      { enabled: false },
      { enabled: false },
    ];
    Object.defineProperty(v, "audioTracks", {
      configurable: true,
      value: Object.assign(tracks, { length: tracks.length }),
    });
    const att = await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    att.setAudioTrack(2);
    expect(tracks.map((t) => t.enabled)).toEqual([false, false, true]);
    att.setAudioTrack(0);
    expect(tracks.map((t) => t.enabled)).toEqual([true, false, false]);
  });

  it("MSE browser → drives playback with hls.js (loadSource + attachMedia)", async () => {
    const v = makeVideo(false);
    const url = "/api/v1/sessions/s/hls/index.m3u8";
    const att = await attachHls(v, url);
    expect(att.mode).toBe("hls.js");
    expect(instances).toHaveLength(1);
    expect(instances[0].loadSource).toHaveBeenCalledWith(url);
    expect(instances[0].attachMedia).toHaveBeenCalledWith(v);
    // No progressive src on the HLS-over-MSE path.
    expect(v.getAttribute("src")).toBeNull();
    att.detach();
    expect(instances[0].destroy).toHaveBeenCalled();
  });

  it("caps the hls.js buffers so remux bitrates fit Chrome's SourceBuffer quota", async () => {
    // hls.js's defaults (30s ahead, unlimited back-buffer) exceed Chrome's
    // ~150MB SourceBuffer quota at copied-video remux bitrates (60-80 Mbps 4K),
    // spamming mediaError/bufferFullError on every start. The seam must pass
    // finite, bounded buffer caps.
    const v = makeVideo(false);
    await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    const cfg = instances[0].config;
    expect(cfg.maxBufferLength).toBeLessThanOrEqual(15);
    expect(cfg.maxMaxBufferLength).toBeLessThanOrEqual(60);
    expect(cfg.backBufferLength).toBeLessThanOrEqual(60);
  });

  it("no native HLS AND hls.js unsupported → throws (honest dead-end)", async () => {
    HlsMock.supported = false;
    const v = makeVideo(false);
    await expect(attachHls(v, "/x.m3u8")).rejects.toThrow(/cannot play HLS/i);
  });

  it("setTextTrack drives the hls.js in-band SUBTITLES rendition (slice 03)", async () => {
    const v = makeVideo(false);
    const att = await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    const hls = instances[0];

    // Selecting rendition 1 enables display + sets the track index.
    att.setTextTrack(1);
    expect(hls.subtitleDisplay).toBe(true);
    expect(hls.subtitleTrack).toBe(1);

    // Off turns display off and clears the track (-1 = none).
    att.setTextTrack(null);
    expect(hls.subtitleDisplay).toBe(false);
    expect(hls.subtitleTrack).toBe(-1);
  });

  it("re-applies the desired subtitle once the renditions load (async parse)", async () => {
    const v = makeVideo(false);
    const att = await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    const hls = instances[0];

    // A selection made before the renditions parsed is remembered, then re-applied
    // when hls.js fires SUBTITLE_TRACKS_UPDATED.
    att.setTextTrack(0);
    hls.subtitleTrack = -1; // simulate hls.js resetting during (re)parse
    hls.handlers[HlsMock.Events.SUBTITLE_TRACKS_UPDATED]?.();
    expect(hls.subtitleTrack).toBe(0);
    expect(hls.subtitleDisplay).toBe(true);
  });

  it("setAudioTrack drives the hls.js in-band AUDIO rendition (audio-streams/04)", async () => {
    const v = makeVideo(false);
    const att = await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    const hls = instances[0];

    // Selecting rendition 2 sets the audio-track index (audio is never off).
    att.setAudioTrack(2);
    expect(hls.audioTrack).toBe(2);
    att.setAudioTrack(0);
    expect(hls.audioTrack).toBe(0);
  });

  it("re-applies the desired audio once the renditions load (async parse)", async () => {
    const v = makeVideo(false);
    const att = await attachHls(v, "/api/v1/sessions/s/hls/master.m3u8");
    const hls = instances[0];

    // A pick made before the AUDIO renditions parsed is remembered, then re-applied
    // when hls.js fires AUDIO_TRACKS_UPDATED (which also fires when hls.js re-selects
    // its default and would otherwise stomp the viewer's choice).
    att.setAudioTrack(1);
    hls.audioTrack = 0; // simulate hls.js resetting to the default during (re)parse
    hls.handlers[HlsMock.Events.AUDIO_TRACKS_UPDATED]?.();
    expect(hls.audioTrack).toBe(1);
  });
});

describe("canPlayHlsNatively", () => {
  it("true when the browser claims the HLS MIME and exposes audioTracks (Safari)", () => {
    expect(canPlayHlsNatively(makeVideo(true))).toBe(true);
  });
  it("false otherwise", () => {
    expect(canPlayHlsNatively(makeVideo(false))).toBe(false);
  });
  it("false when the browser claims HLS but lacks audioTracks (modern Chrome)", () => {
    // Chromium ships native HLS (canPlayType answers "maybe") but its
    // video.audioTracks is flag-gated: on the native player an in-band audio switch
    // silently does nothing and none of the hls.js error handling runs. Such a
    // browser must take the hls.js path.
    const v = document.createElement("video");
    vi.spyOn(v, "canPlayType").mockImplementation((mime: string) =>
      mime.includes("mpegurl") ? "maybe" : "",
    );
    Object.defineProperty(v, "audioTracks", { value: undefined, configurable: true });
    expect(canPlayHlsNatively(v)).toBe(false);
  });
});

describe("hls.js error recovery", () => {
  // hls.js does not log its errors and a FATAL one silently stops all loading —
  // without this wiring the player froze with no console output at all (the
  // Chrome silent-stall bug). The seam must log every error and run the standard
  // recovery ladder for fatal ones.
  async function attachWithError(opts?: { onSessionLost?: () => void }) {
    const att = await attachHls(makeVideo(false), "/hls/master.m3u8", opts);
    expect(att.mode).toBe("hls.js");
    const hls = instances[0];
    expect(hls.handlers[HlsMock.Events.ERROR], "an ERROR handler is registered").toBeTruthy();
    return hls;
  }

  it("restarts loading on a fatal network error", async () => {
    const hls = await attachWithError();
    vi.spyOn(console, "error").mockImplementation(() => {});
    hls.handlers[HlsMock.Events.ERROR]?.({}, { type: "networkError", details: "fragLoadError", fatal: true });
    expect(hls.startLoad).toHaveBeenCalled();
    expect(hls.recoverMediaError).not.toHaveBeenCalled();
  });

  it("escalates a PERSISTENT fatal network error to onSessionLost (reaped session)", async () => {
    // The reported freeze: after a long pause the server reaped the session, so its
    // playlist + segments all 404. hls.js raises fatal networkErrors; startLoad on
    // dead URLs can't recover, so a run of them re-negotiates a fresh session.
    const onSessionLost = vi.fn();
    const hls = await attachWithError({ onSessionLost });
    vi.spyOn(console, "error").mockImplementation(() => {});
    const fire = () =>
      hls.handlers[HlsMock.Events.ERROR]?.({}, { type: "networkError", details: "levelLoadError", fatal: true });

    // First fatal: still treated as transient — restart loading, don't give up yet.
    fire();
    expect(hls.startLoad).toHaveBeenCalledTimes(1);
    expect(onSessionLost).not.toHaveBeenCalled();

    // A second fatal right after means restarting isn't helping — the session is
    // gone. Escalate to a re-negotiation and DON'T loop startLoad on dead URLs.
    fire();
    expect(onSessionLost).toHaveBeenCalledTimes(1);
    expect(hls.startLoad).toHaveBeenCalledTimes(1);
  });

  it("without onSessionLost, keeps the old best-effort restart (no re-negotiator wired)", async () => {
    const hls = await attachWithError();
    vi.spyOn(console, "error").mockImplementation(() => {});
    const fire = () =>
      hls.handlers[HlsMock.Events.ERROR]?.({}, { type: "networkError", details: "levelLoadError", fatal: true });
    fire();
    fire();
    // No onSessionLost to escalate to → both fall back to restarting the load.
    expect(hls.startLoad).toHaveBeenCalledTimes(2);
  });

  it("recovers a fatal media error, escalating to swapAudioCodec on a quick repeat", async () => {
    const hls = await attachWithError();
    vi.spyOn(console, "error").mockImplementation(() => {});
    const fire = () =>
      hls.handlers[HlsMock.Events.ERROR]?.({}, { type: "mediaError", details: "bufferAppendError", fatal: true });
    fire();
    expect(hls.recoverMediaError).toHaveBeenCalledTimes(1);
    expect(hls.swapAudioCodec).not.toHaveBeenCalled();
    // A second fatal media error right away escalates per the hls.js playbook.
    fire();
    expect(hls.swapAudioCodec).toHaveBeenCalledTimes(1);
    expect(hls.recoverMediaError).toHaveBeenCalledTimes(2);
    // A third inside the window is left alone (genuinely dead, reason on console).
    fire();
    expect(hls.recoverMediaError).toHaveBeenCalledTimes(2);
  });

  it("logs but does not react to a non-fatal error", async () => {
    const hls = await attachWithError();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    hls.handlers[HlsMock.Events.ERROR]?.({}, { type: "networkError", details: "fragLoadError", fatal: false });
    expect(warn).toHaveBeenCalled();
    expect(hls.startLoad).not.toHaveBeenCalled();
    expect(hls.recoverMediaError).not.toHaveBeenCalled();
  });
});
