import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type {
  AlbumTracks,
  ArtistAlbums,
  ArtistsPage,
  Library,
  TitleDetail,
} from "../api/types";

// The MUSIC half of issue 15: Artist rows, Album rows and Track rows out of a
// mirror wear the same badge and the same greying as everything else. Music is
// where the mark is easiest to lose — an Album's Library is its ARTIST's, and a
// queue built from a Track list outlives the screen it was built on — which is
// why issue 14 put the pair on `albumJSON` and `trackSummaryJSON` in the first
// place.
//
// The browse (movie/TV) surfaces are the counterpart file,
// src/browse/linkedRows.test.tsx.

const {
  getLibrary,
  listArtists,
  getArtistAlbums,
  getAlbumTracks,
  getTitle,
  listPlaylists,
  listLibraries,
  listLinks,
} = vi.hoisted(() => ({
  getLibrary: vi.fn(),
  listArtists: vi.fn(),
  getArtistAlbums: vi.fn(),
  getAlbumTracks: vi.fn(),
  getTitle: vi.fn(),
  listPlaylists: vi.fn(),
  listLibraries: vi.fn(),
  listLinks: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      listArtists: (...a: unknown[]) => listArtists(...a),
      getArtistAlbums: (...a: unknown[]) => getArtistAlbums(...a),
      getAlbumTracks: (...a: unknown[]) => getAlbumTracks(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      listLinks: (...a: unknown[]) => listLinks(...a),
    },
  };
});

import MusicLibraryScreen from "./MusicLibraryScreen";
import ArtistDetailScreen from "./ArtistDetailScreen";
import AlbumDetailScreen from "./AlbumDetailScreen";
import TrackDetailScreen from "./TrackDetailScreen";

const musicLib: Library = { id: "lib1", name: "Music", kind: "music", rootFolders: [] };

const MIRRORED_ARTIST = {
  id: "ar1",
  kind: "artist",
  name: "Borrowed Band",
  libraryId: "lib1",
  overview: "",
  genres: [],
  linked: true,
  available: true,
};
const LOCAL_ARTIST = {
  id: "ar2",
  kind: "artist",
  name: "Our Band",
  libraryId: "lib1",
  overview: "",
  genres: [],
};

function album(id: string, title: string, marks = {}) {
  return {
    id,
    artistId: "ar1",
    artistName: "Borrowed Band",
    title,
    year: 1997,
    hasArtwork: false,
    releaseType: "",
    trackCount: 1,
    genres: [],
    ...marks,
  };
}

function track(id: string, title: string, n: number, marks = {}) {
  return {
    id,
    kind: "track",
    title,
    discNumber: 1,
    trackNumber: n,
    durationMs: 60000,
    needsReview: false,
    resumePositionMs: 0,
    watched: false,
    overview: "",
    ...marks,
  };
}

beforeEach(() => {
  for (const fn of [
    getLibrary,
    listArtists,
    getArtistAlbums,
    getAlbumTracks,
    getTitle,
    listPlaylists,
    listLibraries,
    listLinks,
  ]) {
    fn.mockReset();
  }
  listLibraries.mockResolvedValue([]);
  listLinks.mockResolvedValue([]);
  listPlaylists.mockResolvedValue([]);
});

describe("the Artist list", () => {
  it("badges a mirrored Artist and not the local one beside it", async () => {
    getLibrary.mockResolvedValue(musicLib);
    listArtists.mockResolvedValue({
      artists: [MIRRORED_ARTIST, LOCAL_ARTIST],
      nextCursor: null,
    } as ArtistsPage);
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());

    const all = screen.getAllByTestId("poster-tile");
    const mirrored = all.find((t) => t.getAttribute("data-artist-id") === "ar1")!;
    const local = all.find((t) => t.getAttribute("data-artist-id") === "ar2")!;
    expect(within(mirrored).getByTestId("linked-badge")).toHaveTextContent("Linked");
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });
});

