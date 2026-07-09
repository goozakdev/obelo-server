import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { AlbumTracks, TitleDetail, WatchStateResult } from "../api/types";
import { useQueue } from "../player/queue/useQueue";

// The music Track detail (separate-music-ui): the play affordance builds the
// album-from-here Queue and opens the shared player; Add to queue / Play next
// mutate the shared Queue; the manual played/unplayed toggle hits the server. A
// non-track that lands here is redirected to the generic Title detail. Driven
// against a faked apiClient + the shared QueueProvider (renderWithAuth).

const { getTitle, getAlbumTracks, setWatchState } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  getAlbumTracks: vi.fn(),
  setWatchState: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      getAlbumTracks: (...a: unknown[]) => getAlbumTracks(...a),
      setWatchState: (...a: unknown[]) => setWatchState(...a),
    },
  };
});

import TrackDetailScreen from "./TrackDetailScreen";

function trackDetail(id: string, opts: Partial<TitleDetail> = {}): TitleDetail {
  return {
    id,
    kind: "track",
    title: "Paranoid Android",
    year: 0,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [
      {
        id: "ed1",
        name: "",
        files: [
          { id: "f1", path: "/music/x.flac", container: "flac", width: 0, height: 0, bitrate: 0, durationMs: 200000, sizeBytes: 0, missing: false, streams: [] },
        ],
      },
    ],
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
    ...opts,
  };
}

const albumTracks: AlbumTracks = {
  album: { id: "al1", artistId: "ar1", artistName: "Radiohead", title: "OK Computer", year: 1997, hasArtwork: false, trackCount: 3 },
  tracks: [1, 2, 3].map((n) => ({
    id: `t${n}`,
    kind: "track" as const,
    title: `Track ${n}`,
    discNumber: 1,
    trackNumber: n,
    needsReview: false,
    resumePositionMs: 0,
    watched: false,
  })),
};

// A probe over the shared Queue so tests assert observable Queue state.
function QueueProbe() {
  const q = useQueue();
  return (
    <ul data-testid="probe">
      {q.entries.map((e) => (
        <li key={e.entryId} data-testid="probe-entry" data-title-id={e.title.id}>
          {e.title.title}
        </li>
      ))}
    </ul>
  );
}

function probeTitleIds() {
  return screen.getAllByTestId("probe-entry").map((li) => li.getAttribute("data-title-id"));
}

function renderTrack(id = "t2") {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/music/tracks/:titleId" element={<TrackDetailScreen />} />
        <Route path="/titles/:titleId" element={<div data-testid="generic-detail" />} />
      </Routes>
      <QueueProbe />
    </>,
    { initialEntries: [`/music/tracks/${id}`] },
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue(trackDetail("t2"));
  getAlbumTracks.mockReset().mockResolvedValue(albumTracks);
  setWatchState.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("Music TrackDetailScreen", () => {
  it("renders the Artist · Album context and the play affordance", async () => {
    renderTrack();
    await screen.findByTestId("detail");
    const ctx = screen.getByTestId("track-context");
    expect(ctx).toHaveTextContent("Radiohead");
    expect(ctx).toHaveTextContent("OK Computer");
    expect(screen.getByTestId("play-button")).toBeEnabled();
    // It's inside the music shell (which uses the shared app header).
    expect(screen.getByTestId("current-user")).toBeInTheDocument();
  });

  it("Play builds the album-from-here Queue (playback starts in the bar)", async () => {
    renderTrack();
    await screen.findByTestId("detail");

    fireEvent.click(screen.getByTestId("play-button"));

    // Album-from-here: t2, t3 (sliced from the chosen track). Playback begins in
    // the persistent Now Playing bar — no navigation.
    await waitFor(() => expect(probeTitleIds()).toEqual(["t2", "t3"]));
    expect(getAlbumTracks).toHaveBeenCalledWith("al1", undefined);
  });

  it("Add to queue appends the Track to the END of the Queue", async () => {
    renderTrack();
    await screen.findByTestId("detail");

    fireEvent.click(screen.getByTestId("add-to-queue-button"));
    await waitFor(() => expect(probeTitleIds()).toEqual(["t2"]));
    expect(screen.getByTestId("queue-notice")).toBeInTheDocument();
  });

  it("the manual toggle writes the played state to the server", async () => {
    const next: WatchStateResult = { titleId: "t2", resumePositionMs: 0, watched: true };
    setWatchState.mockResolvedValue(next);

    renderTrack();
    await screen.findByTestId("detail");
    expect(screen.getByTestId("watch-unwatched")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("watch-toggle"));
    await waitFor(() => expect(setWatchState).toHaveBeenCalledWith("t2", true));
    await screen.findByTestId("watch-watched");
  });

  it("redirects a non-track to the generic Title detail", async () => {
    getTitle.mockResolvedValue(trackDetail("m1", { kind: "movie", track: undefined }));
    renderTrack("m1");
    await screen.findByTestId("generic-detail");
  });
});
