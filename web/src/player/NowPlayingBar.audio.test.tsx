import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { AudioStream, PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";

// Audio menu (audio-streams/04, ADR-0022): the Now Playing bar lists a File's
// selectable audio Streams, marks the RESOLVED one active, orders by the preferred
// audio language, and switches — in-band on the HLS tiers (instant, no restart) or,
// on direct play, via a fresh negotiation carrying audioStreamId (the one escalating
// switch, mirroring image-subtitle burn-in). The menu is hidden for a single-audio
// File. We drive the bar through the same faked-apiClient seam as the captions test.

const { getTitle, startPlayback, reportProgress, endSession, searchSubtitles, fetchSubtitle } =
  vi.hoisted(() => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    searchSubtitles: vi.fn(),
    fetchSubtitle: vi.fn(),
  }));

const { attachHls, setAudioTrack, setTextTrack } = vi.hoisted(() => ({
  attachHls: vi.fn(),
  setAudioTrack: vi.fn(),
  setTextTrack: vi.fn(),
}));
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
      searchSubtitles: (...a: unknown[]) => searchSubtitles(...a),
      fetchSubtitle: (...a: unknown[]) => fetchSubtitle(...a),
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

function audio(p: Partial<AudioStream> & { id: string; index: number }): AudioStream {
  return {
    id: p.id,
    index: p.index,
    codec: p.codec ?? "aac",
    language: p.language,
    channels: p.channels ?? 6,
    layout: p.layout ?? "5.1",
    isDefault: p.isDefault ?? false,
    commentary: p.commentary,
    label: p.label ?? p.id,
  };
}

// A multi-audio File: English 5.1 (default, index 1) + Japanese 5.1 (index 2). The
// decision's resolved audioStream defaults to the English one (index 1).
const EN = audio({ id: "en", index: 1, language: "en", label: "English 5.1", isDefault: true });
const JA = audio({ id: "ja", index: 2, language: "ja", label: "Japanese 5.1" });

