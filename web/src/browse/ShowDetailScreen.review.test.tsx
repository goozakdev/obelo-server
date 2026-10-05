import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { SeasonEpisodes, ShowSeasons } from "../api/types";

// Review fixes on the Show detail screen: the open Season's episodes refetch when
// a Targeted scan reloads the Show (R05-03), a failed Play surfaces an error
// (R05-08), and a failed episode still falls back to the placeholder (R05-20).
// Also drives useTargetedScan through the SSE hub (R05-10).

const { getShowSeasons, getSeasonEpisodes, listLibraries, scanEntity, subscribeEvents } =
  vi.hoisted(() => ({
    getShowSeasons: vi.fn(),
    getSeasonEpisodes: vi.fn(),
    listLibraries: vi.fn(),
    scanEntity: vi.fn(),
    subscribeEvents: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      scanEntity: (...a: unknown[]) => scanEntity(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import ShowDetailScreen from "./ShowDetailScreen";

const show = {
  id: "sh1",
  libraryId: "lib-tv",
  kind: "show",
  title: "The Bear",
  year: 2022,
  needsReview: false,
  unwatchedEpisodeCount: 3,
  overview: "",
  genres: [],
  cast: [],
};
const season = { id: "s1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 1 };
const seasons = { show, seasons: [season], resumePoint: null } as unknown as ShowSeasons;

function episode(id: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    kind: "episode",
    title: `Ep ${id}`,
    seasonNumber: 1,
    episodeNumber: 1,
    episodeLabel: "",
    needsReview: false,
    resumePositionMs: 0,
    watched: false,
    overview: "",
    ...extra,
  };
}

function episodes(list: ReturnType<typeof episode>[]): SeasonEpisodes {
  return { season, episodes: list } as unknown as SeasonEpisodes;
}

let emit: (type: string, data: unknown) => void;

beforeEach(() => {
  getShowSeasons.mockReset().mockResolvedValue(seasons);
  getSeasonEpisodes.mockReset().mockResolvedValue(episodes([episode("e1")]));
  listLibraries.mockReset().mockResolvedValue([]);
  scanEntity.mockReset().mockResolvedValue({ libraryId: "lib-tv" });
  subscribeEvents.mockReset().mockImplementation((fn: typeof emit) => {
    emit = fn;
    return () => {};
  });
});

function renderDetail() {
  return renderWithAuth(
    <Routes>
      <Route path="/shows/:showId" element={<ShowDetailScreen />} />
    </Routes>,
    { initialEntries: ["/shows/sh1"] },
  );
}

describe("ShowDetailScreen review fixes", () => {
  it("refetches the open Season's episodes after a Targeted scan (R05-03)", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getAllByTestId("episode-row")).toHaveLength(1));

    getSeasonEpisodes.mockResolvedValue(episodes([episode("e1"), episode("e2")]));
    await userEvent.click(screen.getByTestId("overflow-menu-button"));
    await userEvent.click(screen.getByTestId("scan-item"));
    await waitFor(() => expect(scanEntity).toHaveBeenCalled());
    await act(async () => {
      emit("scanProgress", { libraryId: "lib-tv", complete: true, added: 1, removed: 0 });
    });

    await waitFor(() => expect(screen.getAllByTestId("episode-row")).toHaveLength(2));
  });

  it("shows the placeholder when an episode still fails to load (R05-20)", async () => {
    getSeasonEpisodes.mockResolvedValue(
      episodes([episode("e1", { stillUrl: "/still-404.jpg" })]),
    );
    renderDetail();
    const still = await screen.findByTestId("episode-still");
    fireEvent.error(still);
    await waitFor(() => expect(screen.queryByTestId("episode-still")).toBeNull());
    expect(screen.getByText("▶")).toBeInTheDocument();
  });

  it("tells the user when Play cannot load the first season (R05-08)", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("play-button")).toBeEnabled());
    getSeasonEpisodes.mockRejectedValue(new Error("season fetch failed"));
    await userEvent.click(screen.getByTestId("play-button"));
    await waitFor(() => expect(screen.getByTestId("queue-error")).toBeInTheDocument());
  });
});
