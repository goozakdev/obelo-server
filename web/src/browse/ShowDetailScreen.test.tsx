import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { useQueue } from "../player/queue/useQueue";
import type { ShowSeasons, SeasonEpisodes, ResumePointMode } from "../api/types";

// The Show detail SCREEN against a faked apiClient, focused on the resume-point
// block (issue 02, ADR-0028): the four modes it renders — not started
// (description + Play), in-progress (S/E · title · synopsis + Continue + Restart),
// next (block + Play), fully watched (description, no Play) — and the button
// wiring (Continue seeks to resumePositionMs, Restart to 0, both build the
// show-from-here Queue with the resume-point Episode as the head).

const { getShowSeasons, getSeasonEpisodes, listLibraries } = vi.hoisted(() => ({
  getShowSeasons: vi.fn(),
  getSeasonEpisodes: vi.fn(),
  listLibraries: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
    },
  };
});

import ShowDetailScreen from "./ShowDetailScreen";

const MEMBER = { id: "u2", username: "ada", role: "member" };

// A probe over the shared Queue so tests assert the resume-point actions built the
// Queue with the right head Episode and start offset (playback begins in the
// persistent bar — no navigation).
function QueueProbe() {
  const q = useQueue();
  return (
    <>
      <div data-testid="queue-current-id">{q.current?.title.id ?? ""}</div>
      <div data-testid="queue-current-resume">
        {q.current ? String(q.current.title.resumePositionMs) : ""}
      </div>
    </>
  );
}

function renderDetail() {
  return renderWithAuth(
    <Routes>
      <Route
        path="/shows/:showId"
        element={
          <>
            <ShowDetailScreen />
            <QueueProbe />
          </>
        }
      />
    </Routes>,
    { initialEntries: ["/shows/sh1"], user: MEMBER },
  );
}

const showSummary = {
  id: "sh1",
  libraryId: "lib-tv",
  kind: "show",
  title: "The Bear",
  year: 2022,
  needsReview: false,
  unwatchedEpisodeCount: 0,
  overview: "A chef returns home to run a chaotic sandwich shop.",
  genres: [],
  cast: [],
};

