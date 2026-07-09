import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, TitlesPage, TitleSort, TitleSummary } from "../api/types";

// Regression test for the Movies-grid pagination bug: the infinite-scroll
// sentinel renders only AFTER the first page loads, so the old observer wiring
// (a useEffect that ran once on mount with a null ref and never re-ran) never
// observed it — the grid was stuck at the first 20 titles. The fix wires the
// observer via a callback ref that attaches when the sentinel mounts.
//
// jsdom has no real IntersectionObserver. The global test setup installs a no-op
// stub; here we override it with a CONTROLLABLE mock that captures each observer's
// callback and the node it observes, so the test can simulate the sentinel
// scrolling into view (isIntersecting:true) and assert the next page is appended.

type Observed = { cb: IntersectionObserverCallback; el: Element };
let observed: Observed[] = [];

class ControllableIntersectionObserver implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = "";
  readonly thresholds = [];
  private cb: IntersectionObserverCallback;
  constructor(cb: IntersectionObserverCallback) {
    this.cb = cb;
  }
  observe(el: Element): void {
    observed.push({ cb: this.cb, el });
  }
  unobserve(el: Element): void {
    observed = observed.filter((o) => o.el !== el);
  }
  disconnect(): void {
    observed = observed.filter((o) => o.cb !== this.cb);
  }
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}

/** Fire isIntersecting:true on every currently-observed sentinel, as the browser
 * would when the sentinel scrolls into view. */
function scrollSentinelIntoView() {
  for (const { cb, el } of observed) {
    cb(
      [{ isIntersecting: true, target: el } as unknown as IntersectionObserverEntry],
      {} as IntersectionObserver,
    );
  }
}

const { listTitles, getLibrary } = vi.hoisted(() => ({
  listTitles: vi.fn(),
  getLibrary: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listTitles: (...a: unknown[]) => listTitles(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";

const lib: Library = { id: "lib1", name: "Movies", kind: "movie", rootFolders: [] };

function titles(prefix: string, n: number): TitleSummary[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `${prefix}${i}`,
    kind: "movie" as const,
    title: `${prefix}${i}`,
    year: 2000,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
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

beforeEach(() => {
  observed = [];
  vi.stubGlobal("IntersectionObserver", ControllableIntersectionObserver);
  listTitles.mockReset();
  getLibrary.mockReset();
  getLibrary.mockResolvedValue(lib);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("LibraryGridScreen infinite scroll", () => {
  it("appends the next page when the sentinel scrolls into view, until the end", async () => {
    // Page 1: 20 titles WITH a nextCursor. Page 2: 20 more, no cursor → the end.
    const page1: TitlesPage = { titles: titles("a", 20), nextCursor: "c1" };
    const page2: TitlesPage = { titles: titles("b", 20), nextCursor: null };
    listTitles.mockImplementation((_id: string, opts: { cursor?: string | null }) =>
      Promise.resolve(opts?.cursor ? page2 : page1),
    );

    renderGrid();

    // First page renders: exactly 20 tiles, more pages remain (no end marker).
    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile")).toHaveLength(20),
    );
    expect(screen.queryByTestId("grid-end")).not.toBeInTheDocument();
    // The sentinel mounted and is being observed (the crux of the bug).
    expect(screen.getByTestId("grid-sentinel")).toBeInTheDocument();
    expect(observed.length).toBeGreaterThan(0);

    // Simulate the sentinel scrolling into view → loadMore fires → page 2 appends.
    await act(async () => {
      scrollSentinelIntoView();
    });

    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile").length).toBeGreaterThan(20),
    );
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(40);
    // No more pages → the end marker shows.
    expect(screen.getByTestId("grid-end")).toHaveTextContent("That's everything.");
  });
});
