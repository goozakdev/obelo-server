import { describe, it, expect, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { usePaginatedList, type Page } from "./usePaginatedList";

// Engine behavior of the generic paginated-list hook: append pages, de-dup by id
// across pages, the in-flight/hasMore guard on loadMore, and reset+refetch when
// the fetcher identity changes. (The Movie title grid's useTitleGrid is a thin
// adapter over this; useTitleGrid.test.ts covers the listTitles binding.)

interface Item {
  id: string;
}

const getId = (i: Item) => i.id;

function page(ids: string[], nextCursor: string | null): Page<Item> {
  return { items: ids.map((id) => ({ id })), nextCursor };
}

describe("usePaginatedList", () => {
  it("loads page one, then appends the next page on loadMore with no dupes", async () => {
    const fetchPage = (cursor: string | null) =>
      Promise.resolve(cursor === "c1" ? page(["c", "d"], null) : page(["a", "b"], "c1"));

    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.items.map((i) => i.id)).toEqual(["a", "b"]);
    expect(result.current.hasMore).toBe(true);

    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));

    expect(result.current.items.map((i) => i.id)).toEqual(["a", "b", "c", "d"]);
    expect(result.current.hasMore).toBe(false);
  });

  it("de-dups an item that reappears across pages (no gaps either)", async () => {
    const fetchPage = (cursor: string | null) =>
      // Page 2 overlaps "b"; it must not double, but "c" still appends.
      Promise.resolve(cursor === "c1" ? page(["b", "c"], null) : page(["a", "b"], "c1"));

    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.hasMore).toBe(false));
    expect(result.current.items.map((i) => i.id)).toEqual(["a", "b", "c"]);
  });

  it("resets to page one when the fetcher identity changes", async () => {
    const fetchA = () => Promise.resolve(page(["a", "b"], "c1"));
    const fetchB = () => Promise.resolve(page(["z", "y"], null));

    const { result, rerender } = renderHook(
      ({ fetchPage }: { fetchPage: typeof fetchA }) =>
        usePaginatedList(fetchPage, getId),
      { initialProps: { fetchPage: fetchA } },
    );
    await waitFor(() => expect(result.current.items.map((i) => i.id)).toEqual(["a", "b"]));

    rerender({ fetchPage: fetchB });
    // A new fetcher replaces (not appends) the list.
    await waitFor(() => expect(result.current.items.map((i) => i.id)).toEqual(["z", "y"]));
    expect(result.current.hasMore).toBe(false);
  });

  it("loadMore is a no-op once there are no more pages (hasMore guard)", async () => {
    const fetchPage = vi.fn(() => Promise.resolve(page(["a"], null)));
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.hasMore).toBe(false);
    expect(fetchPage).toHaveBeenCalledTimes(1);

    await act(async () => {
      result.current.loadMore();
    });
    // No extra fetch: nothing more to load.
    expect(fetchPage).toHaveBeenCalledTimes(1);
    expect(result.current.items.map((i) => i.id)).toEqual(["a"]);
  });

  it("surfaces an error from the fetcher", async () => {
    const fetchPage = () => Promise.reject(new Error("boom"));
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.error).toBe("boom"));
    expect(result.current.items).toEqual([]);
  });

  it("refresh surfaces newly-indexed items into an empty list (live scan)", async () => {
    // A brand-new Library: first load is empty (scan hasn't found anything yet);
    // a later refresh re-reads page one and the indexed Titles appear.
    let data: Record<string, Page<Item>> = { "": page([], null) };
    const fetchPage = (cursor: string | null) => Promise.resolve(data[cursor ?? ""]);
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.items).toEqual([]);

    data = { "": page(["a", "b"], null) };
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map((i) => i.id)).toEqual(["a", "b"]));
    expect(result.current.hasMore).toBe(false);
  });

  it("refresh merges the loaded window in place — inserts new items, keeps the rest, never blanks", async () => {
    let data: Record<string, Page<Item>> = {
      "": page(["a", "b"], "c1"),
      c1: page(["c", "d"], null),
    };
    const fetchPage = (cursor: string | null) => Promise.resolve(data[cursor ?? ""]);
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));

    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));
    expect(result.current.items.map((i) => i.id)).toEqual(["a", "b", "c", "d"]);

    // The server now has a new "a2" sorted into page one; "b" stays.
    data = {
      "": page(["a", "a2", "b"], "c1"),
      c1: page(["c", "d"], null),
    };
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() =>
      expect(result.current.items.map((i) => i.id)).toEqual(["a", "a2", "b", "c", "d"]),
    );
    // refresh is silent: it never flips the first-load spinner on.
    expect(result.current.loading).toBe(false);
  });

  it("refresh is a no-op while a load is in flight (single-flight)", async () => {
    let resolve: ((p: Page<Item>) => void) | null = null;
    const fetchPage = vi.fn(
      (_cursor: string | null) =>
        new Promise<Page<Item>>((res) => {
          resolve = res;
        }),
    );
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    // First load is pending (not yet resolved). A refresh now must be ignored,
    // not issue a second concurrent fetch.
    expect(fetchPage).toHaveBeenCalledTimes(1);
    act(() => {
      result.current.refresh();
    });
    expect(fetchPage).toHaveBeenCalledTimes(1);
    await act(async () => {
      resolve?.(page(["a"], null));
    });
    await waitFor(() => expect(result.current.items.map((i) => i.id)).toEqual(["a"]));
  });
});