describe("the Artist detail", () => {
  it("badges the header and every mirrored Album under it", async () => {
    getArtistAlbums.mockResolvedValue({
      artist: MIRRORED_ARTIST,
      albums: [
        album("al1", "Borrowed LP", { linked: true, available: true }),
        album("al2", "Our LP"),
      ],
    } as unknown as ArtistAlbums);
    renderWithAuth(
      <Routes>
        <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/artists/ar1"] },
    );
    await waitFor(() => expect(screen.getByTestId("artist-detail")).toBeInTheDocument());

    expect(screen.getByTestId("artist-linked-badge")).toHaveTextContent("Linked");
    const all = screen.getAllByTestId("poster-tile");
    const mirrored = all.find((t) => t.getAttribute("data-album-id") === "al1")!;
    const local = all.find((t) => t.getAttribute("data-album-id") === "al2")!;
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });

  it("greys an Album whose sharer is away, leaving it on the shelf and openable", async () => {
    getArtistAlbums.mockResolvedValue({
      artist: { ...MIRRORED_ARTIST, available: false },
      albums: [album("al1", "Borrowed LP", { linked: true, available: false })],
    } as unknown as ArtistAlbums);
    renderWithAuth(
      <Routes>
        <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/artists/ar1"] },
    );
    await waitFor(() => expect(screen.getByTestId("artist-detail")).toBeInTheDocument());

    const tile = screen.getByTestId("poster-tile");
    expect(tile.className).toContain("is-unavailable");
    expect(within(tile).getByRole("link")).toHaveAttribute("href", "/music/albums/al1");
    expect(screen.getByTestId("artist-linked-badge")).toHaveAttribute(
      "data-available",
      "false",
    );
  });
});

describe("the Album detail", () => {
  function render() {
    return renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
  }

  it("badges the Album header and each mirrored Track row", async () => {
    getAlbumTracks.mockResolvedValue({
      album: album("al1", "Borrowed LP", { linked: true, available: true }),
      tracks: [
        track("tr1", "Airbag", 1, { linked: true, available: true }),
        track("tr2", "A local B-side", 2),
      ],
    } as unknown as AlbumTracks);
    render();
    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());

    expect(screen.getByTestId("album-linked-badge")).toHaveTextContent("Linked");
    const rows = screen.getAllByTestId("track-row");
    const mirrored = rows.find((r) => r.getAttribute("data-track-id") === "tr1")!;
    const local = rows.find((r) => r.getAttribute("data-track-id") === "tr2")!;
    expect(within(mirrored).getByTestId("track-linked-badge")).toBeInTheDocument();
    expect(within(local).queryByTestId("track-linked-badge")).toBeNull();
  });

  it("greys an unreachable Track row and keeps its play control", async () => {
    getAlbumTracks.mockResolvedValue({
      album: album("al1", "Borrowed LP", { linked: true, available: false }),
      tracks: [track("tr1", "Airbag", 1, { linked: true, available: false })],
    } as unknown as AlbumTracks);
    render();
    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());

    const row = screen.getByTestId("track-row");
    expect(row.className).toContain("is-unavailable");
    // Greyed, never disabled — the honest "unreachable" sentence belongs at play
    // time, and the player already says it (issue 10).
    expect(within(row).getByTestId("track-play")).toBeEnabled();
  });

  it("leaves a wholly local Album with no mark anywhere", async () => {
    getAlbumTracks.mockResolvedValue({
      album: album("al1", "Our LP"),
      tracks: [track("tr1", "Ours", 1)],
    } as unknown as AlbumTracks);
    render();
    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());

    expect(screen.queryByTestId("album-linked-badge")).toBeNull();
    expect(screen.queryByTestId("track-linked-badge")).toBeNull();
    expect(document.querySelectorAll(".is-unavailable")).toHaveLength(0);
  });
});

describe("the Track detail", () => {
  const detail: TitleDetail = {
    id: "tr1",
    libraryId: "lib1",
    kind: "track",
    title: "Airbag",
    year: 1997,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [],
    artwork: [],
    subtitles: [],
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
    track: { artistId: "ar1", artistName: "Borrowed Band", albumId: "al1", albumTitle: "Borrowed LP" },
  } as TitleDetail;

  function render() {
    return renderWithAuth(
      <Routes>
        <Route path="/music/tracks/:titleId" element={<TrackDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/tracks/tr1"] },
    );
  }

  it("derives the mark from the Track's Library, which the detail document lacks", async () => {
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([
      { ...musicLib, linked: true, available: true },
    ]);
    render();
    await waitFor(() =>
      expect(screen.getByTestId("detail-linked-badge")).toHaveTextContent("Linked"),
    );
  });

  it("shows nothing for a Track in an ordinary local Library", async () => {
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([musicLib]);
    render();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("detail-linked-badge")).toBeNull();
  });
});
