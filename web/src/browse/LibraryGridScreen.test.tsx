import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, TitleSummary, TitlesPage, TitleSort } from "../api/types";

// The grid SCREEN against a faked apiClient (the one seam). We mock the client
// module so the singleton the screen imports returns canned, NORMALIZED data —
// covering sort switching (the screen passes the right sort to the client and
// re-renders the new order), the watch-state badges on tiles, and the live
// refresh that an SSE scan/enrich event drives (realtime-events web slice): we
// stub subscribeEvents to capture the stream callback and emit synthetic events.

const { listTitles, getLibrary, subscribeEvents } = vi.hoisted(() => ({
  listTitles: vi.fn(),
  getLibrary: vi.fn(),
  subscribeEvents: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listTitles: (...a: unknown[]) => listTitles(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";

const lib: Library = { id: "lib1", name: "Movies", kind: "movie", rootFolders: [] };

// The SSE callback the hub registered, captured from the stubbed subscribeEvents
// so a test can push events as the server would.
let emitEvent: ((type: string, data: unknown) => void) | null = null;

function movie(id: string, title: string, extra: Partial<TitleSummary> = {}): TitleSummary {
  return {
    id, kind: "movie", title, year: 0, needsReview: false, ambiguous: false,
    resumePositionMs: 0, watched: false, genres: [], ...extra,
  };
}

function pageFor(sort: TitleSort): TitlesPage {
  // Distinct order per sort so the test can prove the screen re-fetched on
  // sort change and rendered the new ordering.
  if (sort === "dateAdded") {
    return {
      titles: [
        { id: "t3", kind: "movie", title: "Zulu", year: 1964, needsReview: false, ambiguous: false, resumePositionMs: 0, watched: false },
        { id: "t1", kind: "movie", title: "Alien", year: 1979, needsReview: false, ambiguous: false, resumePositionMs: 0, watched: true },
      ],
      nextCursor: null,
    };
  }
  return {
    titles: [
      { id: "t1", kind: "movie", title: "Alien", year: 1979, needsReview: false, ambiguous: false, resumePositionMs: 0, watched: true },
      { id: "t2", kind: "movie", title: "Dune", year: 2021, needsReview: false, ambiguous: false, resumePositionMs: 42000, watched: false },
      { id: "t3", kind: "movie", title: "Zulu", year: 1964, needsReview: false, ambiguous: false, resumePositionMs: 0, watched: false },
    ],
    nextCursor: null,
  };
}

function renderGrid() {
  return renderWithAuth(
    <Routes>
      <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
    </Routes>,
    { initialEntries: ["/libraries/lib1"] },
  );
}

beforeEach(() => {
  listTitles.mockReset();
  getLibrary.mockReset();
  subscribeEvents.mockReset();
  getLibrary.mockResolvedValue(lib);
  listTitles.mockImplementation((_id: string, opts: { sort?: TitleSort }) =>
    Promise.resolve(pageFor(opts?.sort ?? "title")),
  );
  emitEvent = null;
  subscribeEvents.mockImplementation((onEvent: (type: string, data: unknown) => void) => {
    emitEvent = onEvent;
    return () => {
      emitEvent = null;
    };
  });
});

describe("LibraryGridScreen", () => {
  it("renders the grid with watched badge and resume marker", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    const tiles = screen.getAllByTestId("poster-tile");
    expect(tiles).toHaveLength(3);

    // Alien is watched → watched badge; Dune has a resume position → resume badge.
    const alien = tiles.find((t) => t.getAttribute("data-title-id") === "t1")!;
    expect(within(alien).getByTestId("badge-watched")).toBeInTheDocument();
    const dune = tiles.find((t) => t.getAttribute("data-title-id") === "t2")!;
    expect(within(dune).getByTestId("badge-resume")).toHaveTextContent("0:42");
  });

  it("re-fetches with the chosen sort and renders the new order", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    // Initial default sort is "title".
    expect(listTitles).toHaveBeenCalledWith(
      "lib1",
      expect.objectContaining({ sort: "title" }),
      expect.anything(),
    );

    await userEvent.selectOptions(screen.getByTestId("sort-select"), "dateAdded");

    await waitFor(() =>
      expect(listTitles).toHaveBeenCalledWith(
        "lib1",
        expect.objectContaining({ sort: "dateAdded" }),
        expect.anything(),
      ),
    );
    // The dateAdded ordering (Zulu, then Alien) is now first in the grid.
    await waitFor(() => {
      const ids = screen
        .getAllByTestId("poster-tile")
        .map((t) => t.getAttribute("data-title-id"));
      expect(ids).toEqual(["t3", "t1"]);
    });
  });

  it("live-appends newly-scanned Titles on a scanProgress event (no manual reload)", async () => {
    // Start mid-scan with one Title indexed.
    listTitles.mockImplementation(() =>
      Promise.resolve({ titles: [movie("t1", "Alien")], nextCursor: null }),
    );
    renderGrid();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(1));

    // The scanner indexes a second Title; the next list read returns both.
    listTitles.mockImplementation(() =>
      Promise.resolve({
        titles: [movie("t1", "Alien"), movie("t2", "Dune")],
        nextCursor: null,
      }),
    );
    // A scanProgress event for this Library arrives over the stream.
    await act(async () => {
      emitEvent?.("scanProgress", { libraryId: "lib1", titlesFound: 2 });
    });

    // The new Title appears in place — and the existing one is still there (the
    // grid merged rather than blanking-and-reloading).
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(2));
    expect(screen.getByText("Dune")).toBeInTheDocument();
    expect(screen.getByText("Alien")).toBeInTheDocument();
  });

  it("cache-busts each poster with its own artworkVersion (not a global token)", async () => {
    // t1 has artwork (a version token); t2 has none. Only t1's <img> should carry
    // a ?v= bust — so a re-enrich that bumps t1's version reloads t1 alone.
    listTitles.mockImplementation(() =>
      Promise.resolve({
        titles: [
          movie("t1", "Alien", { artworkVersion: "2025-06-01 00:00:00" }),
          movie("t2", "Dune"),
        ],
        nextCursor: null,
      }),
    );
    renderGrid();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(2));

    const tiles = screen.getAllByTestId("poster-tile");
    const t1 = tiles.find((t) => t.getAttribute("data-title-id") === "t1")!;
    const t2 = tiles.find((t) => t.getAttribute("data-title-id") === "t2")!;
    const t1Src = within(t1).getByTestId("poster-img").getAttribute("src")!;
    const t2Src = within(t2).getByTestId("poster-img").getAttribute("src")!;
    // t1's src carries its version token; t2 (no artwork) has a bare URL.
    expect(t1Src).toContain(`?v=${encodeURIComponent("2025-06-01 00:00:00")}`);
    expect(t2Src).not.toContain("?v=");
  });

  it("ignores scan events for a different Library", async () => {
    renderGrid();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(3));
    listTitles.mockClear();
    await act(async () => {
      emitEvent?.("scanProgress", { libraryId: "other", titlesFound: 9 });
    });
    // No refetch was triggered for an unrelated Library.
    expect(listTitles).not.toHaveBeenCalled();
  });
});
