import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, TitleSummary, TitlesPage, HomeRows } from "../api/types";
import { layoutModeKey } from "./browseLayout";

// The browse layout switch (appletv-web-parity §5) at the SCREEN level, against a
// faked apiClient (the one seam). Proves the acceptance criteria: the toggle draws
// the same already-loaded list three ways WITHOUT a refetch, Detail rows are built
// only from loaded summary fields (no per-row detail fetch — client ADR-0007), the
// mode round-trips through localStorage keyed per Library, and the toggle is absent
// on Home and Playlists.

const { listTitles, getLibrary, subscribeEvents, getHome, listPlaylists } = vi.hoisted(
  () => ({
    listTitles: vi.fn(),
    getLibrary: vi.fn(),
    subscribeEvents: vi.fn(),
    getHome: vi.fn(),
    listPlaylists: vi.fn(),
  }),
);

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listTitles: (...a: unknown[]) => listTitles(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
      getHome: (...a: unknown[]) => getHome(...a),
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";
import HomeScreen from "../screens/HomeScreen";
import PlaylistsScreen from "./PlaylistsScreen";

const lib: Library = { id: "lib1", name: "Movies", kind: "movie", rootFolders: [] };

function movie(id: string, title: string, extra: Partial<TitleSummary> = {}): TitleSummary {
  return {
    id, kind: "movie", title, year: 0, needsReview: false, ambiguous: false,
    resumePositionMs: 0, watched: false, genres: [], ...extra,
  };
}

const PAGE: TitlesPage = {
  titles: [
    movie("t1", "Alien", { year: 1979, contentRating: "R", genres: ["Sci-Fi", "Horror"], artworkVersion: "v1" }),
    movie("t2", "Dune", { year: 2021, genres: ["Sci-Fi"] }),
  ],
  nextCursor: null,
};

function renderGrid() {
  return renderWithAuth(
    <Routes>
      <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
    </Routes>,
    { initialEntries: ["/libraries/lib1"] },
  );
}

beforeEach(() => {
  window.localStorage.clear();
  listTitles.mockReset();
  getLibrary.mockReset();
  subscribeEvents.mockReset();
  getHome.mockReset();
  listPlaylists.mockReset();
  getLibrary.mockResolvedValue(lib);
  listTitles.mockResolvedValue(PAGE);
  subscribeEvents.mockImplementation(() => () => {});
});

describe("browse layout switch", () => {
  it("draws the same already-loaded list three ways without refetching", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    // Default is the Tile wall.
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "tile");
    expect(listTitles).toHaveBeenCalledTimes(1);

    // Switch to Detail: the same two Titles, now as rows — and NO second fetch.
    await userEvent.click(screen.getByTestId("layout-detail"));
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "detail");
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);
    expect(listTitles).toHaveBeenCalledTimes(1);

    // Switch to List: still the same list, still no refetch.
    await userEvent.click(screen.getByTestId("layout-list"));
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "list");
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);
    expect(listTitles).toHaveBeenCalledTimes(1);
  });

  it("builds Detail rows only from already-loaded fields (no per-row fetch)", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    await userEvent.click(screen.getByTestId("layout-detail"));

    const alien = screen
      .getAllByTestId("poster-tile")
      .find((el) => el.getAttribute("data-title-id") === "t1")!;
    // Name + a secondary line from loaded summary fields (year · rating · genres);
    // the poster thumbnail reuses the loaded artworkVersion cache-bust token.
    expect(within(alien).getByTestId("poster-title")).toHaveTextContent("Alien");
    expect(within(alien).getByTestId("browse-row-meta")).toHaveTextContent(
      "1979 · R · Sci-Fi, Horror",
    );
    expect(within(alien).getByTestId("poster-img").getAttribute("src")).toContain("?v=v1");

    // The list load was the ONLY network read — no detail fetch per row.
    expect(listTitles).toHaveBeenCalledTimes(1);
  });

  it("List shows names only — no thumbnails or meta", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    await userEvent.click(screen.getByTestId("layout-list"));

    expect(screen.getByText("Alien")).toBeInTheDocument();
    expect(screen.queryByTestId("poster-img")).not.toBeInTheDocument();
    expect(screen.queryByTestId("browse-row-meta")).not.toBeInTheDocument();
  });

  it("persists the mode in localStorage keyed per Library and restores it on return", async () => {
    const first = renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    await userEvent.click(screen.getByTestId("layout-detail"));
    // Written under this Library's own key.
    expect(window.localStorage.getItem(layoutModeKey("lib1"))).toBe("detail");

    // Remount (as a return visit): the stored mode is restored, not the default.
    first.unmount();
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "detail");
  });

  it("is absent on Home", async () => {
    const rows: HomeRows = { continueWatching: [], upNext: [], recentlyAdded: [] };
    getHome.mockResolvedValue(rows);
    renderWithAuth(
      <Routes>
        <Route path="/" element={<HomeScreen />} />
      </Routes>,
      { initialEntries: ["/"] },
    );
    await waitFor(() => expect(getHome).toHaveBeenCalled());
    expect(screen.queryByTestId("layout-toggle")).not.toBeInTheDocument();
  });

  it("is absent on Playlists", async () => {
    listPlaylists.mockResolvedValue([]);
    renderWithAuth(
      <Routes>
        <Route path="/playlists" element={<PlaylistsScreen />} />
      </Routes>,
      { initialEntries: ["/playlists"] },
    );
    await waitFor(() => expect(listPlaylists).toHaveBeenCalled());
    expect(screen.queryByTestId("layout-toggle")).not.toBeInTheDocument();
  });
});
