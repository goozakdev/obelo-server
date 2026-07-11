import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { ArtistSummary, ArtistsPage, Library } from "../api/types";

// Regression test for the Artist-list pagination gap: ArtistList used to do a
// single useAsync fetch capped at 100 Artists, ignoring the cursor — a Music
// library with more than one page was silently truncated. It now cursor-
// paginates through ALL Artists via usePaginatedList + an IntersectionObserver
// sentinel (the Movie grid's infinite scroll). This test FAILS on the old
// single-fetch code (no sentinel was rendered/observed and the second page never
// appended) and passes after the fix. Drives the music library landing
// (MusicLibraryScreen → ArtistList) at /music/libraries/:id.
//
// jsdom has no real IntersectionObserver. The global setup installs a no-op stub;
// here we override it with a CONTROLLABLE mock that captures each observer's
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

function scrollSentinelIntoView() {
  for (const { cb, el } of observed) {
    cb(
      [{ isIntersecting: true, target: el } as unknown as IntersectionObserverEntry],
      {} as IntersectionObserver,
    );
  }
}

const { listArtists, getLibrary } = vi.hoisted(() => ({
  listArtists: vi.fn(),
  getLibrary: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listArtists: (...a: unknown[]) => listArtists(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
    },
  };
});

import MusicLibraryScreen from "./MusicLibraryScreen";

const lib: Library = { id: "lib1", name: "Music", kind: "music", rootFolders: [] };

function artists(prefix: string, n: number): ArtistSummary[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `${prefix}${i}`,
    kind: "artist" as const,
    name: `${prefix}${i}`,
  }));
}

function renderGrid() {
  return renderWithAuth(
    <Routes>
      <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
    </Routes>,
    { initialEntries: ["/music/libraries/lib1"] },
  );
}

beforeEach(() => {
  observed = [];
  vi.stubGlobal("IntersectionObserver", ControllableIntersectionObserver);
  listArtists.mockReset();
  getLibrary.mockReset();
  getLibrary.mockResolvedValue(lib);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ArtistList infinite scroll", () => {
  it("appends the next page of Artists when the sentinel scrolls into view, until the end", async () => {
    // Page 1: 20 Artists WITH a nextCursor. Page 2: 20 more, no cursor → the end.
    const page1: ArtistsPage = { artists: artists("a", 20), nextCursor: "c1" };
    const page2: ArtistsPage = { artists: artists("b", 20), nextCursor: null };
    listArtists.mockImplementation((_id: string, opts: { cursor?: string | null }) =>
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

  it("keeps loading pages while the sentinel stays on-screen, with no manual scroll", async () => {
    // Reproduces the reported bug: a big library on a wide window. A real
    // IntersectionObserver fires only on intersection *transitions*, so once the
    // first page or two load and the sentinel is STILL in view (the short grid
    // didn't overflow the viewport), nothing re-fires and the list stalls at 40.
    // AlwaysInViewObserver models a sentinel that never leaves the viewport: every
    // observe() delivers isIntersecting:true (exactly as the real observer
    // re-delivers current state when a target is observed). On the pre-fix hook —
    // which attached the observer once and never re-observed — this stalls at 40;
    // the reobserveKey re-observe makes all three pages load untouched.
    class AlwaysInViewObserver implements IntersectionObserver {
      readonly root = null;
      readonly rootMargin = "";
      readonly thresholds = [];
      private cb: IntersectionObserverCallback;
      constructor(cb: IntersectionObserverCallback) {
        this.cb = cb;
      }
      observe(el: Element): void {
        queueMicrotask(() =>
          this.cb(
            [{ isIntersecting: true, target: el } as unknown as IntersectionObserverEntry],
            this,
          ),
        );
      }
      unobserve(): void {}
      disconnect(): void {}
      takeRecords(): IntersectionObserverEntry[] {
        return [];
      }
    }
    vi.stubGlobal("IntersectionObserver", AlwaysInViewObserver);

    const page1: ArtistsPage = { artists: artists("a", 20), nextCursor: "c1" };
    const page2: ArtistsPage = { artists: artists("b", 20), nextCursor: "c2" };
    const page3: ArtistsPage = { artists: artists("c", 20), nextCursor: null };
    listArtists.mockImplementation((_id: string, opts: { cursor?: string | null }) => {
      if (opts?.cursor === "c1") return Promise.resolve(page2);
      if (opts?.cursor === "c2") return Promise.resolve(page3);
      return Promise.resolve(page1);
    });

    renderGrid();

    // All three pages load without any scrollSentinelIntoView() call.
    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile")).toHaveLength(60),
    );
    expect(screen.getByTestId("grid-end")).toHaveTextContent("That's everything.");
  });
});
