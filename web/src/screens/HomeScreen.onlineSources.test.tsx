import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";

// Online sources on Home (ADR-0068, issue 01): one tile per source from its own
// endpoint, nothing when there are none or the call fails, and never a cost to
// Home's own rows.

const { getHome, getOnlineSources } = vi.hoisted(() => ({
  getHome: vi.fn(),
  getOnlineSources: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getHome: (...a: unknown[]) => getHome(...a),
      getOnlineSources: (...a: unknown[]) => getOnlineSources(...a),
    },
  };
});

import HomeScreen from "./HomeScreen";

function renderHome() {
  return renderWithAuth(
    <Routes>
      <Route path="/" element={<HomeScreen />} />
    </Routes>,
    { initialEntries: ["/"] },
  );
}

beforeEach(() => {
  getHome.mockReset().mockResolvedValue({ continueWatching: [], upNext: [], recentlyAdded: [] });
  getOnlineSources.mockReset();
});

describe("HomeScreen — Online source tiles", () => {
  it("shows ONE tile per source, named for it, linking to its page", async () => {
    getOnlineSources.mockResolvedValue([
      { id: "tube", name: "Test Tube", iconUrl: null },
    ]);
    renderHome();

    const tile = await screen.findByTestId("online-source-tile");
    expect(tile).toHaveTextContent("Test Tube");
    expect(tile).toHaveAttribute("href", "/online/tube");
    expect(screen.getAllByTestId("online-source-tile")).toHaveLength(1);
  });

  it("shows no Online section when the caller has no source", async () => {
    getOnlineSources.mockResolvedValue([]);
    renderHome();

    await screen.findByTestId("home-recently-added");
    await waitFor(() => expect(getOnlineSources).toHaveBeenCalled());
    expect(screen.queryByTestId("home-online-sources")).toBeNull();
  });

  it("keeps Home's own rows when the tile call fails", async () => {
    getOnlineSources.mockRejectedValue(new ApiError(500, "INTERNAL", "boom"));
    renderHome();

    expect(await screen.findByTestId("home-recently-added")).toBeInTheDocument();
    expect(screen.queryByTestId("home-online-sources")).toBeNull();
    expect(screen.queryByTestId("home-error")).toBeNull();
  });
});
