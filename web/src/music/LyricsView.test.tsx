import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { Lyrics, TitleDetail } from "../api/types";

// The lyrics view: whatever Local lyrics a Track has, Synced lines highlighted
// against the playback position while that Track is the one playing, Plain text
// shown as it is, and a quiet empty state — never an error — when there are
// none. Driven against a faked apiClient, a faked Queue (which Track is current)
// and a faked transport (where playback is).

const { getLyrics, getTitle, markLyricsWrong, playback } = vi.hoisted(() => ({
  getLyrics: vi.fn(),
  getTitle: vi.fn(),
  markLyricsWrong: vi.fn(),
  playback: { currentId: null as string | null, positionMs: 0 },
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getLyrics: (...a: unknown[]) => getLyrics(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
      markLyricsWrong: (...a: unknown[]) => markLyricsWrong(...a),
    },
  };
});

vi.mock("../player/queue/useQueue", () => ({
  useQueue: () => ({
    current: playback.currentId ? { entryId: "e1", title: { id: playback.currentId } } : null,
    playNow: () => {},
    enqueue: () => {},
    playNext: () => {},
  }),
}));

vi.mock("../player/transport", () => ({
  usePlaybackTransport: () => ({
    playing: playback.currentId !== null,
    toggle: () => {},
    publishPlaying: () => {},
    registerToggle: () => {},
    positionMs: () => playback.positionMs,
    registerPosition: () => {},
  }),
}));

