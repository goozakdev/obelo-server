import { describe, it, expect } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { usePaginatedList, type Page } from "./usePaginatedList";

interface Item {
  id: string;
}

const getId = (i: Item) => i.id;

function page(ids: string[], nextCursor: string | null): Page<Item> {
  return { items: ids.map((id) => ({ id })), nextCursor };
}

describe("usePaginatedList refresh (review)", () => {
  // R05-02
  it("drops items the server removed when the refresh walk reached the end", async () => {
    let server = ["a", "b", "c"];
    const fetchPage = () => Promise.resolve(page(server, null));
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["a", "b", "c"]));

    server = ["a", "c"];
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["a", "c"]));
  });

  it("keeps loaded items past the window when the walk stopped early", async () => {
    const all = ["a", "b", "c", "d"];
    const fetchPage = (cursor: string | null) => {
      const start = cursor === null ? 0 : Number(cursor);
      const ids = all.slice(start, start + 2);
      return Promise.resolve(page(ids, start + 2 < all.length ? String(start + 2) : null));
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));
    // Window = 4 items; the walk covers pages 0 and 2 and ends. Nothing dropped.
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(all));
  });

  // R05-06
  it("re-runs a refresh that was skipped while a loadMore was in flight", async () => {
    let releasePage2: () => void = () => {};
    let server = ["a", "b"];
    const fetchPage = (cursor: string | null) => {
      if (cursor === "c1") {
        return new Promise<Page<Item>>((resolve) => {
          releasePage2 = () => resolve(page(["c"], null));
        });
      }
      return Promise.resolve(page(server, "c1"));
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));

    await act(async () => {
      result.current.loadMore(); // in flight, held
    });
    server = ["a", "b", "new"];
    await act(async () => {
      result.current.refresh(); // skipped: must be remembered
    });
    await act(async () => {
      releasePage2();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toContain("new"));
  });
});
