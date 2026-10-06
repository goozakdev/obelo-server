import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { PlaybackTransportProvider } from "./transport";

// The Media Session bridge (appletv-parity/11): app-wide glue that feeds the
// current music Track + play state to `useMediaSession`. Driven through the same
// seeded-Queue seam the NowPlayingBar tests use (a Queue in sessionStorage the
// QueueProvider hydrates), with a faked apiClient for the Title-detail fetch that
// supplies Artist/Album, and a stubbed `navigator.mediaSession`. We assert it
// mirrors a music Track, updates on advance, and stays clear for a video entry
// (music-only) — the bridge-specific behaviour on top of the hook's own unit test.

const { getTitle, events } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  events: { emit: (_type: string, _data: unknown) => {} },
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      subscribeEvents: (cb: (type: string, data: unknown) => void) => {
        events.emit = cb;
        return () => {};
      },
    },
  };
});

import MediaSessionBridge from "./MediaSessionBridge";

// A minimal fake of `navigator.mediaSession` + the `MediaMetadata` constructor
// (jsdom implements neither), enough to observe what the bridge sets.
interface FakeMediaSession {
  metadata: { title?: string; artist?: string; album?: string; artwork?: { src: string }[] } | null;
  playbackState: string;
  setActionHandler: (action: string, handler: (() => void) | null) => void;
  handlers: Map<string, (() => void) | null>;
  fire(action: string): void;
}
function makeFakeSession(): FakeMediaSession {
  const handlers = new Map<string, (() => void) | null>();
  return {
    metadata: null,
    playbackState: "none",
    handlers,
    setActionHandler: (action, handler) => handlers.set(action, handler),
    fire: (action) => handlers.get(action)?.(),
  };
}
class FakeMediaMetadata {
  title: string;
  artist: string;
  album: string;
  artwork: { src: string }[];
  constructor(init: { title?: string; artist?: string; album?: string; artwork?: { src: string }[] }) {
    this.title = init.title ?? "";
    this.artist = init.artist ?? "";
    this.album = init.album ?? "";
    this.artwork = init.artwork ?? [];
  }
}

function trackSummary(id: string, name: string): TitleSummary {
  return {
    id,
    kind: "track",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}
function movieSummary(id: string, name: string): TitleSummary {
  return { ...trackSummary(id, name), kind: "movie" };
}
function trackDetail(id: string, name: string, artist: string, album: string): Partial<TitleDetail> {
  return {
    id,
    kind: "track",
    title: name,
    libraryId: "lib1",
    track: { artistId: "ar1", artistName: artist, albumId: `al-${id}`, albumTitle: album },
  };
}

let session: FakeMediaSession;

function seedAndRender(entries: QueueEntry[], currentIndex = 0) {
  const state: QueueState = { entries, currentIndex, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(
    <PlaybackTransportProvider>
      <MediaSessionBridge />
    </PlaybackTransportProvider>,
  );
}

/** Emits `ticks` library progress ticks 100 ms apart, then lets the burst go quiet
 * (the bridge refetches once it has), on fake timers that are restored afterwards. */
async function burstThenQuiet(libraryId: string, ticks: number) {
  vi.useFakeTimers();
  try {
    for (let i = 0; i < ticks; i++) {
      act(() => events.emit("scanProgress", { libraryId, complete: false }));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(100);
      });
    }
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500);
    });
  } finally {
    vi.useRealTimers();
  }
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  vi.stubGlobal("MediaMetadata", FakeMediaMetadata);
  session = makeFakeSession();
  Object.defineProperty(navigator, "mediaSession", { configurable: true, value: session });
  getTitle.mockReset();
});

afterEach(() => {
  // Unmount BEFORE unstubbing: afterEach hooks run in reverse order, so without this
  // a late effect can construct MediaMetadata after the stub is gone.
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator as unknown as Record<string, unknown>, "mediaSession");
});

