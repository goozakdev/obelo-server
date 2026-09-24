import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { ShowSeasons, SeasonEpisodes } from "../api/types";

// "Detect markers now" (ADR-0065 §4) on the Show detail's ⋯ menu: an Admin sees
// it and it POSTs for this Show; a Member never sees it.

const { getShowSeasons, getSeasonEpisodes, listLibraries, detectShowMarkers } = vi.hoisted(() => ({
  getShowSeasons: vi.fn(),
  getSeasonEpisodes: vi.fn(),
  listLibraries: vi.fn(),
  detectShowMarkers: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      detectShowMarkers: (...a: unknown[]) => detectShowMarkers(...a),
    },
  };
});

import ShowDetailScreen from "./ShowDetailScreen";

const ADMIN = { id: "u1", username: "operator", role: "admin" };
const MEMBER = { id: "u2", username: "ada", role: "member" };

const season = { id: "s1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 0 };
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
const episodes: SeasonEpisodes = { season, episodes: [] } as unknown as SeasonEpisodes;

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
  listLibraries.mockReset().mockResolvedValue([{ id: "lib-tv", name: "Shows", kind: "tv", rootFolders: [] }]);
  detectShowMarkers.mockReset().mockResolvedValue(undefined);
});

describe("ShowDetailScreen — detect markers now", () => {
  it("lets an Admin queue detection for this Show", async () => {
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("detect-markers-item"));
    await waitFor(() => expect(detectShowMarkers).toHaveBeenCalledWith("sh1"));
    expect(await screen.findByTestId("markers-notice")).toHaveTextContent(/in the background/);
  });

  it("offers a Member no such action", async () => {
    const user = userEvent.setup();
    renderDetail(MEMBER);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("detect-markers-item")).toBeNull();
  });
});
