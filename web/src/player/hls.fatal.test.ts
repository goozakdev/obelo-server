import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { attachHls } from "./hls";

// R04-11: a fatal hls.js error the seam gives up on is no longer a console-only
// freeze — it is reported through `onFatal`, and a session-lost escalation the
// caller declines (returns false) falls back to restarting the load.

const { HlsMock, instances } = vi.hoisted(() => {
  const instances: Array<{
    startLoad: ReturnType<typeof vi.fn>;
    handlers: Record<string, (...args: unknown[]) => void>;
  }> = [];
  class HlsMock {
    static isSupported() {
      return true;
    }
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
    subtitleTrack = -1;
    subtitleDisplay = false;
    audioTrack = 0;
    handlers: Record<string, (...args: unknown[]) => void> = {};
    on = vi.fn((evt: string, cb: (...args: unknown[]) => void) => {
      this.handlers[evt] = cb;
    });
    constructor() {
      instances.push(this);
    }
  }
  return { HlsMock, instances };
});

vi.mock("hls.js", () => ({ default: HlsMock }));

beforeEach(() => {
  instances.length = 0;
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

async function attach(opts: Parameters<typeof attachHls>[2]) {
  await attachHls(document.createElement("video"), "/hls/master.m3u8", opts);
  const hls = instances[0];
  const fire = (type: string) =>
    hls.handlers[HlsMock.Events.ERROR]?.({}, { type, details: "x", fatal: true });
  return { hls, fire };
}

describe("attachHls — fatal errors are surfaced", () => {
  it("reports a media error that outlasts both recoveries", async () => {
    const onFatal = vi.fn();
    const { fire } = await attach({ onFatal });
    fire("mediaError");
    fire("mediaError");
    expect(onFatal).not.toHaveBeenCalled();
    fire("mediaError");
    expect(onFatal).toHaveBeenCalledTimes(1);
  });

  it("reports an unrecoverable error type", async () => {
    const onFatal = vi.fn();
    const { fire } = await attach({ onFatal });
    fire("otherError");
    expect(onFatal).toHaveBeenCalledTimes(1);
  });

  it("falls back to startLoad when the caller declines the session-lost escalation", async () => {
    const onSessionLost = vi.fn(() => false);
    const { hls, fire } = await attach({ onSessionLost });
    fire("networkError"); // transient: restart
    fire("networkError"); // escalates; declined, so it still restarts
    expect(onSessionLost).toHaveBeenCalledTimes(1);
    expect(hls.startLoad).toHaveBeenCalledTimes(2);
  });

  it("does not restart when the caller takes the escalation", async () => {
    const onSessionLost = vi.fn(() => true);
    const { hls, fire } = await attach({ onSessionLost });
    fire("networkError");
    fire("networkError");
    expect(hls.startLoad).toHaveBeenCalledTimes(1);
  });
});
