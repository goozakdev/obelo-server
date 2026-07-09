import { describe, it, expect, vi, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { apiClient } from "../api/client";
import { appEvents, useEnrichmentActivity, useLibraryLiveRefresh } from "./enrichEvents";

// The browser-side live-events spine (realtime-events web slice). We spy on
// apiClient.subscribeEvents to capture the SSE callback and drive synthetic
// events — no real EventSource — then assert the hub fan-out and the hooks
// (enrichment activity indicator + the grid live-refresh).

afterEach(() => {
  vi.restoreAllMocks();
});

// captureEmit replaces apiClient.subscribeEvents with a spy that hands back the
// onEvent callback so a test can push events as the server would.
function captureEmit() {
  let emit: ((type: string, data: unknown) => void) | null = null;
  const close = vi.fn();
  vi.spyOn(apiClient, "subscribeEvents").mockImplementation((onEvent) => {
    emit = onEvent;
    return close;
  });
  return {
    emit: (type: string, data: unknown) => emit?.(type, data),
    close,
  };
}

describe("useEnrichmentActivity", () => {
  it("is active while a pass runs and clears on the terminal complete event", async () => {
    const bus = captureEmit();
    const { result } = renderHook(() => useEnrichmentActivity());
    expect(result.current).toBe(false);

    act(() => bus.emit("enrichProgress", { libraryId: "lib1", complete: false }));
    await waitFor(() => expect(result.current).toBe(true));

    act(() => bus.emit("enrichProgress", { libraryId: "lib1", complete: true }));
    await waitFor(() => expect(result.current).toBe(false));
  });

  it("scopes to a Library id when given", async () => {
    const bus = captureEmit();
    const { result } = renderHook(() => useEnrichmentActivity("lib1"));

    act(() => bus.emit("enrichProgress", { libraryId: "other", complete: false }));
    expect(result.current).toBe(false); // a different Library is ignored

    act(() => bus.emit("enrichProgress", { libraryId: "lib1", complete: false }));
    await waitFor(() => expect(result.current).toBe(true));
  });

  it("ignores non-enrich event types", async () => {
    const bus = captureEmit();
    const { result } = renderHook(() => useEnrichmentActivity());
    // A scan event must not light up the *enrichment* indicator.
    act(() => bus.emit("scanProgress", { libraryId: "lib1", complete: false }));
    expect(result.current).toBe(false);
  });
});

describe("useLibraryLiveRefresh", () => {
  it("refreshes for its Library on scan / enrich / libraryUpdated and ignores others", () => {
    const bus = captureEmit();
    const onRefresh = vi.fn();
    renderHook(() => useLibraryLiveRefresh("lib1", onRefresh));

    // A different Library is ignored.
    act(() => bus.emit("scanProgress", { libraryId: "other", titlesFound: 1 }));
    expect(onRefresh).not.toHaveBeenCalled();

    // A scan tick for this Library refreshes (first tick is not debounced away).
    act(() => bus.emit("scanProgress", { libraryId: "lib1", titlesFound: 1 }));
    expect(onRefresh).toHaveBeenCalledTimes(1);

    // A terminal libraryUpdated nudge always refreshes, bypassing the debounce.
    act(() => bus.emit("libraryUpdated", { libraryId: "lib1" }));
    expect(onRefresh).toHaveBeenCalledTimes(2);

    // A terminal enrich complete also bypasses the debounce.
    act(() => bus.emit("enrichProgress", { libraryId: "lib1", complete: true }));
    expect(onRefresh).toHaveBeenCalledTimes(3);
  });

  it("debounces a burst of non-terminal progress ticks", () => {
    const bus = captureEmit();
    const onRefresh = vi.fn();
    renderHook(() => useLibraryLiveRefresh("lib1", onRefresh));

    // First tick fires; immediate follow-ups within the window are coalesced.
    act(() => bus.emit("scanProgress", { libraryId: "lib1", titlesFound: 1 }));
    act(() => bus.emit("scanProgress", { libraryId: "lib1", titlesFound: 2 }));
    act(() => bus.emit("scanProgress", { libraryId: "lib1", titlesFound: 3 }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it("session events never trigger a library refresh", () => {
    const bus = captureEmit();
    const onRefresh = vi.fn();
    renderHook(() => useLibraryLiveRefresh("lib1", onRefresh));
    act(() => bus.emit("nowPlaying", { libraryId: "lib1", positionMs: 5 }));
    expect(onRefresh).not.toHaveBeenCalled();
  });
});

describe("appEvents hub", () => {
  it("fans one SSE event out to every listener and opens a single stream", () => {
    const spy = vi
      .spyOn(apiClient, "subscribeEvents")
      .mockImplementation(() => vi.fn());
    const l1 = vi.fn();
    const l2 = vi.fn();
    const off1 = appEvents.subscribe(l1);
    const off2 = appEvents.subscribe(l2);
    // One shared EventSource for both listeners.
    expect(spy).toHaveBeenCalledTimes(1);
    off1();
    off2();
  });

  it("closes the shared stream when the last subscriber unmounts", () => {
    const bus = captureEmit();
    const a = renderHook(() => useLibraryLiveRefresh("lib1", () => {}));
    const b = renderHook(() => useEnrichmentActivity());
    expect(bus.close).not.toHaveBeenCalled();
    a.unmount();
    expect(bus.close).not.toHaveBeenCalled(); // one subscriber remains
    b.unmount();
    expect(bus.close).toHaveBeenCalledTimes(1); // last one out closes it
  });
});
