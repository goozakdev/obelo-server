import { StrictMode } from "react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError, NetworkError } from "../api/errors";
import type {
  Marker,
  PlaybackDecision,
  TitleDetail,
  TitleSummary,
} from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { useQueue } from "./queue/useQueue";
import { AUTO_PREFERENCE, savePreference } from "./playbackPreference";

// Regression tests for the player findings of the web code review (R04-xx). One
// file because they share a single faked-apiClient seam; each `describe` names the
// finding it pins.

const {
  getTitle,
  startPlayback,
  reportProgress,
  endSession,
  searchSubtitles,
  fetchSubtitle,
  getSessionMarkers,
} = vi.hoisted(() => ({
  getTitle: vi.fn(),
  startPlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
  searchSubtitles: vi.fn(),
  fetchSubtitle: vi.fn(),
  getSessionMarkers: vi.fn(),
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
      getSessionMarkers: (...a: unknown[]) => getSessionMarkers(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";

function summary(id: string, kind = "movie"): TitleSummary {
  return {
    id,
    kind,
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

function decisionOf(over: Partial<PlaybackDecision> = {}): PlaybackDecision {
  return {
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
    ...over,
  };
}

/** Render a RESTORED Queue (loads paused — no autoplay). */
function seedAndRender(entries: QueueEntry[], repeat: QueueState["repeat"] = "off") {
  const state: QueueState = { entries, currentIndex: 0, repeat, authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

/** A Play gesture (so the entry AUTO-plays), plus the bar. */
let queueStore: ReturnType<typeof useQueue>;
function Harness({ titles, strict }: { titles: TitleSummary[]; strict?: boolean }) {
  const queue = useQueue();
  queueStore = queue;
  const body = (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow(titles.map((t) => entryFromTitle(t)))}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
  return strict ? <StrictMode>{body}</StrictMode> : body;
}

async function playToStage(titles: TitleSummary[], strict = false) {
  renderWithAuth(<Harness titles={titles} strict={strict} />, { initialEntries: ["/"] });
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  await screen.findByTestId("now-playing-collapse"); // on the stage
  return video;
}

function setMedia(video: HTMLVideoElement, currentTime: number, duration: number, paused = true) {
  Object.defineProperty(video, "currentTime", { value: currentTime, writable: true, configurable: true });
  Object.defineProperty(video, "duration", { value: duration, configurable: true });
  Object.defineProperty(video, "paused", { value: paused, configurable: true });
  act(() => {
    fireEvent.durationChange(video);
    fireEvent.timeUpdate(video);
  });
}

let playSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockImplementation((id: string) =>
    Promise.resolve({ id, kind: "movie", title: id } as Partial<TitleDetail>),
  );
  startPlayback.mockReset().mockResolvedValue(decisionOf());
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  searchSubtitles.mockReset().mockResolvedValue([]);
  fetchSubtitle.mockReset();
  getSessionMarkers.mockReset().mockResolvedValue([]);
  attachHls.mockReset().mockResolvedValue({
    mode: "hls.js",
    detach: vi.fn(),
    setTextTrack: vi.fn(),
    setAudioTrack: vi.fn(),
  });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
  playSpy = vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("R04-01 — the player reads the user id from either storage tier", () => {
  it("a session-only login (user in sessionStorage) still gets its committed preference", async () => {
    // renderWithAuth always seeds localStorage's user; hide it from the player so
    // only the session-only tier knows who is logged in.
    const realGet = Storage.prototype.getItem;
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(function (this: Storage, key: string) {
      if (key === "obelo.user" && this === window.localStorage) return null;
      return realGet.call(this, key);
    });
    window.sessionStorage.setItem(
      "obelo.user",
      JSON.stringify({ id: "u1", username: "operator", role: "admin" }),
    );
    getTitle.mockResolvedValue({
      id: "t1",
      kind: "movie",
      title: "Dune",
      editions: [{ id: "ed-fc", name: "Final Cut", files: [] }],
    });
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      { ...AUTO_PREFERENCE, editionName: "Final Cut" },
    );
    seedAndRender([entryFromTitle(summary("t1"))]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ editionId: "ed-fc" }),
        expect.anything(),
      ),
    );
  });
});

describe("R04-02 — a stored TEXT subtitle preference is applied", () => {
  it("selects the matching text track on load", async () => {
    startPlayback.mockResolvedValue(
      decisionOf({
        subtitles: [
          { id: "fr", source: "embedded", kind: "text", language: "fr", forced: false, label: "French", url: "/s/fr.vtt" },
          { id: "en", source: "embedded", kind: "text", language: "en", forced: false, label: "English", url: "/s/en.vtt" },
        ],
      }),
    );
    getTitle.mockResolvedValue({ id: "t1", kind: "movie", title: "Dune", editions: [] });
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      { ...AUTO_PREFERENCE, subtitle: { language: "en", forced: false } },
    );
    seedAndRender([entryFromTitle(summary("t1"))]);
    const btn = await screen.findByTestId("now-playing-captions");
    await waitFor(() => expect(btn).toHaveAttribute("aria-pressed", "true"));
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(screen.getByText("✓ English")).toHaveAttribute("aria-checked", "true");
  });
});

describe("R04-03 — the blocked-autoplay retry", () => {
  async function blockedAutoplay() {
    playSpy.mockReset().mockRejectedValueOnce(new DOMException("blocked", "NotAllowedError")).mockResolvedValue(undefined);
    vi.spyOn(console, "error").mockImplementation(() => {});
    const video = await playToStage([summary("t1")]);
    await act(async () => {
      fireEvent.loadedMetadata(video);
    });
    await waitFor(() => expect(playSpy).toHaveBeenCalledTimes(1));
    return video;
  }

  it("one gesture retries once and leaves NO leftover listener behind", async () => {
    await blockedAutoplay();
    await act(async () => {
      fireEvent.keyDown(document.body, { key: "a" });
    });
    expect(playSpy).toHaveBeenCalledTimes(2);
    // The other gesture's listener must be gone too: a later pointer/click after a
    // deliberate pause must not resume playback.
    await act(async () => {
      fireEvent.pointerDown(document.body);
      fireEvent.click(document.body);
      fireEvent.keyDown(document.body, { key: "b" });
    });
    expect(playSpy).toHaveBeenCalledTimes(2);
  });

  it("the retry is dropped when the player unmounts (no ghost playback)", async () => {
    await blockedAutoplay();
    // Unmount without a click (a click is itself a gesture that would retry).
    act(() => queueStore.clear());
    await waitFor(() => expect(screen.queryByTestId("player-video")).toBeNull());
    await act(async () => {
      fireEvent.keyDown(document.body, { key: "a" });
      fireEvent.pointerDown(document.body);
      fireEvent.click(document.body);
    });
    expect(playSpy).toHaveBeenCalledTimes(1);
  });
});

describe("R04-03 — Space/k after a blocked autoplay start playback", () => {
  // jsdom's play()/pause() do not move `paused`; track it so a retry-then-toggle
  // (play, then pause) is observable as a final paused state.
  async function blockedStage() {
    const video = await (async () => {
      let paused = true;
      playSpy.mockReset().mockRejectedValueOnce(new DOMException("blocked", "NotAllowedError")).mockImplementation(() => {
        paused = false;
        return Promise.resolve(undefined);
      });
      vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {
        paused = true;
      });
      vi.spyOn(HTMLMediaElement.prototype, "paused", "get").mockImplementation(() => paused);
      vi.spyOn(console, "error").mockImplementation(() => {});
      return playToStage([summary("t1")]);
    })();
    await act(async () => {
      fireEvent.loadedMetadata(video);
    });
    await waitFor(() => expect(playSpy).toHaveBeenCalledTimes(1));
    return video;
  }

  it.each([" ", "k"])("%j on the stage leaves the element playing", async (key) => {
    const video = await blockedStage();
    await act(async () => {
      fireEvent.keyDown(document.body, { key });
    });
    expect(video.paused).toBe(false);
  });

  it("a Space on a focused button is left to the button's own click", async () => {
    const video = await blockedStage();
    const btn = screen.getByTestId("now-playing-collapse");
    await act(async () => {
      fireEvent.keyDown(btn, { key: " " });
    });
    expect(playSpy).toHaveBeenCalledTimes(1); // retry still armed, not spent
    expect(video.paused).toBe(true);
  });
});

describe("R04-02 — a stored image-subtitle burn preference without details at mount", () => {
  const subs = [
    { id: "img-en", source: "embedded", kind: "image", language: "en", forced: false, label: "English", url: undefined },
    { id: "txt-en", source: "embedded", kind: "text", language: "en", forced: false, label: "English text", url: "/s/en.vtt" },
  ] as PlaybackDecision["subtitles"];
  // A title id of its own: the detail fetch is cached per id across tests, and a warm
  // cache is exactly the case where the preference is NOT pending at mount.
  function setup() {
    startPlayback.mockResolvedValue(decisionOf({ tier: "transcode", subtitles: subs }));
    getTitle.mockResolvedValue({ id: "tburn", kind: "movie", title: "Dune", editions: [], subtitles: subs });
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "tburn" },
      { ...AUTO_PREFERENCE, aacStereo: true, subtitle: { language: "en", forced: false } },
    );
    seedAndRender([entryFromTitle(summary("tburn"))]);
  }

  it("the first negotiation carries burnSubtitleId, and no same-language text track is ALSO selected", async () => {
    const setTextTrack = vi.fn();
    attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack });
    setup();
    await waitFor(() => expect(setTextTrack).toHaveBeenCalled());
    expect((startPlayback.mock.calls[0][1] as { burnSubtitleId?: string }).burnSubtitleId).toBe("img-en");
    // The burn already carries the English captions: no in-band text rendition on
    // (a second caption layer would show the same line twice).
    expect(setTextTrack.mock.calls.every((c) => c[0] === null)).toBe(true);
  });
});

