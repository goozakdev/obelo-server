import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type {
  AlbumTracks,
  PlaybackDecision,
  SeasonEpisodes,
  ShowSeasons,
} from "../api/types";
import AlbumDetailScreen from "../music/AlbumDetailScreen";
import ShowDetailScreen from "./ShowDetailScreen";

// The album & show PLAY AFFORDANCES end to end (queue/02, now-playing-bar/01):
// clicking Play on a Track / Episode row builds the play-context Queue
// (buildAlbumQueue / buildShowQueue) into the shared useQueue store; playback
// begins in the persistent Now Playing bar (no navigation) and the bar walks the
// Queue. We assert which Title the bar NEGOTIATES (startPlayback) as the pointer
// advances — the album-from-here slice and the show-from-here cross-season walk.
// The browse screen and the bar share one QueueProvider (renderWithAuth), exactly
// as the app mounts it (the bar OUTSIDE <Routes>).

const {
  getAlbumTracks,
  getShowSeasons,
  getSeasonEpisodes,
  getTitle,
  startPlayback,
  reportProgress,
  endSession,
} = vi.hoisted(() => ({
  getAlbumTracks: vi.fn(),
  getShowSeasons: vi.fn(),
  getSeasonEpisodes: vi.fn(),
  getTitle: vi.fn(),
  startPlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
}));

const { attachHls } = vi.hoisted(() => ({ attachHls: vi.fn() }));
vi.mock("../player/hls", () => ({
  attachHls: (...a: unknown[]) => attachHls(...a),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getAlbumTracks: (...a: unknown[]) => getAlbumTracks(...a),
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
      startPlayback: (...a: unknown[]) => startPlayback(...a),
      reportProgress: (...a: unknown[]) => reportProgress(...a),
      endSession: (...a: unknown[]) => endSession(...a),
    },
  };
});

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "e1", name: "1080p" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

function albumTracks(): AlbumTracks {
  return {
    album: {
      id: "al1",
      artistId: "ar1",
      title: "OK Computer",
      year: 1997,
      hasArtwork: false,
      trackCount: 3,
      genres: [],
    },
    tracks: [1, 2, 3].map((n) => ({
      id: `tr${n}`,
      kind: "track",
      title: `Track ${n}`,
      discNumber: 1,
      trackNumber: n,
      needsReview: false,
      resumePositionMs: 0,
      watched: false,
      overview: "",
    })),
  };
}

const showSeasons: ShowSeasons = {
  show: {
    id: "sh1",
    kind: "show",
    title: "The Bear",
    year: 2022,
    needsReview: false,
    // A NOT-started Show (no resume point, unwatched Episodes remaining) — the
    // toolbar's whole-series Play is offered (issue 02, ADR-0028).
    unwatchedEpisodeCount: 3,
    overview: "",
    genres: [],
  },
  seasons: [
    { id: "s1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 2 },
    { id: "s2", showId: "sh1", seasonNumber: 2, specials: false, episodeCount: 1 },
  ],
  resumePoint: null,
};

function seasonEpisodes(seasonId: string): SeasonEpisodes {
  const byId: Record<string, { id: string; n: number; sn: number }[]> = {
    s1: [
      { id: "e1", n: 1, sn: 1 },
      { id: "e2", n: 2, sn: 1 },
    ],
    s2: [{ id: "e3", n: 1, sn: 2 }],
  };
  const season = showSeasons.seasons.find((s) => s.id === seasonId)!;
  return {
    season,
    episodes: byId[seasonId].map((e) => ({
      id: e.id,
      kind: "episode",
      title: `Episode ${e.id}`,
      seasonNumber: e.sn,
      episodeNumber: e.n,
      episodeLabel: "",
      needsReview: false,
      resumePositionMs: 0,
      watched: false,
      overview: "",
    })),
  };
}

/** The Titles the player negotiated, in order. */
function negotiatedTitleIds() {
  return startPlayback.mock.calls.map((c) => c[0]);
}

beforeEach(() => {
  window.sessionStorage.clear();
  getAlbumTracks.mockReset().mockResolvedValue(albumTracks());
  getShowSeasons.mockReset().mockResolvedValue(showSeasons);
  getSeasonEpisodes
    .mockReset()
    .mockImplementation((id: string) => Promise.resolve(seasonEpisodes(id)));
  // The bar fetches getTitle per current entry for its now-playing label; a lean
  // stub keeps that path happy (the label itself isn't asserted here).
  getTitle.mockReset().mockResolvedValue({ id: "x", kind: "movie", title: "x" });
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress
    .mockReset()
    .mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

function renderAlbum() {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
        <Route path="/music/tracks/:titleId" element={<div data-testid="detail-route" />} />
      </Routes>
      <NowPlayingBar />
    </>,
    { initialEntries: ["/music/albums/al1"] },
  );
}

function renderShow() {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
        <Route path="/titles/:titleId" element={<div data-testid="detail-route" />} />
      </Routes>
      <NowPlayingBar />
    </>,
    { initialEntries: ["/shows/sh1"] },
  );
}

// NowPlayingBar is imported after the mocks above are registered.
import NowPlayingBar from "../player/NowPlayingBar";

describe("Album Play affordance → album-from-here Queue", () => {
  it("plays the chosen Track and advances through the rest of the album", async () => {
    renderAlbum();

    // Click Play on Track 2 → the Queue is Tracks 2–3 (album-from-here).
    await screen.findByTestId("track-list");
    const play2 = screen
      .getAllByTestId("track-play")
      .find((b) => b.getAttribute("data-track-id") === "tr2")!;
    fireEvent.click(play2);

    // The player negotiates tr2 (the now-playing entry) — not tr1.
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("tr2", expect.anything(), expect.anything()),
    );
    expect(negotiatedTitleIds()).not.toContain("tr1");

    // Ending advances to tr3 (the album continues from here).
    fireEvent.ended(video);
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("tr3", expect.anything(), expect.anything()),
    );
  });
});

