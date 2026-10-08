import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";

// The Online source tile image (ADR-0068 decision 12, issue 08): a source whose
// package carried an icon shows it from the Server's own path; one without shows a
// generic tile that still names the source.

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

describe("HomeScreen — Online source tile icon", () => {
  it("shows the source's icon from the Server's own path, with the name beside it", async () => {
    getOnlineSources.mockResolvedValue([
      { id: "tube", name: "Test Tube", iconUrl: "/api/v1/onlineSources/tube/icon" },
    ]);
    renderHome();

    const tile = await screen.findByTestId("online-source-tile");
    const img = within(tile).getByTestId("online-source-tile-icon");
    expect(img).toHaveAttribute("src", "/api/v1/onlineSources/tube/icon");
    expect(tile).toHaveTextContent("Test Tube");
    expect(within(tile).queryByTestId("online-source-tile-generic")).toBeNull();
  });

  it("shows a generic tile naming the source when it has no icon", async () => {
    getOnlineSources.mockResolvedValue([{ id: "tube", name: "Test Tube", iconUrl: null }]);
    renderHome();

    const tile = await screen.findByTestId("online-source-tile");
    expect(within(tile).queryByTestId("online-source-tile-icon")).toBeNull();
    expect(within(tile).getByTestId("online-source-tile-generic")).toBeInTheDocument();
    expect(tile).toHaveTextContent("Test Tube");
    expect(tile.querySelector("img")).toBeNull();
  });

  it("falls back to the generic tile when the icon fails to load", async () => {
    getOnlineSources.mockResolvedValue([
      { id: "tube", name: "Test Tube", iconUrl: "/api/v1/onlineSources/tube/icon" },
    ]);
    renderHome();

    const tile = await screen.findByTestId("online-source-tile");
    const img = within(tile).getByTestId("online-source-tile-icon");
    img.dispatchEvent(new Event("error"));

    expect(await within(tile).findByTestId("online-source-tile-generic")).toBeInTheDocument();
    expect(within(tile).queryByTestId("online-source-tile-icon")).toBeNull();
    expect(tile).toHaveTextContent("Test Tube");
  });
});
