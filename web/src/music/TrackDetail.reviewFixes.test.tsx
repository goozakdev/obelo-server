import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { AlbumTracks, TitleDetail } from "../api/types";
import { useQueue } from "../player/queue/useQueue";

// Regression tests for R06-05 (latest-wins Play) and R06-11 (album cover on the
// track hero).

const { getTitle, getAlbumTracks } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  getAlbumTracks: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      getAlbumTracks: (...a: unknown[]) => getAlbumTracks(...a),
    },
  };
});

import TrackDetailScreen from "./TrackDetailScreen";

const detail = {
  id: "t2",
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
  editions: [
    {
      id: "ed1",
      name: "",
      files: [{ id: "f1", path: "/m.flac", container: "flac", width: 0, height: 0, bitrate: 0, durationMs: 1, sizeBytes: 0, missing: false, streams: [], audioStreams: [], videoStreams: [] }],
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
  track: { artistId: "ar1", artistName: "Radiohead", albumId: "al1", albumTitle: "OK Computer", albumYear: 1997, discNumber: 1, trackNumber: 2 },
} as unknown as TitleDetail;

function tracks(ids: string[]): AlbumTracks {
  return {
    album: { id: "al1", artistId: "ar1", artistName: "Radiohead", title: "OK Computer", year: 1997, hasArtwork: false, trackCount: ids.length, releaseType: "album", genres: [] },
    tracks: ids.map((id, i) => ({ id, kind: "track" as const, title: id, discNumber: 1, trackNumber: i + 1, durationMs: 1, needsReview: false, resumePositionMs: 0, watched: false, overview: "" })),
  };
}

function QueueProbe() {
  const q = useQueue();
  return (
    <ul>
      {q.entries.map((e) => (
        <li key={e.entryId} data-testid="probe-entry" data-title-id={e.title.id} />
      ))}
    </ul>
  );
}

function renderTrack() {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/music/tracks/:titleId" element={<TrackDetailScreen />} />
      </Routes>
      <QueueProbe />
    </>,
    { initialEntries: ["/music/tracks/t2"] },
  );
}

beforeEach(() => {
  getTitle.mockReset().mockResolvedValue(detail);
  getAlbumTracks.mockReset();
});

describe("R06-05 Play is latest-wins", () => {
  it("a slow earlier Play cannot overwrite the Queue from a later one", async () => {
    let resolveSlow!: (v: AlbumTracks) => void;
    getAlbumTracks
      .mockImplementationOnce(() => new Promise<AlbumTracks>((r) => (resolveSlow = r)))
      .mockResolvedValueOnce(tracks(["t3"]));
    renderTrack();
    await screen.findByTestId("detail");

    fireEvent.click(screen.getByTestId("play-button"));
    fireEvent.click(screen.getByTestId("play-button"));
    await waitFor(() =>
      expect(screen.getAllByTestId("probe-entry").map((e) => e.getAttribute("data-title-id"))).toEqual(["t3"]),
    );
    resolveSlow(tracks(["t2", "t3"]));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getAllByTestId("probe-entry").map((e) => e.getAttribute("data-title-id"))).toEqual(["t3"]);
  });
});

describe("R06-11 track hero artwork", () => {
  it("requests the album cover rather than the track's own poster role", async () => {
    renderTrack();
    const img = await screen.findByTestId("poster-img");
    expect(img.getAttribute("src")).toBe("/api/v1/albums/al1/artwork");
  });
});