describe("R04-04 — auto-skip only for an entry the server cannot play", () => {
  function twoEntries() {
    return [entryFromTitle(summary("t1")), entryFromTitle(summary("t2"))];
  }

  it("does NOT skip when the server is unreachable (shows the error instead)", async () => {
    startPlayback.mockRejectedValue(new NetworkError("could not reach the server"));
    seedAndRender(twoEntries());
    await screen.findByTestId("player-negotiate-error");
    await act(async () => {});
    expect(startPlayback.mock.calls.map((c) => c[0])).toEqual(["t1"]);
  });

  it("does NOT skip on a linked-server outage", async () => {
    startPlayback.mockRejectedValue(new ApiError(503, "LINK_UNREACHABLE", "down"));
    seedAndRender(twoEntries());
    await screen.findByTestId("player-negotiate-error");
    await act(async () => {});
    expect(startPlayback.mock.calls.map((c) => c[0])).toEqual(["t1"]);
  });

  it("does NOT skip when a mid-play re-negotiation fails", async () => {
    startPlayback
      .mockResolvedValueOnce(decisionOf({ tier: "transcode", streamUrl: "/hls/1.m3u8" }))
      .mockRejectedValue(new NetworkError("could not reach the server"));
    seedAndRender(twoEntries());
    await screen.findByTestId("player-video");
    await waitFor(() => expect(attachHls).toHaveBeenCalledTimes(1));
    const lost = (attachHls.mock.calls[0][2] as { onSessionLost: () => void }).onSessionLost;
    await act(async () => {
      lost();
    });
    await screen.findByTestId("player-negotiate-error");
    await act(async () => {});
    expect(startPlayback.mock.calls.map((c) => c[0])).toEqual(["t1", "t1"]);
  });
});

