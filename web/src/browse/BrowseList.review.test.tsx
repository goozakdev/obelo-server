import { describe, it, expect, vi } from "vitest";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import BrowseList, { type BrowseRowData } from "./BrowseList";

interface Item {
  id: string;
}

// R05-14 / R05-15: toRow runs once per item per render, and a re-render with the
// same item objects does not re-render the rows.
describe("BrowseList rows", () => {
  it("calls toRow once per item and skips unchanged rows on re-render", () => {
    const items: Item[] = [{ id: "a" }, { id: "b" }, { id: "c" }];
    const toRow = vi.fn((i: Item): BrowseRowData => ({ key: i.id, to: `/x/${i.id}`, name: i.id }));
    const ui = (list: Item[]) => (
      <MemoryRouter>
        <BrowseList mode="list" items={list} renderTile={() => null} toRow={toRow} />
      </MemoryRouter>
    );
    const { rerender, container } = render(ui(items));
    expect(toRow).toHaveBeenCalledTimes(3);

    // Append one: the three existing rows keep their DOM, only toRow is re-asked.
    const firstLi = container.querySelector("li");
    rerender(ui([...items, { id: "d" }]));
    expect(toRow).toHaveBeenCalledTimes(3 + 4);
    expect(container.querySelector("li")).toBe(firstLi);
    expect(container.querySelectorAll("li")).toHaveLength(4);
  });
});
