import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { ArtistAlbums, ArtistsPage, Library } from "../api/types";
import { layoutModeKey } from "../browse/browseLayout";

// The browse layout switch (appletv-web-parity §5) on the two MUSIC browse grids —
// the Artists wall (MusicLibraryScreen) and an Artist's Albums (ArtistDetailScreen)
// — proving the toggle is wired there too and redraws the already-loaded list
// without a refetch. (The Movie grid + store round-trip are covered in browse/.)

const { getLibrary, listArtists, getArtistAlbums, subscribeEvents } = vi.hoisted(() => ({
  getLibrary: vi.fn(),
  listArtists: vi.fn(),
  getArtistAlbums: vi.fn(),
  subscribeEvents: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      listArtists: (...a: unknown[]) => listArtists(...a),
      getArtistAlbums: (...a: unknown[]) => getArtistAlbums(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import MusicLibraryScreen from "./MusicLibraryScreen";
import ArtistDetailScreen from "./ArtistDetailScreen";

const musicLib: Library = { id: "lib1", name: "Music", kind: "music", rootFolders: [] };

const artistsPage: ArtistsPage = {
  artists: [
    { id: "ar1", kind: "artist", name: "Radiohead", genres: ["Alt Rock"] } as ArtistsPage["artists"][number],
    { id: "ar2", kind: "artist", name: "Portishead", genres: [] } as ArtistsPage["artists"][number],
  ],
  nextCursor: null,
};

const radioheadAlbums: ArtistAlbums = {
  // A libraryId so the Album grid can key its layout per (Music) Library.
  artist: { id: "ar1", kind: "artist", name: "Radiohead", libraryId: "lib1" } as ArtistAlbums["artist"],
  albums: [
    { id: "al1", artistId: "ar1", artistName: "Radiohead", title: "OK Computer", year: 1997, hasArtwork: false, trackCount: 12, genres: [] } as ArtistAlbums["albums"][number],
  ],
};

beforeEach(() => {
  window.localStorage.clear();
  getLibrary.mockReset();
  listArtists.mockReset();
  getArtistAlbums.mockReset();
  subscribeEvents.mockReset();
  getLibrary.mockResolvedValue(musicLib);
  listArtists.mockResolvedValue(artistsPage);
  getArtistAlbums.mockResolvedValue(radioheadAlbums);
  subscribeEvents.mockImplementation(() => () => {});
});

describe("music browse layout switch", () => {
  it("toggles the Artists wall to List without refetching", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "tile");
    expect(listArtists).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByTestId("layout-list"));
    expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "list");
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);
    // Same list — no second read.
    expect(listArtists).toHaveBeenCalledTimes(1);
    expect(window.localStorage.getItem(layoutModeKey("lib1"))).toBe("list");
  });

  it("toggles an Artist's Albums to Detail without refetching", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/artists/ar1"] },
    );
    await waitFor(() => expect(screen.getByTestId("album-grid")).toBeInTheDocument());
    expect(screen.getByTestId("album-grid")).toHaveAttribute("data-layout", "tile");
    expect(getArtistAlbums).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByTestId("layout-detail"));
    expect(screen.getByTestId("album-grid")).toHaveAttribute("data-layout", "detail");
    // The row shows the album's already-loaded facts (year · track count).
    expect(screen.getByTestId("browse-row-meta")).toHaveTextContent("1997 · 12 tracks");
    expect(getArtistAlbums).toHaveBeenCalledTimes(1);
    // Persisted under the Artist's owning Music Library.
    expect(window.localStorage.getItem(layoutModeKey("lib1"))).toBe("detail");
  });
});