function decisionWithAudio(
  streams: AudioStream[],
  over: Partial<PlaybackDecision> = {},
): PlaybackDecision {
  return {
    sessionId: "sess-1",
    tier: "directPlay",
    streamUrl: "/api/v1/sessions/sess-1/stream",
    edition: { id: "e1", name: "1080p" },
    videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
    videoStreams: [],
    audioStream: { index: 1, codec: "aac", channels: 6, language: "en" },
    audioStreams: streams,
    subtitles: [],
    estimatedBitrate: 6_000_000,
    ...over,
  };
}

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "movie", title: "Multi Movie" } as Partial<TitleDetail>);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  searchSubtitles.mockReset().mockResolvedValue([]);
  fetchSubtitle.mockReset();
  setAudioTrack.mockReset();
  setTextTrack.mockReset();
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack, setAudioTrack });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("NowPlayingBar — Audio menu", () => {
  it("lists every audio Stream and marks the server-resolved one active", async () => {
    startPlayback.mockResolvedValue(decisionWithAudio([EN, JA]));
    seedAndRender([entryFromTitle(movieSummary("t1", "Multi Movie"))]);

    const btn = await screen.findByTestId("now-playing-audio");
    expect(btn).toHaveAttribute("aria-label", "Audio");
    await act(async () => {
      fireEvent.click(btn);
    });
    const menu = screen.getByTestId("now-playing-audio-menu");
    expect(menu).toHaveTextContent("English 5.1");
    expect(menu).toHaveTextContent("Japanese 5.1");
    // The resolved (English) Stream is checked; the other is not.
    const en = menu.querySelector('[data-audio-id="en"]') as HTMLElement;
    const ja = menu.querySelector('[data-audio-id="ja"]') as HTMLElement;
    expect(en).toHaveAttribute("aria-checked", "true");
    expect(ja).toHaveAttribute("aria-checked", "false");
    // CONTEXT.md: the surface is "Audio", never the coined "Audio track".
    expect(menu.textContent).not.toContain("Audio track");
  });

  it("is hidden for a single-audio File (nothing to pick)", async () => {
    startPlayback.mockResolvedValue(decisionWithAudio([EN]));
    seedAndRender([entryFromTitle(movieSummary("t1", "Mono Movie"))]);

    // The captions button always renders once ready; the audio button must not.
    await screen.findByTestId("player-video");
    expect(screen.queryByTestId("now-playing-audio")).toBeNull();
  });

  it("switches audio IN-BAND on the HLS tier (setAudioTrack, no re-negotiation)", async () => {
    startPlayback.mockResolvedValue(
      decisionWithAudio([EN, JA], {
        tier: "transcode",
        streamUrl: "/api/v1/sessions/sess-1/hls/master.m3u8",
      }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Multi Movie"))]);

    // On attach the resolved English rendition (server-order index 0) is applied.
    await waitFor(() => expect(setAudioTrack).toHaveBeenCalledWith(0));

    const btn = await screen.findByTestId("now-playing-audio");
    await act(async () => {
      fireEvent.click(btn);
    });
    const menu = screen.getByTestId("now-playing-audio-menu");
    await act(async () => {
      fireEvent.click(menu.querySelector('[data-audio-id="ja"]') as HTMLElement);
    });

    // Japanese is rendition index 1 (server order) — switched in-band, instantly.
    await waitFor(() => expect(setAudioTrack).toHaveBeenLastCalledWith(1));
    // No second negotiation — in-band switching never touches the negotiation path.
    expect(startPlayback).toHaveBeenCalledTimes(1);
    expect(endSession).not.toHaveBeenCalled();
    // But the pick IS reported through the progress surface so the server remembers
    // it (audio-streams/05): a watch-state write carrying audioStreamId, no restart.
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith(
        "sess-1",
        expect.objectContaining({ audioStreamId: "ja" }),
      ),
    );
    // Re-open the menu (picking closes it) — Japanese is now marked active.
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(
      screen.getByTestId("now-playing-audio-menu").querySelector('[data-audio-id="ja"]'),
    ).toHaveAttribute("aria-checked", "true");
  });

  it("escalates a direct-play non-default pick via a fresh negotiation (audioStreamId)", async () => {
    startPlayback
      .mockResolvedValueOnce(decisionWithAudio([EN, JA]))
      .mockResolvedValueOnce(
        decisionWithAudio([EN, JA], {
          sessionId: "sess-2",
          tier: "directStream",
          streamUrl: "/api/v1/sessions/sess-2/hls/master.m3u8",
          audioStream: { index: 2, codec: "aac", channels: 6, language: "ja" },
        }),
      );
    seedAndRender([entryFromTitle(movieSummary("t1", "Multi Movie"))]);

    const btn = await screen.findByTestId("now-playing-audio");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-audio-menu").querySelector('[data-audio-id="ja"]') as HTMLElement,
      );
    });

    // The second negotiation carried audioStreamId; the mount one did not. The prior
    // session was ended cleanly (the one restart).
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    const firstOpts = startPlayback.mock.calls[0][1] as { audioStreamId?: string };
    const secondOpts = startPlayback.mock.calls[1][1] as { audioStreamId?: string };
    expect(firstOpts.audioStreamId).toBeUndefined();
    expect(secondOpts.audioStreamId).toBe("ja");
    expect(endSession).toHaveBeenCalledWith("sess-1");

    // After the escalation lands (demuxed HLS), re-open the menu — Japanese is now
    // the resolved active track (re-initialized from the re-negotiated decision).
    const btn2 = await screen.findByTestId("now-playing-audio");
    await act(async () => {
      fireEvent.click(btn2);
    });
    await waitFor(() =>
      expect(
        screen.getByTestId("now-playing-audio-menu").querySelector('[data-audio-id="ja"]'),
      ).toHaveAttribute("aria-checked", "true"),
    );
  });

  it("shows the busy/retry UX when a direct-play escalation hits SERVER_BUSY", async () => {
    const { ApiError } = await import("../api/errors");
    startPlayback
      .mockResolvedValueOnce(decisionWithAudio([EN, JA]))
      // The escalation is rejected at the transcode cap; no suggested bitrate → no
      // auto-retry, so the manual busy UX is shown and playback options stay open.
      .mockRejectedValueOnce(new ApiError(503, "SERVER_BUSY", "server busy", { retryable: true }));
    seedAndRender([entryFromTitle(movieSummary("t1", "Multi Movie"))]);

    const btn = await screen.findByTestId("now-playing-audio");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-audio-menu").querySelector('[data-audio-id="ja"]') as HTMLElement,
      );
    });

    expect(await screen.findByTestId("player-busy")).toBeInTheDocument();
    expect(screen.getByTestId("player-busy-retry")).toBeInTheDocument();
  });
});
