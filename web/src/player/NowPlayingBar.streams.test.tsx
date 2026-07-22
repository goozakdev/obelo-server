import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { savePreference } from "./playbackPreference";

// Pre-play Audio / Video Stream picks (appletv-web-parity §1, issue 04) reaching the
// persistent player: the Playback Options sheet's Play seeds them onto the TRANSIENT
// Queue entry, and the bar hands them to `startPlayback` as `audioStreamId` /
// `videoStreamId` — WITHOUT ever going through the persisted preference (client
// ADR-0011). A plain entry (no picks) omits them, exactly as before.

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

function movieDetail(id: string, name: string): Partial<TitleDetail> {
  return { id, kind: "movie", title: name, editions: [{ id: "ed1", name: "Default", files: [] }] };
}

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed1", name: "Default" },
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
  getTitle.mockReset().mockResolvedValue(movieDetail("t1", "Dune"));
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

describe("NowPlayingBar — pre-play Audio / Video Stream picks", () => {
  it("no picks on the entry → startPlayback omits both ids (unchanged behaviour)", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    expect(startPlayback).toHaveBeenCalledWith(
      "t1",
      expect.objectContaining({ audioStreamId: undefined, videoStreamId: undefined }),
      expect.anything(),
    );
  });

  it("the entry's picks reach startPlayback as audioStreamId / videoStreamId", async () => {
    const entry: QueueEntry = {
      ...entryFromTitle(movieSummary("t1", "Dune")),
      audioStreamId: "a-com",
      videoStreamId: "v-bw",
    };
    seedAndRender([entry]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ audioStreamId: "a-com", videoStreamId: "v-bw" }),
        expect.anything(),
      ),
    );
  });

  it("picks seed WITHOUT any stored preference — they never enter the pref store", async () => {
    // No savePreference call at all; the entry alone carries the picks. The pref store
    // stays empty (client ADR-0011), yet the ids still reach negotiation.
    const entry: QueueEntry = {
      ...entryFromTitle(movieSummary("t1", "Dune")),
      audioStreamId: "a-fr",
    };
    seedAndRender([entry]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ audioStreamId: "a-fr" }),
        expect.anything(),
      ),
    );
    expect(window.localStorage.getItem("juicebox.playback-pref.u1.title.t1")).toBeNull();
  });

  it("seeds alongside a stored Edition preference (both reach startPlayback)", async () => {
    savePreference(window.localStorage, "u1", { kind: "title", id: "t1" }, { editionName: "Default", qualityCap: null });
    const entry: QueueEntry = {
      ...entryFromTitle(movieSummary("t1", "Dune")),
      audioStreamId: "a-en",
    };
    seedAndRender([entry]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ editionId: "ed1", audioStreamId: "a-en" }),
        expect.anything(),
      ),
    );
  });
});