vi.mock("../auth/session", () => ({ useAuth: () => ({ isAdmin: false }) }));
vi.mock("../browse/librariesContext", () => ({
  useLibraryMarks: () => undefined,
  useLibraryProvider: () => undefined,
}));
vi.mock("./MusicShell", () => ({
  default: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));
vi.mock("../browse/AddToPlaylist", () => ({ default: () => null }));

import LyricsView from "./LyricsView";
import TrackDetailScreen from "./TrackDetailScreen";

const synced: Lyrics = {
  kind: "synced",
  source: "local",
  lines: [
    { startMs: 1000, text: "First line" },
    { startMs: 3000, text: "Second line" },
    { startMs: 5000, text: "Third line" },
  ],
  text: "",
};

const plain: Lyrics = {
  kind: "plain",
  source: "local",
  lines: [],
  text: "Plain words\nA second plain line",
};

function activeLine(): string | null {
  const active = screen
    .getAllByTestId("lyric-line")
    .filter((el) => el.getAttribute("aria-current") === "true");
  expect(active.length).toBeLessThanOrEqual(1);
  return active[0]?.textContent ?? null;
}

describe("LyricsView", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    getLyrics.mockReset();
    getTitle.mockReset();
    playback.currentId = null;
    playback.positionMs = 0;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("highlights the Synced line the playback position is in, and follows it", async () => {
    getLyrics.mockResolvedValue(synced);
    playback.currentId = "t1";
    playback.positionMs = 3500;
    render(<LyricsView titleId="t1" />);

    const lines = await screen.findAllByTestId("lyric-line");
    expect(lines.map((l) => l.textContent)).toEqual(["First line", "Second line", "Third line"]);
    await waitFor(() => expect(activeLine()).toBe("Second line"));

    playback.positionMs = 5200;
    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(activeLine()).toBe("Third line");

    playback.positionMs = 200;
    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(activeLine()).toBeNull();
    expect(getLyrics).toHaveBeenCalledWith("t1", expect.anything());
  });

  it("highlights a line from exactly its start time", async () => {
    getLyrics.mockResolvedValue(synced);
    playback.currentId = "t1";
    playback.positionMs = 3000;
    render(<LyricsView titleId="t1" />);

    await screen.findAllByTestId("lyric-line");
    await waitFor(() => expect(activeLine()).toBe("Second line"));
  });

  it("highlights nothing while a different Track is playing", async () => {
    getLyrics.mockResolvedValue(synced);
    playback.currentId = "other";
    playback.positionMs = 3500;
    render(<LyricsView titleId="t1" />);

    await screen.findAllByTestId("lyric-line");
    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(activeLine()).toBeNull();
  });

  it("shows Plain lyrics as static text", async () => {
    getLyrics.mockResolvedValue(plain);
    playback.currentId = "t1";
    playback.positionMs = 3500;
    render(<LyricsView titleId="t1" />);

    const text = await screen.findByTestId("lyrics-plain");
    expect(text.textContent).toBe("Plain words\nA second plain line");
    expect(screen.queryAllByTestId("lyric-line")).toHaveLength(0);
  });

  it("shows an empty state, not an error, when the Track has no lyrics", async () => {
    getLyrics.mockResolvedValue(null);
    render(<LyricsView titleId="t1" />);

    expect(await screen.findByTestId("lyrics-empty")).toHaveTextContent("No lyrics for this track yet.");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("Wrong lyrics", () => {
  const fetched: Lyrics = { ...synced, source: "fetched", id: "answer-1" };

  beforeEach(() => {
    getLyrics.mockReset();
    markLyricsWrong.mockReset();
    playback.currentId = null;
  });

  it("rejects the provider's answer on show, by its id, and shows what replaces it", async () => {
    getLyrics.mockResolvedValue(fetched);
    markLyricsWrong.mockResolvedValue({ ...plain, source: "fetched", text: "The right words" });
    render(<LyricsView titleId="t1" />);

    fireEvent.click(await screen.findByTestId("lyrics-reject-button"));
    expect(await screen.findByTestId("lyrics-plain")).toHaveTextContent("The right words");
    expect(markLyricsWrong).toHaveBeenCalledWith("t1", "answer-1");
    expect(screen.queryAllByTestId("lyric-line")).toHaveLength(0);
  });

  it("shows the empty state when no other answer is left", async () => {
    getLyrics.mockResolvedValue(fetched);
    markLyricsWrong.mockResolvedValue(null);
    render(<LyricsView titleId="t1" />);

    fireEvent.click(await screen.findByTestId("lyrics-reject-button"));
    expect(await screen.findByTestId("lyrics-empty")).toBeInTheDocument();
    expect(screen.queryByTestId("lyrics-reject-button")).toBeNull();
  });

  it("is not offered on Local lyrics", async () => {
    getLyrics.mockResolvedValue(synced);
    render(<LyricsView titleId="t1" />);

    await screen.findAllByTestId("lyric-line");
    expect(screen.queryByTestId("lyrics-reject-button")).toBeNull();
  });

  it("keeps the lyrics on screen and says so when the rejection fails", async () => {
    getLyrics.mockResolvedValue(fetched);
    markLyricsWrong.mockRejectedValue(new Error("server unreachable"));
    render(<LyricsView titleId="t1" />);

    fireEvent.click(await screen.findByTestId("lyrics-reject-button"));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getAllByTestId("lyric-line")).toHaveLength(3);
  });
});

describe("Track detail lyrics", () => {
  beforeEach(() => {
    getLyrics.mockReset();
    getTitle.mockReset();
  });

  it("opens the lyrics view on demand, asking for the lyrics only then", async () => {
    getTitle.mockResolvedValue(trackDetail("t1"));
    getLyrics.mockResolvedValue(plain);
    render(
      <MemoryRouter initialEntries={["/music/tracks/t1"]}>
        <Routes>
          <Route path="/music/tracks/:titleId" element={<TrackDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );

    const button = await screen.findByTestId("lyrics-button");
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(getLyrics).not.toHaveBeenCalled();
    expect(screen.queryByTestId("lyrics-view")).toBeNull();

    fireEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByTestId("lyrics-plain")).toHaveTextContent("Plain words");
    expect(getLyrics).toHaveBeenCalledWith("t1", expect.anything());

    fireEvent.click(button);
    expect(screen.queryByTestId("lyrics-view")).toBeNull();
  });
});

function trackDetail(id: string): TitleDetail {
  return {
    id,
    libraryId: "lib1",
    kind: "track",
    title: "Paranoid Android",
    year: 0,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    subtitles: [],
    editions: [],
    artwork: [],
    overview: "",
    tagline: "",
    contentRating: "",
    releaseDate: "",
    runtimeMinutes: 0,
    studio: "",
    genres: [],
    cast: [],
    enrichmentStatus: "",
    lockedFields: [],
    displayTitle: "",
    track: {
      artistId: "ar1",
      artistName: "Radiohead",
      albumId: "al1",
      albumTitle: "OK Computer",
      albumYear: 1997,
      discNumber: 1,
      trackNumber: 2,
    },
  };
}
