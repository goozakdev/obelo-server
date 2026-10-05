import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderWithAuth } from "./test/renderWithAuth";
import { apiClient } from "./api/client";
import { appEvents } from "./events/enrichEvents";
import { EventsKeepAlive } from "./App";

// Every screen renders its own AppHeader, which listens on the shared events
// stream; a route change runs the old screen's cleanup before the new screen
// subscribes. The keep-alive is what stops that from closing and reopening
// /events on every navigation (losing any event published in the gap).

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});
afterEach(() => {
  vi.restoreAllMocks();
});

describe("EventsKeepAlive", () => {
  it("keeps one stream open across a screen swap while signed in", () => {
    const close = vi.fn();
    const open = vi.spyOn(apiClient, "subscribeEvents").mockImplementation(() => close);
    const view = renderWithAuth(<EventsKeepAlive />);
    expect(open).toHaveBeenCalledTimes(1);

    // A navigation: the old screen's listener leaves, then the new one arrives.
    const offOld = appEvents.subscribe(() => {});
    offOld();
    const offNew = appEvents.subscribe(() => {});
    expect(open).toHaveBeenCalledTimes(1);
    expect(close).not.toHaveBeenCalled();

    offNew();
    view.unmount();
    expect(close).toHaveBeenCalledTimes(1); // signing out / leaving closes it
  });
});