describe("MediaSessionBridge", () => {
  it("mirrors the current music Track's metadata (title/artist/album)", async () => {
    getTitle.mockResolvedValue(trackDetail("t1", "Paranoid Android", "Radiohead", "OK Computer"));
    seedAndRender([entryFromTitle(trackSummary("t1", "Paranoid Android"))]);

    await waitFor(() => expect(session.metadata?.artist).toBe("Radiohead"));
    expect(session.metadata?.title).toBe("Paranoid Android");
    expect(session.metadata?.album).toBe("OK Computer");
  });

  it("cache-busts the OS cover with the album's artworkVersion", async () => {
    const detail = trackDetail("t1", "Paranoid Android", "Radiohead", "OK Computer");
    getTitle.mockResolvedValue({
      ...detail,
      track: { ...detail.track!, albumArtworkVersion: "v9" },
    });
    seedAndRender([entryFromTitle(trackSummary("t1", "Paranoid Android"))]);

    await waitFor(() => expect(session.metadata?.artist).toBe("Radiohead"));
    expect(session.metadata?.artwork).toEqual([{ src: "/api/v1/albums/al-t1/artwork?v=v9" }]);
  });

  it("re-reads the album cover version when its Library signals a change mid-track", async () => {
    const detail = trackDetail("t1", "Paranoid Android", "Radiohead", "OK Computer");
    getTitle.mockResolvedValueOnce({ ...detail, track: { ...detail.track!, albumArtworkVersion: "v1" } });
    seedAndRender([entryFromTitle(trackSummary("t1", "Paranoid Android"))]);
    await waitFor(() =>
      expect(session.metadata?.artwork).toEqual([{ src: "/api/v1/albums/al-t1/artwork?v=v1" }]),
    );

    // The cover is re-picked while the same track plays; the Library nudge refreshes it.
    getTitle.mockResolvedValue({ ...detail, track: { ...detail.track!, albumArtworkVersion: "v2" } });
    await burstThenQuiet("lib1", 1);
    await waitFor(() =>
      expect(session.metadata?.artwork).toEqual([{ src: "/api/v1/albums/al-t1/artwork?v=v2" }]),
    );
  });

  it("re-reads once per settled scan burst, not once per tick", async () => {
    const detail = trackDetail("t1", "Paranoid Android", "Radiohead", "OK Computer");
    getTitle.mockResolvedValue({ ...detail, track: { ...detail.track!, albumArtworkVersion: "v1" } });
    seedAndRender([entryFromTitle(trackSummary("t1", "Paranoid Android"))]);
    await waitFor(() => expect(session.metadata?.artist).toBe("Radiohead"));
    const before = getTitle.mock.calls.length;

    await burstThenQuiet("lib1", 100); // a 10 s burst of ticks 100 ms apart
    expect(getTitle.mock.calls.length - before).toBe(1);
  });

  it("does not keep a refreshed cover version when coming back to the track", async () => {
    let t1Version = "v1";
    getTitle.mockImplementation((id: string) => {
      if (id === "t1") {
        const d = trackDetail("t1", "Song One", "The Band", "First");
        return Promise.resolve({ ...d, track: { ...d.track!, albumArtworkVersion: t1Version } });
      }
      return Promise.resolve(trackDetail("t2", "Song Two", "The Band", "Second"));
    });
    seedAndRender([
      entryFromTitle(trackSummary("t1", "Song One")),
      entryFromTitle(trackSummary("t2", "Song Two")),
    ]);
    await waitFor(() => expect(session.metadata?.title).toBe("Song One"));

    t1Version = "v2"; // re-picked while A plays; the live signal refreshes it
    await burstThenQuiet("lib1", 1);
    await waitFor(() =>
      expect(session.metadata?.artwork).toEqual([{ src: "/api/v1/albums/al-t1/artwork?v=v2" }]),
    );

    act(() => session.fire("nexttrack"));
    await waitFor(() => expect(session.metadata?.title).toBe("Song Two"));
    t1Version = "v3"; // re-picked again while A was not playing
    act(() => session.fire("previoustrack"));
    await waitFor(() => expect(session.metadata?.title).toBe("Song One"));
    await waitFor(() =>
      expect(session.metadata?.artwork).toEqual([{ src: "/api/v1/albums/al-t1/artwork?v=v3" }]),
    );
  });

  it("ignores another Library's change signal", async () => {
    const detail = trackDetail("t1", "Paranoid Android", "Radiohead", "OK Computer");
    getTitle.mockResolvedValue({ ...detail, track: { ...detail.track!, albumArtworkVersion: "v1" } });
    seedAndRender([entryFromTitle(trackSummary("t1", "Paranoid Android"))]);
    await waitFor(() => expect(session.metadata?.artist).toBe("Radiohead"));

    act(() => events.emit("libraryUpdated", { libraryId: "other" }));
    await act(async () => {});
    expect(getTitle).toHaveBeenCalledTimes(1);
  });

  it("advances the Queue when the OS nexttrack control fires, updating metadata", async () => {
    getTitle.mockImplementation((id: string) =>
      id === "t1"
        ? Promise.resolve(trackDetail("t1", "Song One", "The Band", "First"))
        : Promise.resolve(trackDetail("t2", "Song Two", "The Band", "Second")),
    );
    seedAndRender([
      entryFromTitle(trackSummary("t1", "Song One")),
      entryFromTitle(trackSummary("t2", "Song Two")),
    ]);

    await waitFor(() => expect(session.metadata?.title).toBe("Song One"));
    // nexttrack is registered (there IS a next entry) and walks the Queue.
    expect(session.handlers.get("nexttrack")).toBeInstanceOf(Function);
    act(() => session.fire("nexttrack"));
    await waitFor(() => expect(session.metadata?.title).toBe("Song Two"));
  });

  it("is music-only: a video entry leaves the session cleared", async () => {
    getTitle.mockResolvedValue({ id: "m1", kind: "movie", title: "Dune" });
    seedAndRender([entryFromTitle(movieSummary("m1", "Dune"))]);

    // Flush the (no-op) async detail settle under act, then assert nothing was set
    // and no Title fetch was made for a video entry.
    await act(async () => {});
    expect(session.playbackState).toBe("none");
    expect(session.metadata).toBeNull();
    expect(getTitle).not.toHaveBeenCalled();
  });
});
