import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, SeekPreviews, TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue } from "./queue/useQueue";

// Seek previews (ADR-0070, DECISIONS 9): hovering or dragging the seek bar on the
// full video stage shows the frame under the pointer from the decision's tile-sheet
// manifest, with its timestamp. Never on the collapsed surfaces, never for audio,
// never without a manifest. Sheets load lazily: the needed sheet + its neighbour on
// the first hover/drag, each at most once.

const { getTitle, startPlayback, reportProgress, endSession, getSessionMarkers } = vi.hoisted(
  () => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    getSessionMarkers: vi.fn(),
  }),
);

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
      getSessionMarkers: (...a: unknown[]) => getSessionMarkers(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";

// The checked-in wire-contract fixture, read from the repo rather than copied.
const manifest: SeekPreviews = JSON.parse(
  // vitest runs from web/, so the repo root is one level up.
  readFileSync(resolve(process.cwd(), "../internal/api/testdata/seekpreviews/manifest.json"), "utf8"),
);

const episode: TitleSummary = {
  id: "t1",
  kind: "episode",
  title: "The Fire",
  year: 0,
  needsReview: false,
  ambiguous: false,
  resumePositionMs: 0,
  watched: false,
  genres: [],
};

const track: TitleSummary = { ...episode, id: "t2", kind: "track", title: "A Song" };

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
  seekPreviews: manifest,
};

function Harness({ title = episode }: { title?: TitleSummary }) {
  const queue = useQueue();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow([entryFromTitle(title)])}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
}

// Records every Image the preview preloads (the sheet fetches).
let requested: string[] = [];

