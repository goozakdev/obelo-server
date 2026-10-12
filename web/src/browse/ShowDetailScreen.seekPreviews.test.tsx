import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { ShowSeasons, SeasonEpisodes } from "../api/types";

// "Generate seek previews now" (ADR-0070) on the Show detail's ⋯ menu: an Admin
// sees it on a local Show and it POSTs for this Show; a Member, or a mirrored
// Show, never sees it. A refusal (503, no ffmpeg) shows as the notice.

const {
  getShowSeasons,
  getSeasonEpisodes,
  listLibraries,
  generateShowSeekPreviews,
} = vi.hoisted(() => ({
  getShowSeasons: vi.fn(),
  getSeasonEpisodes: vi.fn(),
  listLibraries: vi.fn(),
  generateShowSeekPreviews: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      generateShowSeekPreviews: (...a: unknown[]) =>
        generateShowSeekPreviews(...a),
    },
  };
});

import ShowDetailScreen from "./ShowDetailScreen";

const ADMIN = { id: "u1", username: "operator", role: "admin" };
const MEMBER = { id: "u2", username: "ada", role: "member" };

const season = {
  id: "s1",
  showId: "sh1",
  seasonNumber: 1,
  specials: false,
  episodeCount: 0,
};
const seasons: ShowSeasons = {
  show: {
    id: "sh1",
    libraryId: "lib-tv",
    kind: "show",
    title: "The Bear",
    year: 2022,
    needsReview: false,
    unwatchedEpisodeCount: 0,
    overview: "",
    genres: [],
    cast: [],
  },
  seasons: [season],
  resumePoint: null,
} as unknown as ShowSeasons;
const episodes: SeasonEpisodes = {
  season,
  episodes: [],
} as unknown as SeasonEpisodes;

function renderDetail(user: { id: string; username: string; role: string }) {
  return renderWithAuth(
    <Routes>
      <Route path="/shows/:showId" element={<ShowDetailScreen />} />
    </Routes>,
    { initialEntries: ["/shows/sh1"], user },
  );
}

beforeEach(() => {
  getShowSeasons.mockReset().mockResolvedValue(seasons);
  getSeasonEpisodes.mockReset().mockResolvedValue(episodes);
  listLibraries
    .mockReset()
    .mockResolvedValue([
      { id: "lib-tv", name: "Shows", kind: "tv", rootFolders: [] },
    ]);
  generateShowSeekPreviews.mockReset().mockResolvedValue(undefined);
});

describe("ShowDetailScreen — generate seek previews now", () => {
  it("lets an Admin queue generation for this Show", async () => {
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("generate-seek-previews-item"));
    await waitFor(() =>
      expect(generateShowSeekPreviews).toHaveBeenCalledWith("sh1"),
    );
    expect(await screen.findByTestId("seek-previews-notice")).toHaveTextContent(
      /in the background/,
    );
  });

  it("offers a Member no such action", async () => {
    const user = userEvent.setup();
    renderDetail(MEMBER);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("generate-seek-previews-item")).toBeNull();
  });

  it("offers no such action on a mirrored Show", async () => {
    getShowSeasons.mockResolvedValue({
      ...seasons,
      show: { ...seasons.show, linked: true, available: true },
    });
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("generate-seek-previews-item")).toBeNull();
  });

  it("shows the server's refusal when there is no ffmpeg", async () => {
    generateShowSeekPreviews.mockRejectedValue(
      new ApiError(503, "UNAVAILABLE", "no usable ffmpeg on this server"),
    );
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("generate-seek-previews-item"));
    expect(await screen.findByTestId("seek-previews-notice")).toHaveTextContent(
      "no usable ffmpeg on this server",
    );
  });
});
