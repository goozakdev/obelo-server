import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library } from "../api/types";

const { listLibraries } = vi.hoisted(() => ({ listLibraries: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: { listLibraries: (...a: unknown[]) => listLibraries(...a) },
  };
});

import LibraryListScreen from "./LibraryListScreen";

function renderList() {
  return renderWithAuth(<LibraryListScreen />);
}

// The error-rendering path (a rejected load → libraries-error) is covered
// deterministically by useAsync.test.ts (the hook all these screens share) and
// at screen level by the detail/grid screen tests, so it is not re-asserted here:
// a directly-mounted fast reject races vitest's unhandled-rejection detector in
// a way the Routes-wrapped screen tests don't.

beforeEach(() => listLibraries.mockReset());

describe("LibraryListScreen", () => {
  it("renders libraries as links to their grids", async () => {
    const libs: Library[] = [
      { id: "lib1", name: "Movies", kind: "movie", rootFolders: [{ id: "r1", path: "/m" }] },
    ];
    listLibraries.mockResolvedValue(libs);
    renderList();
    await waitFor(() => expect(screen.getByTestId("libraries")).toBeInTheDocument());
    const item = screen.getByTestId("library-item");
    expect(item).toHaveAttribute("href", "/libraries/lib1");
    expect(item).toHaveTextContent("Movies");
  });

  it("shows a clean empty state when there are no libraries", async () => {
    listLibraries.mockResolvedValue([]);
    renderList();
    await waitFor(() => expect(screen.getByTestId("libraries-empty")).toBeInTheDocument());
  });
});