describe("Show Play affordance → show-from-here Queue (cross-season)", () => {
  it("plays the chosen Episode and advances across the season boundary", async () => {
    renderShow();

    // Wait for season 1's episodes to load, then Play e1.
    await waitFor(() =>
      expect(
        screen.getAllByTestId("episode-play").some(
          (b) => b.getAttribute("data-episode-id") === "e1",
        ),
      ).toBe(true),
    );
    const playE1 = screen
      .getAllByTestId("episode-play")
      .find((b) => b.getAttribute("data-episode-id") === "e1")!;
    fireEvent.click(playE1);

    // Now-playing is e1 (resolved after one fetch — no need to await the walk).
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e1", expect.anything(), expect.anything()),
    );

    // Advance through the rest of season 1 (e2)…
    fireEvent.ended(video);
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e2", expect.anything(), expect.anything()),
    );

    // …then across the boundary into season 2 (e3), via the lazily-appended tail.
    fireEvent.ended(screen.getByTestId("player-video"));
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e3", expect.anything(), expect.anything()),
    );
    expect(negotiatedTitleIds()).toEqual(["e1", "e2", "e3"]);
  });
});

// The Show detail's TOOLBAR (matching the Movie detail): a primary Play that
// starts the series from the beginning, plus a ⋯ overflow with the whole-series
// Queue actions. Both drive the same shared Queue → Now Playing bar the episode
// rows do; we assert the Titles the bar negotiates as the pointer advances.
describe("Show detail toolbar → whole-series Queue", () => {
  it("Play starts the series from the beginning and walks across seasons", async () => {
    renderShow();

    // The toolbar's Play is enabled once the Seasons load (Season 1 has Episodes).
    const play = await screen.findByTestId("play-button");
    await waitFor(() => expect(play).toBeEnabled());
    fireEvent.click(play);

    // Now-playing is e1 — the very first Episode — resolved after one fetch.
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e1", expect.anything(), expect.anything()),
    );

    // Advancing walks e1 → e2 (rest of Season 1) → e3 (across into Season 2).
    fireEvent.ended(video);
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e2", expect.anything(), expect.anything()),
    );
    fireEvent.ended(screen.getByTestId("player-video"));
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e3", expect.anything(), expect.anything()),
    );
    expect(negotiatedTitleIds()).toEqual(["e1", "e2", "e3"]);
  });

  it("the overflow Add to Queue enqueues the whole series in order", async () => {
    renderShow();

    // Open the ⋯ overflow, then append the whole series. Into an empty Queue the
    // first appended Episode becomes now-playing, so the bar negotiates e1.
    fireEvent.click(await screen.findByTestId("overflow-menu-button"));
    fireEvent.click(await screen.findByTestId("add-to-queue-button"));

    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e1", expect.anything(), expect.anything()),
    );
    fireEvent.ended(video);
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e2", expect.anything(), expect.anything()),
    );
    fireEvent.ended(screen.getByTestId("player-video"));
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("e3", expect.anything(), expect.anything()),
    );
    expect(negotiatedTitleIds()).toEqual(["e1", "e2", "e3"]);
  });
});
