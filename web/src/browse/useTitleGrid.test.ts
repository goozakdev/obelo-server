import { describe, it, expect } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useTitleGrid } from "./useTitleGrid";
import { fakeClient, jsonResponse, errorResponse } from "../test/fakeClient";
import type { TitleSort } from "../api/types";

// Pagination + sort behavior of the grid data hook — the cursor handling the
// issue calls out: a fake returning a nextCursor then none → loadMore appends
// without dupes/gaps; switching sort resets to page one.

function page(ids: string[], nextCursor?: string) {
  return jsonResponse({
    titles: ids.map((id) => ({ id, kind: "movie", title: id.toUpperCase() })),
    ...(nextCursor ? { nextCursor } : {}),
  });
}

describe("useTitleGrid", () => {
  it("loads page one, then appends the next page on loadMore with no dupes", async () => {
    const client = fakeClient({
      // First page (no cursor) → a, b + a cursor; second page (cursor=c1) → c, d, end.
      "GET /libraries/lib1/titles?sort=title": () => page(["a", "b"], "c1"),
      "GET /libraries/lib1/titles?cursor=c1&sort=title": () => page(["c", "d"]),
    });

    const { result } = renderHook(() =>
      useTitleGrid(client, "lib1", "title"),
    );

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.titles.map((t) => t.id)).toEqual(["a", "b"]);
    expect(result.current.hasMore).toBe(true);

    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.titles.length).toBe(4));

    // Appended, in order, no duplicates, and now at the end.
    expect(result.current.titles.map((t) => t.id)).toEqual(["a", "b", "c", "d"]);
    expect(result.current.hasMore).toBe(false);
  });

  it("de-dups a Title that reappears across pages (defensive, no gaps either)", async () => {
    const client = fakeClient({
      "GET /libraries/lib1/titles?sort=title": () => page(["a", "b"], "c1"),
      // Overlapping "b" must not double; "c" still appends.
      "GET /libraries/lib1/titles?cursor=c1&sort=title": () => page(["b", "c"]),
    });
    const { result } = renderHook(() => useTitleGrid(client, "lib1", "title"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.hasMore).toBe(false));
    expect(result.current.titles.map((t) => t.id)).toEqual(["a", "b", "c"]);
  });

  it("resets to page one when the sort changes", async () => {
    const client = fakeClient({
      "GET /libraries/lib1/titles?sort=title": () => page(["a", "b"], "c1"),
      "GET /libraries/lib1/titles?sort=dateAdded": () => page(["z", "y"]),
    });
    const { result, rerender } = renderHook(
      ({ sort }: { sort: TitleSort }) => useTitleGrid(client, "lib1", sort),
      { initialProps: { sort: "title" as TitleSort } },
    );
    await waitFor(() => expect(result.current.titles.map((t) => t.id)).toEqual(["a", "b"]));

    rerender({ sort: "dateAdded" });
    // New ordering replaces (not appends) the list.
    await waitFor(() => expect(result.current.titles.map((t) => t.id)).toEqual(["z", "y"]));
    expect(result.current.hasMore).toBe(false);
  });

  it("surfaces an API error from the envelope", async () => {
    const client = fakeClient({
      "GET /libraries/lib1/titles?sort=title": () =>
        errorResponse(500, "INTERNAL", "failed to list titles"),
    });
    const { result } = renderHook(() => useTitleGrid(client, "lib1", "title"));
    await waitFor(() => expect(result.current.error).toBe("failed to list titles"));
    expect(result.current.titles).toEqual([]);
  });
});
