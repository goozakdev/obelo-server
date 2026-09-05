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

// A linked Library (ADR-0056 §1) browses like any other; the two marks it carries
// here are the whole of its treatment on this screen.
describe("LibraryListScreen — a linked library", () => {
  const CARTOONS: Library = {
    id: "lib9",
    name: "Cartoons",
    kind: "movie",
    rootFolders: [],
    linked: true,
    available: true,
  };

  it("badges it and says it is shared rather than claiming zero folders", async () => {
    listLibraries.mockResolvedValue([CARTOONS]);
    renderList();
    await waitFor(() => expect(screen.getByTestId("libraries")).toBeInTheDocument());

    expect(screen.getByTestId("library-linked-badge")).toHaveTextContent("Linked");
    // "0 folders" would be a statement about this disk, and a mirror has none.
    expect(screen.getByTestId("library-item")).not.toHaveTextContent(/0 folders/);
    expect(screen.getByTestId("library-item")).toHaveTextContent(/shared with you/i);
  });

  it("greys an unavailable one but LEAVES IT ON THE SHELF, still openable", async () => {
    listLibraries.mockResolvedValue([{ ...CARTOONS, available: false }]);
    renderList();
    await waitFor(() => expect(screen.getByTestId("libraries")).toBeInTheDocument());

    const item = screen.getByTestId("library-item");
    expect(item).toHaveAttribute("data-available", "false");
    expect(item.className).toContain("is-unavailable");
    expect(screen.getByTestId("library-unavailable")).toBeInTheDocument();
    // Still a link to the grid: the catalog is here, only the bytes are away, and
    // a library that vanishes when a friend reboots teaches people it is gone.
    expect(item).toHaveAttribute("href", "/libraries/lib9");
  });

  it("marks a local library neither linked nor unavailable", async () => {
    listLibraries.mockResolvedValue([
      { id: "l1", name: "Movies", kind: "movie", rootFolders: [{ id: "r", path: "/m" }] },
    ]);
    renderList();
    await waitFor(() => expect(screen.getByTestId("libraries")).toBeInTheDocument());

    expect(screen.queryByTestId("library-linked-badge")).toBeNull();
    expect(screen.getByTestId("library-item")).not.toHaveAttribute("data-available");
    expect(screen.getByTestId("library-item")).toHaveTextContent("1 folder");
  });
});
