import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { useInfiniteScrollSentinel } from "./useInfiniteScrollSentinel";

// Unit test for the callback-ref observer wiring: attaching the ref to a node
// observes it; an intersection fires onMore; detaching (ref(null)) disconnects.

let lastCb: IntersectionObserverCallback | null = null;
const observe = vi.fn();
const disconnect = vi.fn();

class MockIO implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = "";
  readonly thresholds = [];
  constructor(cb: IntersectionObserverCallback) {
    lastCb = cb;
  }
  observe(el: Element) {
    observe(el);
  }
  unobserve() {}
  disconnect() {
    disconnect();
  }
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}

function fireIntersect(target: Element) {
  lastCb?.(
    [{ isIntersecting: true, target } as unknown as IntersectionObserverEntry],
    {} as IntersectionObserver,
  );
}

beforeEach(() => {
  lastCb = null;
  observe.mockReset();
  disconnect.mockReset();
  vi.stubGlobal("IntersectionObserver", MockIO);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("useInfiniteScrollSentinel", () => {
  it("observes the node when the ref attaches and fires onMore on intersection", () => {
    const onMore = vi.fn();
    const { result } = renderHook(() => useInfiniteScrollSentinel(onMore));
    const node = document.createElement("div");

    result.current(node); // sentinel mounts
    expect(observe).toHaveBeenCalledWith(node);

    fireIntersect(node);
    expect(onMore).toHaveBeenCalledTimes(1);
  });

  it("disconnects the observer when the node unmounts (ref(null))", () => {
    const onMore = vi.fn();
    const { result } = renderHook(() => useInfiniteScrollSentinel(onMore));
    const node = document.createElement("div");

    result.current(node);
    result.current(null); // sentinel unmounts
    expect(disconnect).toHaveBeenCalled();
  });

  it("calls the latest onMore even if its identity changed without re-observing", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { result, rerender } = renderHook(
      ({ cb }: { cb: () => void }) => useInfiniteScrollSentinel(cb),
      { initialProps: { cb: first } },
    );
    const node = document.createElement("div");
    result.current(node);
    observe.mockClear();

    rerender({ cb: second });
    fireIntersect(node);

    // The latest callback runs; the observer was not torn down/re-created.
    expect(second).toHaveBeenCalledTimes(1);
    expect(first).not.toHaveBeenCalled();
    expect(observe).not.toHaveBeenCalled();
  });
});
