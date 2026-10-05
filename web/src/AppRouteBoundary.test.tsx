import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { apiClient } from "./api/client";
import App from "./App";

// A route screen that throws while rendering must be caught by App's route
// boundary (inline "Reload" prompt) while the app-shell siblings stay mounted.
// Replacing <RouteBoundary> with a fragment must fail this test.

vi.mock("./screens/SignInCallbackScreen", () => ({
  default: () => {
    throw new Error("boom");
  },
}));
vi.mock("./player/NowPlayingBar", () => ({
  default: () => <div data-testid="now-playing-stub" />,
}));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.pushState({}, "", "/sign-in/callback");
  vi.spyOn(apiClient, "getServerInfo").mockReturnValue(new Promise(() => {}));
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

describe("App route boundary", () => {
  it("shows the reload prompt for a throwing route and keeps the shell mounted", () => {
    render(<App />);
    expect(screen.getByTestId("chunk-error")).toHaveTextContent("Something went wrong");
    expect(screen.getByTestId("now-playing-stub")).toBeInTheDocument();
  });
});
