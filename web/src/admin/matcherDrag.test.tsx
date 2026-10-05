import { describe, it, expect, vi, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useDragPlacement } from "./matcherDrag";

// jsdom has no elementFromPoint, so the tests below assign one; put back whatever was
// there so it cannot leak into other tests in this file's worker.
const originalElementFromPoint = document.elementFromPoint;

afterEach(() => {
  vi.restoreAllMocks();
  document.elementFromPoint = originalElementFromPoint;
});

describe("useDragPlacement unmount (R01-14)", () => {
  it("drops nothing and leaves no window listeners when unmounted mid-drag", () => {
    const target = document.createElement("div");
    target.dataset.drop = "unassigned";
    document.body.appendChild(target);
    document.elementFromPoint = vi.fn(() => target);

    const onDrop = vi.fn();
    const removed = vi.spyOn(window, "removeEventListener");
    const { result, unmount } = renderHook(() => useDragPlacement(onDrop));

    act(() => result.current.startDrag("/a.mkv", { clientX: 0, clientY: 0, button: 0 }));
    act(() => {
      window.dispatchEvent(new MouseEvent("pointermove", { clientX: 40, clientY: 40 }));
    });
    expect(result.current.drag).not.toBeNull();

    unmount();
    expect(removed).toHaveBeenCalledWith("pointerup", expect.any(Function));

    window.dispatchEvent(new MouseEvent("pointerup", { clientX: 40, clientY: 40 }));
    expect(onDrop).not.toHaveBeenCalled();
    target.remove();
  });
});

describe("test hygiene", () => {
  it("does not leave a stubbed elementFromPoint behind from the previous test", () => {
    expect(document.elementFromPoint).toBe(originalElementFromPoint);
  });
});
