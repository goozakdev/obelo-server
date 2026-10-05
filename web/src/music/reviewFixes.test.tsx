import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes, useNavigate } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { AlbumTracks, ArtistAlbums, ArtistsPage } from "../api/types";

// Regression tests for the music web-code-review findings (R06-xx).

const { getLibrary, listArtists, getArtistAlbums, getAlbumTracks, listPlaylists } =
  vi.hoisted(() => ({
    getLibrary: vi.fn(),
    listArtists: vi.fn(),
    getArtistAlbums: vi.fn(),
    getAlbumTracks: vi.fn(),
    listPlaylists: vi.fn(),
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
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
    },
  };
});

import MusicLibraryScreen from "./MusicLibraryScreen";
import ArtistDetailScreen from "./ArtistDetailScreen";
import AlbumDetailScreen from "./AlbumDetailScreen";

const album = (id: string, title: string) => ({
  id,
  artistId: "ar1",
  artistName: "Radiohead",
  title,
  year: 1997,
  hasArtwork: false,
  trackCount: 2,
  releaseType: "album" as const,
  genres: [],
});
const track = (id: string, n: number) => ({
  id,
  kind: "track" as const,
  title: `Track ${id}`,
  discNumber: 1,
  trackNumber: n,
  durationMs: 1000,
  needsReview: false,
  resumePositionMs: 0,
  watched: false,
  overview: "",
});
const tracksOf = (albumId: string, ids: string[]): AlbumTracks => ({
  album: album(albumId, `Album ${albumId}`),
  tracks: ids.map((id, i) => track(id, i + 1)),
});

beforeEach(() => {
  for (const f of [getLibrary, listArtists, getArtistAlbums, getAlbumTracks, listPlaylists]) f.mockReset();
  listPlaylists.mockResolvedValue([]);
});

describe("R06-03 no guaranteed-404 artwork requests", () => {
  it("renders placeholders without <img> for artwork-less artists and albums", async () => {
    const page: ArtistsPage = {
      artists: [{ id: "ar2", libraryId: "lib1", kind: "artist", name: "Various", overview: "", genres: [] }],
      nextCursor: null,
    };
    listArtists.mockResolvedValue(page);
    getLibrary.mockResolvedValue({ id: "lib1", name: "Music", kind: "music", rootFolders: [] });
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );
    await screen.findByTestId("poster-grid");
    expect(screen.queryByTestId("poster-img")).toBeNull();
    expect(screen.getByTestId("poster-placeholder")).toBeInTheDocument();
  });

  it("album detail cover-less album shows the placeholder, not an <img>", async () => {
    getAlbumTracks.mockResolvedValue(tracksOf("al1", ["t1"]));
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
    await screen.findByTestId("album-detail");
    expect(screen.queryByTestId("poster-img")).toBeNull();
    expect(screen.getByTestId("poster-placeholder")).toBeInTheDocument();
  });
});

describe("R06-02 album play uses the tracks already loaded", () => {
  it("does not refetch /albums/{id}/tracks and queues from the chosen track", async () => {
    getAlbumTracks.mockResolvedValue(tracksOf("al1", ["t1", "t2", "t3"]));
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
    await screen.findByTestId("album-detail");
    expect(getAlbumTracks).toHaveBeenCalledTimes(1);
    const rows = screen.getAllByTestId("track-row");
    fireEvent.click(within(rows[1]).getByTestId("track-play"));
    await waitFor(() => expect(screen.getAllByTestId("track-row")[1]).toHaveClass("is-current"));
    expect(getAlbumTracks).toHaveBeenCalledTimes(1);
  });
});

describe("R06-07 album/artist id change", () => {
  function Hop({ to }: { to: string }) {
    const nav = useNavigate();
    return <button data-testid="hop" onClick={() => nav(to)} />;
  }

  it("does not keep showing the old album under the new id", async () => {
    getAlbumTracks.mockImplementation((id: string) =>
      id === "al1" ? Promise.resolve(tracksOf("al1", ["t1"])) : new Promise(() => {}),
    );
    renderWithAuth(
      <>
        <Hop to="/music/albums/al2" />
        <Routes>
          <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
        </Routes>
      </>,
      { initialEntries: ["/music/albums/al1"] },
    );
    await screen.findByTestId("album-detail");
    fireEvent.click(screen.getByTestId("hop"));
    await screen.findByTestId("album-loading");
    expect(screen.queryByTestId("album-detail")).toBeNull();
  });

  it("does not keep showing the old artist under the new id", async () => {
    getArtistAlbums.mockImplementation((id: string) =>
      id === "ar1"
        ? Promise.resolve({
            artist: { id: "ar1", libraryId: "lib1", kind: "artist", name: "Radiohead", overview: "", genres: [] },
            albums: [album("al1", "OK Computer")],
          } satisfies ArtistAlbums)
        : new Promise(() => {}),
    );
    renderWithAuth(
      <>
        <Hop to="/music/artists/ar2" />
        <Routes>
          <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
        </Routes>
      </>,
      { initialEntries: ["/music/artists/ar1"] },
    );
    await screen.findByTestId("artist-detail");
    await act(async () => {
      fireEvent.click(screen.getByTestId("hop"));
    });
    expect(screen.queryByTestId("artist-detail")).toBeNull();
  });
});

describe("R06-08/09 track menu playlists", () => {
  function renderAlbum() {
    getAlbumTracks.mockResolvedValue(tracksOf("al1", ["t1", "t2"]));
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
  }
  async function openSubmenu(row: number) {
    const rows = await screen.findAllByTestId("track-row");
    fireEvent.click(within(rows[row]).getByTestId("track-menu-toggle"));
    fireEvent.click(within(rows[row]).getByTestId("track-menu-add-playlist"));
  }

  it("fetches the playlists once for the whole album, not once per row", async () => {
    listPlaylists.mockResolvedValue([{ id: "pl1", name: "Roadtrip", kind: "music", itemCount: 1 }]);
    renderAlbum();
    await openSubmenu(0);
    await screen.findByTestId("track-menu-playlist-option");
    await openSubmenu(1);
    await waitFor(() => expect(screen.getAllByTestId("track-menu-playlist-option")).toHaveLength(2));
    expect(listPlaylists).toHaveBeenCalledTimes(1);
  });

  it("shows a load error with retry rather than 'No playlists'", async () => {
    listPlaylists.mockRejectedValueOnce(new Error("boom"));
    listPlaylists.mockResolvedValueOnce([{ id: "pl1", name: "Roadtrip", kind: "music", itemCount: 1 }]);
    renderAlbum();
    await openSubmenu(0);
    const err = await screen.findByTestId("track-menu-playlists-error");
    expect(err).toHaveTextContent("Couldn't load playlists");
    expect(screen.queryByText("No playlists")).toBeNull();
    fireEvent.click(screen.getByTestId("track-menu-playlists-retry"));
    await screen.findByTestId("track-menu-playlist-option");
  });
});

describe("R06-10 library name", () => {
  it("does not fetch GET /libraries/{id} for the header name", async () => {
    listArtists.mockResolvedValue({ artists: [], nextCursor: null });
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );
    await waitFor(() => expect(listArtists).toHaveBeenCalled());
    expect(getLibrary).not.toHaveBeenCalled();
  });
});
