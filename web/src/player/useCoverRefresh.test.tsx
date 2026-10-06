import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { getTitle, live } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  live: { fire: () => {} },
}));

vi.mock("../api/client", () => ({
  apiClient: { getTitle: (...a: unknown[]) => getTitle(...a) },
}));
// Capture the refresh callback so a test can fire the library change signal directly.
vi.mock("../events/enrichEvents", () => ({
  useLibraryLiveRefresh: (_lib: string, cb: () => void) => {
    live.fire = cb;
  },
}));

import { COVER_REFRESH_QUIET_MS, useCoverRefresh } from "./useCoverRefresh";

const detail = (albumId: string, version: string) => ({
  track: { albumId, albumArtworkVersion: version },
});

beforeEach(() => {
  vi.useFakeTimers();
  getTitle.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("useCoverRefresh", () => {
  it("re-reads once after a burst settles and returns the fresh cover", async () => {
    getTitle.mockResolvedValue(detail("al1", "v2"));
    const { result } = renderHook(() => useCoverRefresh("t1", "lib", true));
    expect(result.current).toBeUndefined();
    // A scan/enrich burst: each tick re-arms the debounce instead of queueing a read.
    for (let i = 0; i < 5; i++) {
      act(() => live.fire());
      await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS - 1));
      expect(getTitle).not.toHaveBeenCalled();
    }
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS));
    expect(getTitle).toHaveBeenCalledTimes(1);
    expect(result.current).toMatchObject({ albumId: "al1", version: "v2" });
  });

  it("never returns another entry's fresh cover, even for one render", async () => {
    getTitle.mockResolvedValue(detail("al1", "v2"));
    const seen: Record<string, unknown[]> = { t1: [], t2: [] };
    const { rerender } = renderHook(
      ({ id }) => {
        const r = useCoverRefresh(id, "lib", true);
        seen[id].push(r);
        return r;
      },
      { initialProps: { id: "t1" } },
    );
    act(() => live.fire());
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS));
    rerender({ id: "t2" });
    expect(seen.t2.length).toBeGreaterThan(0);
    expect(seen.t2.every((r) => r === undefined)).toBe(true);
  });

  it("drops the fresh cover on leaving a track, so returning to it shows none", async () => {
    getTitle.mockResolvedValue(detail("al1", "v2"));
    const { result, rerender } = renderHook(({ id }) => useCoverRefresh(id, "lib", true), {
      initialProps: { id: "t1" },
    });
    act(() => live.fire());
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS));
    expect(result.current).toBeDefined();
    rerender({ id: "t2" });
    rerender({ id: "t1" });
    expect(result.current).toBeUndefined();
  });

  it("clears a pending debounce timer when the queue entry changes", async () => {
    getTitle.mockResolvedValue(detail("al1", "v2"));
    const { rerender } = renderHook(({ id }) => useCoverRefresh(id, "lib", true), {
      initialProps: { id: "t1" },
    });
    act(() => live.fire());
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS - 1));
    rerender({ id: "t2" });
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS * 2));
    expect(getTitle).not.toHaveBeenCalled();
  });

  it("clears a pending debounce timer on unmount", async () => {
    getTitle.mockResolvedValue(detail("al1", "v2"));
    const { unmount } = renderHook(() => useCoverRefresh("t1", "lib", true));
    act(() => live.fire());
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS - 1));
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS * 2));
    expect(getTitle).not.toHaveBeenCalled();
  });

  it("drops a late response for a previous entry", async () => {
    let resolveA: (d: unknown) => void = () => {};
    // The mock ignores the abort signal, like a response already past the network.
    getTitle.mockImplementationOnce(() => new Promise((r) => (resolveA = r)));
    const { result, rerender } = renderHook(({ id }) => useCoverRefresh(id, "lib", true), {
      initialProps: { id: "t1" },
    });
    act(() => live.fire());
    await act(() => vi.advanceTimersByTimeAsync(COVER_REFRESH_QUIET_MS));
    expect(getTitle).toHaveBeenCalledTimes(1);
    rerender({ id: "t2" });
    rerender({ id: "t1" });
    // The answer for the earlier visit to t1 lands now; it must not become t1's fresh cover.
    await act(async () => resolveA(detail("stale", "old")));
    expect(result.current).toBeUndefined();
  });
});
