import { describe, it, expect, beforeEach } from "vitest";
import { screen, act } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue, type QueueStore } from "./queue/useQueue";
import QueuePanel from "./QueuePanel";

// A queued row's poster carries the Title's artwork version, so a re-picked image
// reloads instead of serving the browser-cached one.

function summary(id: string, artworkVersion?: string): TitleSummary {
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
    artworkVersion,
  };
}

let store: QueueStore;
function Panel() {
  store = useQueue();
  return <QueuePanel queue={store} />;
}

beforeEach(() => {
  window.sessionStorage.clear();
  renderWithAuth(<Panel />, { initialEntries: ["/"] });
});

describe("QueuePanel poster", () => {
  it("cache-busts each row's poster with the entry's artworkVersion", () => {
    act(() => {
      store.playNow([entryFromTitle(summary("a", "v7")), entryFromTitle(summary("b"))]);
    });
    const src = (id: string) =>
      screen
        .getAllByTestId("queue-entry")
        .find((li) => li.getAttribute("data-title-id") === id)!
        .querySelector("img")
        ?.getAttribute("src");
    expect(src("a")).toBe("/api/v1/titles/a/artwork/poster?v=v7");
    expect(src("b")).toBe("/api/v1/titles/b/artwork/poster");
  });
});
