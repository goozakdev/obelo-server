import { describe, it, expect, beforeEach, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import AppHeader from "./AppHeader";

// AppHeader gates its Playlists / Collections utility links on the server's
// advertised feature flags (Apple TV → Web parity §4): a server that does not
// advertise the capability offers no such route, so the link is hidden rather
// than a dead end. This is the real consumer proving the feature-gate path
// end-to-end. The apiClient is left unmocked (the header's Libraries fetch just
// errors into the harmless fallback link; the SSE hub self-guards).

beforeEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

async function openUserMenu() {
  await userEvent.click(screen.getByTestId("user-menu-toggle"));
}

describe("AppHeader feature gating", () => {
  it("shows the Playlists and Collections links when the flags are advertised", async () => {
    renderWithAuth(<AppHeader />, {
      features: { playlists: true, collections: true },
    });
    await openUserMenu();
    expect(screen.getByTestId("nav-playlists")).toBeInTheDocument();
    expect(screen.getByTestId("nav-collections")).toBeInTheDocument();
  });

  it("hides the Playlists link when the flag is not advertised", async () => {
    renderWithAuth(<AppHeader />, { features: { playlists: false } });
    await openUserMenu();
    expect(screen.queryByTestId("nav-playlists")).not.toBeInTheDocument();
    // Collections defaults on, so the gate hides only what the server withholds.
    expect(screen.getByTestId("nav-collections")).toBeInTheDocument();
  });

  it("hides the Collections link when the flag is not advertised", async () => {
    renderWithAuth(<AppHeader />, { features: { collections: false } });
    await openUserMenu();
    expect(screen.queryByTestId("nav-collections")).not.toBeInTheDocument();
    expect(screen.getByTestId("nav-playlists")).toBeInTheDocument();
  });
});
