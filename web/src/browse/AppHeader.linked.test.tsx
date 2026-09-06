import { describe, it, expect, beforeEach, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { apiClient } from "../api/client";
import type { Library } from "../api/types";
import AppHeader from "./AppHeader";

// Issue 18: a linked Library in the main menu names the Server it came from,
// with the chain LinkIcon + the sharing Server's name — the same LinkedMark
// every other surface shows, and it works for a Member because `linkedServer`
// rides the wire, not the Admin-only /links join. Two shapes: a kind with a
// single Library (a direct nav link) and a kind with several (a dropdown of
// names).

beforeEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

function lib(over: Partial<Library> & Pick<Library, "id" | "name" | "kind">): Library {
  return { rootFolders: [], ...over };
}

describe("AppHeader — linked libraries in the nav (issue 18)", () => {
  it("names the sharing server on a single linked library's kind link", async () => {
    vi.spyOn(apiClient, "listLibraries").mockResolvedValue([
      lib({
        id: "m1",
        name: "Kate's films",
        kind: "movie",
        linked: true,
        linkedServer: "Kate's Obelo",
      }),
    ]);

    renderWithAuth(<AppHeader />);

    const movies = await screen.findByTestId("nav-movies");
    await waitFor(() =>
      expect(within(movies).getByTestId("linked-badge")).toHaveTextContent(
        "Kate's Obelo",
      ),
    );
    expect(within(movies).getByTestId("linked-badge").querySelector("svg")).not.toBeNull();
  });

  it("names the sharing server on a linked entry inside the kind dropdown", async () => {
    vi.spyOn(apiClient, "listLibraries").mockResolvedValue([
      lib({ id: "m1", name: "Our films", kind: "movie" }),
      lib({
        id: "m2",
        name: "Kate's films",
        kind: "movie",
        linked: true,
        linkedServer: "Kate's Obelo",
      }),
    ]);

    renderWithAuth(<AppHeader />);

    const toggle = await screen.findByTestId("nav-movies");
    await userEvent.click(toggle);

    const options = await screen.findAllByTestId("nav-library-option");
    const linkedOption = options.find(
      (o) => o.getAttribute("data-library-id") === "m2",
    )!;
    const localOption = options.find(
      (o) => o.getAttribute("data-library-id") === "m1",
    )!;
    expect(within(linkedOption).getByTestId("linked-badge")).toHaveTextContent(
      "Kate's Obelo",
    );
    // The local shelf beside it wears no mark at all.
    expect(within(localOption).queryByTestId("linked-badge")).toBeNull();
  });

  it("shows no mark when nothing in the nav is linked", async () => {
    vi.spyOn(apiClient, "listLibraries").mockResolvedValue([
      lib({ id: "m1", name: "Our films", kind: "movie" }),
    ]);

    renderWithAuth(<AppHeader />);

    const movies = await screen.findByTestId("nav-movies");
    expect(within(movies).queryByTestId("linked-badge")).toBeNull();
  });
});
