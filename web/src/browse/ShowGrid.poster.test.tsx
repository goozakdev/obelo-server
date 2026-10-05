import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import { layoutModeKey } from "./browseLayout";
import type { ShowSummary } from "../api/types";

// R05-11: ShowGrid passes `posterUrl ?? null` to <Poster>, so a Show with no
// artwork renders the placeholder directly instead of requesting an <img> that
// 404s. Without the `?? null` the Poster falls back to the title-keyed URL and
// mounts an <img>. Covered in the tile grid and the Detail row.

const { listShows } = vi.hoisted(() => ({ listShows: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: { listShows: (...a: unknown[]) => listShows(...a) },
  };
});

import ShowGrid from "./ShowGrid";

function show(id: string, posterUrl?: string): ShowSummary {
  return {
    id,
    libraryId: "lib1",
    kind: "show",
    title: id,
    year: 2022,
    needsReview: false,
    unwatchedEpisodeCount: 0,
    overview: "",
    genres: [],
    cast: [],
    posterUrl,
  };
}

beforeEach(() => {
  window.localStorage.clear();
  listShows.mockReset();
  listShows.mockResolvedValue({
    shows: [show("withArt", "/api/v1/shows/withArt/artwork"), show("artless")],
    nextCursor: null,
  });
});

async function expectArtlessShowHasNoImage() {
  await waitFor(() => expect(screen.getAllByTestId("poster-img")).toHaveLength(1));
  const imgs = screen.getAllByTestId("poster-img");
  expect(imgs[0]).toHaveAttribute("src", "/api/v1/shows/withArt/artwork");
  // The artless Show has a placeholder and no <img> at all (no request to 404).
  const placeholders = screen.getAllByTestId("poster-placeholder");
  expect(placeholders).toHaveLength(1);
  expect(placeholders[0]).toHaveTextContent("AR");
  expect(document.querySelectorAll("img")).toHaveLength(1);
}

describe("ShowGrid poster call sites (R05-11)", () => {
  it("tile mode: an artless Show renders the placeholder with no <img> request", async () => {
    renderWithAuth(<ShowGrid libraryId="lib1" libraryName="Shows" />);
    await expectArtlessShowHasNoImage();
    const tile = screen.getAllByTestId("poster-tile").find((t) => t.getAttribute("data-show-id") === "artless")!;
    expect(within(tile).queryByTestId("poster-img")).toBeNull();
  });

  it("detail mode: an artless Show row renders the placeholder with no <img> request", async () => {
    window.localStorage.setItem(layoutModeKey("lib1"), "detail");
    renderWithAuth(<ShowGrid libraryId="lib1" libraryName="Shows" />);
    await expectArtlessShowHasNoImage();
  });
});
