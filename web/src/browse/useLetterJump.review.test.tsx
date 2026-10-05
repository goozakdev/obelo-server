import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useLetterJump } from "./useLetterJump";
import { usePaginatedList, type Page } from "./usePaginatedList";

interface Item {
  id: string;
}

// R05-01: a failing loadMore while a jump is pending must not retry in a loop.
describe("useLetterJump error handling", () => {
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn() as unknown as typeof Element.prototype.scrollIntoView;
  });

  it("stops retrying loadMore once the pager reports an error", async () => {
    const fetchPage = vi.fn(async (cursor: string | null): Promise<Page<Item>> => {
      if (cursor === null) return { items: [{ id: "a" }], nextCursor: "c1" };
      throw new Error("boom");
    });
    const { result } = renderHook(() => {
      const grid = usePaginatedList(fetchPage, (i: Item) => i.id);
      const jump = useLetterJump(grid.items, (i) => i.id, grid);
      return { grid, jump };
    });
    await waitFor(() => expect(result.current.grid.loading).toBe(false));

    await act(async () => {
      result.current.jump.jumpTo("z");
    });
    await waitFor(() => expect(result.current.grid.error).not.toBeNull());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    // page one + exactly one failed page two.
    expect(fetchPage.mock.calls.length).toBeLessThanOrEqual(3);
  });
});
