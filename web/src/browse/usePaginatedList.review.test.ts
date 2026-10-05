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

  // D005: an early-stopped walk replaces the list; the cursor continues from it.
  it("replaces the list on an early stop and loadMore yields the pushed-out item next", async () => {
    // 3 pages of 2; the user has loaded 4 (a-d). A new x lands at the head, so a
    // 4-item window walk is [x,a,b,c] and stops with a next cursor; d is the next
    // page's head, not a kept tail.
    let all = ["a", "b", "c", "d", "e", "f"];
    const fetchPage = (cursor: string | null) => {
      const start = cursor === null ? 0 : Number(cursor);
      return Promise.resolve(
        page(all.slice(start, start + 2), start + 2 < all.length ? String(start + 2) : null),
      );
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));
    all = ["x", ...all];
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["x", "a", "b", "c"]));
    expect(result.current.hasMore).toBe(true);
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() =>
      expect(result.current.items.map(getId)).toEqual(["x", "a", "b", "c", "d", "e"]),
    );
  });

  it("does not keep a deleted tail item or grow the list on repeated early-stopped refreshes", async () => {
    let all = ["a", "b", "c", "d", "e", "f"];
    const fetchPage = (cursor: string | null) => {
      const start = cursor === null ? 0 : Number(cursor);
      return Promise.resolve(
        page(all.slice(start, start + 2), start + 2 < all.length ? String(start + 2) : null),
      );
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));
    all = ["a", "b", "c", "e", "f"]; // d (the loaded tail) is deleted
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["a", "b", "c", "e"]));
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["a", "b", "c", "e"]));
    expect(result.current.items).toHaveLength(4);
  });

  it("still drops an item deleted inside the window on an early stop", async () => {
    let all = ["a", "b", "c", "d", "e", "f"];
    const fetchPage = (cursor: string | null) => {
      const start = cursor === null ? 0 : Number(cursor);
      return Promise.resolve(
        page(all.slice(start, start + 2), start + 2 < all.length ? String(start + 2) : null),
      );
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.items.length).toBe(4));
    all = ["a", "c", "d", "e", "f"];
    await act(async () => {
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.items.map(getId)).toEqual(["a", "c", "d", "e"]));
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

  it("keeps the page a loadMore just appended when a refresh was replayed after it", async () => {
    let releasePage2: () => void = () => {};
    let fetchCalls = 0;
    const fetchPage = (cursor: string | null) => {
      fetchCalls++;
      if (cursor === "2") {
        return new Promise<Page<Item>>((resolve) => {
          releasePage2 = () => resolve(page(["c", "d"], null));
        });
      }
      return Promise.resolve(page(["a", "b"], "2"));
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      result.current.loadMore();
    });
    await act(async () => {
      result.current.refresh();
    });
    await act(async () => {
      releasePage2(); // loadMore lands; the replayed refresh starts and holds on page 2
    });
    expect(result.current.items.map(getId)).toEqual(["a", "b", "c", "d"]);
    await act(async () => {
      releasePage2(); // let the replayed refresh complete
    });
    await waitFor(() => expect(fetchCalls).toBe(4));
    expect(result.current.items.map(getId)).toEqual(["a", "b", "c", "d"]);
    expect(result.current.hasMore).toBe(false);
  });

  it("clears a failed loadMore's error when the pending refresh succeeds", async () => {
    let failNext = false;
    let holdRefresh: Promise<void> | null = null;
    const fetchPage = async (cursor: string | null) => {
      if (cursor === "2" && failNext) throw new Error("boom");
      if (cursor === null && holdRefresh) await holdRefresh; // the replayed refresh walk
      return page(["a", "b"], "2");
    };
    const { result } = renderHook(() => usePaginatedList(fetchPage, getId));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBeNull();

    failNext = true;
    let releaseRefresh!: () => void;
    holdRefresh = new Promise<void>((resolve) => {
      releaseRefresh = resolve;
    });
    await act(async () => {
      result.current.loadMore();
      result.current.refresh(); // skipped while loadMore is in flight, replayed after
    });
    // The failure is on screen while the replayed refresh is still in flight.
    await waitFor(() => expect(result.current.error).toBe("boom"));

    await act(async () => {
      releaseRefresh();
    });
    await waitFor(() => expect(result.current.error).toBeNull());
    expect(result.current.items.map(getId)).toEqual(["a", "b"]);
  });
});
