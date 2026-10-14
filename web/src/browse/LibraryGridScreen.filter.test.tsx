import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";

// The Library browse reads an applied filter from the URL (?genre= ... ?person=),
// passes it to the client, shows it as a chip, and clears it on click.

const { listTitles, listShows, getLibrary } = vi.hoisted(() => ({
  listTitles: vi.fn(),
  listShows: vi.fn(),
  getLibrary: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listTitles: (...a: unknown[]) => listTitles(...a),
      listShows: (...a: unknown[]) => listShows(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      subscribeEvents: () => () => {},
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";

function Where() {
  const l = useLocation();
  return <span data-testid="where">{l.pathname + l.search}</span>;
}

function renderAt(url: string) {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>
      <Where />
    </>,
    { initialEntries: [url] },
  );
}

beforeEach(() => {
  listTitles.mockReset().mockResolvedValue({ titles: [], nextCursor: null });
  listShows.mockReset().mockResolvedValue({ shows: [], nextCursor: null });
  getLibrary.mockReset();
});

describe("LibraryGridScreen URL filter", () => {
  it("applies a movie filter from the URL, shows a chip, and clears it", async () => {
    getLibrary.mockResolvedValue({ id: "lib1", name: "Movies", kind: "movie", rootFolders: [] });
    renderAt("/libraries/lib1?studio=A24");

    const chip = await screen.findByTestId("filter-chip");
    expect(chip).toHaveTextContent("A24");
    await waitFor(() => expect(listTitles).toHaveBeenCalled());
    expect(listTitles.mock.calls[0][1]).toMatchObject({ filter: { type: "studio", value: "A24" } });

    await userEvent.click(screen.getByTestId("filter-clear"));
    await waitFor(() => expect(screen.queryByTestId("filter-chip")).toBeNull());
    expect(screen.getByTestId("where")).toHaveTextContent("/libraries/lib1");
    expect(screen.getByTestId("where").textContent).not.toContain("studio");
    await waitFor(() => expect(listTitles.mock.calls.at(-1)?.[1].filter).toBeUndefined());
  });

  it("applies a TV filter and labels a person by personName", async () => {
    getLibrary.mockResolvedValue({ id: "lib1", name: "Shows", kind: "tv", rootFolders: [] });
    renderAt("/libraries/lib1?person=tmdb:31&personName=Tom%20Hanks");

    expect(await screen.findByTestId("filter-chip")).toHaveTextContent("Tom Hanks");
    await waitFor(() => expect(listShows).toHaveBeenCalled());
    expect(listShows.mock.calls[0][1]).toMatchObject({ filter: { type: "person", value: "tmdb:31" } });
  });

  it("shows no chip and sends no filter without URL params", async () => {
    getLibrary.mockResolvedValue({ id: "lib1", name: "Movies", kind: "movie", rootFolders: [] });
    renderAt("/libraries/lib1");
    await waitFor(() => expect(listTitles).toHaveBeenCalled());
    expect(screen.queryByTestId("filter-chip")).toBeNull();
    expect(listTitles.mock.calls[0][1].filter).toBeUndefined();
  });
});
