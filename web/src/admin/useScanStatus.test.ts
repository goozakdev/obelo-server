import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { ScanStatus } from "../api/types";

// The scan-status poller: it polls GET /libraries/{id}/scan while a scan is
// running and stops the moment it settles (idle/error). Driven with fake timers
// so the interval is deterministic. apiClient is faked at the module boundary
// (the PRD's one seam).
//
// Under fake timers we can't use Testing Library's waitFor (it relies on real
// timers); instead we flush pending microtasks with `await act` after each tick
// so the awaited getScanStatus resolutions are applied.

const { getScanStatus } = vi.hoisted(() => ({ getScanStatus: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: { getScanStatus: (...a: unknown[]) => getScanStatus(...a) } };
});

import { useScanStatus } from "./useScanStatus";

function status(over: Partial<ScanStatus>): ScanStatus {
  return { libraryId: "lib1", state: "idle", titlesFound: 0, filesFound: 0, ...over };
}

/** Flush the microtask queue so awaited promise resolutions land in state. */
async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  getScanStatus.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("useScanStatus", () => {
  it("does an initial read on mount and stops when already idle", async () => {
    getScanStatus.mockResolvedValue(status({ state: "idle", titlesFound: 3, filesFound: 5 }));
    const { result } = renderHook(() => useScanStatus("lib1", { intervalMs: 1000 }));

    await flush();
    expect(result.current.status).toMatchObject({
      state: "idle",
      titlesFound: 3,
      filesFound: 5,
    });
    // One read on mount; no interval because it's already settled.
    expect(getScanStatus).toHaveBeenCalledTimes(1);
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    await flush();
    expect(getScanStatus).toHaveBeenCalledTimes(1);
  });

  it("begin(idle) seeds the status without starting an interval", async () => {
    const { result } = renderHook(() =>
      useScanStatus("lib1", { enabled: false, intervalMs: 1000 }),
    );
    act(() => result.current.begin(status({ state: "idle", titlesFound: 2, filesFound: 3 })));
    expect(result.current.status).toMatchObject({ state: "idle", titlesFound: 2 });
    // enabled:false → no reads at all.
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(getScanStatus).not.toHaveBeenCalled();
  });

  it("polls running → idle and reflects the final counts", async () => {
    getScanStatus
      .mockImplementationOnce(() => Promise.resolve(status({ state: "running" })))
      .mockImplementationOnce(() =>
        Promise.resolve(status({ state: "idle", titlesFound: 7, filesFound: 9 })),
      );

    const { result } = renderHook(() => useScanStatus("lib1", { intervalMs: 1000 }));

    // Mount read → running, so polling is active.
    await flush();
    expect(result.current.status?.state).toBe("running");
    expect(getScanStatus).toHaveBeenCalledTimes(1);

    // One interval tick → second read returns idle, polling stops.
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    await flush();
    expect(result.current.status).toMatchObject({
      state: "idle",
      titlesFound: 7,
      filesFound: 9,
    });

    // No further reads after settling.
    const calls = getScanStatus.mock.calls.length;
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    await flush();
    expect(getScanStatus).toHaveBeenCalledTimes(calls);
  });

  it("surfaces a poll error and stops polling", async () => {
    getScanStatus.mockImplementationOnce(() =>
      Promise.reject(new Error("status read failed")),
    );
    const { result } = renderHook(() => useScanStatus("lib1", { intervalMs: 1000 }));
    await flush();
    expect(result.current.error).toMatch(/status read failed/);
    const calls = getScanStatus.mock.calls.length;
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    await flush();
    expect(getScanStatus).toHaveBeenCalledTimes(calls);
  });
});
