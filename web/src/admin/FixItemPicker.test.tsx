import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import FixItemPicker from "./FixItemPicker";

function cand(id: string) {
  return { externalId: id, title: `Title ${id}`, year: 2000, kind: "movie" };
}

describe("FixItemPicker", () => {
  it("R01-07: Show more ignores a box edited after the search ran", async () => {
    const search = vi
      .fn()
      .mockResolvedValueOnce({ candidates: [cand("1"), cand("2")], hasMore: true })
      .mockResolvedValue({ candidates: [cand("3")], hasMore: false });
    render(
      <FixItemPicker
        seed="Dune"
        applyLabel="Use this"
        applyHint=""
        search={search}
        onApply={async () => {}}
        onCancel={() => {}}
      />,
    );
    const more = await screen.findByTestId("fix-picker-show-more");
    const input = screen.getByTestId("fix-picker-input");
    await userEvent.clear(input);
    await userEvent.type(input, "Arrival");
    await userEvent.click(more);

    await waitFor(() => expect(search).toHaveBeenCalledTimes(2));
    expect(search).toHaveBeenLastCalledWith("Dune", 1, {});
  });

  it("R01-07: the empty state names the query that ran, not the edited box", async () => {
    const search = vi.fn().mockResolvedValue({ candidates: [], hasMore: false });
    render(
      <FixItemPicker
        seed="Dune"
        applyLabel="Use this"
        applyHint=""
        search={search}
        onApply={async () => {}}
        onCancel={() => {}}
      />,
    );
    const empty = await screen.findByTestId("fix-picker-no-candidates");
    await userEvent.type(screen.getByTestId("fix-picker-input"), "xyz");
    expect(empty).toHaveTextContent("No matches for “Dune”");
  });
});
