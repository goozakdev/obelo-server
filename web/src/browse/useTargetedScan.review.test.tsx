import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";

// R05-10: a scan whose terminal event never arrives (SSE down) stops "scanning"
// after a timeout, and a terminal event for ANOTHER Library seen before the 202
// does not end this scan.

const { scanEntity, subscribeEvents } = vi.hoisted(() => ({
  scanEntity: vi.fn(),
  subscribeEvents: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      scanEntity: (...a: unknown[]) => scanEntity(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import { useTargetedScan } from "./useTargetedScan";

let emit: (type: string, data: unknown) => void;

beforeEach(() => {
  vi.useFakeTimers();
  scanEntity.mockReset();
  subscribeEvents.mockReset().mockImplementation((fn: typeof emit) => {
    emit = fn;
    return () => {};
  });
});
afterEach(() => vi.useRealTimers());

describe("useTargetedScan", () => {
  it("gives up waiting for a terminal event after the timeout", async () => {
    scanEntity.mockResolvedValue({ libraryId: "lib1" });
    const { result } = renderHook(() => useTargetedScan(() => {}));
    await act(async () => {
      result.current.scan("titles", "t1");
    });
    expect(result.current.scanning).toBe(true);

    await act(async () => {
      vi.advanceTimersByTime(5 * 60 * 1000 + 1);
    });
    expect(result.current.scanning).toBe(false);
    expect(result.current.message).toMatch(/longer than expected/);
  });

  it("ignores another Library's terminal event that lands before the 202", async () => {
    let resolveScan: (v: { libraryId: string }) => void = () => {};
    scanEntity.mockReturnValue(new Promise((r) => (resolveScan = r)));
    const onScanned = vi.fn();
    const { result } = renderHook(() => useTargetedScan(onScanned));
    await act(async () => {
      result.current.scan("titles", "t1");
    });

    await act(async () => {
      emit("scanProgress", { libraryId: "other", complete: true });
    });
    expect(result.current.scanning).toBe(true);

    await act(async () => {
      resolveScan({ libraryId: "lib1" });
    });
    expect(result.current.scanning).toBe(true);
    expect(onScanned).not.toHaveBeenCalled();

    await act(async () => {
      emit("scanProgress", { libraryId: "lib1", complete: true });
    });
    expect(result.current.scanning).toBe(false);
    expect(onScanned).toHaveBeenCalledTimes(1);
  });
});
