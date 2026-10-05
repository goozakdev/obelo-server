import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";

const { listLibraries } = vi.hoisted(() => ({ listLibraries: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: { listLibraries: (...a: unknown[]) => listLibraries(...a) },
  };
});

import LibraryListScreen from "./LibraryListScreen";

// R05-13: the screen reads the app-wide list instead of fetching its own copy.
describe("LibraryListScreen — one fetch", () => {
  it("does not issue a second GET /libraries beside the provider's", async () => {
    listLibraries.mockResolvedValue([
      { id: "lib1", name: "Movies", kind: "movie", rootFolders: [] },
    ]);
    renderWithAuth(<LibraryListScreen />);
    await waitFor(() => expect(screen.getByTestId("libraries")).toBeInTheDocument());
    expect(listLibraries).toHaveBeenCalledTimes(1);
  });
});
