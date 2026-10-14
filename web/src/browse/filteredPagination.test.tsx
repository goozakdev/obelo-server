import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act, renderHook } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiClient } from "../api/client";
import { memoryTokenStore } from "../api/token";
import { useTitleGrid } from "./useTitleGrid";
import { jsonResponse } from "../test/fakeClient";
import type { Library, ShowSummary } from "../api/types";

// "See all" lands on a filtered browse; the filter must ride along on EVERY page
// request, not just the first, or page two silently leaves the filtered set.

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

beforeEach(() => {
  observed = [];
  vi.stubGlobal("IntersectionObserver", ControllableIntersectionObserver);
  listShows.mockReset();
  getLibrary.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("filtered browse pagination", () => {
  it("movie: the second page request carries the same filter[...] param", async () => {
    const urls: string[] = [];
    const fetchImpl: typeof fetch = async (input) => {
      const url = input.toString();
      urls.push(url);
      const second = url.includes("cursor=c1");
      return jsonResponse({
        titles: [{ id: second ? "b" : "a", kind: "movie", title: "T" }],
        ...(second ? {} : { nextCursor: "c1" }),
      });
    };
    const client = new ApiClient({ fetchImpl, tokenStore: memoryTokenStore("t") });

    const { result } = renderHook(() =>
      useTitleGrid(client, "lib1", "title", undefined, { type: "genre", value: "Drama" }),
    );
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.titles.length).toBe(2));

    expect(urls).toHaveLength(2);
    for (const u of urls) expect(decodeURIComponent(u)).toContain("filter[genre]=Drama");
    expect(urls[1]).toContain("cursor=c1");
  });

  it("tv: the second page request carries the same filter", async () => {
    const lib: Library = { id: "lib1", name: "Shows", kind: "tv", rootFolders: [] };
    getLibrary.mockResolvedValue(lib);
    const show = (id: string): ShowSummary => ({
      id, libraryId: "lib1", kind: "show", title: id, year: 2022, needsReview: false,
      unwatchedEpisodeCount: 0, overview: "", genres: [], cast: [],
    });
    listShows.mockImplementation((_id: string, opts: { cursor?: string | null }) =>
      Promise.resolve(
        opts?.cursor
          ? { shows: [show("b")], nextCursor: null }
          : { shows: [show("a")], nextCursor: "c1" },
      ),
    );

    renderWithAuth(
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>,
      { initialEntries: ["/libraries/lib1?network=HBO"] },
    );
    await screen.findByTestId("grid-sentinel");
    await act(async () => {
      for (const { cb, el } of observed) {
        cb([{ isIntersecting: true, target: el } as unknown as IntersectionObserverEntry], {} as IntersectionObserver);
      }
    });
    await waitFor(() => expect(listShows).toHaveBeenCalledTimes(2));

    for (const call of listShows.mock.calls) {
      expect(call[1]).toMatchObject({ filter: { type: "network", value: "HBO" } });
    }
    expect(listShows.mock.calls[1][1]).toMatchObject({ cursor: "c1" });
  });
});
