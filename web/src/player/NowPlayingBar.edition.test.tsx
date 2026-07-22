import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { savePreference } from "./playbackPreference";

// Edition REPLAY through the persistent player (appletv-web-parity §1/§2): a
// committed Playback preference is resolved by the bar (from the store + the entry's
// detail) and reaches `startPlayback` as `editionId`. Auto / no preference omits it
// (unchanged behaviour), and a SHOW preference replays across Episodes BY NAME —
// each Episode's own Edition id for that name.

const { getTitle, startPlayback, reportProgress, endSession } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  startPlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
}));

const { attachHls } = vi.hoisted(() => ({ attachHls: vi.fn() }));
vi.mock("./hls", () => ({ attachHls: (...a: unknown[]) => attachHls(...a) }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      startPlayback: (...a: unknown[]) => startPlayback(...a),
      reportProgress: (...a: unknown[]) => reportProgress(...a),
      endSession: (...a: unknown[]) => endSession(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";

function movieSummary(id: string, name: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

/** A Movie detail carrying two Editions the preference can resolve against. */
function movieDetailWithEditions(id: string, name: string): Partial<TitleDetail> {
  return {
    id,
    kind: "movie",
    title: name,
    editions: [
      { id: "ed-tc", name: "Theatrical Cut", files: [] },
      { id: "ed-fc", name: "Final Cut", files: [] },
    ],
  };
}

/** Episode detail whose "Director's Cut" Edition carries a DIFFERENT id per episode
 * — the drift the by-name Show preference survives. */
function episodeDetail(id: string, dcId: string): Partial<TitleDetail> {
  return {
    id,
    kind: "episode",
    title: `Episode ${id}`,
    episode: { showId: "sh1", showTitle: "The Bear", seasonId: "s1", seasonNumber: 1, episodeNumber: 1 },
    editions: [
      { id: `${id}-std`, name: "Broadcast", files: [] },
      { id: dcId, name: "Director's Cut", files: [] },
    ],
  };
}

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed-fc", name: "Final Cut" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  // renderWithAuth seeds the authenticated user (its token/user), which localStorage
  // .clear() would wipe — but renderWithAuth re-seeds it on render, so clear first.
  getTitle.mockReset().mockResolvedValue(movieDetailWithEditions("t1", "Dune"));
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("NowPlayingBar — Edition preference replay", () => {
  it("no stored preference → startPlayback omits editionId (unchanged behaviour)", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    expect(startPlayback).toHaveBeenCalledWith(
      "t1",
      expect.objectContaining({ editionId: undefined }),
      expect.anything(),
    );
  });

  it("a committed Movie preference reaches startPlayback as the resolved editionId", async () => {
    savePreference(window.localStorage, "u1", { kind: "title", id: "t1" }, { editionName: "Final Cut", qualityCap: null });
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ editionId: "ed-fc" }),
        expect.anything(),
      ),
    );
  });

  it("a Show preference replays across Episodes by NAME (each Episode's own id)", async () => {
    savePreference(window.localStorage, "u1", { kind: "show", id: "sh1" }, { editionName: "Director's Cut", qualityCap: null });
    // Episode 7's "Director's Cut" is id ep7-dc; the entry carries showId (as a Show
    // walk would thread it), so the Show preference keys synchronously.
    getTitle.mockResolvedValue(episodeDetail("ep7", "ep7-dc"));
    const entry: QueueEntry = { ...entryFromTitle({ ...movieSummary("ep7", "System"), kind: "episode" }), showId: "sh1" };
    seedAndRender([entry]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "ep7",
        expect.objectContaining({ editionId: "ep7-dc" }),
        expect.anything(),
      ),
    );
  });
});
