import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, ShowsPage, ShowSummary } from "../api/types";
import { sortFirstChar } from "./useLetterJump";

// Covers the alphabetical jump bar (LetterJumpBar + useLetterJump): the first-
// letter sort key that mirrors the backend (article-stripped), and the on-demand
// paging — a letter range whose items haven't been scrolled to yet pulls further
// pages until the target item is in the DOM, then scrolls to it.

describe("sortFirstChar", () => {
  it("lower-cases and strips a leading article, longest first", () => {
    expect(sortFirstChar("The Matrix")).toBe("m");
    expect(sortFirstChar("An Education")).toBe("e");
    expect(sortFirstChar("A Serious Man")).toBe("s");
    expect(sortFirstChar("Alien")).toBe("a"); // "a" without a trailing space isn't the article
  });

  it("keeps digits and symbols as-is (they sort before letters)", () => {
    expect(sortFirstChar("300")).toBe("3");
    expect(sortFirstChar("  Weird  ")).toBe("w");
    expect(sortFirstChar("")).toBe("");
  });
});

// --- Jump paging integration (through the TV Show grid) --------------------

const { listShows, getLibrary } = vi.hoisted(() => ({
  listShows: vi.fn(),
  getLibrary: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listShows: (...a: unknown[]) => listShows(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";

const lib: Library = { id: "lib1", name: "Shows", kind: "tv", rootFolders: [] };

function shows(titles: string[]): ShowSummary[] {
  return titles.map((title) => ({
    id: title,
    kind: "show" as const,
    title,
    year: 2022,
    needsReview: false,
    unwatchedEpisodeCount: 0,
  }));
}

function renderGrid() {
  return renderWithAuth(
    <Routes>
      <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
    </Routes>,
    { initialEntries: ["/libraries/lib1"] },
  );
}

let scrollSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  // jsdom doesn't implement scrollIntoView; capture the element it's called on.
  scrollSpy = vi.fn();
  Element.prototype.scrollIntoView = scrollSpy as unknown as typeof Element.prototype.scrollIntoView;
  listShows.mockReset();
  getLibrary.mockReset();
  getLibrary.mockResolvedValue(lib);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("letter jump", () => {
  it("pages forward until the target letter is loaded, then scrolls to it", async () => {
    // Page 1 is all A-titles (cursor c1); the M-titles the user jumps to live on
    // page 2, which isn't loaded until the jump asks for it.
    const page1: ShowsPage = { shows: shows(["Apple", "Arrow"]), nextCursor: "c1" };
    const page2: ShowsPage = { shows: shows(["Mango", "Melon"]), nextCursor: null };
    listShows.mockImplementation((_id: string, opts: { cursor?: string | null }) =>
      Promise.resolve(opts?.cursor ? page2 : page1),
    );

    renderGrid();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(2));

    // Jump to M-P. Page 2 isn't loaded, so the target letter isn't in the DOM.
    await act(async () => {
      screen.getByTestId("letter-jump-m").click();
    });

    // The jump pulled page 2 and scrolled to the first M tile.
    await waitFor(() => expect(scrollSpy).toHaveBeenCalled());
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(4);
    const scrolled = scrollSpy.mock.instances[scrollSpy.mock.instances.length - 1] as HTMLElement;
    expect(scrolled).toHaveAttribute("data-show-id", "Mango");
  });

  it("lands on the last item when the range has no matching items", async () => {
    const page1: ShowsPage = { shows: shows(["Apple", "Banana"]), nextCursor: null };
    listShows.mockResolvedValue(page1);

    renderGrid();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(2));

    // No V-Z items and no more pages → land on the last tile.
    await act(async () => {
      screen.getByTestId("letter-jump-v").click();
    });

    await waitFor(() => expect(scrollSpy).toHaveBeenCalled());
    const scrolled = scrollSpy.mock.instances[scrollSpy.mock.instances.length - 1] as HTMLElement;
    expect(scrolled).toHaveAttribute("data-show-id", "Banana");
  });
});