const season = { id: "s1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 2 };

// The resume-point Episode as it comes back on GET /seasons/{id}/episodes — the
// head buildShowQueue slices from. Its own stored resume is overridden by the
// Continue/Restart head offset, so the value here is deliberately distinct (99000)
// to prove the override wins.
const headEpisode = {
  id: "ep1",
  kind: "episode",
  title: "System",
  seasonNumber: 1,
  episodeNumber: 1,
  episodeLabel: "",
  needsReview: false,
  resumePositionMs: 99000,
  watched: false,
  overview: "Carmy takes over.",
};

function seasonsResponse(
  resumePoint: {
    mode: ResumePointMode;
    resumePositionMs: number;
    durationMs?: number;
  } | null,
  unwatchedEpisodeCount = 0,
): ShowSeasons {
  return {
    show: { ...showSummary, unwatchedEpisodeCount },
    seasons: [season],
    resumePoint: resumePoint
      ? {
          id: "ep1",
          kind: "episode",
          seasonId: "s1",
          seasonNumber: 1,
          episodeNumber: 1,
          episodeLabel: "",
          title: "System",
          overview: "Carmy takes over.",
          resumePositionMs: resumePoint.resumePositionMs,
          durationMs: resumePoint.durationMs ?? 0,
          mode: resumePoint.mode,
        }
      : null,
  };
}

const episodesResponse: SeasonEpisodes = { season, episodes: [headEpisode] };

beforeEach(() => {
  getShowSeasons.mockReset();
  getSeasonEpisodes.mockReset();
  listLibraries.mockReset();
  listLibraries.mockResolvedValue([{ id: "lib-tv", name: "Shows", kind: "tv", rootFolders: [] }]);
  getSeasonEpisodes.mockResolvedValue(episodesResponse);
});

describe("ShowDetailScreen — resume-point modes", () => {
  it("not started: shows the Show description + Play (no resume-point block)", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse(null, 3));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    expect(screen.queryByTestId("resume-point")).toBeNull();
    expect(screen.getByTestId("show-overview")).toHaveTextContent("sandwich shop");
    // The whole-series Play (series from the first Episode) is offered.
    expect(screen.getByTestId("play-button")).toBeEnabled();
    expect(screen.queryByTestId("continue-button")).toBeNull();
  });

  it("fully watched: shows the Show description with NO Play", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse(null, 0));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    expect(screen.queryByTestId("resume-point")).toBeNull();
    expect(screen.getByTestId("show-overview")).toBeInTheDocument();
    // A finished series drops the Play entirely (restarting it is not a flow).
    expect(screen.queryByTestId("play-button")).toBeNull();
    expect(screen.queryByTestId("resume-play-button")).toBeNull();
  });

  it("in-progress: shows S/E · title · synopsis + Continue + Restart", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse({ mode: "inProgress", resumePositionMs: 42000 }));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    const block = screen.getByTestId("resume-point");
    expect(block).toHaveAttribute("data-mode", "inProgress");
    expect(screen.getByTestId("resume-point-code")).toHaveTextContent("S01E01");
    expect(screen.getByTestId("resume-point-title")).toHaveTextContent("System");
    expect(screen.getByTestId("resume-point-synopsis")).toHaveTextContent("Carmy takes over");
    expect(screen.getByTestId("continue-button")).toBeInTheDocument();
    expect(screen.getByTestId("restart-button")).toBeInTheDocument();
    // The whole-series Play and the Show description give way to the block.
    expect(screen.queryByTestId("play-button")).toBeNull();
    expect(screen.queryByTestId("show-overview")).toBeNull();
  });

  it("in-progress with a known duration: renders the Continue progress bar + minutes-remaining", async () => {
    // 2 min into a 10 min Episode → 20% played, 8 minutes left.
    getShowSeasons.mockResolvedValue(
      seasonsResponse({ mode: "inProgress", resumePositionMs: 120000, durationMs: 600000 }),
    );
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    expect(screen.getByTestId("resume-point-progress")).toBeInTheDocument();
    expect(screen.getByTestId("resume-progress-fill")).toHaveStyle({ width: "20%" });
    expect(screen.getByTestId("resume-progress-remaining")).toHaveTextContent("8 min left");
    // The bar sits above the Continue/Restart buttons, which are still present.
    expect(screen.getByTestId("continue-button")).toBeInTheDocument();
  });

  it("no Continue progress bar when the duration is unknown or the mode is next", async () => {
    // Unknown duration (0) → no bar even though it's resumable.
    getShowSeasons.mockResolvedValue(
      seasonsResponse({ mode: "inProgress", resumePositionMs: 42000, durationMs: 0 }),
    );
    const { unmount } = renderDetail();
    await waitFor(() => expect(screen.getByTestId("resume-point")).toBeInTheDocument());
    expect(screen.queryByTestId("resume-point-progress")).toBeNull();
    unmount();

    // A fresh next Episode (no resume) never shows the bar.
    getShowSeasons.mockResolvedValue(
      seasonsResponse({ mode: "next", resumePositionMs: 0, durationMs: 600000 }),
    );
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("resume-point")).toBeInTheDocument());
    expect(screen.queryByTestId("resume-point-progress")).toBeNull();
  });

  it("in-progress Continue: builds the show-from-here Queue with the head resumed at resumePositionMs", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse({ mode: "inProgress", resumePositionMs: 42000 }));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    await userEvent.click(screen.getByTestId("continue-button"));

    // The resume-point Episode is the now-playing head, resumed at 42000 (Continue),
    // NOT the Episode's own stored resume (99000) — the override wins.
    await waitFor(() => expect(screen.getByTestId("queue-current-id")).toHaveTextContent("ep1"));
    expect(screen.getByTestId("queue-current-resume")).toHaveTextContent("42000");
    // Playback started in the bar; we stayed on the detail screen (no navigation).
    expect(screen.getByTestId("show-detail")).toBeInTheDocument();
  });

  it("in-progress Restart: builds the same Queue with the head at 0", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse({ mode: "inProgress", resumePositionMs: 42000 }));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    await userEvent.click(screen.getByTestId("restart-button"));

    await waitFor(() => expect(screen.getByTestId("queue-current-id")).toHaveTextContent("ep1"));
    // Restart starts the head at 0 (re-watch from the top of that Episode).
    expect(screen.getByTestId("queue-current-resume")).toHaveTextContent("0");
  });

  it("next: shows the block with a single Play that starts the head at 0", async () => {
    getShowSeasons.mockResolvedValue(seasonsResponse({ mode: "next", resumePositionMs: 0 }));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    const block = screen.getByTestId("resume-point");
    expect(block).toHaveAttribute("data-mode", "next");
    expect(screen.getByTestId("resume-point-code")).toHaveTextContent("S01E01");
    // A fresh next Episode: a single Play, no Continue/Restart.
    expect(screen.queryByTestId("continue-button")).toBeNull();
    expect(screen.queryByTestId("restart-button")).toBeNull();
    const play = screen.getByTestId("resume-play-button");

    await userEvent.click(play);

    await waitFor(() => expect(screen.getByTestId("queue-current-id")).toHaveTextContent("ep1"));
    expect(screen.getByTestId("queue-current-resume")).toHaveTextContent("0");
  });
});
