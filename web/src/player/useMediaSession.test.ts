import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { useMediaSession, type MediaSessionInput } from "./useMediaSession";

// The Media Session hook (appletv-parity/11): mirrors the current music Track into
// `navigator.mediaSession` and routes the OS transport controls back to the passed
// callbacks. jsdom implements NEITHER `navigator.mediaSession` NOR the
// `MediaMetadata` constructor, so we stand up a minimal fake for the "present"
// tests and simply omit it for the "absent" (graceful no-op) case.

interface FakeMediaSession {
  metadata: unknown;
  playbackState: string;
  setActionHandler: ReturnType<typeof vi.fn>;
  handlers: Map<string, (() => void) | null>;
  fire(action: string): void;
}

function makeFakeSession(): FakeMediaSession {
  const handlers = new Map<string, (() => void) | null>();
  return {
    metadata: null,
    playbackState: "none",
    handlers,
    setActionHandler: vi.fn((action: string, handler: (() => void) | null) => {
      handlers.set(action, handler);
    }),
    fire(action: string) {
      handlers.get(action)?.();
    },
  };
}

// A stand-in for the browser `MediaMetadata` constructor: just record the init.
class FakeMediaMetadata {
  title: string;
  artist: string;
  album: string;
  artwork: { src: string }[];
  constructor(init: {
    title?: string;
    artist?: string;
    album?: string;
    artwork?: { src: string }[];
  }) {
    this.title = init.title ?? "";
    this.artist = init.artist ?? "";
    this.album = init.album ?? "";
    this.artwork = init.artwork ?? [];
  }
}

function installMediaSession(session: FakeMediaSession | null) {
  Object.defineProperty(navigator, "mediaSession", {
    configurable: true,
    value: session,
  });
}

function baseInput(over: Partial<MediaSessionInput> = {}): MediaSessionInput {
  return {
    track: { title: "Paranoid Android", artist: "Radiohead", album: "OK Computer" },
    playing: true,
    onPlay: vi.fn(),
    onPause: vi.fn(),
    onPreviousTrack: vi.fn(),
    onNextTrack: vi.fn(),
    ...over,
  };
}

let session: FakeMediaSession;

beforeEach(() => {
  vi.stubGlobal("MediaMetadata", FakeMediaMetadata);
  session = makeFakeSession();
  installMediaSession(session);
});

afterEach(() => {
  vi.unstubAllGlobals();
  // Remove the property so a later "absent" test truly sees no mediaSession.
  Reflect.deleteProperty(navigator as unknown as Record<string, unknown>, "mediaSession");
});

describe("useMediaSession — metadata", () => {
  it("sets metadata from the current Track (title/artist/album/artwork)", () => {
    renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput({
        track: {
          title: "Paranoid Android",
          artist: "Radiohead",
          album: "OK Computer",
          artworkSrc: "/api/v1/albums/al1/artwork",
        },
      }),
    });
    const md = session.metadata as FakeMediaMetadata;
    expect(md.title).toBe("Paranoid Android");
    expect(md.artist).toBe("Radiohead");
    expect(md.album).toBe("OK Computer");
    expect(md.artwork[0].src).toBe("/api/v1/albums/al1/artwork");
  });

  it("updates metadata when the Track changes", () => {
    const { rerender } = renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput(),
    });
    expect((session.metadata as FakeMediaMetadata).title).toBe("Paranoid Android");
    rerender(
      baseInput({ track: { title: "Karma Police", artist: "Radiohead", album: "OK Computer" } }),
    );
    expect((session.metadata as FakeMediaMetadata).title).toBe("Karma Police");
  });
});

describe("useMediaSession — playbackState", () => {
  it("reflects the real play/pause state", () => {
    const { rerender } = renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput({ playing: true }),
    });
    expect(session.playbackState).toBe("playing");
    rerender(baseInput({ playing: false }));
    expect(session.playbackState).toBe("paused");
  });

  it("clears metadata and playbackState on stop (no Track)", () => {
    const { rerender } = renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput(),
    });
    expect(session.metadata).not.toBeNull();
    rerender(baseInput({ track: null }));
    expect(session.metadata).toBeNull();
    expect(session.playbackState).toBe("none");
  });
});

describe("useMediaSession — action handlers", () => {
  it("routes play/pause/previoustrack/nexttrack to the callbacks", () => {
    const input = baseInput();
    renderHook((p: MediaSessionInput) => useMediaSession(p), { initialProps: input });

    session.fire("play");
    expect(input.onPlay).toHaveBeenCalledTimes(1);
    session.fire("pause");
    expect(input.onPause).toHaveBeenCalledTimes(1);
    session.fire("previoustrack");
    expect(input.onPreviousTrack).toHaveBeenCalledTimes(1);
    session.fire("nexttrack");
    expect(input.onNextTrack).toHaveBeenCalledTimes(1);
  });

  it("greys out prev/next (null handler) when there is no prev/next", () => {
    renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput({ onPreviousTrack: null, onNextTrack: null }),
    });
    expect(session.handlers.get("previoustrack")).toBeNull();
    expect(session.handlers.get("nexttrack")).toBeNull();
    // play/pause are always registered.
    expect(session.handlers.get("play")).toBeInstanceOf(Function);
    expect(session.handlers.get("pause")).toBeInstanceOf(Function);
  });

  it("calls the LATEST callback after a rerender (no stale closure)", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook((p: MediaSessionInput) => useMediaSession(p), {
      initialProps: baseInput({ onPlay: first }),
    });
    rerender(baseInput({ onPlay: second }));
    session.fire("play");
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledTimes(1);
  });
});

describe("useMediaSession — feature detection", () => {
  it("is a graceful no-op when navigator.mediaSession is absent", () => {
    installMediaSession(null);
    expect(() =>
      renderHook((p: MediaSessionInput) => useMediaSession(p), { initialProps: baseInput() }),
    ).not.toThrow();
  });

  it("is a graceful no-op when MediaMetadata is unavailable", () => {
    vi.stubGlobal("MediaMetadata", undefined);
    expect(() =>
      renderHook((p: MediaSessionInput) => useMediaSession(p), { initialProps: baseInput() }),
    ).not.toThrow();
    // Without the constructor the session is treated as unsupported → untouched.
    expect(session.setActionHandler).not.toHaveBeenCalled();
    expect(session.metadata).toBeNull();
  });
});
