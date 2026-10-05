import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { apiClient } from "./api/client";
import App from "./App";

// A lazy route whose chunk fails to load must be caught by the
// route boundary — inline "Reload" prompt — while the NowPlayingBar is neither
// replaced nor remounted, so the playing media is not torn down. Moving
// <NowPlayingBar /> inside <RouteBoundary> must fail this test.

// The import resolves, but reading its default export throws the browser's dynamic-import
// failure (a TypeError) when the lazy route renders. The boundary catches that render error,
// which has the shape it classifies as a chunk-load failure (vitest would wrap an error
// thrown by the factory itself).
vi.mock("./screens/ProfileScreen", () => ({
  get default(): never {
    throw new TypeError("Failed to fetch dynamically imported module: /assets/ProfileScreen.js");
  },
}));

const barLifecycle = vi.hoisted(() => ({ mounts: 0, unmounts: 0 }));
vi.mock("./player/NowPlayingBar", () => ({
  default: function BarStub() {
    useEffect(() => {
      barLifecycle.mounts++;
      return () => {
        barLifecycle.unmounts++;
      };
    }, []);
    return <div data-testid="now-playing-stub" />;
  },
}));

beforeEach(() => {
  barLifecycle.mounts = 0;
  barLifecycle.unmounts = 0;
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.localStorage.setItem("obelo.token", "fake-token");
  window.localStorage.setItem(
    "obelo.user",
    JSON.stringify({ id: "u1", username: "operator", role: "admin" }),
  );
  window.history.pushState({}, "", "/profile");
  vi.spyOn(apiClient, "getServerInfo").mockResolvedValue({
    id: "srv1",
    name: "Test Server",
    version: "test",
    supportedVersions: [1],
    features: {},
    setupRequired: false,
  });
  vi.spyOn(apiClient, "verifySession").mockResolvedValue({} as never);
  vi.spyOn(apiClient, "listLibraries").mockResolvedValue([] as never);
  vi.spyOn(apiClient, "subscribeEvents").mockImplementation(() => () => {});
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
  window.history.pushState({}, "", "/");
});

describe("App chunk-load failure", () => {
  it("shows the Reload prompt and keeps the NowPlayingBar mounted (never remounted)", async () => {
    render(<App />);
    const prompt = await screen.findByTestId("chunk-error");
    expect(prompt).toHaveTextContent(/this page failed to load/i);
    expect(prompt).toHaveTextContent(/reload/i);
    await waitFor(() => expect(screen.getByTestId("now-playing-stub")).toBeInTheDocument());
    expect(barLifecycle.mounts).toBe(1);
    expect(barLifecycle.unmounts).toBe(0);
  });
});
