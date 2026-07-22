import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import type { ApiClient } from "../api/client";
import type { PlaybackDecision, StartPlaybackOptions } from "../api/types";
import { usePlayerSession, type PlayerPreference } from "./usePlayerSession";

// The pre-play Audio / Video Stream seed (appletv-web-parity §1, issue 04) reaches the
// FIRST negotiation as `audioStreamId` / `videoStreamId`, and — critically — is SEEDED
// ONCE. An in-player switch owns the value afterwards, and the (stale) preference prop
// NEVER re-seeds over it: a later re-negotiation carries the in-player pick, not the
// pre-play sheet value. This is the drift guard client ADR-0011 demands — the sheet
// picks are the server's Remembered audio/video, not a duplicated local axis.

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed1", name: "Default" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

const startPlayback = vi.fn();
const reportProgress = vi.fn();
const endSession = vi.fn();

const fakeClient = {
  startPlayback: (...a: unknown[]) => startPlayback(...a),
  reportProgress: (...a: unknown[]) => reportProgress(...a),
  endSession: (...a: unknown[]) => endSession(...a),
} as unknown as ApiClient;

/** The options the Nth startPlayback call negotiated with. */
function optsOfCall(n: number): StartPlaybackOptions {
  return startPlayback.mock.calls[n][1] as StartPlaybackOptions;
}

beforeEach(() => {
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue(undefined);
  endSession.mockReset().mockResolvedValue(undefined);
});

describe("usePlayerSession — pre-play Audio / Video seed", () => {
  it("seeds the FIRST negotiation with the pre-play audio + video ids", async () => {
    renderHook(() =>
      usePlayerSession(fakeClient, "t1", 0, { audioStreamId: "a1", videoStreamId: "v1" }),
    );
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(1));
    expect(optsOfCall(0)).toMatchObject({ audioStreamId: "a1", videoStreamId: "v1" });
  });

  it("omits both ids when no pre-play pick is given (Auto → server memory)", async () => {
    renderHook(() => usePlayerSession(fakeClient, "t1", 0));
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(1));
    expect(optsOfCall(0).audioStreamId).toBeUndefined();
    expect(optsOfCall(0).videoStreamId).toBeUndefined();
  });

  it("an in-player audio switch is NOT resurrected by the stale sheet value on re-negotiation", async () => {
    // The sheet handed audio "a1"; the prop keeps carrying it every render.
    const preference: PlayerPreference = { audioStreamId: "a1", videoStreamId: "v1" };
    const { result, rerender } = renderHook((p: PlayerPreference) =>
      usePlayerSession(fakeClient, "t1", 0, p), { initialProps: preference });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(1));
    expect(optsOfCall(0)).toMatchObject({ audioStreamId: "a1", videoStreamId: "v1" });

    // The viewer switches audio in-player to "a2" (a direct-play escalation → a fresh
    // negotiation). It carries "a2"; the video seed "v1" survives the switch.
    act(() => result.current.selectAudioStream("a2", 5000));
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    expect(optsOfCall(1)).toMatchObject({ audioStreamId: "a2", videoStreamId: "v1" });

    // Re-render with the SAME stale preference (the sheet value "a1" is still the prop),
    // then force another re-negotiation. The in-player pick "a2" MUST win — the seed is
    // one-shot, so "a1" never resurrects.
    rerender(preference);
    act(() => result.current.recover(8000));
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(3));
    expect(optsOfCall(2).audioStreamId).toBe("a2");
    expect(optsOfCall(2).audioStreamId).not.toBe("a1");
  });
});