async function play(title = episode) {
  renderWithAuth(<Harness title={title} />, { initialEntries: ["/"] });
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  Object.defineProperty(video, "duration", { value: 1_500, configurable: true });
  act(() => {
    fireEvent.durationChange(video);
  });
  const seek = await screen.findByTestId("now-playing-progress");
  // 1000 px wide bar starting at x=0 with a 20 px thumb (the CSS custom property the
  // player reads): the thumb centre travels x=10..990 over 0..1500 s.
  seek.style.setProperty("--seek-thumb", "20px");
  seek.getBoundingClientRect = () =>
    ({ left: 0, right: 1000, width: 1000, top: 0, bottom: 10, height: 10, x: 0, y: 0 }) as DOMRect;
  return { video, seek };
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  requested = [];
  // jsdom has no PointerEvent, so fireEvent.pointerMove would drop clientX.
  vi.stubGlobal("PointerEvent", class extends MouseEvent {});
  vi.stubGlobal(
    "Image",
    class {
      set src(v: string) {
        requested.push(v);
      }
    },
  );
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "episode", title: "The Fire" });
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  getSessionMarkers.mockReset().mockResolvedValue([]);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("NowPlayingBar — Seek preview", () => {
  it("hover shows the frame under the pointer with its timestamp; leave hides it", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse"); // on the stage
    expect(screen.queryByTestId("seek-preview")).toBeNull();

    // x=500 of 1000 -> 750 s -> frame 75: sheet 0, cell 75 -> col 5, row 7.
    fireEvent.pointerMove(seek, { clientX: 500 });
    const preview = await screen.findByTestId("seek-preview");
    expect(preview).toHaveTextContent("12:30");
    const frame = screen.getByTestId("seek-preview-frame");
    expect(frame.style.backgroundImage).toContain(manifest.sheets[0]);
    expect(frame.style.backgroundSize).toBe("3200px 1440px");
    expect(frame.style.backgroundPosition).toBe(`-${5 * 320}px -${7 * 180}px`);

    fireEvent.pointerLeave(seek);
    expect(screen.queryByTestId("seek-preview")).toBeNull();
  });

  it("dragging the thumb shows the preview for the drag position until release", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerDown(seek, { clientX: 0 });
    // The drag moves the range value to 1490 s -> frame 149 -> sheet 1, col 9, row 6.
    fireEvent.change(seek, { target: { value: "1490" } });
    const frame = await screen.findByTestId("seek-preview-frame");
    expect(frame.style.backgroundImage).toContain(manifest.sheets[1]);
    expect(frame.style.backgroundPosition).toBe(`-${9 * 320}px -${6 * 180}px`);

    fireEvent.pointerUp(window);
    expect(screen.queryByTestId("seek-preview")).toBeNull();
  });

  it("shows no preview without seekPreviews", async () => {
    const { seekPreviews: omitted, ...bare } = decision;
    void omitted;
    startPlayback.mockResolvedValue(bare);
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(screen.queryByTestId("seek-preview")).toBeNull();
    expect(requested).toEqual([]);
  });

  it("shows no preview on the collapsed surface", async () => {
    const { seek } = await play();
    fireEvent.click(await screen.findByTestId("now-playing-collapse"));
    await screen.findByTestId("now-playing-pip-expand");
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(screen.queryByTestId("seek-preview")).toBeNull();
    expect(requested).toEqual([]);
  });

  it("shows no preview for an audio-only decision", async () => {
    getTitle.mockResolvedValue({ id: "t2", kind: "track", title: "A Song" });
    const { videoStream: dropped, ...audioOnly } = decision;
    void dropped;
    startPlayback.mockResolvedValue(audioOnly);
    const { seek } = await play(track);
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(screen.queryByTestId("seek-preview")).toBeNull();
    expect(requested).toEqual([]);
  });

  it("fetches lazily: nothing before the first hover, then the sheet + its neighbour, once each", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    expect(requested).toEqual([]);

    fireEvent.pointerMove(seek, { clientX: 100 }); // frame 15 -> sheet 0
    expect(requested).toEqual([manifest.sheets[0], manifest.sheets[1]]);

    fireEvent.pointerMove(seek, { clientX: 900 }); // sheet 1, no neighbour
    fireEvent.pointerLeave(seek);
    fireEvent.pointerMove(seek, { clientX: 100 });
    expect(requested).toEqual([manifest.sheets[0], manifest.sheets[1]]);
  });

  it("shows no preview when the decision has no videoStream, even for a video Title", async () => {
    const { videoStream: dropped, ...noVideo } = decision;
    void dropped;
    startPlayback.mockResolvedValue(noVideo);
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(screen.queryByTestId("seek-preview")).toBeNull();
    expect(requested).toEqual([]);
  });

  it("shows no preview for a keyboard seek", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.change(seek, { target: { value: "1490" } }); // no pointer down
    expect(screen.queryByTestId("seek-preview")).toBeNull();
    expect(requested).toEqual([]);
  });

  it("does not resurrect a stale preview after the surface changes and returns", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(await screen.findByTestId("seek-preview")).toBeInTheDocument();

    // Collapse to pip while the pointer sits on the bar, then come back.
    fireEvent.click(screen.getByTestId("now-playing-collapse"));
    fireEvent.click(await screen.findByTestId("now-playing-pip-expand"));
    await screen.findByTestId("now-playing-collapse");
    // No pointer is on the bar any more, so nothing shows.
    expect(screen.queryByTestId("seek-preview")).toBeNull();
  });

  it("maps the pointer like the native range: thumb centre at the ends", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerMove(seek, { clientX: 10 }); // W=1000, thumb 20 -> t=0
    expect(await screen.findByTestId("seek-preview")).toHaveTextContent("0:00");
    expect(screen.getByTestId("seek-preview-frame").style.backgroundPosition).toBe("0px 0px");

    fireEvent.pointerMove(seek, { clientX: 990 }); // W - thumb/2 -> the duration
    expect(screen.getByTestId("seek-preview")).toHaveTextContent("25:00");
    const frame = screen.getByTestId("seek-preview-frame");
    expect(frame.style.backgroundImage).toContain(manifest.sheets[1]);
    expect(frame.style.backgroundPosition).toBe(`-${9 * 320}px -${6 * 180}px`);

    // Before the thumb's travel starts (x=0) is still t=0.
    fireEvent.pointerMove(seek, { clientX: 0 });
    expect(screen.getByTestId("seek-preview")).toHaveTextContent("0:00");
  });

  it("uses only the input value during a drag (no flicker between sources)", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerDown(seek, { clientX: 990 });
    fireEvent.change(seek, { target: { value: "1490" } });
    fireEvent.pointerMove(seek, { clientX: 10 }); // x says frame 0; the value says 149
    expect(screen.getByTestId("seek-preview-frame").style.backgroundPosition).toBe(
      `-${9 * 320}px -${6 * 180}px`,
    );
  });

  it("places a touch tap with no prior move at the tapped position", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerDown(seek, { clientX: 500 });
    fireEvent.change(seek, { target: { value: "750" } });
    expect(screen.getByTestId("seek-preview").style.left).toBe("500px");
  });

  it("quotes the sheet url in the CSS", async () => {
    const { seek } = await play();
    await screen.findByTestId("now-playing-collapse");
    fireEvent.pointerMove(seek, { clientX: 500 });
    expect(screen.getByTestId("seek-preview-frame").style.backgroundImage).toBe(
      `url("${manifest.sheets[0]}")`,
    );
  });
});
