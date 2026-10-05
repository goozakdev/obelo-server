import { describe, it, expect, beforeEach } from "vitest";
import { screen, fireEvent, act } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue, type QueueStore } from "./queue/useQueue";
import QueuePanel from "./QueuePanel";

// R04-20: the up-next rows reorder from the keyboard (Alt+ArrowUp / Alt+ArrowDown on
// a focused row), through the same queue.reorder drag and drop uses.

function summary(id: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

let store: QueueStore;
function Panel() {
  store = useQueue();
  return <QueuePanel queue={store} />;
}

function order() {
  return screen.getAllByTestId("queue-entry").map((li) => li.getAttribute("data-title-id"));
}
function row(titleId: string) {
  return screen.getAllByTestId("queue-entry").find((li) => li.getAttribute("data-title-id") === titleId)!;
}

beforeEach(() => {
  window.sessionStorage.clear();
  renderWithAuth(<Panel />, { initialEntries: ["/"] });
  act(() => {
    store.playNow(["a", "b", "c", "d"].map((id) => entryFromTitle(summary(id))));
  });
});

describe("QueuePanel keyboard reorder (R04-20)", () => {
  it("up-next rows are focusable; the now-playing row is not", () => {
    expect(row("a")).not.toHaveAttribute("tabindex");
    expect(row("b")).toHaveAttribute("tabindex", "0");
  });

  it("Alt+ArrowDown / Alt+ArrowUp move the focused row one place and keep focus on it", () => {
    row("c").focus();
    fireEvent.keyDown(row("c"), { key: "ArrowDown", altKey: true });
    expect(order()).toEqual(["a", "b", "d", "c"]);
    expect(row("c")).toHaveFocus();
    fireEvent.keyDown(row("c"), { key: "ArrowUp", altKey: true });
    fireEvent.keyDown(row("c"), { key: "ArrowUp", altKey: true });
    expect(order()).toEqual(["a", "c", "b", "d"]);
    expect(row("c")).toHaveFocus();
  });

  it("stops at both ends of the up-next list and never passes the now-playing entry", () => {
    fireEvent.keyDown(row("b"), { key: "ArrowUp", altKey: true });
    expect(order()).toEqual(["a", "b", "c", "d"]);
    fireEvent.keyDown(row("d"), { key: "ArrowDown", altKey: true });
    expect(order()).toEqual(["a", "b", "c", "d"]);
  });

  it("ignores plain arrows and an Alt+Arrow aimed at a control inside the row", () => {
    fireEvent.keyDown(row("c"), { key: "ArrowUp" });
    fireEvent.keyDown(row("c").querySelector("button")!, { key: "ArrowUp", altKey: true });
    expect(order()).toEqual(["a", "b", "c", "d"]);
  });
});
