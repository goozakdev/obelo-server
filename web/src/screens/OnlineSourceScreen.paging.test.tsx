import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { OnlineRow } from "../api/types";

// An Online row pages forward by the opaque cursor the source named (ADR-0068,
// issue 04): the source page shows a "more" affordance only while a row has a
// cursor, asks for the next page with that cursor, appends it, and takes the
// affordance away when the last page arrives.

const { getOnlineSources, getOnlineRows, getOnlineRowPage } = vi.hoisted(() => ({
  getOnlineSources: vi.fn(),
  getOnlineRows: vi.fn(),
  getOnlineRowPage: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getOnlineSources: (...a: unknown[]) => getOnlineSources(...a),
      getOnlineRows: (...a: unknown[]) => getOnlineRows(...a),
      getOnlineRowPage: (...a: unknown[]) => getOnlineRowPage(...a),
      subscribeEvents: () => () => {},
    },
  };
});

import OnlineSourceScreen from "./OnlineSourceScreen";

const item = (id: string) => ({
  id,
  title: `Talk ${id}`,
  thumbnailUrl: `/api/v1/onlineSources/tube/items/${id}/thumbnail`,
  durationMs: 1000,
});

const rows: OnlineRow[] = [
  { id: "recent", label: "Recently added", nextCursor: "c1", items: [item("a")] },
  { id: "done", label: "Everything", nextCursor: null, items: [item("z")] },
];

function renderPage() {
  return renderWithAuth(
    <Routes>
      <Route path="/online/:sourceId" element={<OnlineSourceScreen />} />
    </Routes>,
    { initialEntries: ["/online/tube"] },
  );
}

beforeEach(() => {
  getOnlineSources.mockReset().mockResolvedValue([{ id: "tube", name: "Test Tube", iconUrl: null }]);
  getOnlineRows.mockReset().mockResolvedValue(rows);
  getOnlineRowPage.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("OnlineSourceScreen row paging", () => {
  it("shows a more affordance only on a row that has a cursor", async () => {
    renderPage();
    await screen.findByTestId("online-row-recent");

    expect(screen.getByTestId("online-row-more-recent")).toBeInTheDocument();
    expect(screen.queryByTestId("online-row-more-done")).toBeNull();
  });

  it("pages forward by the cursor, appends the items, and drops the affordance on the last page", async () => {
    getOnlineRowPage
      .mockResolvedValueOnce({ items: [item("b")], nextCursor: "c2" })
      .mockResolvedValueOnce({ items: [item("c")], nextCursor: null });
    renderPage();
    await screen.findByTestId("online-row-recent");

    fireEvent.click(screen.getByTestId("online-row-more-recent"));
    await waitFor(() => expect(screen.getByTestId("online-item-play-b")).toBeInTheDocument());
    expect(getOnlineRowPage).toHaveBeenLastCalledWith("tube", "recent", "c1", expect.anything());
    expect(screen.getByTestId("online-item-play-a")).toBeInTheDocument();
    expect(screen.getByTestId("online-row-more-recent")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("online-row-more-recent"));
    await waitFor(() => expect(screen.getByTestId("online-item-play-c")).toBeInTheDocument());
    expect(getOnlineRowPage).toHaveBeenLastCalledWith("tube", "recent", "c2", expect.anything());
    expect(screen.queryByTestId("online-row-more-recent")).toBeNull();
    expect(
      Array.from(document.querySelectorAll("[data-testid=online-row-recent] [data-testid=online-item-title]")).map(
        (e) => e.textContent,
      ),
    ).toEqual(["Talk a", "Talk b", "Talk c"]);
  });

  it("drops an item already in the row when a later page repeats it, with no duplicate keys", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    getOnlineRowPage.mockResolvedValueOnce({ items: [item("a"), item("b")], nextCursor: null });
    renderPage();
    await screen.findByTestId("online-row-recent");

    fireEvent.click(screen.getByTestId("online-row-more-recent"));
    await waitFor(() => expect(screen.getByTestId("online-item-play-b")).toBeInTheDocument());
    expect(screen.getAllByTestId("online-item-play-a")).toHaveLength(1);
    expect(screen.getAllByTestId("online-item-play-b")).toHaveLength(1);
    expect(errors.mock.calls.flat().join(" ")).not.toMatch(/same key/);
  });

  it("says so in place of the row's affordance when a page fails, and retries with the same cursor", async () => {
    getOnlineRowPage
      .mockRejectedValueOnce(new ApiError(502, "SOURCE_UNAVAILABLE", "the source is not responding"))
      .mockResolvedValueOnce({ items: [item("b")], nextCursor: null });
    renderPage();
    await screen.findByTestId("online-row-recent");

    fireEvent.click(screen.getByTestId("online-row-more-recent"));
    expect(await screen.findByTestId("online-row-more-error-recent")).toHaveTextContent("isn");
    expect(screen.getByTestId("online-item-play-a")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("online-row-more-recent"));
    await waitFor(() => expect(screen.getByTestId("online-item-play-b")).toBeInTheDocument());
    expect(getOnlineRowPage).toHaveBeenNthCalledWith(2, "tube", "recent", "c1", expect.anything());
    expect(screen.queryByTestId("online-row-more-recent")).toBeNull();
  });
});