describe("R04-05 — Repeat does not loop or wrap a video", () => {
  it("repeat-one does not replay a Movie at its end", async () => {
    seedAndRender([entryFromTitle(summary("t1"))], "one");
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 99, 100);
    playSpy.mockClear();
    await act(async () => {
      fireEvent.ended(video);
    });
    expect(playSpy).not.toHaveBeenCalled();
    expect(video.currentTime).toBe(99);
  });

  it("repeat-all does not wrap a Show back to its first episode", async () => {
    const state: QueueState = {
      entries: [entryFromTitle(summary("e1", "episode")), entryFromTitle(summary("e2", "episode"))],
      currentIndex: 1,
      repeat: "all",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", state);
    renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await act(async () => {
      fireEvent.ended(video);
    });
    await act(async () => {});
    expect(startPlayback.mock.calls.map((c) => c[0])).toEqual(["e2"]);
  });
});

describe("R04-07 — a re-negotiation clears the playing flag", () => {
  it("the bar reads Paused after recovery until the new element actually plays", async () => {
    startPlayback
      .mockResolvedValueOnce(decisionOf({ tier: "transcode", streamUrl: "/hls/1.m3u8" }))
      .mockResolvedValueOnce(
        decisionOf({ sessionId: "sess-2", tier: "transcode", streamUrl: "/hls/2.m3u8" }),
      );
    seedAndRender([entryFromTitle(summary("t1"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() => expect(attachHls).toHaveBeenCalledTimes(1));
    await act(async () => {
      fireEvent.play(video);
    });
    expect(screen.getByTestId("now-playing-play-pause")).toHaveAttribute("aria-pressed", "true");

    const lost = (attachHls.mock.calls[0][2] as { onSessionLost: () => void }).onSessionLost;
    await act(async () => {
      lost();
    });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(attachHls).toHaveBeenCalledTimes(2));
    // The old element was unmounted without a `pause` event; the new one never played.
    expect(screen.getByTestId("now-playing-play-pause")).toHaveAttribute("aria-pressed", "false");
  });
});

describe("R04-09 — seeking reports from the `seeked` event only", () => {
  it("dragging the seek bar sends no per-change progress POST", async () => {
    seedAndRender([entryFromTitle(summary("t1"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 0, 200);
    reportProgress.mockClear();
    const range = screen.getByTestId("now-playing-progress");
    fireEvent.change(range, { target: { value: "10" } });
    fireEvent.change(range, { target: { value: "20" } });
    fireEvent.change(range, { target: { value: "30" } });
    expect(video.currentTime).toBeCloseTo(30, 1);
    expect(reportProgress).not.toHaveBeenCalled();
  });

  it("a burst of seeked events sends one (debounced) report at the final position", async () => {
    seedAndRender([entryFromTitle(summary("t1"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 0, 200);
    reportProgress.mockClear();
    fireEvent.change(screen.getByTestId("now-playing-progress"), { target: { value: "42" } });
    fireEvent.seeked(video);
    fireEvent.seeked(video);
    fireEvent.seeked(video);
    await waitFor(() => expect(reportProgress).toHaveBeenCalledTimes(1));
    expect(reportProgress).toHaveBeenCalledWith("sess-1", expect.objectContaining({ positionMs: 42_000 }));
  });
});

describe("R04-12 — the Credits advance works under StrictMode", () => {
  it("an auto-skipped Credits still advances after the dev double-mount", async () => {
    const credits: Marker[] = [
      { kind: "credits", source: "local", startMs: 1_300_000, endMs: 1_380_000, autoSkip: true, watchedPoint: true },
    ];
    getSessionMarkers.mockResolvedValue(credits);
    const state: QueueState = {
      entries: [entryFromTitle(summary("t1", "episode")), entryFromTitle(summary("t2", "episode"))],
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", state);
    renderWithAuth(
      <StrictMode>
        <NowPlayingBar />
      </StrictMode>,
      { initialEntries: ["/"] },
    );
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    Object.defineProperty(video, "duration", { value: 1_400, configurable: true });
    act(() => {
      fireEvent.durationChange(video);
    });
    await waitFor(() => expect(getSessionMarkers).toHaveBeenCalled());
    await act(async () => {});
    Object.defineProperty(video, "currentTime", { value: 1_310, writable: true, configurable: true });
    act(() => {
      fireEvent.timeUpdate(video);
    });
    await waitFor(() => expect(startPlayback.mock.calls.map((c) => c[0])).toContain("t2"));
  });
});

describe("R04-13 — Escape closes the queue drawer first", () => {
  it("one Esc with the drawer open leaves a windowed stage in place", async () => {
    await playToStage([summary("t1"), summary("t2")]);
    fireEvent.click(screen.getByTestId("now-playing-queue-button"));
    await screen.findByTestId("now-playing-drawer");
    act(() => {
      fireEvent.keyDown(document.body, { key: "Escape" });
    });
    await waitFor(() => expect(screen.queryByTestId("now-playing-drawer")).toBeNull());
    // The stage collapse rides history.back() (an async popstate) — let it land.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
    // A second Esc (drawer now closed) collapses the stage as usual.
    act(() => {
      fireEvent.keyDown(document.body, { key: "Escape" });
    });
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip"),
    );
  });
});

describe("R04-21 — Space on the focused Up Next card does not also toggle playback", () => {
  it("activates the card only", async () => {
    const video = await playToStage([summary("e1", "episode"), summary("e2", "episode")]);
    setMedia(video, 80, 100);
    const card = await screen.findByTestId("now-playing-upnext");
    card.focus();
    playSpy.mockClear();
    await act(async () => {
      fireEvent.keyDown(card, { key: " " });
    });
    await waitFor(() => expect(startPlayback.mock.calls.map((c) => c[0])).toContain("e2"));
    expect(playSpy).not.toHaveBeenCalled();
  });
});

describe("the blocked-autoplay retry is spent only by a key that grants activation", () => {
  async function blockedAutoplay() {
    playSpy.mockReset().mockRejectedValueOnce(new DOMException("blocked", "NotAllowedError")).mockResolvedValue(undefined);
    vi.spyOn(console, "error").mockImplementation(() => {});
    const video = await playToStage([summary("t1")]);
    await act(async () => {
      fireEvent.loadedMetadata(video);
    });
    await waitFor(() => expect(playSpy).toHaveBeenCalledTimes(1));
  }

  it("Escape, a bare modifier and a Ctrl chord leave it armed for the next real gesture", async () => {
    await blockedAutoplay();
    await act(async () => {
      fireEvent.keyDown(document.body, { key: "Shift" });
      fireEvent.keyDown(document.body, { key: "Control", ctrlKey: true });
      fireEvent.keyDown(document.body, { key: "c", ctrlKey: true });
      fireEvent.keyDown(document.body, { key: "Escape" });
    });
    expect(playSpy).toHaveBeenCalledTimes(1);
    await act(async () => {
      fireEvent.click(document.body);
    });
    expect(playSpy).toHaveBeenCalledTimes(2);
  });
});

describe("a burned image subtitle suppresses only same-language text tracks", () => {
  const burnSubs = (textTracks: object[], burnedLang = "en") =>
    [
      { id: "img-en", source: "embedded", kind: "image", language: burnedLang, forced: false, label: "English", url: undefined },
      ...textTracks,
    ] as PlaybackDecision["subtitles"];
  const txt = (id: string, language: string, forced: boolean) => ({
    id,
    source: "embedded",
    kind: "text",
    language,
    forced,
    label: id,
    url: `/s/${id}.vtt`,
  });
  async function lastTextTrackIndex(subs: PlaybackDecision["subtitles"], title: string, prefLang = "en") {
    const setTextTrack = vi.fn();
    attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack });
    startPlayback.mockResolvedValue(decisionOf({ tier: "transcode", subtitles: subs }));
    getTitle.mockResolvedValue({ id: title, kind: "movie", title: "Dune", editions: [], subtitles: subs });
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: title },
      { ...AUTO_PREFERENCE, aacStereo: true, subtitle: { language: prefLang, forced: false } },
    );
    seedAndRender([entryFromTitle(summary(title))]);
    await waitFor(() => expect(setTextTrack).toHaveBeenCalled());
    await act(async () => {});
    return setTextTrack.mock.calls[setTextTrack.mock.calls.length - 1][0];
  }

  it("a forced foreign-language track is still the default beside the burned track", async () => {
    // Deliverable (server) order: [en, fr-forced] → the forced French track is index 1.
    const idx = await lastTextTrackIndex(burnSubs([txt("txt-en", "en", false), txt("txt-fr", "fr", true)]), "tburn-f1");
    expect(idx).toBe(1);
  });

  it("a forced track in the burned language is held back (no doubled captions)", async () => {
    const idx = await lastTextTrackIndex(burnSubs([txt("txt-en", "en", true), txt("txt-fr", "fr", false)]), "tburn-f2");
    expect(idx).toBeNull();
  });

  it("matches the burned language across ISO 639-1 / 639-2 spellings (eng burned, en text)", async () => {
    const idx = await lastTextTrackIndex(
      burnSubs([txt("txt-en", "en", true), txt("txt-fr", "fr", false)], "eng"),
      "tburn-f3",
      "eng", // the stored preference names the burned track's own spelling
    );
    expect(idx).toBeNull();
  });
});
