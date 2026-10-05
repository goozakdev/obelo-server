import { StrictMode, type ReactNode } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import type { ApiClient } from "../api/client";
import { ApiError } from "../api/errors";
import type { PlaybackDecision } from "../api/types";
import { usePlayerSession } from "./usePlayerSession";

// R04-10 (an abandoned in-flight negotiation must not leak a server session) and
// R04-17 (retry must not run its side effect inside a state updater).

function decisionOf(sessionId: string): PlaybackDecision {
  return {
    sessionId,
    tier: "directPlay",
    streamUrl: `/api/v1/sessions/${sessionId}/stream`,
    edition: { id: "ed1", name: "Default" },
    videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
    videoStreams: [],
    audioStream: { index: 1, codec: "aac", channels: 2 },
    audioStreams: [],
    subtitles: [],
    estimatedBitrate: 6_000_000,
  };
}

const startPlayback = vi.fn();
const reportProgress = vi.fn();
const endSession = vi.fn();
const fakeClient = {
  startPlayback: (...a: unknown[]) => startPlayback(...a),
  reportProgress: (...a: unknown[]) => reportProgress(...a),
  endSession: (...a: unknown[]) => endSession(...a),
} as unknown as ApiClient;

beforeEach(() => {
  startPlayback.mockReset();
  reportProgress.mockReset().mockResolvedValue(undefined);
  endSession.mockReset().mockResolvedValue(undefined);
});

describe("usePlayerSession — abandoned negotiation (R04-10)", () => {
  it("ends the session an unmounted hook's in-flight negotiation created", async () => {
    let resolve!: (d: PlaybackDecision) => void;
    startPlayback.mockImplementation(() => new Promise((r) => (resolve = r)));
    const { unmount } = renderHook(() => usePlayerSession(fakeClient, "t1", 0));
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(1));
    unmount();
    // The server created the session before we hung up: the late answer is stale.
    await act(async () => resolve(decisionOf("late")));
    expect(endSession).toHaveBeenCalledWith("late");
  });

  it("ends the session a superseded negotiation created, keeps the current one", async () => {
    const resolvers: Array<(d: PlaybackDecision) => void> = [];
    startPlayback.mockImplementation(() => new Promise((r) => resolvers.push(r)));
    const { result } = renderHook(() => usePlayerSession(fakeClient, "t1", 0));
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(1));
    act(() => result.current.recover(1000)); // supersedes the first request
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    await act(async () => resolvers[0](decisionOf("old")));
    await act(async () => resolvers[1](decisionOf("new")));
    expect(endSession).toHaveBeenCalledWith("old");
    expect(endSession).not.toHaveBeenCalledWith("new");
    expect(result.current.status.kind).toBe("ready");
  });
});

describe("usePlayerSession — retry (R04-17)", () => {
  it("sends ONE request per retry even under StrictMode's double-invoked updaters", async () => {
    startPlayback.mockRejectedValue(new ApiError(503, "SERVER_BUSY", "busy"));
    const wrapper = ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode>;
    const { result } = renderHook(() => usePlayerSession(fakeClient, "t1", 0), { wrapper });
    await waitFor(() => expect(result.current.status.kind).toBe("busy"));
    const before = startPlayback.mock.calls.length;
    act(() => result.current.retry());
    await waitFor(() => expect(result.current.status.kind).toBe("busy"));
    expect(startPlayback.mock.calls.length - before).toBe(1);
  });
});
