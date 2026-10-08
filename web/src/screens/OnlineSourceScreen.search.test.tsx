import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { OnlineRow } from "../api/types";

// Search on an Online source's page (ADR-0068, issue 05): a search box on the
// source page only; results replace the rows, each submit is a fresh Plugin call,
// and a failed search says "{source} isn't responding" in place of results while
// the box stays usable.

const { getOnlineSources, getOnlineRows, searchOnlineItems } = vi.hoisted(() => ({
  getOnlineSources: vi.fn(),
  getOnlineRows: vi.fn(),
  searchOnlineItems: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getOnlineSources: (...a: unknown[]) => getOnlineSources(...a),
      getOnlineRows: (...a: unknown[]) => getOnlineRows(...a),
      searchOnlineItems: (...a: unknown[]) => searchOnlineItems(...a),
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

const rows: OnlineRow[] = [{ id: "recent", label: "Recently added", nextCursor: null, items: [item("a")] }];

function renderPage() {
  return renderWithAuth(
    <Routes>
      <Route path="/online/:sourceId" element={<OnlineSourceScreen />} />
    </Routes>,
    { initialEntries: ["/online/tube"] },
  );
}

function search(q: string) {
  fireEvent.change(screen.getByTestId("online-search-input"), { target: { value: q } });
  fireEvent.submit(screen.getByTestId("online-search-form"));
}

beforeEach(() => {
  getOnlineSources.mockReset().mockResolvedValue([{ id: "tube", name: "Test Tube", iconUrl: null }]);
  getOnlineRows.mockReset().mockResolvedValue(rows);
  searchOnlineItems.mockReset().mockResolvedValue([item("hit")]);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("OnlineSourceScreen search", () => {
  it("shows the Plugin's results in place of the rows, and asks again for the same query", async () => {
    renderPage();
    await screen.findByTestId("online-row-recent");

    search("  cats ");
    const results = await screen.findByTestId("online-search-results");
    expect(results).toHaveTextContent("Talk hit");
    expect(screen.queryByTestId("online-row-recent")).toBeNull();
    expect(searchOnlineItems).toHaveBeenCalledWith("tube", "cats", expect.anything());

    search("cats");
    await waitFor(() => expect(searchOnlineItems).toHaveBeenCalledTimes(2));
  });

  it("says when nothing matched, and a blank query returns to the rows without a call", async () => {
    searchOnlineItems.mockResolvedValue([]);
    renderPage();
    await screen.findByTestId("online-row-recent");

    search("zzz");
    expect(await screen.findByTestId("online-search-empty")).toHaveTextContent("Test Tube");

    search("   ");
    expect(await screen.findByTestId("online-row-recent")).toBeInTheDocument();
    expect(screen.queryByTestId("online-search-empty")).toBeNull();
    expect(searchOnlineItems).toHaveBeenCalledTimes(1);
  });

  it("shows the same message in place of results on a failure and leaves the box usable", async () => {
    searchOnlineItems
      .mockRejectedValueOnce(new ApiError(502, "SOURCE_UNAVAILABLE", "the source is not responding"))
      .mockResolvedValue([item("hit")]);
    renderPage();
    await screen.findByTestId("online-row-recent");

    search("cats");
    const err = await screen.findByTestId("online-search-error");
    expect(err).toHaveTextContent("Test Tube isn’t responding.");
    expect(screen.queryByTestId("online-search-results")).toBeNull();
    expect(screen.getByTestId("online-search-input")).not.toBeDisabled();

    search("dogs");
    expect(await screen.findByTestId("online-search-results")).toHaveTextContent("Talk hit");
    expect(screen.queryByTestId("online-search-error")).toBeNull();
  });

  it("keeps the search box when the page itself failed to load", async () => {
    getOnlineRows.mockRejectedValue(new ApiError(502, "SOURCE_UNAVAILABLE", "the source is not responding"));
    renderPage();
    expect(await screen.findByTestId("online-source-error")).toHaveTextContent("Test Tube isn’t responding.");
    expect(screen.getByTestId("online-search-input")).toBeInTheDocument();
  });
});
