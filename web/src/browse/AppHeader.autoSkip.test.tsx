import { describe, it, expect, beforeEach, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { apiClient } from "../api/client";
import type { MarkerAutoSkip } from "../api/types";
import AppHeader from "./AppHeader";

// Auto-skip (ADR-0065 §6) is set from the user menu, one switch per Marker kind.
// The setting is the server's (/me/marker-auto-skip) — read when the menu opens
// and saved on each toggle — never kept in the browser, so it follows the User
// to every client.

const off: MarkerAutoSkip = { intro: false, recap: false, credits: false, preview: false };

beforeEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

async function openUserMenu() {
  await userEvent.click(screen.getByTestId("user-menu-toggle"));
}

describe("AppHeader auto-skip switches", () => {
  it("shows the server's setting, one switch per kind", async () => {
    vi.spyOn(apiClient, "getMarkerAutoSkip").mockResolvedValue({ ...off, intro: true });
    renderWithAuth(<AppHeader />);
    await openUserMenu();
    await waitFor(() =>
      expect(screen.getByTestId("auto-skip-intro")).toHaveAttribute("aria-checked", "true"),
    );
    for (const kind of ["recap", "credits", "preview"]) {
      expect(screen.getByTestId(`auto-skip-${kind}`)).toHaveAttribute("aria-checked", "false");
    }
  });

  it("saves the whole setting to the server on a toggle, not to local storage", async () => {
    vi.spyOn(apiClient, "getMarkerAutoSkip").mockResolvedValue({ ...off, intro: true });
    const set = vi
      .spyOn(apiClient, "setMarkerAutoSkip")
      .mockImplementation((s: MarkerAutoSkip) => Promise.resolve(s));
    renderWithAuth(<AppHeader />);
    await openUserMenu();
    await waitFor(() =>
      expect(screen.getByTestId("auto-skip-intro")).toHaveAttribute("aria-checked", "true"),
    );
    const before = JSON.stringify({ ...window.localStorage });

    await userEvent.click(screen.getByTestId("auto-skip-credits"));

    expect(set).toHaveBeenCalledWith({ ...off, intro: true, credits: true });
    await waitFor(() =>
      expect(screen.getByTestId("auto-skip-credits")).toHaveAttribute("aria-checked", "true"),
    );
    expect(JSON.stringify({ ...window.localStorage })).toBe(before);
  });

  it("shows no switches when the setting cannot be read", async () => {
    vi.spyOn(apiClient, "getMarkerAutoSkip").mockRejectedValue(new Error("boom"));
    renderWithAuth(<AppHeader />);
    await openUserMenu();
    await waitFor(() => expect(apiClient.getMarkerAutoSkip).toHaveBeenCalled());
    expect(screen.queryByTestId("auto-skip-intro")).toBeNull();
  });
});
