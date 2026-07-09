import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";

// Captions menu (subtitles/02): the NOW PLAYING bar lists a Title's deliverable
// TEXT subtitle tracks, orders them by the preferred language, auto-displays a
// forced track (subtitles otherwise default off), and switches instantly by
// toggling the native <track> mode — no reload, no server round-trip. We drive the
// bar through the same faked-apiClient seam as NowPlayingBar.test.tsx.

const { getTitle, startPlayback, reportProgress, endSession, searchSubtitles, fetchSubtitle } =
  vi.hoisted(() => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    searchSubtitles: vi.fn(),
    fetchSubtitle: vi.fn(),
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

function decisionWithSubs(subs: PlaybackDecision["subtitles"]): PlaybackDecision {
  return {
    sessionId: "sess-1",
    tier: "directPlay",
    streamUrl: "/api/v1/sessions/sess-1/stream",
    edition: { id: "e1", name: "1080p" },
    videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
    videoStreams: [],
    audioStream: { index: 1, codec: "aac", channels: 2 },
    audioStreams: [],
    subtitles: subs,
    estimatedBitrate: 6_000_000,
  };
}

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "movie", title: "Sub Movie" } as Partial<TitleDetail>);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  searchSubtitles.mockReset().mockResolvedValue([]);
  fetchSubtitle.mockReset();
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("NowPlayingBar — captions menu", () => {
  it("lists Off + text tracks + image tracks (image marked burn-in), slice 04", async () => {
    startPlayback.mockResolvedValue(
      decisionWithSubs([
        { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/api/v1/titles/t1/subtitles/en.vtt" },
        { id: "fr", source: "sidecar", kind: "text", language: "fr", forced: false, label: "French", url: "/api/v1/titles/t1/subtitles/fr.vtt" },
        { id: "de", source: "sidecar", kind: "image", language: "de", forced: false, label: "German", url: undefined },
      ]),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);

    const btn = await screen.findByTestId("now-playing-captions");
    await act(async () => {
      fireEvent.click(btn);
    });
    const menu = screen.getByTestId("now-playing-captions-menu");
    expect(menu).toBeInTheDocument();
    expect(screen.getByTestId("captions-off")).toBeInTheDocument();
    // Both text tracks are listed; the image track is now listed too (slice 04),
    // marked as a burn-in.
    expect(menu).toHaveTextContent("English");
    expect(menu).toHaveTextContent("French");
    const image = menu.querySelector('[data-sub-id="de"]') as HTMLElement;
    expect(image).toBeInTheDocument();
    expect(image).toHaveAttribute("data-sub-kind", "image");
    expect(image.textContent).toContain("burn in");

    // <track> elements exist for the two TEXT tracks only — an image track is never
    // delivered out-of-band (it burns into the picture).
    const video = screen.getByTestId("player-video") as HTMLVideoElement;
    const trackEls = video.querySelectorAll("track");
    expect(trackEls.length).toBe(2);
  });

  it("burns in an image track via a fresh transcode negotiation (burnSubtitleId)", async () => {
    // First negotiation direct-plays; selecting the image track re-negotiates with
    // burnSubtitleId and the server returns a burned-in transcode.
    startPlayback
      .mockResolvedValueOnce(
        decisionWithSubs([
          { id: "de", source: "sidecar", kind: "image", language: "de", forced: false, label: "German", url: undefined },
        ]),
      )
      .mockResolvedValueOnce({
        ...decisionWithSubs([
          { id: "de", source: "sidecar", kind: "image", language: "de", forced: false, label: "German", url: undefined },
        ]),
        sessionId: "sess-2",
        tier: "transcode",
        streamUrl: "/api/v1/sessions/sess-2/hls/index.m3u8",
      });
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);

    const btn = await screen.findByTestId("now-playing-captions");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByText(/German \(burn in\)/));
    });

    // The second startPlayback carried burnSubtitleId; the first (mount) did not.
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    const secondOpts = startPlayback.mock.calls[1][1] as { burnSubtitleId?: string };
    expect(secondOpts.burnSubtitleId).toBe("de");
    // The prior session was ended cleanly (restart on switch).
    expect(endSession).toHaveBeenCalledWith("sess-1");
    // The captions button reflects the active burn.
    await waitFor(() => expect(btn).toHaveAttribute("aria-pressed", "true"));
  });

  it("shows the busy/retry UX when burning in an image sub hits SERVER_BUSY", async () => {
    const { ApiError } = await import("../api/errors");
    startPlayback
      .mockResolvedValueOnce(
        decisionWithSubs([
          { id: "de", source: "sidecar", kind: "image", language: "de", forced: false, label: "German", url: undefined },
        ]),
      )
      // The burn re-negotiation is rejected at the transcode cap; no suggested
      // bitrate → no auto-retry, so the manual busy UX is shown.
      .mockRejectedValueOnce(
        new ApiError(503, "SERVER_BUSY", "server busy", { retryable: true }),
      );
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);

    const btn = await screen.findByTestId("now-playing-captions");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByText(/German \(burn in\)/));
    });

    // The busy state + manual retry affordance appear.
    expect(await screen.findByTestId("player-busy")).toBeInTheDocument();
    expect(screen.getByTestId("player-busy-retry")).toBeInTheDocument();
  });

  it("defaults OFF when no track is forced", async () => {
    startPlayback.mockResolvedValue(
      decisionWithSubs([
        { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/api/v1/titles/t1/subtitles/en.vtt" },
      ]),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);
    const btn = await screen.findByTestId("now-playing-captions");
    // Not active (no forced track → off).
    expect(btn).toHaveAttribute("aria-pressed", "false");
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(screen.getByTestId("captions-off")).toHaveAttribute("aria-checked", "true");
  });

  it("auto-displays a forced text track without the viewer enabling captions", async () => {
    startPlayback.mockResolvedValue(
      decisionWithSubs([
        { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/api/v1/titles/t1/subtitles/en.vtt" },
        { id: "fr", source: "embedded", kind: "text", language: "fr", forced: true, label: "French (Forced)", url: "/api/v1/titles/t1/subtitles/fr.vtt" },
      ]),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);
    const btn = await screen.findByTestId("now-playing-captions");
    // The forced track is selected on load — the button reads active.
    expect(btn).toHaveAttribute("aria-pressed", "true");
    await act(async () => {
      fireEvent.click(btn);
    });
    const fr = screen.getByText("✓ French (Forced)");
    expect(fr).toHaveAttribute("aria-checked", "true");
  });

  it("switches tracks instantly and can turn captions off", async () => {
    startPlayback.mockResolvedValue(
      decisionWithSubs([
        { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/api/v1/titles/t1/subtitles/en.vtt" },
        { id: "fr", source: "sidecar", kind: "text", language: "fr", forced: false, label: "French", url: "/api/v1/titles/t1/subtitles/fr.vtt" },
      ]),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);
    const btn = await screen.findByTestId("now-playing-captions");

    // Select English.
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByText("English"));
    });
    // The chosen <track> is marked default and the button is active.
    const video = screen.getByTestId("player-video") as HTMLVideoElement;
    const enTrack = video.querySelector('track[data-sub-id="en"]') as HTMLTrackElement;
    expect(enTrack.default).toBe(true);
    expect(btn).toHaveAttribute("aria-pressed", "true");

    // Now turn captions off — the button goes inactive.
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByTestId("captions-off"));
    });
    expect(btn).toHaveAttribute("aria-pressed", "false");
  });

  it("on the HLS tiers delivers subtitles IN-BAND: no <track>, drives the rendition (slice 03)", async () => {
    // The forced French track auto-displays; both text tracks are listed. Delivery
    // is in-band via the master playlist's SUBTITLES rendition, so NO out-of-band
    // <track> elements are rendered and selection drives the hls.js attachment's
    // setTextTrack (by deliverable-text-track index in server order).
    const setTextTrack = vi.fn();
    attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack });
    startPlayback.mockResolvedValue({
      ...decisionWithSubs([
        { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/api/v1/titles/t1/subtitles/en.vtt" },
        { id: "fr", source: "embedded", kind: "text", language: "fr", forced: true, label: "French (Forced)", url: "/api/v1/titles/t1/subtitles/fr.vtt" },
      ]),
      tier: "transcode",
      streamUrl: "/api/v1/sessions/sess-1/hls/master.m3u8",
    });
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);

    const btn = await screen.findByTestId("now-playing-captions");
    // No out-of-band <track> elements on the HLS tier (in-band delivery).
    const video = screen.getByTestId("player-video") as HTMLVideoElement;
    expect(video.querySelectorAll("track").length).toBe(0);

    // The forced French track (server index 1) auto-selects → the in-band rendition
    // at index 1 is enabled once the attachment is ready.
    await waitFor(() => expect(setTextTrack).toHaveBeenCalledWith(1));
    expect(btn).toHaveAttribute("aria-pressed", "true");

    // Switch to English (server index 0) — instant, in-band, no reload.
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByText("English"));
    });
    expect(setTextTrack).toHaveBeenLastCalledWith(0);

    // Turn captions off → the rendition is cleared (null).
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(screen.getByTestId("captions-off"));
    });
    expect(setTextTrack).toHaveBeenLastCalledWith(null);
  });

  it("still shows the captions button with no tracks so the viewer can search online", async () => {
    startPlayback.mockResolvedValue(decisionWithSubs([]));
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);
    // Even with no local/embedded tracks the captions affordance is present — it now
    // hosts the "search online" action (subtitles/05), so a Title with no subtitle in
    // the wanted language can still fetch one. The menu offers Off + Search online.
    await screen.findByTestId("player-video");
    const cc = await screen.findByTestId("now-playing-captions");
    expect(cc).not.toBeNull();
    await act(async () => {
      fireEvent.click(cc);
    });
    expect(screen.getByTestId("captions-off")).toBeInTheDocument();
    expect(screen.getByTestId("captions-search-online")).toBeInTheDocument();
  });

  it("search online → pick a candidate → the fetched track appears in the menu", async () => {
    startPlayback.mockResolvedValue(decisionWithSubs([]));
    searchSubtitles.mockResolvedValue([
      { id: "c1", language: "de", format: "srt", forced: false, hearingImpaired: false, label: "German — Rel [exact]" },
    ]);
    fetchSubtitle.mockResolvedValue({
      id: "fetched-1",
      source: "fetched",
      kind: "text",
      language: "de",
      forced: false,
      label: "German",
      url: "/api/v1/titles/t1/subtitles/fetched-1.vtt",
    });
    seedAndRender([entryFromTitle(movieSummary("t1", "Sub Movie"))]);
    await screen.findByTestId("player-video");

    const cc = await screen.findByTestId("now-playing-captions");
    await act(async () => {
      fireEvent.click(cc);
    });
    // Trigger the online search.
    await act(async () => {
      fireEvent.click(screen.getByTestId("captions-search-online"));
    });
    const candidate = await screen.findByTestId("captions-candidate");
    expect(candidate).toHaveTextContent("German — Rel [exact]");
    expect(searchSubtitles.mock.calls[0][0]).toBe("t1");

    // Pick it → fetchSubtitle is called and the fetched track appears in the menu.
    await act(async () => {
      fireEvent.click(candidate);
    });
    await waitFor(() => expect(fetchSubtitle).toHaveBeenCalled());
    await act(async () => {
      fireEvent.click(cc); // re-open (a pick closes the menu)
    });
    expect(screen.getByTestId("now-playing-captions-menu")).toHaveTextContent("German");
  });
});
